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

// planetAttributeHandler ports cache.handle_event_planet_attribute, final
// form in cache-trigger-add-queue-20260203-add-new-events.sql:95-158.
//
// One table, no stat side effects, no `structs.planet` write.
//
// v0.18.0 planet_activity derivation: there is no dedicated chain event for
// the new shield_change / block_raid_start categories, so this handler emits
// them from the planetaryShield (attrType 0) and blockStartRaid (attrType 10)
// attributes. The upsert returns the prior structs.planet_attribute.val
// and we emit a timeline row carrying both the old and new value whenever it
// changes (including a change to zero). planet_id is the attribute's own
// object id ("2-<index>").
//
// v0.21.0: ore mine/refine clocks moved from struct attrs 3/4 onto the
// planet (attrTypes 12/13). Those emit the existing
// struct_block_ore_mine_start / struct_block_ore_refine_start categories
// with detail {planet_id, block}. Raid-pause (11) and active-quantity
// (14/15) are indexed only — no grass emit.
//
// attributeId grammar: "{attrType}-{objectTypeId}-{objectIndex}"
//
//   - attrType     (split_part 1) — 0..15 (label table below).
//   - objectTypeId (split_part 2) — 0..11 (objecttype labels).
//   - objectIndex  (split_part 3) — numeric index for the planet.
//
// "" or numeric 0 → UPSERT val=0 (keep-zero: "absent" must not mean both
// never-indexed and cleared, and updated_since must see clears).
type planetAttributeHandler struct{}

func (planetAttributeHandler) CompositeKey() string {
	return "structs.structs.EventPlanetAttribute.planetAttributeRecord"
}

// planetAttrLabels is indexed by attrType (split_part 1). Types 0–10
// match the 20260203 SQL CASE. Types 11–15 are v0.21.0 ore clocks
// (stored string keys, not proto enum names — proto uses
// planetBlockStartOreMine / planetBlockStartOreRefine because the
// struct-type names were already taken).
var planetAttrLabels = [...]string{
	"planetaryShield",
	"repairNetworkQuantity",
	"defensiveCannonQuantity",
	"coordinatedGlobalShieldNetworkQuantity",
	"lowOrbitBallisticsInterceptorNetworkQuantity",
	"advancedLowOrbitBallisticsInterceptorNetworkQuantity",
	"lowOrbitBallisticsInterceptorNetworkSuccessRateNumerator",
	"lowOrbitBallisticsInterceptorNetworkSuccessRateDenominator",
	"orbitalJammingStationQuantity",
	"advancedOrbitalJammingStationQuantity",
	"blockStartRaid",
	"blockRaiderArrived",
	"blockStartOreMine",
	"blockStartOreRefine",
	"oreMiningActiveQuantity",
	"oreRefiningActiveQuantity",
}

// PostgreSQL 18 returns OLD.val atomically with the write, avoiding a
// separate lookup. A new row starts at zero; unchanged rows return no result.
const planetAttributeUpsertSQL = `
INSERT INTO structs.planet_attribute (
    id, object_id, object_type, attribute_type, val, updated_at
) VALUES ($1, $2, $3, $4, $5, NOW())
ON CONFLICT (id) DO UPDATE
   SET val        = EXCLUDED.val,
       updated_at = EXCLUDED.updated_at
 WHERE structs.planet_attribute.val IS DISTINCT FROM EXCLUDED.val
 RETURNING CASE WHEN OLD.id IS NULL THEN '0' ELSE trunc(OLD.val)::text END`

func (planetAttributeHandler) Handle(ctx context.Context, tx pgx.Tx, bctx BlockContext, raw json.RawMessage) error {
	p, err := payload.Decode[payload.PlanetAttribute](raw)
	if err != nil {
		return err
	}
	if p.AttributeID == "" {
		return fmt.Errorf("planet_attribute: empty attributeId")
	}
	attrType, objTypeID, objIndex, err := parsePlanetAttributeID(p.AttributeID)
	if err != nil {
		return fmt.Errorf("planet_attribute: %w", err)
	}

	// Empty or "0" → newVal 0; keep the row so updated_since sees clears.
	newVal, err := parseAttributeValue(p.Value)
	if err != nil {
		return fmt.Errorf("planet_attribute: invalid val %q for id=%s: %w", p.Value, p.AttributeID, err)
	}

	objTypeLabel := ""
	if k, ok := objecttype.FromID(objTypeID); ok {
		objTypeLabel = k.Label()
	}
	attrLabel := planetAttrLabelFor(attrType)
	rawObjectID := strconv.Itoa(objTypeID) + "-" + strconv.Itoa(objIndex)

	var oldText string
	err = tx.QueryRow(ctx, planetAttributeUpsertSQL,
		p.AttributeID,
		rawObjectID,
		nullIfEmpty(objTypeLabel),
		nullIfEmpty(attrLabel),
		payload.Uint64(newVal).PgValue(),
	).Scan(&oldText)
	if errors.Is(err, pgx.ErrNoRows) {
		return nil
	}
	if err != nil {
		return fmt.Errorf("planet_attribute upsert id=%s: %w", p.AttributeID, err)
	}

	oldVal, err := parseAttributeValue(oldText)
	if err != nil {
		return fmt.Errorf("planet_attribute prev val id=%s: %w", p.AttributeID, err)
	}

	if err := emitPlanetAttributeActivity(ctx, tx, bctx, attrType, objTypeID, objIndex, oldVal, newVal); err != nil {
		return fmt.Errorf("planet_attribute: emit planet_activity id=%s: %w", p.AttributeID, err)
	}
	return nil
}

