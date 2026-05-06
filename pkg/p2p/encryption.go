package p2p

import (
	"strings"
)

// GetP2PEncryptionKey derives the LEVEL_1 P2P encryption key.
// Formula: last 7 chars of station SN + 9 chars from p2pDid starting at first dash.
// Example: SN="T8420N1234567890", p2pDid="ABCDEF-123456-XYZ"
//   → "4567890" + "-123456-X" = "4567890-123456-X" (16 chars)
// The dash IS included in the p2pDid substring (Node.js includes it).
func GetP2PEncryptionKey(serialNumber, p2pDid string) string {
	if len(serialNumber) < 7 {
		return ""
	}

	serialSuffix := serialNumber[len(serialNumber)-7:]

	dashIdx := strings.Index(p2pDid, "-")
	if dashIdx < 0 {
		return ""
	}

	end := dashIdx + 9
	if end > len(p2pDid) {
		end = len(p2pDid)
	}

	return serialSuffix + p2pDid[dashIdx:end]
}
