package events

import (
	"context"
	"testing"

	"github.com/jackc/pgx/v5"
)

func TestHandler_WorkUint64Attributes(t *testing.T) {
	inTx(t, connect(t), func(tx pgx.Tx) {
		ctx := context.Background()
		bc := bctx()
		if _, err := tx.Exec(ctx, `INSERT INTO structs.struct(id,location_type,location_id)
VALUES('5-990980','planet','2-990980'),('5-18446744073709551615','planet','2-990980')`); err != nil {
			t.Fatal(err)
		}
		for _, spec := range []struct {
			id, table string
			handler   Handler
		}{
			{"0-5-990980", "struct_attribute", structAttributeHandler{}},
			{"2-5-990980", "struct_attribute", structAttributeHandler{}},
			{"0-2-990980", "planet_attribute", planetAttributeHandler{}},
			{"12-2-990980", "planet_attribute", planetAttributeHandler{}},
		} {
			for _, value := range []string{"2147483648", "9007199254740993", "9223372036854775808", "18446744073709551615"} {
				raw := mustJSON(t, map[string]string{"attributeId": spec.id, "value": value})
				handle(t, ctx, tx, spec.handler, bc, raw)
				var actual string
				if err := tx.QueryRow(ctx, `SELECT val::text FROM structs.`+spec.table+` WHERE id=$1`, spec.id).Scan(&actual); err != nil || actual != value {
					t.Fatalf("%s exact=%s wanted=%s err=%v", spec.id, actual, value, err)
				}
				var count, replayed int
				if err := tx.QueryRow(ctx, `SELECT count(*) FROM structs.planet_activity WHERE planet_id='2-990980'`).Scan(&count); err != nil {
					t.Fatal(err)
				}
				handle(t, ctx, tx, spec.handler, bc, raw)
				if err := tx.QueryRow(ctx, `SELECT count(*) FROM structs.planet_activity WHERE planet_id='2-990980'`).Scan(&replayed); err != nil || replayed != count {
					t.Fatalf("%s replay changed activity %d to %d: %v", spec.id, count, replayed, err)
				}
			}
			for _, invalid := range []string{"-1", "+1", "1.5", "1e3", " 1", "18446744073709551616"} {
				if err := spec.handler.Handle(ctx, tx, bc, mustJSON(t, map[string]string{"attributeId": spec.id, "value": invalid})); err == nil {
					t.Fatalf("%s accepted %q", spec.id, invalid)
				}
			}
			var before string
			if err := tx.QueryRow(ctx, `SELECT val::text FROM structs.`+spec.table+` WHERE id=$1`, spec.id).Scan(&before); err != nil || before != "18446744073709551615" {
				t.Fatalf("invalid event changed %s: %s, %v", spec.id, before, err)
			}
			handle(t, ctx, tx, spec.handler, bc, mustJSON(t, map[string]string{"attributeId": spec.id, "value": ""}))
			var cleared int
			if err := tx.QueryRow(ctx, `SELECT val FROM structs.`+spec.table+` WHERE id=$1`, spec.id).Scan(&cleared); err != nil || cleared != 0 {
				t.Fatalf("%s clear=%d err=%v", spec.id, cleared, err)
			}
		}
		// Reading the previous NUMERIC must preserve the protected id above MaxInt64.
		handle(t, ctx, tx, structAttributeHandler{}, bc,
			mustJSON(t, map[string]string{"attributeId": "5-5-990980", "value": "18446744073709551615"}))
		handle(t, ctx, tx, structDefenderClearHandler{}, bc,
			mustJSON(t, map[string]string{"defendingStructId": "5-990980"}))
		var protected string
		if err := tx.QueryRow(ctx, `SELECT detail->>'protected_struct_id' FROM structs.planet_activity
WHERE planet_id='2-990980' AND category='struct_defense_remove' ORDER BY seq DESC LIMIT 1`).Scan(&protected); err != nil || protected != "5-18446744073709551615" {
			t.Fatalf("wide protected id lost: %s, %v", protected, err)
		}
		// Stat COPY must retain the same full range as the current-state table.
		flushBuf(t, ctx, tx, bc)
		var health string
		if err := tx.QueryRow(ctx, `SELECT max(value)::text FROM structs.stat_struct_health WHERE object_index=990980`).Scan(&health); err != nil || health != "18446744073709551615" {
			t.Fatalf("wide health stat=%s err=%v", health, err)
		}
	})
}

func TestHandler_WorkUint64Catalog(t *testing.T) {
	inTx(t, connect(t), func(tx pgx.Tx) {
		ctx := context.Background()
		raw := mustJSON(t, map[string]any{"id": 990980, "type": "uint64-test",
			"buildDifficulty": "18446744073709551615", "oreMiningDifficulty": "9223372036854775808", "oreRefiningDifficulty": "9007199254740993"})
		handle(t, ctx, tx, structTypeHandler{}, bctx(), raw)
		var build, mine, refine string
		if err := tx.QueryRow(ctx, `SELECT build_difficulty::text,ore_mining_difficulty::text,ore_refining_difficulty::text
FROM structs.struct_type WHERE id=990980`).Scan(&build, &mine, &refine); err != nil || build != "18446744073709551615" || mine != "9223372036854775808" || refine != "9007199254740993" {
			t.Fatalf("catalog precision: %s %s %s, %v", build, mine, refine, err)
		}
	})
}
