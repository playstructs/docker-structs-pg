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

// structAttributeHandler ports cache.handle_event_struct_attribute, final
// form in cache-trigger-add-queue-20260207-add-destroyed-block.sql:5-93.
//
// attributeId grammar: "{attrType}-{objectTypeId}-{objectIndex}[-{subIndex}]"
//
//   - attrType        (split_part 1) — 0..6 (label table below).
//   - objectTypeId    (split_part 2) — 0..11 (objecttype labels).
//   - objectIndex     (split_part 3) — numeric index for the struct.
//   - subIndex        (split_part 4) — optional, defaults to 0.
//
// Two branches keyed on payload.value:
//
//   - value == "" or numeric 0 → DELETE structs.struct_attribute. If the
//     row was actually deleted (rowcount > 0) AND attrType == 0 (health),
//     also append a `value=0` row to stat_struct_health (status's stat
//     delete-side is intentionally commented out in 20260203 — we match).
//
//   - else → UPSERT structs.struct_attribute with the `IS DISTINCT FROM val`
//     guard. If the upsert actually wrote (rowcount > 0):
//
//   - attrType 0 (health) → append to stat_struct_health.
//   - attrType 1 (status) → append to stat_struct_status AND, if
//     bit 32 (destroyed flag) is set, UPDATE structs.struct
//     setting is_destroyed=true and destroyed_block=bctx.Height.
//
// Note on destroyed_block: the SQL reads
// `(SELECT height FROM structs.current_block LIMIT 1)`. We use
// bctx.Height instead — it's the height of the block currently being
// processed, which is exactly what current_block will be set to once
// the per-block tx commits. No race, no extra round trip.
type structAttributeHandler struct{}

func (structAttributeHandler) CompositeKey() string {
	return "structs.structs.EventStructAttribute.structAttributeRecord"
}

// structAttrLabels mirrors the SQL CASE on lines 57-65 of the 20260207
// migration. Indexed by attrType (split_part 1).
var structAttrLabels = [...]string{
	"health",               // 0
	"status",               // 1
	"blockStartBuild",      // 2
	"blockStartOreMine",    // 3
	"blockStartOreRefine",  // 4
	"protectedStructIndex", // 5
	"typeCount",            // 6
}

// structDestroyedBit is the status bitmask that, when set, indicates the
// struct has been destroyed. Same constant the SQL uses on line 82
// (`(value & 32) > 0`) and the same bit view-struct.sql:51 keys
// `view.struct_status.destroyed` off of.
const structDestroyedBit = 32

const structAttributeDeleteSQL = `DELETE FROM structs.struct_attribute WHERE id = $1`

const structAttributeUpsertSQL = `
INSERT INTO structs.struct_attribute (
    id, object_id, object_type, sub_index, attribute_type, val, updated_at
) VALUES ($1, $2, $3, $4, $5, $6, NOW())
ON CONFLICT (id) DO UPDATE
   SET val        = EXCLUDED.val,
       updated_at = EXCLUDED.updated_at
 WHERE structs.struct_attribute.val IS DISTINCT FROM EXCLUDED.val`

const (
	statStructHealthInsertSQL = `INSERT INTO structs.stat_struct_health (time, object_index, value, block_height) VALUES (NOW(), $1, $2, $3)`
	statStructStatusInsertSQL = `INSERT INTO structs.stat_struct_status (time, object_index, value, block_height) VALUES (NOW(), $1, $2, $3)`
)

// structDestroyedUpdateSQL applies the destruction stamp when a status
// upsert lands a value with bit 32 set. No IS DISTINCT FROM guard — the
// SQL doesn't have one either; the rowcount gate on the outer
// struct_attribute write is what keeps it idempotent.
const structDestroyedUpdateSQL = `
UPDATE structs.struct
   SET is_destroyed     = TRUE,
       destroyed_block  = $2
 WHERE id = $1`

// structAttrPrevSelectSQL grabs the existing val so we can pass OLD.val
// to the planet_activity emit functions, mirroring the dropped SQL
// trigger's access to OLD.val.
const structAttrPrevSelectSQL = `SELECT val FROM structs.struct_attribute WHERE id = $1`

