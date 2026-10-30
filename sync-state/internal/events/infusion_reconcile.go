package events

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"sort"
	"strings"

	"github.com/jackc/pgx/v5"

	"sync-state/internal/payload"
	"sync-state/internal/rpc"
)

// ReconcileLegacyInfusionPositions resolves unknown phases from a complete
// block, never from an individual retried event. The caller must hold the
// writer and maintenance locks and verify that this is the indexed cursor's
// exact block hash. A stored legacy position must identify the final mutation
// in execution order, and its row must match that snapshot (or deletion).
// Only then can we adopt its phase and previously unhandled extension fields.
// No fuel delta is emitted: the snapshot has already been applied.
func ReconcileLegacyInfusionPositions(ctx context.Context, tx pgx.Tx, bctx BlockContext, ordered []rpc.PositionedEvent) (int, error) {
	type mutation struct {
		position rpc.PositionedEvent
		infusion payload.Infusion
		deleted  bool
	}
	last := make(map[string]mutation)
	for _, positioned := range ordered {
		ev := positioned.Event
		attribute := ""
		switch ev.Type {
		case "structs.structs.EventInfusion":
			attribute = "infusion"
		case "structs.structs.EventDelete":
			attribute = "objectId"
		default:
			continue
		}
		var raw json.RawMessage
		for _, attr := range ev.Attributes {
			if attr.Key == attribute {
				if raw != nil {
					return 0, fmt.Errorf("duplicate %s.%s during infusion reconciliation", ev.Type, attribute)
				}
				raw = json.RawMessage(attr.Value)
			}
		}
		m := mutation{position: positioned}
		if attribute == "infusion" {
			p, err := payload.DecodeVersioned[payload.Infusion](raw, bctx.V022Active(), "ownerId", "generatorDisabled")
			if err != nil {
				return 0, err
			}
			if _, err := payload.DecodeRequired[payload.Infusion](raw, "destinationId", "address", "destinationType",
				"playerId", "fuel", "defusing", "power", "ratio", "commission"); err != nil {
				return 0, err
			}
			if p.DestinationID == "" || p.Address == "" || (p.OwnerID != "" && !ownerIDPattern.MatchString(p.OwnerID)) {
				return 0, fmt.Errorf("invalid infusion snapshot during reconciliation")
			}
			m.infusion = p
		} else {
			id, err := decodeDeleteObjectID(raw)
			if err != nil {
				return 0, err
			}
			if strings.Count(id, "-") < 2 {
				continue
			}
			parts := strings.Split(id, "-")
			if len(parts) != 3 || !regexpInfusionDestination.MatchString(parts[0]+"-"+parts[1]) || parts[2] == "" {
				return 0, fmt.Errorf("invalid infusion deletion %q during reconciliation", id)
			}
			m.infusion = payload.Infusion{DestinationID: parts[0] + "-" + parts[1], Address: parts[2]}
			m.deleted = true
		}
		last[m.infusion.DestinationID+"/"+m.infusion.Address] = m
	}
	keys := make([]string, 0, len(last))
	for key := range last {
		keys = append(keys, key)
	}
	sort.Strings(keys)
	reconciled := 0
	for _, key := range keys {
		m := last[key]
		p := m.infusion
		var height int64
		var txIndex, eventIndex int
		var phase *int16
		err := tx.QueryRow(ctx, infusionPositionSelectSQL+" FOR UPDATE", p.DestinationID, p.Address).
			Scan(&height, &txIndex, &eventIndex, &phase)
		if errors.Is(err, pgx.ErrNoRows) {
			continue
		}
		if err != nil {
			return reconciled, err
		}
		if height != bctx.Height || phase != nil {
			continue
		}
		if txIndex != m.position.TxIndex || eventIndex != m.position.EventIndex {
			return reconciled, fmt.Errorf("legacy infusion %s does not identify the block's final mutation; restore a complete earlier boundary", key)
		}
		var matches bool
		if m.deleted {
			err = tx.QueryRow(ctx, `SELECT NOT EXISTS (SELECT 1 FROM structs.infusion
WHERE destination_id=$1 AND address=$2)`, p.DestinationID, p.Address).Scan(&matches)
		} else {
			err = tx.QueryRow(ctx, `SELECT EXISTS (SELECT 1 FROM structs.infusion
WHERE destination_id=$1 AND address=$2
AND destination_type IS NOT DISTINCT FROM $3::text AND player_id IS NOT DISTINCT FROM $4::text
AND fuel_p IS NOT DISTINCT FROM $5::numeric AND defusing_p IS NOT DISTINCT FROM $6::numeric
AND power_p IS NOT DISTINCT FROM $7::numeric AND ratio_p IS NOT DISTINCT FROM $8::numeric
AND commission IS NOT DISTINCT FROM $9::numeric
AND (owner_id IS NULL OR owner_id='' OR owner_id=$10)
AND (NOT generator_disabled OR generator_disabled=$11))`, p.DestinationID, p.Address, p.DestinationType, p.PlayerID,
				p.Fuel.PgValue(), p.Defusing.PgValue(), p.Power.PgValue(), p.Ratio.PgValue(), p.Commission.PgValue(),
				p.OwnerID, p.GeneratorDisabled).Scan(&matches)
		}
		if err != nil {
			return reconciled, err
		}
		if !matches {
			return reconciled, fmt.Errorf("legacy infusion %s disagrees with the final source snapshot; refusing to infer its phase", key)
		}
		if !m.deleted {
			var previousOwner string
			if err := tx.QueryRow(ctx, `UPDATE structs.infusion SET owner_id=$3,generator_disabled=$4
WHERE destination_id=$1 AND address=$2 RETURNING player_id`, p.DestinationID, p.Address,
				p.OwnerID, p.GeneratorDisabled).Scan(&previousOwner); err != nil {
				return reconciled, err
			}
			dirtyOwner(bctx, previousOwner)
			dirtyOwner(bctx, p.AssetOwnerID())
		}
		if _, err := tx.Exec(ctx, infusionPositionUpsertSQL, p.DestinationID, p.Address,
			bctx.Height, m.position.TxIndex, m.position.EventIndex, int16(m.position.Phase)); err != nil {
			return reconciled, err
		}
		bctx.Dirty.Address(p.Address)
		if strings.HasPrefix(p.DestinationID, "3-") {
			bctx.Dirty.Reactor(p.DestinationID)
			bctx.Dirty.NativeAddress(p.Address)
		}
		reconciled++
	}
	var unresolved int
	if err := tx.QueryRow(ctx, `SELECT count(*) FROM sync_state.infusion_event_position
WHERE height=$1 AND phase IS NULL`, bctx.Height).Scan(&unresolved); err != nil {
		return reconciled, err
	}
	if unresolved != 0 {
		return reconciled, fmt.Errorf("%d legacy infusion positions at height %d have no proven final mutation in the source block", unresolved, bctx.Height)
	}
	return reconciled, nil
}
