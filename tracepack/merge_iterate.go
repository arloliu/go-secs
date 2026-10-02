package tracepack

import "fmt"

// Default MergeIterateOptions values, which a zero or negative field takes.
const (
	// DefaultMaxHeldBytes is the default MergeIterateOptions.MaxHeldBytes: 512 MiB.
	DefaultMaxHeldBytes = 512 << 20
	// DefaultMaxConflicts is the default MergeIterateOptions.MaxConflicts: 65536 conflicts.
	DefaultMaxConflicts = 1 << 16
)

// Orders of a read over several packs (the tracepack semantics specification §7.4).
const (
	// OrderCapture yields each capture's records in ascending seq,
	// captures interleaved block by block.
	OrderCapture Order = iota + 1
	// OrderTime yields the records by (ts_utc_ns, capture_id, seq), then version.
	OrderTime
)

// Order selects the order of MergeIterate.
//
// The zero value is not an order; String prints it and any undefined value as "unknown(<n>)".
type Order uint8

var _ fmt.Stringer = Order(0)

// MergeIterateOptions configures MergeIterate.
type MergeIterateOptions struct {
	// Order is the order the records are yielded in; it is required.
	Order Order
	// MaxHeldBytes bounds the bytes of the block buffers MergeIterate holds at once,
	// reserved before each block is read;
	// zero or negative means DefaultMaxHeldBytes.
	// It does not bound the process heap:
	// the readers, the block descriptors and clusters MergeIterate builds before reading,
	// its queues, codec state and Result.Conflicts lie outside it.
	// A read that would exceed it ends with an error wrapping ErrReadLimit, returned with the Result found so far.
	MaxHeldBytes int64
	// MaxConflicts bounds len(Result.Conflicts), not the size of one conflict;
	// zero or negative means DefaultMaxConflicts.
	// One conflict more ends the read with an error wrapping ErrReadLimit, returned with the Result found so far.
	MaxConflicts int
}

// PackError is an error about one pack of a read that does not make the read incomplete.
type PackError struct {
	// Pack is the index into the readers given to MergeIterate of the pack the error concerns; 0 for Reader.Iterate.
	Pack int
	// Err describes the error.
	Err error
}

var _ error = PackError{}

// String returns the order's name, or "unknown(<n>)" for a value this package does not define.
func (o Order) String() string {
	switch o {
	case OrderCapture:
		return "capture"
	case OrderTime:
		return "time"
	default:
		return fmt.Sprintf("unknown(%d)", uint8(o))
	}
}

// Error returns Err's text, prefixed with the pack index.
func (e PackError) Error() string {
	return fmt.Sprintf("tracepack: pack %d: %v", e.Pack, e.Err)
}

// Unwrap returns Err, so errors.Is and errors.As see the error about the pack.
func (e PackError) Unwrap() error {
	return e.Err
}
