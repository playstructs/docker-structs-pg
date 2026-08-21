package payload

// GuildBankAddress matches structs.structs.EventGuildBankAddress.eventGuildBankAddressDetail.
// SQL handler: cache.handle_event_guild_bank_address
// (cache-trigger-add-queue-20260121-bigly-refactor.sql:2210-2249)
//
// Writes TWO address_tag rows for the bankCollateralPool address:
//   - Type='Bank Collateral Pool'
//   - GuildId=guildId
type GuildBankAddress struct {
	BankCollateralPool string `json:"bankCollateralPool"`
	GuildID            string `json:"guildId"`
	// v0.21.0 also emits bankTokenPool. Guild-bank collateral still
	// comes from the Bank Collateral Pool tag; the token-pool address
	// is ignored until product asks to index it.
}
