package genesis

import (
	"context"
	"fmt"
	"strings"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"

	"sync-state/internal/db"
)

// Apply loads the genesis document into structs.ledger and records the
// sync_state.genesis_log row. The whole thing runs in a single
// transaction so a partial failure rolls back cleanly; operators can
// re-run init-genesis without leaving the DB in a weird half-applied
// state.
//
// On success returns an ApplyReport summarising row counts per section,
// the canonical genesis_log row that was written, and the total wall
// time the import took (helpful when porting to chains with very large
// genesis files).
//
// Replay semantics: the first thing inside the tx is a global
// `DELETE FROM structs.ledger WHERE action='genesis'`. That matches the
// shell script's behaviour and is safe because the genesis_log row is
// the single source of truth for "is genesis loaded?" — if you want a
// fresh import, calling Apply does the wipe+reapply atomically.
func Apply(ctx context.Context, pool *pgxpool.Pool, loaded *LoadedDocument) (*ApplyReport, error) {
	if loaded == nil || loaded.Doc == nil {
		return nil, fmt.Errorf("genesis: nil document passed to Apply")
	}
	start := time.Now()
	doc := loaded.Doc

	tx, err := pool.BeginTx(ctx, pgx.TxOptions{})
	if err != nil {
		return nil, fmt.Errorf("genesis: begin tx: %w", err)
	}
	committed := false
	defer func() {
		if !committed {
			_ = tx.Rollback(ctx)
		}
	}()

	deleted, err := db.DeleteGenesisLedgerRows(ctx, tx)
	if err != nil {
		return nil, fmt.Errorf("genesis: delete prior genesis rows: %w", err)
	}

	rps := make(map[string]int64, 4)

	bankRows, err := insertBankBalances(ctx, tx, doc)
	if err != nil {
		return nil, fmt.Errorf("genesis: bank section: %w", err)
	}
	rps["bank"] = bankRows

	delRows, err := insertDelegations(ctx, tx, doc)
	if err != nil {
		return nil, fmt.Errorf("genesis: delegations section: %w", err)
	}
	rps["delegations"] = delRows

	unbRows, err := insertUnbondings(ctx, tx, doc)
	if err != nil {
		return nil, fmt.Errorf("genesis: unbondings section: %w", err)
	}
	rps["unbondings"] = unbRows

	oreRows, err := insertPlayerOre(ctx, tx, doc)
	if err != nil {
		return nil, fmt.Errorf("genesis: player ore section: %w", err)
	}
	rps["ore"] = oreRows

	total := bankRows + delRows + unbRows + oreRows

	logRow := db.GenesisLogRow{
		ChainID:        doc.ChainID,
		Source:         loaded.Source,
		GenesisTime:    doc.GenesisTime,
		SHA256:         loaded.SHA256,
		RowsPerSection: rps,
		TotalRows:      total,
	}
	if err := db.WriteGenesisLog(ctx, tx, logRow); err != nil {
		return nil, fmt.Errorf("genesis: write genesis_log: %w", err)
	}

	if err := tx.Commit(ctx); err != nil {
		return nil, fmt.Errorf("genesis: commit: %w", err)
	}
	committed = true

	return &ApplyReport{
		LogRow:        logRow,
		PreDeleteRows: deleted,
		Elapsed:       time.Since(start),
	}, nil
}

// ApplyReport surfaces what Apply did. Surfaced by the subcommand to
// stderr and by the verify checker for tail-of-eye reporting.
type ApplyReport struct {
	LogRow        db.GenesisLogRow
	PreDeleteRows int64
	Elapsed       time.Duration
}

// --- per-section inserts ----------------------------------------------------

// insertBankBalances flattens (.address, .coins[].denom, .amount) into
// one ledger row per coin. Mirrors section 1 of the shell script
// verbatim (direction='credit', action='genesis').
func insertBankBalances(ctx context.Context, tx pgx.Tx, doc *Document) (int64, error) {
	return execLedgerInserts(ctx, tx, ledgerBankRows(doc))
}

