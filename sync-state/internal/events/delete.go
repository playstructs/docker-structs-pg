package events

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"

	"github.com/jackc/pgx/v5"

	"sync-state/internal/objecttype"
	"sync-state/internal/payload"
)

// deleteHandler applies structs.structs.EventDelete.objectId. v0.21.0
// emits this for expired agreements and torn-down allocations/providers.
// Without it, api_leaderboard_provider.agreement_count and entity
// leaderboards stay stale after the upgrade block.
type deleteHandler struct{}

func (deleteHandler) CompositeKey() string {
	return "structs.structs.EventDelete.objectId"
}

func (deleteHandler) Handle(ctx context.Context, tx pgx.Tx, bctx BlockContext, raw json.RawMessage) error {
	id, err := decodeDeleteObjectID(raw)
	if err != nil {
		return err
	}
	kind, _, _, err := objecttype.Parse(id)
	if err != nil {
		return fmt.Errorf("%w: delete: %v", ErrSkipWithWarn, err)
	}

	switch kind {
	case objecttype.Agreement:
		var provider *string
		if err := tx.QueryRow(ctx, `SELECT provider_id FROM structs.agreement WHERE id = $1`, id).Scan(&provider); err != nil && !errors.Is(err, pgx.ErrNoRows) {
			return fmt.Errorf("delete agreement lookup %s: %w", id, err)
		}
		if _, err := tx.Exec(ctx, `DELETE FROM structs.agreement WHERE id = $1`, id); err != nil {
			return fmt.Errorf("delete agreement %s: %w", id, err)
		}
		if provider != nil {
			bctx.Dirty.Provider(*provider)
		}
	case objecttype.Provider:
		if _, err := tx.Exec(ctx, `DELETE FROM structs.player_object WHERE object_id IN (SELECT id FROM structs.agreement WHERE provider_id = $1)`, id); err != nil {
			return fmt.Errorf("delete provider agreement sidecars %s: %w", id, err)
		}
		if _, err := tx.Exec(ctx, `DELETE FROM structs.agreement WHERE provider_id = $1`, id); err != nil {
			return fmt.Errorf("delete provider agreements %s: %w", id, err)
		}
		if _, err := tx.Exec(ctx, `DELETE FROM structs.provider WHERE id = $1`, id); err != nil {
			return fmt.Errorf("delete provider %s: %w", id, err)
		}
		bctx.Dirty.Provider(id)
	case objecttype.Allocation:
		rows, err := tx.Query(ctx, `SELECT DISTINCT provider_id FROM structs.agreement WHERE allocation_id = $1 AND provider_id IS NOT NULL`, id)
		if err != nil {
			return fmt.Errorf("delete allocation providers %s: %w", id, err)
		}
		var providers []string
		for rows.Next() {
			var provider string
			if err := rows.Scan(&provider); err != nil {
				rows.Close()
				return fmt.Errorf("delete allocation providers %s: %w", id, err)
			}
			providers = append(providers, provider)
		}
		scanErr := rows.Err()
		rows.Close()
		if scanErr != nil {
			return fmt.Errorf("delete allocation providers %s: %w", id, scanErr)
		}
		if _, err := tx.Exec(ctx, `DELETE FROM structs.player_object WHERE object_id IN (SELECT id FROM structs.agreement WHERE allocation_id = $1)`, id); err != nil {
			return fmt.Errorf("delete allocation agreement sidecars %s: %w", id, err)
		}
		if _, err := tx.Exec(ctx, `DELETE FROM structs.agreement WHERE allocation_id = $1`, id); err != nil {
			return fmt.Errorf("delete allocation agreements %s: %w", id, err)
		}
		if _, err := tx.Exec(ctx, `DELETE FROM structs.allocation WHERE id = $1`, id); err != nil {
			return fmt.Errorf("delete allocation %s: %w", id, err)
		}
		for _, provider := range providers {
			bctx.Dirty.Provider(provider)
		}
	case objecttype.Reactor:
		if _, err := tx.Exec(ctx, `DELETE FROM structs.reactor WHERE id = $1`, id); err != nil {
			return fmt.Errorf("delete reactor %s: %w", id, err)
		}
		bctx.Dirty.Reactor(id)
	case objecttype.Substation:
		if _, err := tx.Exec(ctx, `DELETE FROM structs.substation WHERE id = $1`, id); err != nil {
			return fmt.Errorf("delete substation %s: %w", id, err)
		}
		bctx.Dirty.Substation(id)
	case objecttype.Guild:
		if _, err := tx.Exec(ctx, `DELETE FROM structs.guild WHERE id = $1`, id); err != nil {
			return fmt.Errorf("delete guild %s: %w", id, err)
		}
		bctx.Dirty.Guild(id)
	case objecttype.Player:
		if _, err := tx.Exec(ctx, `DELETE FROM structs.player WHERE id = $1`, id); err != nil {
			return fmt.Errorf("delete player %s: %w", id, err)
		}
		bctx.Dirty.Player(id)
	case objecttype.Planet:
		if _, err := tx.Exec(ctx, `DELETE FROM structs.planet WHERE id = $1`, id); err != nil {
			return fmt.Errorf("delete planet %s: %w", id, err)
		}
	case objecttype.Struct:
		if _, err := tx.Exec(ctx, `DELETE FROM structs.struct WHERE id = $1`, id); err != nil {
			return fmt.Errorf("delete struct %s: %w", id, err)
		}
	case objecttype.Fleet:
		if _, err := tx.Exec(ctx, `DELETE FROM structs.fleet WHERE id = $1`, id); err != nil {
			return fmt.Errorf("delete fleet %s: %w", id, err)
		}
	default:
		return fmt.Errorf("%w: delete: unsupported object type %s for %s", ErrSkipWithWarn, kind, id)
	}

	if _, err := tx.Exec(ctx, `DELETE FROM structs.player_object WHERE object_id = $1`, id); err != nil {
		return fmt.Errorf("delete player_object %s: %w", id, err)
	}
	return nil
}

func decodeDeleteObjectID(raw json.RawMessage) (string, error) {
	p, err := payload.Decode[payload.Delete](raw)
	if err == nil && p.ObjectID != "" {
		return p.ObjectID, nil
	}
	var id string
	if uerr := json.Unmarshal(raw, &id); uerr == nil && id != "" {
		return id, nil
	}
	if err != nil {
		return "", err
	}
	return "", fmt.Errorf("delete: empty objectId")
}
