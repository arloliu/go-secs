package hsms

import (
	"encoding/binary"
	"sync/atomic"
)

// sysBytesGen generates monotonically-increasing, concurrency-safe HSMS System
// Bytes values. One generator lives on each connection; the counter wraps at
// 2^32 — the space is large enough that wrap-around during a single connection
// lifetime is not a practical concern (E37 §8.2.6.8 requires uniqueness only
// among currently-open transactions and the most-recently-completed one).
//
// The generator deliberately wraps rather than panics on overflow: E37 requires
// uniqueness, not strict monotonicity past the 32-bit boundary. In practice a
// connection that exhausts 2^32 System Bytes has processed ~4 billion transactions
// without recycling, which vastly exceeds any realistic session.
type sysBytesGen struct {
	n atomic.Uint32
}

// next returns the next System Bytes value as a 4-byte big-endian array.
// It is safe for concurrent use.
func (g *sysBytesGen) next() [4]byte {
	v := g.n.Add(1)
	var b [4]byte
	binary.BigEndian.PutUint32(b[:], v)
	return b
}
