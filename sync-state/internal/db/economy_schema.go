package db

import (
	"context"
	"fmt"

	"github.com/jackc/pgx/v5"
)

type schemaQuerier interface {
	QueryRow(context.Context, string, ...any) pgx.Row
}

// CheckEconomySchema validates the Sqitch-owned contract; it never creates or
// repairs canonical schema. It is mandatory even when optional doctor probes
// are disabled. Catalog errors are failures, not evidence of compatibility.
func CheckEconomySchema(ctx context.Context, q schemaQuerier) error {
	for _, schema := range []string{"structs", "sync_state", "view"} {
		var allowed bool
		if err := q.QueryRow(ctx, `SELECT has_schema_privilege(current_user,$1,'USAGE')`, schema).Scan(&allowed); err != nil {
			return err
		}
		if !allowed {
			return fmt.Errorf("v0.22 schema missing USAGE on %s", schema)
		}
	}
	columns := []struct{ table, column, typ string }{
		{"structs.vault", "owner_id", "character varying"},
		{"structs.vault", "address", "character varying"},
		{"structs.vault", "account_number", "numeric"},
		{"structs.asset_owner", "owner_id", "character varying"},
		{"structs.asset_owner", "is_vault", "boolean"},
		{"structs.infusion", "owner_id", "character varying"},
		{"structs.infusion", "generator_disabled", "boolean"},
		{"structs.substation", "deletion_pending", "boolean"},
		{"structs.substation", "deletion_migration_substation_id", "character varying"},
		{"structs.guild", "rank_purchase_base_price", "numeric"},
		{"structs.guild", "rank_purchase_step_increase", "numeric"},
		{"structs.guild", "rank_purchase_minimum_rank", "numeric"},
		{"structs.guild", "join_by_rank_purchase_enabled", "boolean"},
		{"structs.guild", "bank_convert_in_fee", "numeric"},
		{"structs.guild", "bank_convert_out_fee", "numeric"},
		{"structs.guild", "charter_solver_id", "character varying"},
		{"structs.player", "guild_rank", "numeric"},
		{"structs.guild", "entry_rank", "numeric"},
		{"structs.permission_guild_rank", "rank", "numeric"},
		{"structs.owner_reactor_diversity", "power", "numeric"},
		{"structs.owner_reactor_diversity", "squared_power", "numeric"},
		{"structs.owner_reactor_diversity", "applied_bonus", "numeric"},
		{"structs.reactor_commission_revision", "slash_revision", "numeric"},
		{"structs.reactor_commission_revision", "reconciled_slash_revision", "numeric"},
		{"structs.reactor_stake_cap", "active", "boolean"},
		{"structs.reactor_stake_cap", "cap_bps", "numeric"},
		{"structs.reactor_stake_cap", "min_validators", "numeric"},
		{"sync_state.infusion_event_position", "phase", "smallint"},
		{"sync_state.economy_event_position", "phase", "smallint"},
		{"sync_state.economy_event_position", "height", "bigint"},
		{"sync_state.economy_event_position", "tx_index", "integer"},
		{"sync_state.economy_event_position", "event_index", "integer"},
	}
	for _, c := range columns {
		var typ string
		var modifier int
		err := q.QueryRow(ctx, `SELECT atttypid::regtype::text,atttypmod FROM pg_attribute
            WHERE attrelid=to_regclass($1) AND attname=$2 AND NOT attisdropped`, c.table, c.column).Scan(&typ, &modifier)
		if err != nil {
			return fmt.Errorf("v0.22 schema %s.%s: %w; deploy the structs-pg Sqitch prerequisites", c.table, c.column, err)
		}
		if typ != c.typ || (typ == "numeric" && modifier != -1) {
			return fmt.Errorf("v0.22 schema %s.%s: need %s without numeric coercion, got %s typmod=%d", c.table, c.column, c.typ, typ, modifier)
		}
	}
	for _, key := range []struct {
		table   string
		columns []string
	}{
		{"structs.vault", []string{"owner_id"}},
		{"structs.vault", []string{"address"}},
		{"structs.owner_reactor_diversity", []string{"owner_id"}},
		{"structs.reactor_commission_revision", []string{"reactor_id"}},
		{"structs.reactor_stake_cap", []string{"singleton"}},
		{"sync_state.infusion_event_position", []string{"destination_id", "address"}},
		{"sync_state.economy_event_position", []string{"family", "entity_id"}},
		{"structs.ledger", []string{"time", "chain_id", "tx_index", "msg_index", "event_index", "denom"}},
	} {
		var valid bool
		if err := q.QueryRow(ctx, `SELECT EXISTS (
            SELECT 1 FROM pg_index i WHERE i.indrelid=to_regclass($1)
             AND i.indisunique AND i.indisvalid AND i.indimmediate AND i.indpred IS NULL AND i.indexprs IS NULL
             AND (SELECT array_agg(a.attname::text ORDER BY k.n)
                    FROM unnest(i.indkey) WITH ORDINALITY k(num,n)
                    JOIN pg_attribute a ON a.attrelid=i.indrelid AND a.attnum=k.num
                   WHERE k.n<=i.indnkeyatts) = $2::text[])`, key.table, key.columns).Scan(&valid); err != nil {
			return fmt.Errorf("v0.22 schema unique key %s: %w", key.table, err)
		}
		if !valid {
			return fmt.Errorf("v0.22 schema missing usable unique key %s(%v)", key.table, key.columns)
		}
		privileges := []string{"SELECT", "INSERT", "UPDATE", "DELETE"}
		if key.table == "structs.ledger" {
			privileges = []string{"SELECT", "INSERT"}
		}
		for _, privilege := range privileges {
			if err := q.QueryRow(ctx, `SELECT has_table_privilege(current_user,$1,$2)`, key.table, privilege).Scan(&valid); err != nil {
				return err
			}
			if !valid {
				return fmt.Errorf("v0.22 schema missing %s on %s", privilege, key.table)
			}
		}
	}
	var ownerRead bool
	if err := q.QueryRow(ctx, `SELECT has_table_privilege(current_user,'structs.asset_owner','SELECT')`).Scan(&ownerRead); err != nil {
		return err
	}
	if !ownerRead {
		return fmt.Errorf("v0.22 schema missing SELECT on structs.asset_owner")
	}
	var boundaryReady bool
	if err := q.QueryRow(ctx, `SELECT COALESCE(has_function_privilege(current_user,
to_regprocedure('structs.api_projection_boundary()'),'EXECUTE'),FALSE)`).Scan(&boundaryReady); err != nil {
		return err
	}
	if !boundaryReady {
		return fmt.Errorf("v0.22 schema missing executable structs.api_projection_boundary(); deploy the ownership maintenance migration")
	}
	for _, source := range []string{"sync_state.sync_cursor", "structs.current_block", "structs.struct", "structs.planet", "view.work_live"} {
		var readable bool
		if err := q.QueryRow(ctx, `SELECT has_table_privilege(current_user,$1,'SELECT')`, source).Scan(&readable); err != nil {
			return err
		}
		if !readable {
			return fmt.Errorf("v0.22 maintenance missing SELECT on %s", source)
		}
	}
	var legacyLedgerKey bool
	if err := q.QueryRow(ctx, `SELECT EXISTS (SELECT 1 FROM pg_index i
        WHERE i.indrelid='structs.ledger'::regclass AND i.indisunique AND i.indisvalid
          AND (SELECT array_agg(a.attname::text ORDER BY k.n)
                 FROM unnest(i.indkey) WITH ORDINALITY k(num,n)
                 JOIN pg_attribute a ON a.attrelid=i.indrelid AND a.attnum=k.num
                WHERE k.n<=i.indnkeyatts)=ARRAY['time','chain_id','tx_index','msg_index','event_index'])`).Scan(&legacyLedgerKey); err != nil {
		return err
	}
	if legacyLedgerKey {
		return fmt.Errorf("legacy ledger uniqueness still rejects additional denominations; finish the paired Sqitch/writer switch")
	}
	return nil
}
