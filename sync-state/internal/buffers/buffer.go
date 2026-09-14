// Package buffers holds in-memory row buffers for append-only structs.*
// tables. Handlers push rows during dispatch; the per-block (streaming
// mode) or per-window (bulk mode) orchestrator calls Buffer.Flush which
// emits one pgx.CopyFrom per non-empty table. Row order within each
// table is preserved (chain order in, chain order out).
//
// Safety model: handlers run inside a SAVEPOINT. The router snapshots the
// buffer lengths before calling Handle and restores them on rollback so
// a failing handler's appended rows don't leak into the flush. Same
// pattern wraps the bank.ProcessBlock savepoint in the sync layer.
//
// This package intentionally has no DB dependency beyond pgx — it does
// not know about Querier abstractions or business logic.
package buffers

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"
	"time"

	"github.com/jackc/pgx/v5"
)

// LedgerLegStride is the agreed event_index sub-sequence for multiple
// ledger rows from one chain event. Stored event_index is
// chainEventIndex*LedgerLegStride + leg. Max legs today is 4 (unbond /
// redelegate / complete_redelegation); stride 8 leaves headroom without
// colliding with the next chain event's encoded indexes.
const LedgerLegStride = 8

// EncodeLedgerEventIndex maps (chain event_index, leg) onto the unique
// index column. Every writer — including single-row events at leg 0 —
// must use this so ON CONFLICT is stable across replays.
func EncodeLedgerEventIndex(chainEventIndex, leg int) int {
	return chainEventIndex*LedgerLegStride + leg
}

// LedgerDelta is one newly-inserted ledger row's contribution to
// structs.api_inventory. Replayed events (ON CONFLICT DO NOTHING) are
// omitted, so applying these deltas is idempotent.
type LedgerDelta struct {
	Address string
	Denom   string
	Delta   string // signed NUMERIC text; credit positive, debit negative
}

// LedgerRow mirrors a structs.ledger insert. Counterparty may be empty;
// callers should leave Counterparty as "" for the no-counterparty path
// (coinbase, burn, ore_mine, alpha_refine) — flush emits NULL for that
// column when the field is empty.
//
// ChainID/TxIndex/MsgIndex/EventIndex are the source-event identity
// (table-ledger-20260914-source-event-identity). EventIndex must already
// be EncodeLedgerEventIndex(chainEventIndex, leg). Time must be the
// block time, never now().
type LedgerRow struct {
	Address      string
	Counterparty string
	AmountP      string // NUMERIC; passed as text so big numbers survive
	BlockHeight  int64
	Time         time.Time
	Action       string
	Direction    string
	Denom        string
	ChainID      string
	TxIndex      int
	MsgIndex     int
	EventIndex   int
}

// DefusionRow mirrors structs.defusion.
type DefusionRow struct {
	ValidatorAddress string
	DelegatorAddress string
	DefusionType     string
	AmountP          string
	Denom            string
	CompletedAt      time.Time
	CreatedAt        time.Time
}

// PlanetActivityRow mirrors structs.planet_activity. Seq is assigned by
// nextPlanetActivitySeq at handler time so monotonicity is preserved.
type PlanetActivityRow struct {
	Time        time.Time
	Seq         int64
	PlanetID    string
	Category    string
	Detail      json.RawMessage
	BlockHeight int64
}

// StatRow holds a single point for any stat_* hypertable. The
// ObjectType pointer is nil for tables that don't bind object_type
// (stat_structs_load, stat_connection_capacity, stat_connection_count,
// stat_struct_health, stat_struct_status).
type StatRow struct {
	Time        time.Time
	ObjectType  *string
	ObjectIndex int
	Value       int64
	BlockHeight int64
}

// Buffer aggregates rows for every append-only structs.* table that
// sync-state writes. Zero value is ready to use; NewBuffer is a small
// convenience that pre-sizes the slices.
type Buffer struct {
	Ledger   []LedgerRow
	Defusion []DefusionRow

	PlanetActivity []PlanetActivityRow

	StatOre                []StatRow
	StatFuel               []StatRow
	StatCapacity           []StatRow
	StatLoad               []StatRow
	StatStructsLoad        []StatRow
	StatPower              []StatRow
	StatConnectionCapacity []StatRow
	StatConnectionCount    []StatRow
	StatStructHealth       []StatRow
	StatStructStatus       []StatRow
}

