package events

// THE COMPLETE LIST OF EVENTS sync-state KNOWS HOW TO PROCESS.
//
// Add a new event:
//   1. Add the payload struct to internal/payload/<event_name>.go.
//   2. Create internal/events/<event_name>.go implementing Handler.
//   3. Add one line to AllHandlers() below (alphabetical by composite_key).
//
// Run `sync-state list-handlers` for the runtime view; this slice and the
// runtime view should always agree.
//
// Every handler unconditionally owns its derived side-effects (planet.name
// UGC, infusion ledger, planet_activity emits, address-guild propagation).
// The historical SYNC_STATE_OWN_* flags were retired with the cache.*
// subsystem.
func AllHandlers() []Handler {
	return []Handler{
		// Phase 2 — entity handlers (12)
		allocationHandler{},
		agreementHandler{},
		guildHandler{},
		infusionHandler{},
		fleetHandler{},
		planetHandler{},
		playerHandler{},
		providerHandler{},
		reactorHandler{},
		structHandler{},
		structTypeHandler{},
		substationHandler{},

		// Phase 3 — membership / address / permission / defender (8)
		guildMembershipApplicationHandler{},
		addressHandler{},
		addressActivityHandler{},
		addressAssociationHandler{},
		permissionHandler{},
		guildRankPermissionHandler{},
		structDefenderHandler{},
		structDefenderClearHandler{},

		// Phase 4 — grid / attribute (3)
		gridHandler{},
		structAttributeHandler{},
		planetAttributeHandler{},

		// Phase 5 — activity / ledger (9)
		timeHandler{}, // no-op; sync-state owns current_block (see time.go)
		attackHandler{},
		oreMineHandler{},
		oreMigrateHandler{},
		oreTheftHandler{},
		alphaRefineHandler{},
		raidHandler{},
		providerAddressHandler{},
		guildBankAddressHandler{},
	}
}