// ledgerRow is the canonical (address, counterparty, amount_p, time,
// action, direction, denom) tuple every section ends up producing. We
// build them in pure Go then hand off to execLedgerInserts which does
// the multi-row INSERT in batches.
type ledgerRow struct {
	Address      string
	Counterparty string // empty for non-transfer entries (bank, ore)
	AmountP      string
	Time         time.Time
	Action       string
	Direction    string
	Denom        string
}

func ledgerBankRows(doc *Document) []ledgerRow {
	rows := make([]ledgerRow, 0, len(doc.AppState.Bank.Balances))
	for _, b := range doc.AppState.Bank.Balances {
		for _, c := range b.Coins {
			rows = append(rows, ledgerRow{
				Address:   b.Address,
				AmountP:   c.Amount,
				Time:      doc.GenesisTime,
				Action:    "genesis",
				Direction: "credit",
				Denom:     c.Denom,
			})
		}
	}
	return rows
}

// insertDelegations writes two rows per delegation (delegator side,
// validator side) with denom='ualpha.infused' and direction='credit' on
// both — matching how the shell script and downstream consumers model
// staking positions: both parties carry credit-side accounting for the
// same locked stake, with counterparty cross-referenced.
func insertDelegations(ctx context.Context, tx pgx.Tx, doc *Document) (int64, error) {
	// Pre-build validator lookup once.
	type valInfo struct{ tokens, shares string }
	vals := make(map[string]valInfo, len(doc.AppState.Staking.Validators))
	for _, v := range doc.AppState.Staking.Validators {
		vals[v.OperatorAddress] = valInfo{tokens: v.Tokens, shares: v.DelegatorShares}
	}
	var rows []ledgerRow
	for _, d := range doc.AppState.Staking.Delegations {
		v, ok := vals[d.ValidatorAddress]
		if !ok {
			// Mirror the shell script: missing validator -> tokens=0,
			// shares=1 -> amount=0. Don't fail the whole import.
			v = valInfo{tokens: "0", shares: "1"}
		}
		amt, err := delegationAmount(d.Shares, v.tokens, v.shares)
		if err != nil {
			return 0, fmt.Errorf("delegation %s->%s: %w",
				d.DelegatorAddress, d.ValidatorAddress, err)
		}
		rows = append(rows,
			ledgerRow{
				Address:      d.DelegatorAddress,
				Counterparty: d.ValidatorAddress,
				AmountP:      amt,
				Time:         doc.GenesisTime,
				Action:       "genesis",
				Direction:    "credit",
				Denom:        "ualpha.infused",
			},
			ledgerRow{
				Address:      d.ValidatorAddress,
				Counterparty: d.DelegatorAddress,
				AmountP:      amt,
				Time:         doc.GenesisTime,
				Action:       "genesis",
				Direction:    "credit",
				Denom:        "ualpha.infused",
			},
		)
	}
	return execLedgerInserts(ctx, tx, rows)
}

// insertUnbondings flattens (.entries[].balance) into 2 ledger rows per
// entry, denom='ualpha.defusing', direction='credit'. Same shape as
// delegations, just a different denom.
func insertUnbondings(ctx context.Context, tx pgx.Tx, doc *Document) (int64, error) {
	var rows []ledgerRow
	for _, u := range doc.AppState.Staking.UnbondingDelegations {
		for _, e := range u.Entries {
			rows = append(rows,
				ledgerRow{
					Address:      u.DelegatorAddress,
					Counterparty: u.ValidatorAddress,
					AmountP:      e.Balance,
					Time:         doc.GenesisTime,
					Action:       "genesis",
					Direction:    "credit",
					Denom:        "ualpha.defusing",
				},
				ledgerRow{
					Address:      u.ValidatorAddress,
					Counterparty: u.DelegatorAddress,
					AmountP:      e.Balance,
					Time:         doc.GenesisTime,
					Action:       "genesis",
					Direction:    "credit",
					Denom:        "ualpha.defusing",
				},
			)
		}
	}
	return execLedgerInserts(ctx, tx, rows)
}

