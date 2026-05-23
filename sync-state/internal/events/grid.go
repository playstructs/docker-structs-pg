package events

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strconv"

	"github.com/jackc/pgx/v5"

	"sync-state/internal/buffers"
	"sync-state/internal/objecttype"
	"sync-state/internal/payload"
)

// gridHandler ports cache.handle_event_grid
// (cache-trigger-add-queue-20260121-bigly-refactor.sql:1683-1804).
//
// attributeId grammar: "{gridAttributeType}-{objectTypeId}-{objectIndex}"
//
//   - gridAttributeType (split_part 1) — 0..14 (label table below).
//   - objectTypeId      (split_part 2) — 0..11, maps to structs.object_type
//     labels (objecttype package).
//   - objectIndex       (split_part 3) — numeric index for the object.
//
// Two branches keyed on payload.value:
//
//   - value == ""  → DELETE structs.grid. If a row was actually deleted
//     (rowcount > 0) AND subIdx ∈ 0..7, also insert a `value=0` sentinel
//     into the matching stat_* hypertable so consumers see the zero-out.
//
//   - else         → UPSERT structs.grid with the `IS DISTINCT FROM val`
//     guard. If the upsert actually wrote (rowcount > 0) AND subIdx ∈ 0..7,
//     also append the new `value=N` row to the stat_* hypertable.
//
// Sub-indexes 8..14 are grid-only (no stat partner). Anything outside
// 0..14 is silently accepted as a grid row with NULL attribute_type
// (matches SQL's CASE … END returning NULL for unknown prefixes).
type gridHandler struct{}

func (gridHandler) CompositeKey() string {
	return "structs.structs.EventGrid.gridRecord"
}

// gridAttrLabels mirrors the SQL CASE on lines 1731-1747 of
// bigly-refactor.sql. Indexed by the integer prefix; entries 8..14 are
// grid-only and have no stat_* partner.
var gridAttrLabels = [...]string{
	"ore",                    // 0
	"fuel",                   // 1
	"capacity",               // 2
	"load",                   // 3
	"structsLoad",            // 4
	"power",                  // 5
	"connectionCapacity",     // 6
	"connectionCount",        // 7
	"allocationPointerStart", // 8
	"allocationPointerEnd",   // 9
	"proxyNonce",             // 10
	"lastAction",             // 11
	"nonce",                  // 12
	"ready",                  // 13
	"checkpointBlock",        // 14
}

const gridDeleteSQL = `DELETE FROM structs.grid WHERE id = $1`

const gridUpsertSQL = `
INSERT INTO structs.grid (
    id, attribute_type, object_type, object_index, object_id, val, updated_at
) VALUES ($1, $2, $3, $4, $5, $6, NOW())
ON CONFLICT (id) DO UPDATE
   SET val        = EXCLUDED.val,
       updated_at = EXCLUDED.updated_at
 WHERE structs.grid.val IS DISTINCT FROM EXCLUDED.val`

// Stat rows for sub-indexes 0..7 are buffered (see internal/buffers) and
// flushed via pgx.CopyFrom at end-of-block / end-of-window. The legacy
// per-row INSERT SQL was removed when the bulk-COPY path landed.
//
// Phase 2 also fixed a long-standing replay smell: stat_* used `time =
// NOW()` instead of the block time. The buffer path now stamps each row
// with bctx.BlockTime so backfills and replays land in the correct
// TimescaleDB chunk and `time` is comparable to other height/time
// columns.