// New returns an empty Buffer with no pre-allocation. Callers can also
// just use `&Buffer{}` directly.
func New() *Buffer { return &Buffer{} }

// Snapshot records the current length of every slice so a failed
// handler can roll its buffered rows back via Restore.
type Snapshot struct {
	Ledger                 int
	Defusion               int
	PlanetActivity         int
	StatOre                int
	StatFuel               int
	StatCapacity           int
	StatLoad               int
	StatStructsLoad        int
	StatPower              int
	StatConnectionCapacity int
	StatConnectionCount    int
	StatStructHealth       int
	StatStructStatus       int
}

// Snapshot captures the buffer's current write position.
func (b *Buffer) Snapshot() Snapshot {
	if b == nil {
		return Snapshot{}
	}
	return Snapshot{
		Ledger:                 len(b.Ledger),
		Defusion:               len(b.Defusion),
		PlanetActivity:         len(b.PlanetActivity),
		StatOre:                len(b.StatOre),
		StatFuel:               len(b.StatFuel),
		StatCapacity:           len(b.StatCapacity),
		StatLoad:               len(b.StatLoad),
		StatStructsLoad:        len(b.StatStructsLoad),
		StatPower:              len(b.StatPower),
		StatConnectionCapacity: len(b.StatConnectionCapacity),
		StatConnectionCount:    len(b.StatConnectionCount),
		StatStructHealth:       len(b.StatStructHealth),
		StatStructStatus:       len(b.StatStructStatus),
	}
}

// Restore truncates every slice back to the snapshot's recorded length.
// Used when a handler savepoint is rolled back to discard any rows the
// handler buffered before failing.
func (b *Buffer) Restore(s Snapshot) {
	if b == nil {
		return
	}
	if len(b.Ledger) > s.Ledger {
		b.Ledger = b.Ledger[:s.Ledger]
	}
	if len(b.Defusion) > s.Defusion {
		b.Defusion = b.Defusion[:s.Defusion]
	}
	if len(b.PlanetActivity) > s.PlanetActivity {
		b.PlanetActivity = b.PlanetActivity[:s.PlanetActivity]
	}
	if len(b.StatOre) > s.StatOre {
		b.StatOre = b.StatOre[:s.StatOre]
	}
	if len(b.StatFuel) > s.StatFuel {
		b.StatFuel = b.StatFuel[:s.StatFuel]
	}
	if len(b.StatCapacity) > s.StatCapacity {
		b.StatCapacity = b.StatCapacity[:s.StatCapacity]
	}
	if len(b.StatLoad) > s.StatLoad {
		b.StatLoad = b.StatLoad[:s.StatLoad]
	}
	if len(b.StatStructsLoad) > s.StatStructsLoad {
		b.StatStructsLoad = b.StatStructsLoad[:s.StatStructsLoad]
	}
	if len(b.StatPower) > s.StatPower {
		b.StatPower = b.StatPower[:s.StatPower]
	}
	if len(b.StatConnectionCapacity) > s.StatConnectionCapacity {
		b.StatConnectionCapacity = b.StatConnectionCapacity[:s.StatConnectionCapacity]
	}
	if len(b.StatConnectionCount) > s.StatConnectionCount {
		b.StatConnectionCount = b.StatConnectionCount[:s.StatConnectionCount]
	}
	if len(b.StatStructHealth) > s.StatStructHealth {
		b.StatStructHealth = b.StatStructHealth[:s.StatStructHealth]
	}
	if len(b.StatStructStatus) > s.StatStructStatus {
		b.StatStructStatus = b.StatStructStatus[:s.StatStructStatus]
	}
}

// Len returns the total number of buffered rows across all tables. Used
// by sync-state metrics + logs to report buffer occupancy.
func (b *Buffer) Len() int {
	if b == nil {
		return 0
	}
	return len(b.Ledger) + len(b.Defusion) + len(b.PlanetActivity) +
		len(b.StatOre) + len(b.StatFuel) + len(b.StatCapacity) +
		len(b.StatLoad) + len(b.StatStructsLoad) + len(b.StatPower) +
		len(b.StatConnectionCapacity) + len(b.StatConnectionCount) +
		len(b.StatStructHealth) + len(b.StatStructStatus)
}

