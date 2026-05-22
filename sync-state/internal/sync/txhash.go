package sync

import (
	"crypto/sha256"
	"encoding/hex"
	"strings"
)

// TxHash computes the CometBFT tx hash from raw tx bytes
// (sha256, hex upper).
func TxHash(rawTx []byte) string {
	sum := sha256.Sum256(rawTx)
	return strings.ToUpper(hex.EncodeToString(sum[:]))
}
