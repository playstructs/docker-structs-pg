package events

import (
	"context"
	"fmt"

	"github.com/jackc/pgx/v5"
)

// The empty entity key is the family reset watermark. It prevents an old
// snapshot from resurrecting a row omitted by a later authoritative reset.
func economyEventApplied(ctx context.Context, tx pgx.Tx, b BlockContext, family, id string) (bool, error) {
	var applied bool
	err := tx.QueryRow(ctx, `SELECT EXISTS (
        SELECT 1 FROM sync_state.economy_event_position
         WHERE family=$1 AND entity_id IN ('', $2)
           AND (height,phase,tx_index,event_index) >= ($3,$4,$5,$6))`,
		family, id, b.Height, int16(b.PositionPhase()), b.TxIndex, b.EventIndex).Scan(&applied)
	return applied, err
}

func recordEconomyPosition(ctx context.Context, tx pgx.Tx, b BlockContext, family, id string) error {
	_, err := tx.Exec(ctx, `INSERT INTO sync_state.economy_event_position
        (family,entity_id,height,phase,tx_index,event_index) VALUES ($1,$2,$3,$4,$5,$6)
        ON CONFLICT (family,entity_id) DO UPDATE SET height=EXCLUDED.height,
        phase=EXCLUDED.phase,tx_index=EXCLUDED.tx_index,event_index=EXCLUDED.event_index`,
		family, id, b.Height, int16(b.PositionPhase()), b.TxIndex, b.EventIndex)
	return err
}

// resetEconomy clears only rows at/before the reset. Newer rows survive a
// historical retry, and the reset watermark protects omitted entities. Table
// identifiers are compile-time choices, never derived from a chain attribute.
func resetEconomy(ctx context.Context, tx pgx.Tx, b BlockContext, family string) error {
	applied, err := economyEventApplied(ctx, tx, b, family, "")
	if err != nil || applied {
		return err
	}
	var statement string
	switch family {
	case "diversity":
		statement = `DELETE FROM structs.owner_reactor_diversity d WHERE NOT EXISTS (
            SELECT 1 FROM sync_state.economy_event_position p
             WHERE p.family=$1 AND p.entity_id=d.owner_id
               AND (p.height,p.phase,p.tx_index,p.event_index) > ($2,$3,$4,$5)) RETURNING owner_id`
	case "commission":
		statement = `DELETE FROM structs.reactor_commission_revision d WHERE NOT EXISTS (
            SELECT 1 FROM sync_state.economy_event_position p
             WHERE p.family=$1 AND p.entity_id=d.reactor_id
               AND (p.height,p.phase,p.tx_index,p.event_index) > ($2,$3,$4,$5)) RETURNING reactor_id`
	default:
		return fmt.Errorf("unknown economy reset family %q", family)
	}
	rows, err := tx.Query(ctx, statement, family, b.Height, int16(b.PositionPhase()), b.TxIndex, b.EventIndex)
	if err != nil {
		return err
	}
	defer rows.Close()
	for rows.Next() {
		var id string
		if err := rows.Scan(&id); err != nil {
			return err
		}
		if family == "commission" {
			b.Dirty.Reactor(id)
		} else {
			dirtyOwner(b, id)
		}
	}
	if err := rows.Err(); err != nil {
		return err
	}
	if _, err := tx.Exec(ctx, `DELETE FROM sync_state.economy_event_position
        WHERE family=$1 AND (height,phase,tx_index,event_index) <= ($2,$3,$4,$5)`,
		family, b.Height, int16(b.PositionPhase()), b.TxIndex, b.EventIndex); err != nil {
		return err
	}
	return recordEconomyPosition(ctx, tx, b, family, "")
}
