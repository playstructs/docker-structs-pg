package protocol

// The coordinated testnet release boundary. Other networks must add their
// verified boundary before relying on height-based required-field checks.
const V022TestnetHeight int64 = 3353000

func V022Active(chainID string, height int64) bool {
	return chainID == "structstestnet-111" && height >= V022TestnetHeight
}