func (structAttributeHandler) Handle(ctx context.Context, tx pgx.Tx, bctx BlockContext, raw json.RawMessage) error {
	p, err := payload.Decode[payload.StructAttribute](raw)
	if err != nil {
		return err
	}
	if p.AttributeID == "" {
		return fmt.Errorf("struct_attribute: empty attributeId")
	}
	attrType, objTypeID, objIndex, subIndex, err := parseStructAttributeID(p.AttributeID)
	if err != nil {
		return fmt.Errorf("struct_attribute: %w", err)
	}

	var oldVal int64
	if err := tx.QueryRow(ctx, structAttrPrevSelectSQL, p.AttributeID).Scan(&oldVal); err != nil && !errors.Is(err, pgx.ErrNoRows) {
		return fmt.Errorf("struct_attribute prev id=%s: %w", p.AttributeID, err)
	}

	rawObjectID := strconv.Itoa(objTypeID) + "-" + strconv.Itoa(objIndex)

	// Delete branch: SQL is `IF v.value = '' OR (v.value)::INTEGER = 0`.
	// Empty parses as zero-rows for the int route, so test it separately.
	if p.Value == "" {
		return structAttrDelete(ctx, tx, bctx, p.AttributeID, attrType, objIndex, rawObjectID, oldVal)
	}
	val, err := strconv.ParseInt(p.Value, 10, 64)
	if err != nil {
		return fmt.Errorf("struct_attribute: invalid val %q for id=%s: %w", p.Value, p.AttributeID, err)
	}
	if val == 0 {
		return structAttrDelete(ctx, tx, bctx, p.AttributeID, attrType, objIndex, rawObjectID, oldVal)
	}

	objTypeLabel := ""
	if k, ok := objecttype.FromID(objTypeID); ok {
		objTypeLabel = k.Label()
	}
	attrLabel := structAttrLabelFor(attrType)

	tag, err := tx.Exec(ctx, structAttributeUpsertSQL,
		p.AttributeID,
		rawObjectID,
		nullIfEmpty(objTypeLabel),
		subIndex,
		nullIfEmpty(attrLabel),
		val,
	)
	if err != nil {
		return fmt.Errorf("struct_attribute upsert id=%s: %w", p.AttributeID, err)
	}
	if tag.RowsAffected() == 0 {
		return nil
	}

	switch attrType {
	case 0: // health
		if _, err := tx.Exec(ctx, statStructHealthInsertSQL, objIndex, val, bctx.Height); err != nil {
			return fmt.Errorf("struct_attribute stat_struct_health id=%s: %w", p.AttributeID, err)
		}
	case 1: // status
		if _, err := tx.Exec(ctx, statStructStatusInsertSQL, objIndex, val, bctx.Height); err != nil {
			return fmt.Errorf("struct_attribute stat_struct_status id=%s: %w", p.AttributeID, err)
		}
		if val&structDestroyedBit != 0 {
			if _, err := tx.Exec(ctx, structDestroyedUpdateSQL, rawObjectID, bctx.Height); err != nil {
				return fmt.Errorf("struct_attribute destroy stamp id=%s: %w", rawObjectID, err)
			}
		}
	}

	if err := emitStructAttributeActivity(ctx, tx, bctx, attrType, oldVal, val, rawObjectID); err != nil {
		return fmt.Errorf("struct_attribute: emit activity id=%s: %w", p.AttributeID, err)
	}
	return nil
}

// structAttrDelete handles the "value is empty or zero" branch. Mirrors
// SQL lines 21-33 of the 20260207 migration: only attrType 0 (health)
// writes a sentinel `value=0` to its stat hypertable on delete; status's
// stat insert is commented out in 20260203 and we don't second-guess it.
//
// Activity emit on DELETE: the SQL function references NEW.attribute_type
// inside the TG_OP='DELETE' branch — but on DELETE, NEW is NULL in PG.
// So the CASE never matches and the DELETE branch is effectively dead
// code in the SQL trigger. We FIX this by using attrType (which we
// already parsed from the attributeId) — the protectedStructIndex case
// emits a struct_defense_remove using OLD.val. This is the SQL's
// clearly-intended behavior; documenting the divergence.
func structAttrDelete(ctx context.Context, tx pgx.Tx, bctx BlockContext, attributeID string, attrType, objIndex int, rawObjectID string, oldVal int64) error {
	tag, err := tx.Exec(ctx, structAttributeDeleteSQL, attributeID)
	if err != nil {
		return fmt.Errorf("struct_attribute delete id=%s: %w", attributeID, err)
	}
	if tag.RowsAffected() == 0 {
		return nil
	}
	if attrType == 0 {
		if _, err := tx.Exec(ctx, statStructHealthInsertSQL, objIndex, int64(0), bctx.Height); err != nil {
			return fmt.Errorf("struct_attribute stat_struct_health (delete branch) id=%s: %w", attributeID, err)
		}
	}
	if attrType == 5 && oldVal > 0 {
		// protectedStructIndex delete → fixed-bug emit (see comment above).
		if err := emitStructDefenseRemove(ctx, tx, bctx, oldVal, rawObjectID); err != nil {
			return fmt.Errorf("struct_attribute defense_remove (delete) id=%s: %w", attributeID, err)
		}
	}
	return nil
}

