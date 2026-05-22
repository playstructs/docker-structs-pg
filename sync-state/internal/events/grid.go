package events

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strconv"

	"github.com/jackc/pgx/v5"

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

// Stat-table inserts, one per sub-index 0..7. Pre-built so the dispatch
// is dead-simple and PG's plan cache gets stable text. Sub-indexes 4, 6,
// 7 omit the object_type column to match table-stat.sql:99-130.
//
// Every INSERT names its columns explicitly (rather than positional) so
// the Phase A4 addition of block_height doesn't depend on the table's
// physical column order. block_height comes from bctx.Height at the call
// site so replays land the correct historical block.
const (
	statOreInsertSQL                = `INSERT INTO structs.stat_ore (time, object_type, object_index, value, block_height) VALUES (NOW(), $1::structs.object_type, $2, $3, $4)`
	statFuelInsertSQL               = `INSERT INTO structs.stat_fuel (time, object_type, object_index, value, block_height) VALUES (NOW(), $1::structs.object_type, $2, $3, $4)`
	statCapacityInsertSQL           = `INSERT INTO structs.stat_capacity (time, object_type, object_index, value, block_height) VALUES (NOW(), $1::structs.object_type, $2, $3, $4)`
	statLoadInsertSQL               = `INSERT INTO structs.stat_load (time, object_type, object_index, value, block_height) VALUES (NOW(), $1::structs.object_type, $2, $3, $4)`
	statStructsLoadInsertSQL        = `INSERT INTO structs.stat_structs_load (time, object_index, value, block_height) VALUES (NOW(), $1, $2, $3)`
	statPowerInsertSQL              = `INSERT INTO structs.stat_power (time, object_type, object_index, value, block_height) VALUES (NOW(), $1::structs.object_type, $2, $3, $4)`
	statConnectionCapacityInsertSQL = `INSERT INTO structs.stat_connection_capacity (time, object_index, value, block_height) VALUES (NOW(), $1, $2, $3)`
	statConnectionCountInsertSQL    = `INSERT INTO structs.stat_connection_count (time, object_index, value, block_height) VALUES (NOW(), $1, $2, $3)`
)

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
			if err := writeGridStat(ctx, tx, bctx.Height, subIdx, objTypeID, objIndex, 0); err != nil {
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
		if err := writeGridStat(ctx, tx, bctx.Height, subIdx, objTypeID, objIndex, val); err != nil {
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

// writeGridStat inserts the stat_* row for sub-indexes 0..7. 8..14 (and
// anything else) are no-ops — the SQL CASE has no ELSE clause for those.
// Sub-indexes that bind object_type (NOT NULL on the stat hypertables)
// require objTypeID to be a valid 0..11 enum id; otherwise we surface the
// error rather than insert NULL into a NOT NULL column.
//
// blockHeight is plumbed through every branch so the new
// stat_*.block_height column lands a non-NULL value on every write
// (Phase A4 — feeds the verify check that asserts NULL counts trend
// toward zero).
func writeGridStat(ctx context.Context, tx pgx.Tx, blockHeight int64, subIdx, objTypeID, objIndex int, val int64) error {
	switch subIdx {
	case 0:
		t, err := requireObjectTypeLabel(objTypeID)
		if err != nil {
			return err
		}
		_, err = tx.Exec(ctx, statOreInsertSQL, t, objIndex, val, blockHeight)
		return err
	case 1:
		t, err := requireObjectTypeLabel(objTypeID)
		if err != nil {
			return err
		}
		_, err = tx.Exec(ctx, statFuelInsertSQL, t, objIndex, val, blockHeight)
		return err
	case 2:
		t, err := requireObjectTypeLabel(objTypeID)
		if err != nil {
			return err
		}
		_, err = tx.Exec(ctx, statCapacityInsertSQL, t, objIndex, val, blockHeight)
		return err
	case 3:
		t, err := requireObjectTypeLabel(objTypeID)
		if err != nil {
			return err
		}
		_, err = tx.Exec(ctx, statLoadInsertSQL, t, objIndex, val, blockHeight)
		return err
	case 4:
		_, err := tx.Exec(ctx, statStructsLoadInsertSQL, objIndex, val, blockHeight)
		return err
	case 5:
		t, err := requireObjectTypeLabel(objTypeID)
		if err != nil {
			return err
		}
		_, err = tx.Exec(ctx, statPowerInsertSQL, t, objIndex, val, blockHeight)
		return err
	case 6:
		_, err := tx.Exec(ctx, statConnectionCapacityInsertSQL, objIndex, val, blockHeight)
		return err
	case 7:
		_, err := tx.Exec(ctx, statConnectionCountInsertSQL, objIndex, val, blockHeight)
		return err
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
