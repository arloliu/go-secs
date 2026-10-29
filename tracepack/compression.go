package tracepack

import (
	"fmt"

	"github.com/arloliu/go-secs/tracepack/internal/codec"
)

// CompressionLevel selects how hard the zstd encoder of a Writer, a SegmentWriter or Merge works
// on the block bodies and the footer it encodes.
// Every level writes the same format, which every reader decodes alike:
// a level changes only the bytes written, their length, and the time taken to write and to read them.
// With Uncompressed set nothing is encoded and the level has no effect, although it is still checked.
//
// Equal options, the level included, write equal bytes with the same version of the zstd library tracepack uses;
// another version may write other bytes at any level.
type CompressionLevel uint8

const (
	// CompressionDefault is the zero value: the zstd library's default level, as every earlier version wrote.
	CompressionDefault CompressionLevel = iota
	// CompressionFastest encodes in about two thirds of the default's time, at the cost of slightly larger output:
	// for a writer whose CPU is the constraint.
	CompressionFastest
	// CompressionBest writes the smallest output, in about three to four times the default's time,
	// and its blocks take somewhat longer to read: for archives written once and kept.
	CompressionBest
)

var _ fmt.Stringer = CompressionLevel(0)

// String returns "default", "fastest" or "best", or "unknown(<n>)" for a value this package does not define.
func (l CompressionLevel) String() string {
	switch l {
	case CompressionDefault:
		return "default"
	case CompressionFastest:
		return "fastest"
	case CompressionBest:
		return "best"
	default:
		return fmt.Sprintf("unknown(%d)", uint8(l))
	}
}

// codecLevel returns the internal codec's level for l, which checkCompressionLevel accepted.
func (l CompressionLevel) codecLevel() int {
	if l == CompressionFastest {
		return codec.LevelFastest
	}
	if l == CompressionBest {
		return codec.LevelBest
	}

	return codec.LevelDefault
}

// checkCompressionLevel rejects a level this package does not define, naming the option field.
func checkCompressionLevel(field string, l CompressionLevel) error {
	if l > CompressionBest {
		return fmt.Errorf("tracepack: %s.CompressionLevel %v is not defined", field, l)
	}

	return nil
}