// planet_attribute attrType values that drive a planet_activity timeline row
// (see planetAttrLabels for the full label map). Types 11 / 14 / 15 are
// indexed but do not emit.
const (
	planetAttrTypePlanetaryShield     = 0
	planetAttrTypeBlockStartRaid      = 10
	planetAttrTypeBlockStartOreMine   = 12
	planetAttrTypeBlockStartOreRefine = 13
)

// emitPlanetAttributeActivity buffers a planet_activity row when a
// tracked planet-scoped attribute changes. Shield / raid carry old+new
// values; ore clocks (v0.21.0) reuse the existing grass categories with
// detail {planet_id, block}. Any other attribute, non-planet object, or
// no-op change is skipped.
func emitPlanetAttributeActivity(ctx context.Context, tx pgx.Tx, bctx BlockContext, attrType, objTypeID, objIndex int, oldVal, newVal uint64) error {
	if oldVal == newVal {
		return nil
	}
	if objTypeID != int(objecttype.Planet) {
		return nil
	}

	planetID := objecttype.Format(objecttype.Planet, objIndex)
	var category string
	var detailMap map[string]any
	switch attrType {
	case planetAttrTypePlanetaryShield:
		category = "shield_change"
		detailMap = map[string]any{
			"planetary_shield":     newVal,
			"planetary_shield_old": oldVal,
		}
	case planetAttrTypeBlockStartRaid:
		category = "block_raid_start"
		detailMap = map[string]any{
			"block_start_raid":     newVal,
			"block_start_raid_old": oldVal,
		}
	case planetAttrTypeBlockStartOreMine:
		category = "struct_block_ore_mine_start"
		detailMap = map[string]any{
			"planet_id": planetID,
			"block":     newVal,
		}
	case planetAttrTypeBlockStartOreRefine:
		category = "struct_block_ore_refine_start"
		detailMap = map[string]any{
			"planet_id": planetID,
			"block":     newVal,
		}
	default:
		return nil
	}

	detail, err := json.Marshal(detailMap)
	if err != nil {
		return fmt.Errorf("detail marshal: %w", err)
	}
	bctx.Buf.PlanetActivity = append(bctx.Buf.PlanetActivity, buffers.PlanetActivityRow{
		Time:        bctx.BlockTime.UTC(),
		PlanetID:    planetID,
		Category:    category,
		Detail:      detail,
		BlockHeight: bctx.Height,
	})
	return nil
}

// parsePlanetAttributeID splits "{attrType}-{objTypeId}-{objIndex}" into
// ints. Mirrors handle_event_planet_attribute's split_part chain.
func parsePlanetAttributeID(id string) (attrType, objTypeID, objIndex int, err error) {
	a := splitPart(id, "-", 1)
	b := splitPart(id, "-", 2)
	c := splitPart(id, "-", 3)
	if a == "" || b == "" || c == "" {
		return 0, 0, 0, fmt.Errorf("attributeId %q: expected three dash-separated parts", id)
	}
	if attrType, err = strconv.Atoi(a); err != nil {
		return 0, 0, 0, fmt.Errorf("attributeId %q: attrType: %w", id, err)
	}
	if objTypeID, err = strconv.Atoi(b); err != nil {
		return 0, 0, 0, fmt.Errorf("attributeId %q: objTypeId: %w", id, err)
	}
	if objIndex, err = strconv.Atoi(c); err != nil {
		return 0, 0, 0, fmt.Errorf("attributeId %q: objIndex: %w", id, err)
	}
	return
}

// planetAttrLabelFor returns the SQL CASE label for the given attrType,
// or "" if out of range (paired with nullIfEmpty for SQL-NULL parity).
func planetAttrLabelFor(attrType int) string {
	if attrType < 0 || attrType >= len(planetAttrLabels) {
		return ""
	}
	return planetAttrLabels[attrType]
}