// emitStructAttributeActivity is the INSERT/UPDATE side of
// PLANET_ACTIVITY_STRUCT_ATTRIBUTE (cache-trigger-planet-activity-
// struct-attribute-20260118-fix-bad-location.sql:50-132). Each case is
// the SQL trigger's per-attribute_type branch, ported 1:1 with the
// IS NOT NULL guard the SQL added in the 20260118 fix.
//
// Note: the SQL omits a struct_health row in the DELETE branch and
// in the val-zero UPSERT path (we send "value=0" only as a stat).
// We match.
func emitStructAttributeActivity(ctx context.Context, tx pgx.Tx, bctx BlockContext, attrType int, oldVal, newVal int64, structID string) error {
	switch attrType {
	case 0: // health
		return emitStructAttributeOnStructPlanet(ctx, tx, bctx, structID, "struct_health", map[string]any{
			"struct_id":  structID,
			"health":     newVal,
			"health_old": oldVal,
		})
	case 1: // status
		return emitStructAttributeOnStructPlanet(ctx, tx, bctx, structID, "struct_status", map[string]any{
			"struct_id":  structID,
			"status":     newVal,
			"status_old": oldVal,
		})
	case 2: // blockStartBuild
		return emitStructAttributeOnStructPlanet(ctx, tx, bctx, structID, "struct_block_build_start", map[string]any{
			"struct_id": structID,
			"block":     newVal,
		})
	case 3: // blockStartOreMine
		return emitStructAttributeOnStructPlanet(ctx, tx, bctx, structID, "struct_block_ore_mine_start", map[string]any{
			"struct_id": structID,
			"block":     newVal,
		})
	case 4: // blockStartOreRefine
		return emitStructAttributeOnStructPlanet(ctx, tx, bctx, structID, "struct_block_ore_refine_start", map[string]any{
			"struct_id": structID,
			"block":     newVal,
		})
	case 5: // protectedStructIndex
		if oldVal > 0 {
			if err := emitStructDefenseRemove(ctx, tx, bctx, oldVal, structID); err != nil {
				return err
			}
		}
		if newVal > 0 {
			if err := emitStructDefenseAdd(ctx, tx, bctx, newVal, structID); err != nil {
				return err
			}
		}
	case 6: // typeCount — SQL says "Nothing to do here"
	}
	return nil
}

const structAttrActivityInsertSQL = `
INSERT INTO structs.planet_activity (time, seq, planet_id, category, detail, block_height)
VALUES ($1, $2, $3, $4, $5::jsonb, $6)`

// emitStructAttributeOnStructPlanet anchors the activity on the planet
// the struct currently sits on. Uses getActivityLocationID (Go port of
// structs.GET_ACTIVITY_LOCATION_ID). Honors the SQL's IS NOT NULL guard:
// if the struct isn't yet known to the indexer (no planet resolution),
// skip the emit cleanly — SQL would NULL-trip and silently lose the row.
func emitStructAttributeOnStructPlanet(ctx context.Context, tx pgx.Tx, bctx BlockContext, structID, category string, detail map[string]any) error {
	planetID, err := getActivityLocationID(ctx, tx, structID)
	if err != nil {
		return fmt.Errorf("resolve planet for struct=%s: %w", structID, err)
	}
	if planetID == "" {
		return nil
	}
	return insertStructAttrActivity(ctx, tx, bctx, planetID, category, detail)
}

