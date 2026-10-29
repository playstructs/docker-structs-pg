package payload

// Guild matches structs.structs.EventGuild.guild.
// SQL handler: cache.handle_event_guild
// (cache-trigger-add-queue-20260427-ugc-fields.sql:132-247)
type Guild struct {
	ID                                 string  `json:"id"`
	Index                              JSONInt `json:"index"`
	Endpoint                           string  `json:"endpoint"`
	JoinInfusionMinimum                Uint64  `json:"joinInfusionMinimum"`
	JoinInfusionMinimumBypassByRequest string  `json:"joinInfusionMinimumBypassByRequest"`
	JoinInfusionMinimumBypassByInvite  string  `json:"joinInfusionMinimumBypassByInvite"`
	PrimaryReactorID                   string  `json:"primaryReactorId"`
	EntrySubstationID                  string  `json:"entrySubstationId"`
	EntryRank                          Uint64  `json:"entryRank"`
	Creator                            string  `json:"creator"`
	Owner                              string  `json:"owner"`
	Name                               string  `json:"name"`
	PFP                                string  `json:"pfp"`
	BankConvertInFee                   Numeric `json:"bankConvertInFee"`
	BankConvertOutFee                  Numeric `json:"bankConvertOutFee"`
	CharterSolverID                    string  `json:"charterSolverId"`
	RankPurchaseBasePrice              Uint64  `json:"rankPurchaseBasePrice"`
	RankPurchaseStepIncrease           Uint64  `json:"rankPurchaseStepIncrease"`
	RankPurchaseMinimumRank            Uint64  `json:"rankPurchaseMinimumRank"`
	JoinByRankPurchaseEnabled          bool    `json:"joinByRankPurchaseEnabled"`
}
