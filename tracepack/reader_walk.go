package tracepack

import (
	"context"
	"errors"
	"fmt"
	"math"
	"math/bits"

	"github.com/arloliu/go-secs/tracepack/internal/format"
)

// errNotFinalized is the defect of a pack that is not finalized: more records may follow the walked blocks.
var errNotFinalized = errors.New("tracepack: pack not finalized: more records may follow")

// walk builds the block index by the forward walk of I-1 and records the defects every read of the pack carries.
//
// The walk starts at the end of the pack metadata and ends at footer_offset when the trailer is valid,
// else at the end of the object.
// It reads one block envelope per ReadAt and applies only the checks of the envelope decoder:
// the magic, the envelope CRC and the format limits of the tracepack format specification §2, never MaxBlockLen,
// so a block over the budget is walked past by its checked length.
// An envelope that does not fit before the end, a bad envelope, or a body past the end stops the walk
// with a ReasonTruncated defect at the envelope's offset;
// recovery beyond such a block is deferred (§13).
// A block past ReaderOptions.MaxWalkedBlocks stops the walk with a ReasonLimit defect, wrapping ErrReadLimit, at its offset.
// Walked blocks are not indexed.
func (r *Reader) walk(ctx context.Context) error {
	end := uint64(r.size)
	if r.trailer != nil {
		end = r.trailer.FooterOffset
	}

	var (
		env     [format.EnvelopeLen]byte
		records uint64
		stop    *Defect
	)
	off := r.blocksStart()
	for off < end {
		if end-off < format.EnvelopeLen {
			stop = walkStop(off, fmt.Errorf("tracepack: forward walk: %d bytes at offset %d cannot hold a block envelope", end-off, off))
			break
		}
		if err := readRound(ctx, r.ra, []readReq{{off: int64(off), buf: env[:]}}); err != nil {
			return err
		}

		e, err := format.UnmarshalBlockEnvelope(env[:])
		if err != nil {
			stop = walkStop(off, fmt.Errorf("tracepack: forward walk: block at offset %d: %w", off, err))
			break
		}
		// body_len is at most 2^31-1 and off below end, which is at most 2^63-1, so next cannot wrap.
		next := off + format.EnvelopeLen + uint64(e.BodyLen)
		if next > end {
			stop = walkStop(off, fmt.Errorf("tracepack: forward walk: block at offset %d ends at %d, past the block region's end %d", off, next, end))
			break
		}

		if len(r.blocks) >= r.opts.MaxWalkedBlocks {
			stop = &Defect{Reason: ReasonLimit, Block: -1, Offset: int64(off), Err: fmt.Errorf(
				"tracepack: forward walk: block at offset %d is past MaxWalkedBlocks %d: %w", off, r.opts.MaxWalkedBlocks, ErrReadLimit)}
			break
		}
		r.blocks = append(r.blocks, walkedInfo(off, &e))
		records = addSat(records, uint64(e.RecordCount))
		off = next
	}

	r.walkStop = stop
	r.openDefects = r.walkDefects(stop, off, records)

	return nil
}

// walkStop returns the defect of a forward walk that stops at off:
// at a block envelope it cannot account for, or at the end of a pack that is not finalized.
func walkStop(off uint64, err error) *Defect {
	return &Defect{Reason: ReasonTruncated, Block: -1, Offset: int64(off), Err: err}
}

// walkedInfo returns the BlockInfo of the block whose envelope e the forward walk found at off.
func walkedInfo(off uint64, e *format.BlockEnvelope) BlockInfo {
	return BlockInfo{
		Offset: off,
		// body_len is at most 2^31-1, so the on-disk length fits 32 bits.
		OnDiskLen:       format.EnvelopeLen + e.BodyLen,
		UncompressedLen: e.UncompressedLen,
		RecordCount:     e.RecordCount,
		BodyCRC:         e.BodyCRC,
		RecordHeaderLen: e.RecordHeaderLen,
		FirstSeq:        e.FirstSeq,
	}
}

// addSat returns a + b, or the largest uint64 when the sum overflows.
func addSat(a, b uint64) uint64 {
	sum, carry := bits.Add64(a, b, 0)
	if carry != 0 {
		return math.MaxUint64
	}

	return sum
}

// walkDefects returns the defects of a forward walk that ended at end, or stopped with stop,
// having walked blocks holding records records in all:
// the stop; else, for a pack that is not finalized, one defect at end, since more records may follow;
// else, for a finalized pack, one defect when the walk disagrees with the trailer's block_count or record_count.
// A pack carries at most one such defect.
func (r *Reader) walkDefects(stop *Defect, end, records uint64) []Defect {
	switch {
	case stop != nil:
		return []Defect{*stop}
	case !r.finalized:
		return []Defect{*walkStop(end, errNotFinalized)}
	case uint64(len(r.blocks)) != uint64(r.trailer.BlockCount) || records != r.trailer.RecordCount:
		return []Defect{{Reason: ReasonTruncated, Block: -1, Offset: -1, Err: fmt.Errorf(
			"tracepack: forward walk found %d blocks of %d records, the trailer claims %d blocks of %d records",
			len(r.blocks), records, r.trailer.BlockCount, r.trailer.RecordCount)}}
	default:
		return nil
	}
}