// insertPlayerOre walks app_state.structs.gridList for attribute IDs
// starting with "0-1-" and value != "0" (the chain's encoding for
// per-player ore balances), maps the player index back to its
// primaryAddress via playerList, and emits one ore credit per non-zero
// balance.
//
// Players whose index isn't in playerList are dropped silently —
// matches the shell script's `select($addr != "")` guard. Worth a TODO
// to log them when STRICT mode is added, but for now silent-drop keeps
// us bug-compatible with the existing importer.
func insertPlayerOre(ctx context.Context, tx pgx.Tx, doc *Document) (int64, error) {
	addrByIndex := make(map[string]string, len(doc.AppState.Structs.PlayerList))
	for _, p := range doc.AppState.Structs.PlayerList {
		addrByIndex[p.Index] = p.PrimaryAddress
	}
	var rows []ledgerRow
	for _, g := range doc.AppState.Structs.GridList {
		if !strings.HasPrefix(g.AttributeID, "0-1-") {
			continue
		}
		if g.Value == "" || g.Value == "0" {
			continue
		}
		parts := strings.Split(g.AttributeID, "-")
		if len(parts) < 3 {
			continue
		}
		addr, ok := addrByIndex[parts[2]]
		if !ok || addr == "" {
			continue
		}
		rows = append(rows, ledgerRow{
			Address:   addr,
			AmountP:   g.Value,
			Time:      doc.GenesisTime,
			Action:    "genesis",
			Direction: "credit",
			Denom:     "ore",
		})
	}
	return execLedgerInserts(ctx, tx, rows)
}

// execLedgerInserts performs the actual multi-row INSERT in
// batches. PG's default max parameter count is 65535, and each row
// here uses 7 placeholders → batchSize=8000 keeps us comfortably
// under (8000 * 7 = 56000). Most chain genesis files are
// orders of magnitude smaller than that; the chunking is here for
// production-scale safety, not testnet.
func execLedgerInserts(ctx context.Context, tx pgx.Tx, rows []ledgerRow) (int64, error) {
	if len(rows) == 0 {
		return 0, nil
	}
	const batchSize = 8000
	var total int64
	for i := 0; i < len(rows); i += batchSize {
		end := i + batchSize
		if end > len(rows) {
			end = len(rows)
		}
		n, err := execLedgerBatch(ctx, tx, rows[i:end])
		if err != nil {
			return total, err
		}
		total += n
	}
	return total, nil
}

func execLedgerBatch(ctx context.Context, tx pgx.Tx, rows []ledgerRow) (int64, error) {
	var (
		placeholders = make([]string, 0, len(rows))
		args         = make([]any, 0, len(rows)*7)
	)
	for _, r := range rows {
		base := len(args)
		placeholders = append(placeholders, fmt.Sprintf(
			"($%d, $%d, $%d, 0, $%d, $%d, $%d, $%d)",
			base+1, base+2, base+3, base+4, base+5, base+6, base+7,
		))
		// Order matches the column list below.
		var cp any
		if r.Counterparty != "" {
			cp = r.Counterparty
		} else {
			cp = nil
		}
		args = append(args,
			r.Address, cp, r.AmountP, r.Time, r.Action, r.Direction, r.Denom,
		)
	}
	sql := `
		INSERT INTO structs.ledger
			(address, counterparty, amount_p, block_height, time, action, direction, denom)
		VALUES ` + strings.Join(placeholders, ",")
	tag, err := tx.Exec(ctx, sql, args...)
	if err != nil {
		return 0, err
	}
	return tag.RowsAffected(), nil
}
