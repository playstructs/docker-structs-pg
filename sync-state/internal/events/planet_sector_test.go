package events

import (
	"context"
	"encoding/json"
	"fmt"
	"testing"

	"github.com/jackc/pgx/v5"
	"sync-state/internal/payload"
)

func TestPlanetSectorPayload(t *testing.T) {
	for _, tc := range []struct {
		raw  string
		want int64
	}{
		{`{"sector":"127"}`, 127}, {`{"sector":999}`, 999},
		{`{"sector":"44"}`, 44}, {`{}`, 0}, {`{"sector":"0"}`, 0},
	} {
		p, err := payload.Decode[payload.Planet](json.RawMessage(tc.raw))
		if err != nil {
			t.Fatal(err)
		}
		if p.Sector.Int64() != tc.want {
			t.Fatalf("%s: sector=%d want=%d", tc.raw, p.Sector.Int64(), tc.want)
		}
	}
	for _, value := range []string{`45`, `-1`, `"9223372036854775808"`, `1.5`, `"invalid"`} {
		raw := json.RawMessage(`{"id":"2-999810","sector":` + value + `}`)
		if err := (planetHandler{}).Handle(context.Background(), nil, BlockContext{}, raw); err == nil {
			t.Fatalf("accepted invalid sector %s", value)
		}
	}
}

func TestHandler_PlanetSector(t *testing.T) {
	conn := connect(t)
	inTx(t, conn, func(tx pgx.Tx) {
		ctx := context.Background()
		const id = "2-999810"
		raw := map[string]any{"id": id, "maxOre": "100", "name": "Sector sample", "status": "active"}
		apply := func() {
			t.Helper()
			if err := (planetHandler{}).Handle(ctx, tx, bctx(), mustJSON(t, raw)); err != nil {
				t.Fatal(err)
			}
		}
		read := func(want int64) string {
			t.Helper()
			var stored, exposed int64
			var ctid string
			if err := tx.QueryRow(ctx, `SELECT p.sector,v.sector,p.ctid::text FROM structs.planet p JOIN view.planet v ON v.planet_id=p.id WHERE p.id=$1`, id).Scan(&stored, &exposed, &ctid); err != nil {
				t.Fatal(err)
			}
			if stored != want || exposed != want {
				t.Fatalf("stored=%d exposed=%d want=%d", stored, exposed, want)
			}
			return ctid
		}
		apply()
		previous := read(0)
		for _, sector := range []int64{127, 999, 44} {
			raw["sector"] = fmt.Sprint(sector)
			apply()
			current := read(sector)
			if current == previous {
				t.Fatal("sector-only update was skipped")
			}
			apply()
			if read(sector) != current {
				t.Fatal("identical replay rewrote planet")
			}
			previous = current
		}
		raw["name"] = "Renamed sector sample"
		apply()
		read(44)
		// Historical replay follows the complete source payload, including default zero.
		delete(raw, "sector")
		apply()
		read(0)
	})
}
