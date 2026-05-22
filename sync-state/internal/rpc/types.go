// Package rpc is a small CometBFT/Tendermint JSON-RPC client used by
// sync-state to fetch blocks, block_results, status, and tx_search results
// from a structsd node's :26657 endpoint.
package rpc

import (
	"encoding/json"
	"strconv"
	"time"
)

// Envelope is the standard JSON-RPC 2.0 reply envelope CometBFT uses.
type Envelope struct {
	JSONRPC string          `json:"jsonrpc"`
	ID      json.RawMessage `json:"id"`
	Result  json.RawMessage `json:"result"`
	Error   *RPCError       `json:"error"`
}

type RPCError struct {
	Code    int    `json:"code"`
	Message string `json:"message"`
	Data    string `json:"data"`
}

// --- /status ---------------------------------------------------------------

type StatusResult struct {
	NodeInfo struct {
		Network string `json:"network"`
	} `json:"node_info"`
	SyncInfo struct {
		LatestBlockHeight   string `json:"latest_block_height"`
		EarliestBlockHeight string `json:"earliest_block_height"`
		LatestBlockTime     string `json:"latest_block_time"`
		CatchingUp          bool   `json:"catching_up"`
	} `json:"sync_info"`
}

func (s *StatusResult) Latest() int64 {
	h, _ := strconv.ParseInt(s.SyncInfo.LatestBlockHeight, 10, 64)
	return h
}

func (s *StatusResult) Earliest() int64 {
	h, _ := strconv.ParseInt(s.SyncInfo.EarliestBlockHeight, 10, 64)
	return h
}

// --- /block ----------------------------------------------------------------

type BlockResult struct {
	BlockID struct {
		Hash string `json:"hash"`
	} `json:"block_id"`
	Block struct {
		Header struct {
			Height          string    `json:"height"`
			ChainID         string    `json:"chain_id"`
			Time            time.Time `json:"time"`
			ProposerAddress string    `json:"proposer_address"`
		} `json:"header"`
		Data struct {
			Txs []string `json:"txs"`
		} `json:"data"`
	} `json:"block"`
}

// --- /block_results --------------------------------------------------------

type Attribute struct {
	Key   string `json:"key"`
	Value string `json:"value"`
	Index bool   `json:"index"`
}

type Event struct {
	Type       string      `json:"type"`
	Attributes []Attribute `json:"attributes"`
}

type TxResult struct {
	Code      int     `json:"code"`
	Data      string  `json:"data"`
	Log       string  `json:"log"`
	Info      string  `json:"info"`
	GasWanted string  `json:"gas_wanted"`
	GasUsed   string  `json:"gas_used"`
	Events    []Event `json:"events"`
	Codespace string  `json:"codespace"`
}

type BlockResultsResult struct {
	Height              string     `json:"height"`
	TxsResults          []TxResult `json:"txs_results"`
	FinalizeBlockEvents []Event    `json:"finalize_block_events"`
}

// --- /tx_search ------------------------------------------------------------

type TxSearchResult struct {
	Txs        []TxSearchTx `json:"txs"`
	TotalCount string       `json:"total_count"`
}

type TxSearchTx struct {
	Hash     string   `json:"hash"`
	Height   string   `json:"height"`
	Index    int      `json:"index"`
	TxResult TxResult `json:"tx_result"`
}

// --- /blockchain -----------------------------------------------------------

type BlockchainResult struct {
	LastHeight string         `json:"last_height"`
	BlockMetas []BlockMetaRow `json:"block_metas"`
}

type BlockMetaRow struct {
	NumTxs string `json:"num_txs"`
	Header struct {
		Height string `json:"height"`
	} `json:"header"`
}
