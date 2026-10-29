package events

import (
	"context"
	"encoding/json"
	"fmt"
	"regexp"
	"strings"

	"github.com/jackc/pgx/v5"

	"sync-state/internal/payload"
)

var ownerIDPattern = regexp.MustCompile(`^[01]-[1-9][0-9]*$`)
var reactorIDPattern = regexp.MustCompile(`^3-[1-9][0-9]*$`)

func dirtyOwner(b BlockContext, id string) {
	if strings.HasPrefix(id, "0-") {
		b.Dirty.Guild(id)
	} else if strings.HasPrefix(id, "1-") {
		b.Dirty.Player(id)
	}
}

type vaultAddressHandler struct{}

func (vaultAddressHandler) CompositeKey() string {
	return "structs.structs.EventVaultAddress.eventVaultAddressDetail"
}

func (vaultAddressHandler) Handle(ctx context.Context, tx pgx.Tx, b BlockContext, raw json.RawMessage) error {
	p, err := payload.DecodeRequired[payload.VaultAddress](raw, "ownerId", "vaultAddress", "accountNumber", "accountCreated")
	if err != nil {
		return err
	}
	if !ownerIDPattern.MatchString(p.OwnerID) || p.VaultAddress == "" {
		return fmt.Errorf("vault: invalid owner or empty address")
	}
	// Vault identity is immutable. accountCreated=false still establishes the
	// association; it must not mint a deposit or grant signing authority.
	if _, err := tx.Exec(ctx, `INSERT INTO structs.vault(owner_id,address,account_number)
        VALUES ($1,$2,$3) ON CONFLICT DO NOTHING`, p.OwnerID, p.VaultAddress, p.AccountNumber.PgValue()); err != nil {
		return err
	}
	var matches bool
	if err := tx.QueryRow(ctx, `SELECT EXISTS (SELECT 1 FROM structs.vault
        WHERE owner_id=$1 AND address=$2 AND account_number=$3)`,
		p.OwnerID, p.VaultAddress, p.AccountNumber.PgValue()).Scan(&matches); err != nil {
		return err
	}
	if !matches {
		return fmt.Errorf("vault: conflicting identity for owner %s", p.OwnerID)
	}
	dirtyOwner(b, p.OwnerID)
	b.Dirty.Address(p.VaultAddress)
	return nil
}

type ownerReactorDiversityHandler struct{}

func (ownerReactorDiversityHandler) CompositeKey() string {
	return "structs.structs.EventOwnerReactorDiversity.diversity"
}

func (ownerReactorDiversityHandler) Handle(ctx context.Context, tx pgx.Tx, b BlockContext, raw json.RawMessage) error {
	p, err := payload.DecodeRequired[payload.OwnerReactorDiversity](raw, "ownerId", "power", "squaredPower", "appliedBonus")
	if err != nil {
		return err
	}
	if !ownerIDPattern.MatchString(p.OwnerID) {
		return fmt.Errorf("diversity: invalid owner %q", p.OwnerID)
	}
	if applied, err := economyEventApplied(ctx, tx, b, "diversity", p.OwnerID); err != nil || applied {
		return err
	}
	if _, err := tx.Exec(ctx, `INSERT INTO structs.owner_reactor_diversity(owner_id,power,squared_power,applied_bonus)
        VALUES ($1,$2,$3,$4) ON CONFLICT (owner_id) DO UPDATE SET power=EXCLUDED.power,
        squared_power=EXCLUDED.squared_power,applied_bonus=EXCLUDED.applied_bonus,updated_at=NOW()`,
		p.OwnerID, p.Power.PgValue(), p.SquaredPower.PgValue(), p.AppliedBonus.PgValue()); err != nil {
		return err
	}
	// The capacity event already includes the applied bonus.
	dirtyOwner(b, p.OwnerID)
	return recordEconomyPosition(ctx, tx, b, "diversity", p.OwnerID)
}

type reactorCommissionRevisionHandler struct{}

func (reactorCommissionRevisionHandler) CompositeKey() string {
	return "structs.structs.EventReactorCommissionRevision"
}

