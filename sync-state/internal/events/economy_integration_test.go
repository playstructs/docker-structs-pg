package events

import (
	"context"
	"encoding/json"
	"testing"

	"github.com/jackc/pgx/v5"
	"sync-state/internal/rpc"
)

func TestEconomy_VaultIdentityWithoutMovement(t *testing.T) {
	inTx(t, connect(t), func(tx pgx.Tx) {
		ctx, bc := context.Background(), bctx()
		raw := json.RawMessage(`{"ownerId":"1-990995","vaultAddress":"prefunded-vault","accountNumber":"18446744073709551615","accountCreated":false}`)
		for range 2 {
			if err := (vaultAddressHandler{}).Handle(ctx, tx, bc, raw); err != nil {
				t.Fatal(err)
			}
		}
		var account string
		if err := tx.QueryRow(ctx, `SELECT account_number::text FROM structs.vault WHERE owner_id='1-990995'`).Scan(&account); err != nil || account != "18446744073709551615" {
			t.Fatalf("account=%s err=%v", account, err)
		}
		var signing int
		if err := tx.QueryRow(ctx, `SELECT count(*) FROM structs.player_address WHERE address='prefunded-vault'`).Scan(&signing); err != nil || signing != 0 {
			t.Fatalf("vault became a signing address: %d %v", signing, err)
		}
		if len(bc.Buf.Ledger) != 0 || len(bc.Dirty.Addresses) != 1 || len(bc.Dirty.Players) != 1 {
			t.Fatal("association must reattribute existing assets without a financial leg")
		}
		for _, bad := range []string{
			`{"ownerId":"1-990995","vaultAddress":"different","accountNumber":"18446744073709551615","accountCreated":false}`,
			`{"ownerId":"0-990995","vaultAddress":"prefunded-vault","accountNumber":"0","accountCreated":false}`,
			`{"ownerId":"1-990995","vaultAddress":"prefunded-vault","accountNumber":"0","accountCreated":false}`,
			`{"ownerId":"1-990995","vaultAddress":"prefunded-vault","accountCreated":false}`,
		} {
			if err := (vaultAddressHandler{}).Handle(ctx, tx, bc, json.RawMessage(bad)); err == nil {
				t.Fatalf("accepted conflicting/incomplete association: %s", bad)
			}
		}
	})
}

func TestEconomy_VaultAccountOriginIsEvidenceNotADefault(t *testing.T) {
	for _, created := range []bool{false, true} {
		t.Run(map[bool]string{false: "adopted", true: "created"}[created], func(t *testing.T) {
			inTx(t, connect(t), func(tx pgx.Tx) {
				ctx, bc := context.Background(), bctx()
				if _, err := tx.Exec(ctx, `INSERT INTO structs.vault(owner_id,address,account_number)
                    VALUES('1-990991','vault-origin-fixture',0)`); err != nil {
					t.Fatal(err)
				}
				var unknown bool
				if err := tx.QueryRow(ctx, `SELECT account_created IS NULL FROM structs.vault WHERE owner_id='1-990991'`).Scan(&unknown); err != nil || !unknown {
					t.Fatalf("historical origin was invented: %v %v", unknown, err)
				}
				apply := func(value bool) error {
					return (vaultAddressHandler{}).Handle(ctx, tx, bc, mustJSON(t, map[string]any{
						"ownerId": "1-990991", "vaultAddress": "vault-origin-fixture", "accountNumber": "0", "accountCreated": value,
					}))
				}
				for range 2 {
					if err := apply(created); err != nil {
						t.Fatal(err)
					}
				}
				if err := apply(!created); err == nil {
					t.Fatal("conflicting origin changed spendability evidence")
				}
				var actual bool
				if err := tx.QueryRow(ctx, `SELECT account_created FROM structs.vault WHERE owner_id='1-990991'`).Scan(&actual); err != nil || actual != created {
					t.Fatalf("origin=%v want=%v err=%v", actual, created, err)
				}
			})
		})
	}
}

