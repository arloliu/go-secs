package tracepack

import (
	"cmp"
	"slices"
)

// PackStats is the F-5 pack statistics of a pack whose footer a Reader uses (the tracepack format specification §10):
// the aggregate of its blocks' F-2 entries and F-3 summaries, which the footer validation recomputed.
//
// Reader.Stats returns a fresh PackStats on every call:
// its slices and pointers are the caller's, detached from the Reader.
type PackStats struct {
	// RecordCount is the number of records in the pack.
	RecordCount uint64
	// KindCounts, DirCounts and DecodeStatusCounts count the records per raw kind, dir and decode_status value,
	// as the footer stores them: trailing zero elements are kept, and an element past the end counts 0.
	KindCounts, DirCounts, DecodeStatusCounts []uint64
	// TSMin and TSMax are the smallest and largest ts_utc_ns of the pack's records;
	// meaningful only when RecordCount is not 0.
	TSMin, TSMax int64
	// ContentBytes is the record header and payload bytes of the pack's records.
	ContentBytes uint64
	// QualityUnion is the union of the records' quality bits.
	QualityUnion Quality
	// SeqRanges holds the seqs present, as sorted, non-overlapping, maximal ranges.
	SeqRanges []SeqRange
	// Epochs holds one entry per epoch present, in ascending epoch.
	Epochs []EpochStats
	// Boundaries holds one capture-boundary entry per capture-boundary record of the pack, in ascending seq,
	// entries of one seq in the footer's order;
	// each carries the capture_id of the pack's file header.
	Boundaries []Boundary
}

// SeqRange is one maximal run of consecutive seqs, First to Last inclusive.
type SeqRange struct {
	First, Last uint64
}

// EpochStats is the F-5 summary of one epoch of a pack (the tracepack format specification §10).
type EpochStats struct {
	Epoch       uint32
	RecordCount uint64
	// SeqFirst and SeqLast are the epoch's smallest and largest seq in the pack.
	SeqFirst, SeqLast uint64
	// TSMin and TSMax are the epoch's smallest and largest ts_utc_ns in the pack.
	TSMin, TSMax int64
	// CloseSeq is the smallest seq of a record of the pack that ends the epoch; nil when none does.
	CloseSeq *uint64
}

// normalize puts the validated F-5 statistics st in the order Reader.Stats returns them:
// epochs in ascending epoch, which parsePackStats made unique,
// and boundaries in ascending seq, entries of one seq in the footer's order.
func (st *packStats) normalize() {
	slices.SortFunc(st.epochs, compareEpochs)
	slices.SortStableFunc(st.boundaries, func(a, b boundarySummary) int { return cmp.Compare(a.seq, b.seq) })
}

// public returns st as a PackStats whose boundaries carry capture, every slice and pointer freshly allocated.
func (st *packStats) public(capture UUID) PackStats {
	out := PackStats{
		RecordCount:        st.recordCount,
		KindCounts:         slices.Clone(st.kindCounts),
		DirCounts:          slices.Clone(st.dirCounts),
		DecodeStatusCounts: slices.Clone(st.decodeStatusCounts),
		TSMin:              st.tsMin,
		TSMax:              st.tsMax,
		ContentBytes:       st.contentBytes,
		QualityUnion:       st.qualityUnion,
	}
	if len(st.seqRanges) > 0 {
		out.SeqRanges = make([]SeqRange, len(st.seqRanges))
		for i, r := range st.seqRanges {
			out.SeqRanges[i] = SeqRange{First: r.first, Last: r.last}
		}
	}
	if len(st.epochs) > 0 {
		out.Epochs = make([]EpochStats, len(st.epochs))
		for i := range st.epochs {
			ep := &st.epochs[i]
			out.Epochs[i] = EpochStats{
				Epoch: ep.epoch, RecordCount: ep.recordCount, SeqFirst: ep.seqFirst, SeqLast: ep.seqLast, TSMin: ep.tsMin, TSMax: ep.tsMax,
			}
			if ep.hasCloseSeq {
				out.Epochs[i].CloseSeq = new(ep.closeSeq)
			}
		}
	}
	if len(st.boundaries) > 0 {
		out.Boundaries = make([]Boundary, len(st.boundaries))
		for i := range st.boundaries {
			b := &st.boundaries[i]
			out.Boundaries[i] = cloneBoundary(Boundary{
				CaptureID: capture, Seq: b.seq, Kind: b.kind, TS: b.ts, Epoch: b.epoch, GapStart: b.gapStart, GapEnd: b.gapEnd,
			})
		}
	}

	return out
}