func (reactorCommissionRevisionHandler) Handle(ctx context.Context, tx pgx.Tx, b BlockContext, raw json.RawMessage) error {
	p, err := payload.DecodeRequired[payload.ReactorCommissionRevision](raw, "reactorId", "slashRevision", "reconciledSlashRevision")
	if err != nil {
		return err
	}
	if !reactorIDPattern.MatchString(p.ReactorID) || p.ReconciledSlashRevision > p.SlashRevision {
		return fmt.Errorf("commission: invalid reactor or revisions")
	}
	if applied, err := economyEventApplied(ctx, tx, b, "commission", p.ReactorID); err != nil || applied {
		return err
	}
	if _, err := tx.Exec(ctx, `INSERT INTO structs.reactor_commission_revision(reactor_id,slash_revision,reconciled_slash_revision)
        VALUES ($1,$2,$3) ON CONFLICT (reactor_id) DO UPDATE SET slash_revision=EXCLUDED.slash_revision,
        reconciled_slash_revision=EXCLUDED.reconciled_slash_revision,updated_at=NOW()`,
		p.ReactorID, p.SlashRevision.PgValue(), p.ReconciledSlashRevision.PgValue()); err != nil {
		return err
	}
	b.Dirty.Reactor(p.ReactorID)
	b.Dirty.NativeReactor(p.ReactorID)
	return recordEconomyPosition(ctx, tx, b, "commission", p.ReactorID)
}

type reactorStakeCapHandler struct{}

func (reactorStakeCapHandler) CompositeKey() string { return "structs.structs.EventReactorStakeCap" }

func (reactorStakeCapHandler) Handle(ctx context.Context, tx pgx.Tx, b BlockContext, raw json.RawMessage) error {
	p, err := payload.DecodeRequired[payload.ReactorStakeCap](raw, "active", "capBps", "minValidators")
	if err != nil {
		return err
	}
	if applied, err := economyEventApplied(ctx, tx, b, "stake_cap", "singleton"); err != nil || applied {
		return err
	}
	if _, err := tx.Exec(ctx, `INSERT INTO structs.reactor_stake_cap(singleton,active,cap_bps,min_validators)
        VALUES (TRUE,$1,$2,$3) ON CONFLICT (singleton) DO UPDATE SET active=EXCLUDED.active,
        cap_bps=EXCLUDED.cap_bps,min_validators=EXCLUDED.min_validators,updated_at=NOW()`,
		p.Active, p.CapBps.PgValue(), p.MinValidators.PgValue()); err != nil {
		return err
	}
	return recordEconomyPosition(ctx, tx, b, "stake_cap", "singleton")
}

type economyResetHandler struct{ family string }

func (h economyResetHandler) CompositeKey() string {
	if h.family == "diversity" {
		return "structs.structs.EventOwnerReactorDiversityReset"
	}
	return "structs.structs.EventReactorCommissionRevisionReset"
}

func (h economyResetHandler) Handle(ctx context.Context, tx pgx.Tx, b BlockContext, raw json.RawMessage) error {
	if _, err := payload.DecodeRequired[struct{}](raw); err != nil {
		return err
	}
	return resetEconomy(ctx, tx, b, h.family)
}

type guildRankPurchaseHandler struct{}

func (guildRankPurchaseHandler) CompositeKey() string {
	return "structs.structs.EventGuildRankPurchase"
}

func (guildRankPurchaseHandler) Handle(_ context.Context, _ pgx.Tx, b BlockContext, raw json.RawMessage) error {
	p, err := payload.DecodeRequired[payload.GuildRankPurchase](raw,
		"guildId", "playerId", "previousRank", "rank", "price", "denom", "joined")
	if err != nil {
		return err
	}
	if !ownerIDPattern.MatchString(p.GuildID) || !strings.HasPrefix(p.GuildID, "0-") ||
		!ownerIDPattern.MatchString(p.PlayerID) || !strings.HasPrefix(p.PlayerID, "1-") || p.Denom == "" {
		return fmt.Errorf("rank purchase: invalid guild, player or denom")
	}
	// The receipt does not move money or replace membership. Those have their
	// own authoritative events, including valid zero-price purchases.
	b.Dirty.Guild(p.GuildID)
	b.Dirty.Player(p.PlayerID)
	return nil
}