// Flush emits ledger rows via INSERT … ON CONFLICT DO NOTHING RETURNING
// (so replay is idempotent and the caller can apply inventory deltas),
// then one pgx.CopyFrom per remaining non-empty buffered table.
// Order across tables is fixed (ledger, defusion, planet_activity,
// stat_*) so any FK-style logical dependency lands deterministically;
// row order within each table is preserved as appended.
//
// After a successful flush every slice is reset to zero length so the
// buffer can be reused for the next block / window.
func (b *Buffer) Flush(ctx context.Context, tx pgx.Tx) ([]LedgerDelta, error) {
	if b == nil {
		return nil, nil
	}
	var deltas []LedgerDelta
	if len(b.Ledger) > 0 {
		var err error
		deltas, err = flushLedger(ctx, tx, b.Ledger)
		if err != nil {
			return nil, err
		}
		b.Ledger = b.Ledger[:0]
	}
	if len(b.Defusion) > 0 {
		if err := flushDefusion(ctx, tx, b.Defusion); err != nil {
			return nil, err
		}
		b.Defusion = b.Defusion[:0]
	}
	if len(b.PlanetActivity) > 0 {
		if err := flushPlanetActivity(ctx, tx, b.PlanetActivity); err != nil {
			return nil, err
		}
		b.PlanetActivity = b.PlanetActivity[:0]
	}
	if len(b.StatOre) > 0 {
		if err := flushStat(ctx, tx, "stat_ore", true, b.StatOre); err != nil {
			return nil, err
		}
		b.StatOre = b.StatOre[:0]
	}
	if len(b.StatFuel) > 0 {
		if err := flushStat(ctx, tx, "stat_fuel", true, b.StatFuel); err != nil {
			return nil, err
		}
		b.StatFuel = b.StatFuel[:0]
	}
	if len(b.StatCapacity) > 0 {
		if err := flushStat(ctx, tx, "stat_capacity", true, b.StatCapacity); err != nil {
			return nil, err
		}
		b.StatCapacity = b.StatCapacity[:0]
	}
	if len(b.StatLoad) > 0 {
		if err := flushStat(ctx, tx, "stat_load", true, b.StatLoad); err != nil {
			return nil, err
		}
		b.StatLoad = b.StatLoad[:0]
	}
	if len(b.StatStructsLoad) > 0 {
		if err := flushStat(ctx, tx, "stat_structs_load", false, b.StatStructsLoad); err != nil {
			return nil, err
		}
		b.StatStructsLoad = b.StatStructsLoad[:0]
	}
	if len(b.StatPower) > 0 {
		if err := flushStat(ctx, tx, "stat_power", true, b.StatPower); err != nil {
			return nil, err
		}
		b.StatPower = b.StatPower[:0]
	}
	if len(b.StatConnectionCapacity) > 0 {
		if err := flushStat(ctx, tx, "stat_connection_capacity", false, b.StatConnectionCapacity); err != nil {
			return nil, err
		}
		b.StatConnectionCapacity = b.StatConnectionCapacity[:0]
	}
	if len(b.StatConnectionCount) > 0 {
		if err := flushStat(ctx, tx, "stat_connection_count", false, b.StatConnectionCount); err != nil {
			return nil, err
		}
		b.StatConnectionCount = b.StatConnectionCount[:0]
	}
	if len(b.StatStructHealth) > 0 {
		if err := flushStat(ctx, tx, "stat_struct_health", false, b.StatStructHealth); err != nil {
			return nil, err
		}
		b.StatStructHealth = b.StatStructHealth[:0]
	}
	if len(b.StatStructStatus) > 0 {
		if err := flushStat(ctx, tx, "stat_struct_status", false, b.StatStructStatus); err != nil {
			return nil, err
		}
		b.StatStructStatus = b.StatStructStatus[:0]
	}
	return deltas, nil
}

// nullable returns *string when s is non-empty, else nil. Used by the
// flush helpers to translate "" → SQL NULL for nullable text columns.
func nullable(s string) any {
	if s == "" {
		return nil
	}
	return s
}

func flushLedger(ctx context.Context, tx pgx.Tx, rows []LedgerRow) ([]LedgerDelta, error) {
	const batchSize = 500 // 12 params/row; stay well under PG's 65535 limit
	out := make([]LedgerDelta, 0, len(rows))
	for i := 0; i < len(rows); i += batchSize {
		end := i + batchSize
		if end > len(rows) {
			end = len(rows)
		}
		deltas, err := flushLedgerBatch(ctx, tx, rows[i:end])
		if err != nil {
			return out, err
		}
		out = append(out, deltas...)
	}
	return out, nil
}