func (gridHandler) Handle(ctx context.Context, tx pgx.Tx, bctx BlockContext, raw json.RawMessage) error {
	p, err := payload.Decode[payload.Grid](raw)
	if err != nil {
		return err
	}
	if p.AttributeID == "" {
		return fmt.Errorf("%w: grid: empty attributeId", ErrSkipWithWarn)
	}
	subIdx, objTypeID, objIndex, err := parseGridAttributeID(p.AttributeID)
	if err != nil {
		if errors.Is(err, errGridAttrMissingParts) {
			return fmt.Errorf("%w: grid: %v", ErrSkipWithWarn, err)
		}
		return fmt.Errorf("grid: %w", err)
	}

	if p.Value == "" {
		tag, err := tx.Exec(ctx, gridDeleteSQL, p.AttributeID)
		if err != nil {
			return fmt.Errorf("grid delete id=%s: %w", p.AttributeID, err)
		}
		if tag.RowsAffected() > 0 {
			if err := writeGridStat(bctx, subIdx, objTypeID, objIndex, 0); err != nil {
				return fmt.Errorf("grid stat (delete branch) id=%s: %w", p.AttributeID, err)
			}
		}
		return nil
	}

	val, err := strconv.ParseInt(p.Value, 10, 64)
	if err != nil {
		return fmt.Errorf("grid: invalid val %q for id=%s: %w", p.Value, p.AttributeID, err)
	}

	attrLabel := gridLabelFor(subIdx)
	objTypeLabel := ""
	if k, ok := objecttype.FromID(objTypeID); ok {
		objTypeLabel = k.Label()
	}
	// Same string-concat the SQL does on line 1765: split_part(2) || '-' ||
	// split_part(3). Built from the parsed ints so we don't carry through
	// any unexpected whitespace from the chain payload.
	rawObjectID := strconv.Itoa(objTypeID) + "-" + strconv.Itoa(objIndex)

	tag, err := tx.Exec(ctx, gridUpsertSQL,
		p.AttributeID,
		nullIfEmpty(attrLabel),
		nullIfEmpty(objTypeLabel),
		objIndex,
		rawObjectID,
		val,
	)
	if err != nil {
		return fmt.Errorf("grid upsert id=%s: %w", p.AttributeID, err)
	}
	if tag.RowsAffected() > 0 {
		if err := writeGridStat(bctx, subIdx, objTypeID, objIndex, val); err != nil {
			return fmt.Errorf("grid stat (upsert branch) id=%s: %w", p.AttributeID, err)
		}
	}
	return nil
}

// errGridAttrMissingParts indicates the attributeId has one or more empty
// dash-separated segments (e.g. "2-", "-3-4", ""). These show up in genesis
// state and the SQL trigger silently accepts them — Handle returns
// ErrSkipWithWarn on this so the row is recorded as severity='warn'
// instead of erroring the per-block tx.
var errGridAttrMissingParts = errors.New("attributeId missing one or more dash-separated parts")

// parseGridAttributeID splits "{subIdx}-{objTypeId}-{objIndex}" into ints.
//
// Two error modes:
//   - errGridAttrMissingParts — one or more segments empty. Caller should
//     downgrade to ErrSkipWithWarn (matches SQL behavior, which would
//     INSERT with NULL fields for the missing parts).
//   - any other error — non-numeric segment. The chain emits the id from
//     a "%d-%d-%d" format string (see table-grid.sql:5), so a non-numeric
//     segment is a real chain bug we want to surface as severity='error'.
func parseGridAttributeID(id string) (subIdx, objTypeID, objIndex int, err error) {
	a := splitPart(id, "-", 1)
	b := splitPart(id, "-", 2)
	c := splitPart(id, "-", 3)
	if a == "" || b == "" || c == "" {
		return 0, 0, 0, fmt.Errorf("attributeId %q: %w", id, errGridAttrMissingParts)
	}
	if subIdx, err = strconv.Atoi(a); err != nil {
		return 0, 0, 0, fmt.Errorf("attributeId %q: subIdx: %w", id, err)
	}
	if objTypeID, err = strconv.Atoi(b); err != nil {
		return 0, 0, 0, fmt.Errorf("attributeId %q: objTypeId: %w", id, err)
	}
	if objIndex, err = strconv.Atoi(c); err != nil {
		return 0, 0, 0, fmt.Errorf("attributeId %q: objIndex: %w", id, err)
	}
	return
}

