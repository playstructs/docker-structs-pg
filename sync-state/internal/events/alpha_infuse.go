package events

import (
	"context"
	"encoding/json"
	"fmt"

	"github.com/jackc/pgx/v5"
	"sync-state/internal/payload"
)

type alphaInfuseHandler struct{}

func (alphaInfuseHandler) CompositeKey() string {
	return "structs.structs.EventAlphaInfuse.eventAlphaInfuseDetail"
}

func (alphaInfuseHandler) Handle(_ context.Context, _ pgx.Tx, b BlockContext, raw json.RawMessage) error {
	p, err := payload.DecodeVersioned[payload.AlphaRefine](raw, b.V022Active(), "vaultAddress")
	if err != nil {
		return err
	}
	if _, err := payload.DecodeRequired[payload.AlphaRefine](raw, "primaryAddress", "amount"); err != nil {
		return err
	}
	if p.PrimaryAddress == "" {
		return fmt.Errorf("alpha_infuse: empty primaryAddress")
	}
	// Generator funding carries a delta and vault address; reactor unbonding
	// carries aggregate pending balance with an empty vault. Neither receipt
	// independently authorizes an additional ledger movement.
	b.Dirty.Address(p.PrimaryAddress)
	b.Dirty.Address(p.VaultAddress)
	return nil
}