func flushLedgerBatch(ctx context.Context, tx pgx.Tx, rows []LedgerRow) ([]LedgerDelta, error) {
	placeholders := make([]string, 0, len(rows))
	args := make([]any, 0, len(rows)*12)
	for _, r := range rows {
		base := len(args)
		placeholders = append(placeholders, fmt.Sprintf(
			"($%d,$%d,$%d,$%d,$%d,$%d,$%d,$%d,$%d,$%d,$%d,$%d)",
			base+1, base+2, base+3, base+4, base+5, base+6,
			base+7, base+8, base+9, base+10, base+11, base+12,
		))
		args = append(args,
			r.Time,
			r.Address,
			nullable(r.Counterparty),
			nullable(r.AmountP),
			r.BlockHeight,
			r.Action,
			r.Direction,
			r.Denom,
			r.ChainID,
			r.TxIndex,
			r.MsgIndex,
			r.EventIndex,
		)
	}
	sql := `
INSERT INTO structs.ledger
    (time, address, counterparty, amount_p, block_height, action, direction, denom,
     chain_id, tx_index, msg_index, event_index)
VALUES ` + strings.Join(placeholders, ",") + `
ON CONFLICT (time, chain_id, tx_index, msg_index, event_index) DO NOTHING
RETURNING address, denom,
          CASE direction WHEN 'credit' THEN amount_p ELSE -amount_p END::text AS delta`
	qrows, err := tx.Query(ctx, sql, args...)
	if err != nil {
		return nil, err
	}
	defer qrows.Close()
	var deltas []LedgerDelta
	for qrows.Next() {
		var d LedgerDelta
		if err := qrows.Scan(&d.Address, &d.Denom, &d.Delta); err != nil {
			return nil, err
		}
		deltas = append(deltas, d)
	}
	return deltas, qrows.Err()
}

func flushDefusion(ctx context.Context, tx pgx.Tx, rows []DefusionRow) error {
	cols := []string{"validator_address", "delegator_address", "defusion_type", "amount_p", "denom", "completed_at", "created_at"}
	src := pgx.CopyFromSlice(len(rows), func(i int) ([]any, error) {
		r := rows[i]
		return []any{
			r.ValidatorAddress,
			r.DelegatorAddress,
			r.DefusionType,
			nullable(r.AmountP),
			r.Denom,
			r.CompletedAt,
			r.CreatedAt,
		}, nil
	})
	_, err := tx.CopyFrom(ctx, pgx.Identifier{"structs", "defusion"}, cols, src)
	return err
}

// COPY FROM fires row triggers, including planet_activity_attribute
// which writes structs.planet_activity_player in this same transaction.
func flushPlanetActivity(ctx context.Context, tx pgx.Tx, rows []PlanetActivityRow) error {
	cols := []string{"time", "seq", "planet_id", "category", "detail", "block_height"}
	src := pgx.CopyFromSlice(len(rows), func(i int) ([]any, error) {
		r := rows[i]
		// detail JSON: pgx writes []byte to a jsonb column verbatim,
		// so pass the marshalled payload as []byte.
		return []any{
			r.Time,
			r.Seq,
			r.PlanetID,
			r.Category,
			[]byte(r.Detail),
			r.BlockHeight,
		}, nil
	})
	_, err := tx.CopyFrom(ctx, pgx.Identifier{"structs", "planet_activity"}, cols, src)
	return err
}

// flushStat handles every stat_* hypertable. The two shapes (with vs.
// without object_type) only differ by column list and per-row payload.
// withObjectType=true binds the object_type::structs.object_type column;
// callers must populate ObjectType on each row in that case.
func flushStat(ctx context.Context, tx pgx.Tx, table string, withObjectType bool, rows []StatRow) error {
	var cols []string
	if withObjectType {
		cols = []string{"time", "object_type", "object_index", "value", "block_height"}
	} else {
		cols = []string{"time", "object_index", "value", "block_height"}
	}
	src := pgx.CopyFromSlice(len(rows), func(i int) ([]any, error) {
		r := rows[i]
		if withObjectType {
			var ot any
			if r.ObjectType != nil {
				ot = *r.ObjectType
			}
			return []any{r.Time, ot, r.ObjectIndex, r.Value, r.BlockHeight}, nil
		}
		return []any{r.Time, r.ObjectIndex, r.Value, r.BlockHeight}, nil
	})
	_, err := tx.CopyFrom(ctx, pgx.Identifier{"structs", table}, cols, src)
	return err
}