// gridLabelFor returns the SQL CASE label for the given sub-index, or ""
// if out of range (the SQL CASE has no ELSE so it returns NULL — we
// pair "" with nullIfEmpty at the call site to mirror that exactly).
func gridLabelFor(subIdx int) string {
	if subIdx < 0 || subIdx >= len(gridAttrLabels) {
		return ""
	}
	return gridAttrLabels[subIdx]
}

// writeGridStat buffers the stat_* row for sub-indexes 0..7. 8..14 (and
// anything else) are no-ops — the SQL CASE has no ELSE clause for those.
// Sub-indexes that bind object_type (NOT NULL on the stat hypertables)
// require objTypeID to be a valid 0..11 enum id; otherwise we surface the
// error rather than buffer a NULL into a NOT NULL column.
//
// blockHeight and time both come from bctx so replays + bulk windows
// land the correct historical block in the right TimescaleDB chunk.
func writeGridStat(bctx BlockContext, subIdx, objTypeID, objIndex int, val int64) error {
	t := bctx.BlockTime.UTC()
	switch subIdx {
	case 0:
		ot, err := requireObjectTypeLabel(objTypeID)
		if err != nil {
			return err
		}
		bctx.Buf.StatOre = append(bctx.Buf.StatOre, buffers.StatRow{Time: t, ObjectType: &ot, ObjectIndex: objIndex, Value: val, BlockHeight: bctx.Height})
	case 1:
		ot, err := requireObjectTypeLabel(objTypeID)
		if err != nil {
			return err
		}
		bctx.Buf.StatFuel = append(bctx.Buf.StatFuel, buffers.StatRow{Time: t, ObjectType: &ot, ObjectIndex: objIndex, Value: val, BlockHeight: bctx.Height})
	case 2:
		ot, err := requireObjectTypeLabel(objTypeID)
		if err != nil {
			return err
		}
		bctx.Buf.StatCapacity = append(bctx.Buf.StatCapacity, buffers.StatRow{Time: t, ObjectType: &ot, ObjectIndex: objIndex, Value: val, BlockHeight: bctx.Height})
	case 3:
		ot, err := requireObjectTypeLabel(objTypeID)
		if err != nil {
			return err
		}
		bctx.Buf.StatLoad = append(bctx.Buf.StatLoad, buffers.StatRow{Time: t, ObjectType: &ot, ObjectIndex: objIndex, Value: val, BlockHeight: bctx.Height})
	case 4:
		bctx.Buf.StatStructsLoad = append(bctx.Buf.StatStructsLoad, buffers.StatRow{Time: t, ObjectIndex: objIndex, Value: val, BlockHeight: bctx.Height})
	case 5:
		ot, err := requireObjectTypeLabel(objTypeID)
		if err != nil {
			return err
		}
		bctx.Buf.StatPower = append(bctx.Buf.StatPower, buffers.StatRow{Time: t, ObjectType: &ot, ObjectIndex: objIndex, Value: val, BlockHeight: bctx.Height})
	case 6:
		bctx.Buf.StatConnectionCapacity = append(bctx.Buf.StatConnectionCapacity, buffers.StatRow{Time: t, ObjectIndex: objIndex, Value: val, BlockHeight: bctx.Height})
	case 7:
		bctx.Buf.StatConnectionCount = append(bctx.Buf.StatConnectionCount, buffers.StatRow{Time: t, ObjectIndex: objIndex, Value: val, BlockHeight: bctx.Height})
	}
	return nil
}

// requireObjectTypeLabel maps an objecttype id to its enum label, or
// errors if it's out of range. Used by stat branches that bind a NOT NULL
// object_type column.
func requireObjectTypeLabel(objTypeID int) (string, error) {
	k, ok := objecttype.FromID(objTypeID)
	if !ok {
		return "", fmt.Errorf("object_type id %d out of range", objTypeID)
	}
	return k.Label(), nil
}