// emitStructDefenseRemove / emitStructDefenseAdd emit defense events
// anchored on the planet of the *protected* struct (not the defender).
// The protected struct ID is reconstructed as "5-{val}" — same string
// concat the SQL does (`'5-' || OLD.val` / `'5-' || NEW.val`).
func emitStructDefenseRemove(ctx context.Context, tx pgx.Tx, bctx BlockContext, protectedIndex int64, defenderStructID string) error {
	protectedStructID := "5-" + strconv.FormatInt(protectedIndex, 10)
	planetID, err := getActivityLocationID(ctx, tx, protectedStructID)
	if err != nil {
		return fmt.Errorf("resolve planet for protected=%s: %w", protectedStructID, err)
	}
	if planetID == "" {
		return nil
	}
	return insertStructAttrActivity(ctx, tx, bctx, planetID, "struct_defense_remove", map[string]any{
		"defender_struct_id":  defenderStructID,
		"protected_struct_id": protectedStructID,
	})
}

func emitStructDefenseAdd(ctx context.Context, tx pgx.Tx, bctx BlockContext, protectedIndex int64, defenderStructID string) error {
	protectedStructID := "5-" + strconv.FormatInt(protectedIndex, 10)
	planetID, err := getActivityLocationID(ctx, tx, protectedStructID)
	if err != nil {
		return fmt.Errorf("resolve planet for protected=%s: %w", protectedStructID, err)
	}
	if planetID == "" {
		return nil
	}
	return insertStructAttrActivity(ctx, tx, bctx, planetID, "struct_defense_add", map[string]any{
		"defender_struct_id":  defenderStructID,
		"protected_struct_id": protectedStructID,
	})
}

func insertStructAttrActivity(ctx context.Context, tx pgx.Tx, bctx BlockContext, planetID, category string, detail map[string]any) error {
	seq, err := nextPlanetActivitySeq(ctx, tx, planetID)
	if err != nil {
		return fmt.Errorf("seq planet=%s: %w", planetID, err)
	}
	detailJSON, err := json.Marshal(detail)
	if err != nil {
		return fmt.Errorf("detail marshal: %w", err)
	}
	if _, err := tx.Exec(ctx, structAttrActivityInsertSQL,
		bctx.BlockTime.UTC(),
		seq,
		planetID,
		category,
		detailJSON,
		bctx.Height,
	); err != nil {
		return fmt.Errorf("insert planet_activity planet=%s category=%s: %w", planetID, category, err)
	}
	return nil
}

// parseStructAttributeID splits "{attrType}-{objTypeId}-{objIndex}[-{subIndex}]"
// into ints. subIndex is optional and defaults to 0 (matches the SQL CASE
// on line 55: `WHEN '' THEN 0 ELSE … END`).
func parseStructAttributeID(id string) (attrType, objTypeID, objIndex, subIndex int, err error) {
	a := splitPart(id, "-", 1)
	b := splitPart(id, "-", 2)
	c := splitPart(id, "-", 3)
	d := splitPart(id, "-", 4)
	if a == "" || b == "" || c == "" {
		return 0, 0, 0, 0, fmt.Errorf("attributeId %q: expected at least three dash-separated parts", id)
	}
	if attrType, err = strconv.Atoi(a); err != nil {
		return 0, 0, 0, 0, fmt.Errorf("attributeId %q: attrType: %w", id, err)
	}
	if objTypeID, err = strconv.Atoi(b); err != nil {
		return 0, 0, 0, 0, fmt.Errorf("attributeId %q: objTypeId: %w", id, err)
	}
	if objIndex, err = strconv.Atoi(c); err != nil {
		return 0, 0, 0, 0, fmt.Errorf("attributeId %q: objIndex: %w", id, err)
	}
	if d == "" {
		subIndex = 0
	} else if subIndex, err = strconv.Atoi(d); err != nil {
		return 0, 0, 0, 0, fmt.Errorf("attributeId %q: subIndex: %w", id, err)
	}
	return
}

// structAttrLabelFor returns the SQL CASE label for the given attrType,
// or "" if out of range (paired with nullIfEmpty for SQL-NULL parity).
func structAttrLabelFor(attrType int) string {
	if attrType < 0 || attrType >= len(structAttrLabels) {
		return ""
	}
	return structAttrLabels[attrType]
}
