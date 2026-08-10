package events

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"

	"github.com/jackc/pgx/v5"

	"sync-state/internal/payload"
)

// structDefenderClearHandler ports cache.handle_event_struct_defender_clear
// (cache-trigger-add-queue-20260121-bigly-refactor.sql:865-883).
//
// Always deletes TWO rows:
//   1. structs.struct_defender WHERE defending_struct_id = $1
//   2. structs.struct_attribute WHERE id = '5-' || $1
//
// The struct_attribute side-effect mirrors the SQL handler exactly. The
// '5-' prefix is the attribute type id for defender-related grid state
// (see structs.GET_OBJECT_TYPE). Phase 4's struct_attribute handler also
// touches these ids — we don't coordinate with it here because both
// handlers are idempotent DELETEs/UPSERTs of distinct row sets.
//
// PLANET_ACTIVITY: EventStructDefenderClear is the chain-driven path that
// clears a defender's protectedStructIndex attribute (the chain does not
// also emit an EventStructAttribute delete). The destroy path of
// structAttributeHandler also clears orphaned defender rows when the
// *protected* struct dies — the chain is lazy about that side — via
// clearDefendersOfDestroyedProtected. Both emit struct_defense_remove
// through emitStructDefenseRemove for an identical detail shape.
//
// On the clear path we read the protectedStructIndex val before deleting
// so we know which protected struct's planet to anchor the activity on.
type structDefenderClearHandler struct{}

func (structDefenderClearHandler) CompositeKey() string {
	return "structs.structs.EventStructDefenderClear.structDefenderClearDetail"
}

const structDefenderClearDefenderSQL = `
DELETE FROM structs.struct_defender WHERE defending_struct_id = $1`

const structDefenderClearAttributeSQL = `
DELETE FROM structs.struct_attribute WHERE id = '5-' || $1`

const structDefenderClearPrevValSQL = `
SELECT val FROM structs.struct_attribute WHERE id = '5-' || $1`

// destroyedProtectedDefenderSQL removes every defender relationship that
// points at a just-destroyed protected struct. RETURNING gives us the
// defender ids so we can drop the paired protectedStructIndex attributes
// and emit one struct_defense_remove activity row per defender.
const destroyedProtectedDefenderSQL = `
DELETE FROM structs.struct_defender
 WHERE protected_struct_id = $1
RETURNING defending_struct_id`

func (structDefenderClearHandler) Handle(ctx context.Context, tx pgx.Tx, bctx BlockContext, raw json.RawMessage) error {
	p, err := payload.Decode[payload.StructDefenderClear](raw)
	if err != nil {
		return err
	}
	if p.DefendingStructID == "" {
		return fmt.Errorf("struct_defender_clear: empty defending_struct_id")
	}

	// Capture the protectedStructIndex val before the delete so the
	// struct_defense_remove emit knows which protected struct to anchor on.
	var protectedIndex int64
	if err := tx.QueryRow(ctx, structDefenderClearPrevValSQL, p.DefendingStructID).Scan(&protectedIndex); err != nil && !errors.Is(err, pgx.ErrNoRows) {
		return fmt.Errorf("struct_defender_clear prev val def=%s: %w", p.DefendingStructID, err)
	}

	if _, err := tx.Exec(ctx, structDefenderClearDefenderSQL, p.DefendingStructID); err != nil {
		return fmt.Errorf("struct_defender_clear defender delete def=%s: %w", p.DefendingStructID, err)
	}
	tag, err := tx.Exec(ctx, structDefenderClearAttributeSQL, p.DefendingStructID)
	if err != nil {
		return fmt.Errorf("struct_defender_clear attribute delete def=%s: %w", p.DefendingStructID, err)
	}

	if tag.RowsAffected() > 0 && protectedIndex > 0 {
		if err := emitStructDefenseRemove(ctx, tx, bctx, protectedIndex, p.DefendingStructID); err != nil {
			return fmt.Errorf("struct_defender_clear defense_remove def=%s: %w", p.DefendingStructID, err)
		}
	}
	return nil
}

// clearDefendersOfDestroyedProtected is the DB-side fix for the chain's
// laziness about cleaning defender relationships when the *protected*
// struct is destroyed. Called from the status-attribute destroy stamp
// (bit 32). For each orphaned defender it:
//  1. deletes the structs.struct_defender row (via the RETURNING query)
//  2. deletes the paired structs.struct_attribute id='5-'+defenderID
//  3. emits a struct_defense_remove planet_activity row
//
// The Query result set is fully drained and closed before any further
// Exec/emit — pgx forbids concurrent statements on the same tx while a
// Rows cursor is open.
func clearDefendersOfDestroyedProtected(ctx context.Context, tx pgx.Tx, bctx BlockContext, protectedStructID string, protectedIndex int) error {
	rows, err := tx.Query(ctx, destroyedProtectedDefenderSQL, protectedStructID)
	if err != nil {
		return fmt.Errorf("delete defenders of protected=%s: %w", protectedStructID, err)
	}
	var defenderIDs []string
	for rows.Next() {
		var id string
		if err := rows.Scan(&id); err != nil {
			rows.Close()
			return fmt.Errorf("scan defender of protected=%s: %w", protectedStructID, err)
		}
		defenderIDs = append(defenderIDs, id)
	}
	if err := rows.Err(); err != nil {
		rows.Close()
		return fmt.Errorf("iterate defenders of protected=%s: %w", protectedStructID, err)
	}
	rows.Close()

	for _, defenderID := range defenderIDs {
		if _, err := tx.Exec(ctx, structDefenderClearAttributeSQL, defenderID); err != nil {
			return fmt.Errorf("delete protectedStructIndex attr for defender=%s: %w", defenderID, err)
		}
		if err := emitStructDefenseRemove(ctx, tx, bctx, int64(protectedIndex), defenderID); err != nil {
			return fmt.Errorf("defense_remove for defender=%s protected=%s: %w",
				defenderID, protectedStructID, err)
		}
	}
	return nil
}
