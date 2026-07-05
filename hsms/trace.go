package hsms

import "encoding/hex"

// hexDump renders b as a lowercase hex string, for WithTraceTraffic's per-frame wire dumps.
func hexDump(b []byte) string {
	return hex.EncodeToString(b)
}
