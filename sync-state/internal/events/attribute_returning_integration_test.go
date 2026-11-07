package events

import (
	"context"
	"encoding/json"
	"testing"

	"github.com/jackc/pgx/v5"
)

func TestAttributeUpsert_PreviousValuesAndNoops(t *testing.T) {
	const wide = "18446744073709551615"
	for _, spec := range []struct {
		name, table, id, field, category string
		handler                          Handler
		values                           []string
	}{
		{"shield", "planet_attribute", "0-2-990981", "planetary_shield", "shield_change", planetAttributeHandler{}, []string{"0", "10", wide, wide, "0"}},
		{"health", "struct_attribute", "0-5-990981", "health", "struct_health", structAttributeHandler{}, []string{"0", "10", wide, wide, "0"}},
		{"status", "struct_attribute", "1-5-990981", "status", "struct_status", structAttributeHandler{}, []string{"1", "32", "32", "0"}},
	} {
		t.Run(spec.name, func(t *testing.T) {
			inTx(t, connect(t), func(tx pgx.Tx) {
				ctx := context.Background()
				if _, err := tx.Exec(ctx, `INSERT INTO structs.struct(id,location_type,location_id)
VALUES('5-990981','planet','2-990981')`); err != nil {
					t.Fatal(err)
				}
				bc := bctx()
				previous, oldCTID := "0", ""
				for _, value := range spec.values {
					before := len(bc.Buf.PlanetActivity)
					stats := len(bc.Buf.StatStructHealth) + len(bc.Buf.StatStructStatus)
					if err := spec.handler.Handle(ctx, tx, bc, mustJSON(t, map[string]string{"attributeId": spec.id, "value": value})); err != nil {
						t.Fatal(err)
					}
					statusClear := spec.name == "status" && value == "0"
					emits := value != previous && !statusClear && !(spec.name == "health" && value == "0")
					wantRows := before
					if emits {
						wantRows++
					}
					if len(bc.Buf.PlanetActivity) != wantRows {
						t.Fatalf("%s -> %s: activity count=%d want=%d", previous, value, len(bc.Buf.PlanetActivity), wantRows)
					}
					if emits {
						row := bc.Buf.PlanetActivity[before]
						var detail map[string]json.RawMessage
						if err := json.Unmarshal(row.Detail, &detail); err != nil {
							t.Fatal(err)
						}
						if row.Category != spec.category || string(detail[spec.field]) != value || string(detail[spec.field+"_old"]) != previous {
							t.Fatalf("lost old/new precision: category=%s detail=%s", row.Category, row.Detail)
						}
					}
					var stored, ctid string
					if err := tx.QueryRow(ctx, `SELECT val::text,ctid::text FROM structs.`+spec.table+` WHERE id=$1`, spec.id).Scan(&stored, &ctid); err != nil {
						t.Fatal(err)
					}
					noop := statusClear || (oldCTID != "" && value == previous)
					if noop && (ctid != oldCTID || len(bc.Buf.StatStructHealth)+len(bc.Buf.StatStructStatus) != stats) {
						t.Fatal("no-op rewrote the row or emitted stats")
					}
					if !statusClear {
						previous = value
					}
					if stored != previous {
						t.Fatalf("stored=%s want=%s", stored, previous)
					}
					oldCTID = ctid
				}
			})
		})
	}
}
