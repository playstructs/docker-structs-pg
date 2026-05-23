package events

import (
	"context"
	"encoding/json"
	"fmt"
	"strconv"

	"github.com/jackc/pgx/v5"

	"sync-state/internal/objecttype"
	"sync-state/internal/payload"
)

// planetAttributeHandler ports cache.handle_event_planet_attribute, final
// form in cache-trigger-add-queue-20260203-add-new-events.sql:95-158.
//
// Simplest of the three Phase 4 handlers — one table, no stat side effects,
// no `structs.planet` write. The planet_activity derivation is owned by
// the table trigger that follows in Phase 6.
//
// attributeId grammar: "{attrType}-{objectTypeId}-{objectIndex}"
//
//   - attrType     (split_part 1) — 0..10 (label table below).
//   - objectTypeId (split_part 2) — 0..11 (objecttype labels).
//   - objectIndex  (split_part 3) — numeric index for the planet.
//
// Two branches keyed on payload.value:
//
//   - value == "" or numeric 0 → DELETE structs.planet_attribute.
//   - else                     → UPSERT structs.planet_attribute with the
//     `IS DISTINCT FROM val` guard.
type planetAttributeHandler struct{}

func (planetAttributeHandler) CompositeKey() string {
	return "structs.structs.EventPlanetAttribute.planetAttributeRecord"
}

// planetAttrLabels mirrors the SQL CASE on lines 133-145 of the 20260203
// migration. Indexed by attrType (split_part 1).
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
}

const planetAttributeDeleteSQL = `DELETE FROM structs.planet_attribute WHERE id = $1`

const planetAttributeUpsertSQL = `
INSERT INTO structs.planet_attribute (
    id, object_id, object_type, attribute_type, val, updated_at
) VALUES ($1, $2, $3, $4, $5, NOW())
ON CONFLICT (id) DO UPDATE
   SET val        = EXCLUDED.val,
       updated_at = EXCLUDED.updated_at
 WHERE structs.planet_attribute.val IS DISTINCT FROM EXCLUDED.val`

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

	if p.Value == "" {
		if _, err := tx.Exec(ctx, planetAttributeDeleteSQL, p.AttributeID); err != nil {
			return fmt.Errorf("planet_attribute delete id=%s: %w", p.AttributeID, err)
		}
		return nil
	}
	val, err := strconv.ParseInt(p.Value, 10, 64)
	if err != nil {
		return fmt.Errorf("planet_attribute: invalid val %q for id=%s: %w", p.Value, p.AttributeID, err)
	}
	if val == 0 {
		if _, err := tx.Exec(ctx, planetAttributeDeleteSQL, p.AttributeID); err != nil {
			return fmt.Errorf("planet_attribute delete id=%s: %w", p.AttributeID, err)
		}
		return nil
	}

	objTypeLabel := ""
	if k, ok := objecttype.FromID(objTypeID); ok {
		objTypeLabel = k.Label()
	}
	attrLabel := planetAttrLabelFor(attrType)
	rawObjectID := strconv.Itoa(objTypeID) + "-" + strconv.Itoa(objIndex)

	if _, err := tx.Exec(ctx, planetAttributeUpsertSQL,
		p.AttributeID,
		rawObjectID,
		nullIfEmpty(objTypeLabel),
		nullIfEmpty(attrLabel),
		val,
	); err != nil {
		return fmt.Errorf("planet_attribute upsert id=%s: %w", p.AttributeID, err)
	}
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
