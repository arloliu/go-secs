package tracepack

import (
	"bytes"
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

// AddPackEvidence folds the per-capture evidence of one pack, its F-5 statistics s, into e
// (the tracepack storage specification §5, Per capture).
//
// A nil s, for a pack whose footer is not used, sets e.Partial and changes nothing else.
// Otherwise every boundary of s is added to e.Boundaries,
// which are then sorted by seq, boundary kind, ts, epoch, gap_start and gap_end, an absent gap bound first,
// then by capture_id, so that only equal entries merge whatever the order of the folds,
// and each distinct entry is kept once;
// every close_seq of s is added to e.Closures, which then keep the smallest close_seq per epoch, in ascending epoch;
// and e.End is recomputed from e.Boundaries:
// EndStoppedUnclean when any is a stop-unclean, else EndStopped when any is a clean stop, else EndOpen.
// Partial is never cleared.
// Folding the same packs into an empty evidence in any order, any of them repeated, gives the same evidence.
//
// Which packs to fold is the catalog's registration policy, not the helper's:
// the storage specification records the evidence of every segment or converter archive presented for registration,
// rejected ones included.
//
// Parameters:
//   - e: the evidence to fold into; it must not be nil.
//   - s: the pack's statistics, as Reader.Stats returns them; nil for a pack without them.
//     AddPackEvidence only reads s and keeps nothing of it:
//     the boundaries it adds have fresh GapStart and GapEnd pointers.
func AddPackEvidence(e *CaptureEvidence, s *PackStats) {
	if s == nil {
		e.Partial = true

		return
	}

	for i := range s.Boundaries {
		e.Boundaries = append(e.Boundaries, cloneBoundary(s.Boundaries[i]))
	}
	slices.SortFunc(e.Boundaries, compareEvidenceBoundaries)
	e.Boundaries = slices.CompactFunc(e.Boundaries, func(a, b Boundary) bool { return compareEvidenceBoundaries(a, b) == 0 })

	for i := range s.Epochs {
		if ep := &s.Epochs[i]; ep.CloseSeq != nil {
			e.Closures = append(e.Closures, EpochClosure{Epoch: ep.Epoch, CloseSeq: *ep.CloseSeq})
		}
	}
	slices.SortFunc(e.Closures, func(a, b EpochClosure) int {
		return cmp.Or(cmp.Compare(a.Epoch, b.Epoch), cmp.Compare(a.CloseSeq, b.CloseSeq))
	})
	// The first closure of each epoch is its smallest.
	e.Closures = slices.CompactFunc(e.Closures, func(a, b EpochClosure) bool { return a.Epoch == b.Epoch })

	e.End = EndOpen
	for i := range e.Boundaries {
		if kind := e.Boundaries[i].Kind; kind == BoundaryKindStopUnclean {
			e.End = EndStoppedUnclean
		} else if kind == BoundaryKindStop && e.End == EndOpen {
			e.End = EndStopped
		}
	}
}

// compareEvidenceBoundaries orders capture-boundary entries by seq, boundary kind, ts, epoch, gap_start and gap_end,
// an absent gap bound before any present one, then by capture_id, so that only equal entries compare equal:
// without capture_id, two entries that differ only in it would compare equal,
// and which one a fold keeps would depend on the order of the folds.
func compareEvidenceBoundaries(a, b Boundary) int {
	return cmp.Or(
		cmp.Compare(a.Seq, b.Seq),
		cmp.Compare(a.Kind, b.Kind),
		cmp.Compare(a.TS, b.TS),
		cmp.Compare(a.Epoch, b.Epoch),
		compareOptional(a.GapStart, b.GapStart),
		compareOptional(a.GapEnd, b.GapEnd),
		bytes.Compare(a.Capture[:], b.Capture[:]),
	)
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
				Capture: capture, Seq: b.seq, Kind: b.kind, TS: b.ts, Epoch: b.epoch, GapStart: b.gapStart, GapEnd: b.gapEnd,
			})
		}
	}

	return out
}