func TestEconomy_RevisionResetAndPhaseReplay(t *testing.T) {
	inTx(t, connect(t), func(tx pgx.Tx) {
		ctx, bc := context.Background(), bctx()
		apply := func(index int, revision string) {
			bc.EventIndex = index
			handle(t, ctx, tx, reactorCommissionRevisionHandler{}, bc, mustJSON(t, map[string]string{
				"reactorId": "3-990996", "slashRevision": revision, "reconciledSlashRevision": revision,
			}))
		}
		apply(0, "18446744073709551615")
		bc.EventIndex = 1
		handle(t, ctx, tx, economyResetHandler{"commission"}, bc, json.RawMessage(`{}`))
		apply(2, "0") // reset permits a lower pair
		bc.Phase, bc.TxIndex = rpc.Transaction, 0
		apply(0, "2")
		bc.Phase, bc.TxIndex = rpc.EndBlock, -1
		apply(3, "3")
		bc.Phase, bc.TxIndex, bc.EventIndex = rpc.PreBlock, -1, 1
		handle(t, ctx, tx, economyResetHandler{"commission"}, bc, json.RawMessage(`{}`))
		apply(2, "0") // retry cannot replace EndBlock
		var revision string
		if err := tx.QueryRow(ctx, `SELECT slash_revision::text FROM structs.reactor_commission_revision WHERE reactor_id='3-990996'`).Scan(&revision); err != nil || revision != "3" {
			t.Fatalf("revision=%s err=%v", revision, err)
		}
		bc.Height++
		bc.EventIndex = 0
		handle(t, ctx, tx, economyResetHandler{"commission"}, bc, json.RawMessage(`{}`))
		bc.Height--
		apply(2, "1") // omitted entity stays absent after a later reset
		var count int
		if err := tx.QueryRow(ctx, `SELECT count(*) FROM structs.reactor_commission_revision WHERE reactor_id='3-990996'`).Scan(&count); err != nil || count != 0 {
			t.Fatalf("reset resurrected row: %d %v", count, err)
		}
	})
}

func TestEconomy_ExactDiversityZeroCapAndReceipt(t *testing.T) {
	inTx(t, connect(t), func(tx pgx.Tx) {
		ctx, bc := context.Background(), bctx()
		handle(t, ctx, tx, ownerReactorDiversityHandler{}, bc, json.RawMessage(`{"ownerId":"0-990997","power":"18446744073709551616","squaredPower":"340282366920938463463374607431768211456","appliedBonus":"0"}`))
		var squared string
		if err := tx.QueryRow(ctx, `SELECT squared_power::text FROM structs.owner_reactor_diversity WHERE owner_id='0-990997'`).Scan(&squared); err != nil || squared != "340282366920938463463374607431768211456" {
			t.Fatalf("squared=%s err=%v", squared, err)
		}
		bc.EventIndex++
		handle(t, ctx, tx, ownerReactorDiversityHandler{}, bc, json.RawMessage(`{"ownerId":"0-990997","power":"0","squaredPower":"0","appliedBonus":"0"}`))
		if err := tx.QueryRow(ctx, `SELECT squared_power::text FROM structs.owner_reactor_diversity WHERE owner_id='0-990997'`).Scan(&squared); err != nil || squared != "0" {
			t.Fatalf("zero replacement=%s err=%v", squared, err)
		}
		handle(t, ctx, tx, reactorStakeCapHandler{}, bc, json.RawMessage(`{"active":true,"capBps":"1000","minValidators":"10"}`))
		bc.EventIndex++
		handle(t, ctx, tx, reactorStakeCapHandler{}, bc, json.RawMessage(`{"active":false,"capBps":"0","minValidators":"0"}`))
		var active bool
		if err := tx.QueryRow(ctx, `SELECT active FROM structs.reactor_stake_cap`).Scan(&active); err != nil || active {
			t.Fatalf("false snapshot not applied: %v %v", active, err)
		}
		if err := (reactorStakeCapHandler{}).Handle(ctx, tx, bc, json.RawMessage(`{"capBps":"0","minValidators":"0"}`)); err == nil {
			t.Fatal("missing active treated as false")
		}
		handle(t, ctx, tx, guildRankPurchaseHandler{}, bc, json.RawMessage(`{"guildId":"0-990997","playerId":"1-990997","previousRank":"0","rank":"18446744073709551615","price":"0","denom":"uguild.0-990997","joined":true}`))
		if len(bc.Buf.Ledger) != 0 {
			t.Fatal("rank receipt created a payment")
		}
	})
}
