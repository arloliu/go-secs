package corpus

import (
	"bytes"
	"cmp"
	"fmt"
	"slices"

	"github.com/arloliu/go-secs/tracepack"
)

// Fact is one fact of a lookup's gaps (the tracepack corpus specification §5.11, Facts):
// one condition that keeps the lookup from establishing absence or makes it incomplete,
// its keys present as the fact table gives them for its reason.
type Fact struct {
	Reason string `json:"reason"`
	// Hours is ascending; omitted for a reason without hours.
	Hours    []I64         `json:"hours,omitempty"`
	Pack     *int          `json:"pack,omitempty"`
	Block    *int          `json:"block,omitempty"`
	Offset   *U64          `json:"offset,omitempty"`
	Seq      *U64          `json:"seq,omitempty"`
	Defect   string        `json:"defect,omitempty"`
	Coverage *Coverage     `json:"coverage,omitempty"`
	Boundary *FactBoundary `json:"boundary,omitempty"`
	// Basis holds the bases of a barrier, in the order of the basis table; omitted for other reasons.
	Basis []string `json:"basis,omitempty"`
}

// FactBoundary is the boundary of a barrier or capture-boundary fact:
// the boundary's capture, then the format specification's nested boundary names.
// A capture-boundary fact holds only CaptureID, Seq, BoundaryKind and Epoch.
type FactBoundary struct {
	CaptureID    string `json:"capture_id"`
	Seq          U64    `json:"seq"`
	BoundaryKind string `json:"boundary_kind"`
	TS           *I64   `json:"ts,omitempty"`
	Epoch        uint32 `json:"epoch"`
	GapStart     *I64   `json:"gap_start,omitempty"`
	GapEnd       *I64   `json:"gap_end,omitempty"`
}

// Fact reasons, in the order of the fact table of the tracepack corpus specification §5.11.
const (
	FactNoKey             = "no-key"
	FactConflict          = "conflict"
	FactIndex             = "index"
	FactScopeBreach       = "scope-breach"
	FactCold              = "cold"
	FactConflicted        = "conflicted"
	FactRead              = "read"
	FactCoverage          = "coverage"
	FactUnevaluated       = "unevaluated"
	FactEvidence          = "evidence"
	FactBarrier           = "barrier"
	FactCaptureBoundary   = "capture-boundary"
	FactOrderingUncertain = "ordering-uncertain"
	FactCorrelation       = "correlation"
	FactSeqGap            = "seq-gap"
	FactOpenWindow        = "open-window"
	FactUnavailable       = "unavailable"
	FactContradiction     = "contradiction"
)

// Bases of a barrier fact, in the order of the tracepack corpus specification §5.11.
const (
	BasisEpoch = "epoch"
	BasisTime  = "time"
)

// factKey is one key of a fact, a bit of a set of keys.
type factKey uint16

const (
	keyHours factKey = 1 << iota
	keyPack
	keyBlock
	keyOffset
	keySeq
	keyDefect
	keyCoverage
	keyBoundary
	keyBasis
)

// factRow is one row of the fact table: a reason and the keys its facts hold, always or when they have a value.
type factRow struct {
	reason             string
	required, optional factKey
}

// factTable is the fact table of the tracepack corpus specification §5.11, its rows in the order of reasons.
var factTable = []factRow{
	{FactNoKey, 0, 0},
	{FactConflict, keyHours | keySeq, 0},
	{FactIndex, keyHours | keyPack | keyBlock | keyOffset, 0},
	{FactScopeBreach, keyHours | keyPack | keyBlock | keySeq, 0},
	{FactCold, keyHours, 0},
	{FactConflicted, keyHours, 0},
	{FactRead, keyHours | keyPack | keyDefect, keyBlock | keyOffset},
	{FactCoverage, keyHours | keyPack | keyCoverage, 0},
	{FactUnevaluated, keyHours | keyPack, 0},
	{FactEvidence, 0, 0},
	{FactBarrier, keyBoundary | keyBasis, 0},
	{FactCaptureBoundary, keyBoundary, 0},
	{FactOrderingUncertain, 0, 0},
	{FactCorrelation, 0, 0},
	{FactSeqGap, 0, 0},
	{FactOpenWindow, 0, 0},
	{FactUnavailable, keyHours | keyPack | keyBlock | keySeq, 0},
	{FactContradiction, keySeq, 0},
}

// Reasons of a read defect of a read fact, in the order of the tracepack corpus specification §5.11.
var defectOrder = []string{"truncated", "corrupt-block", "unknown-codec"}

// basisOrder holds the bases of a barrier in the order of the tracepack corpus specification §5.11.
var basisOrder = []string{BasisEpoch, BasisTime}

// incompleteFacts are the reasons that make a lookup's outcome incomplete, the first four rows of the fact table.
var incompleteFacts = []string{FactNoKey, FactConflict, FactIndex, FactScopeBreach}

// factRowOf returns the row of reason and its position in the fact table, or -1.
func factRowOf(reason string) (factRow, int) {
	i := slices.IndexFunc(factTable, func(r factRow) bool { return r.reason == reason })
	if i < 0 {
		return factRow{}, -1
	}

	return factTable[i], i
}

// keys returns the set of keys f holds.
func (f *Fact) keys() factKey {
	var k factKey
	for _, x := range []struct {
		key factKey
		has bool
	}{
		{keyHours, len(f.Hours) > 0}, {keyPack, f.Pack != nil}, {keyBlock, f.Block != nil}, {keyOffset, f.Offset != nil},
		{keySeq, f.Seq != nil}, {keyDefect, f.Defect != ""}, {keyCoverage, f.Coverage != nil},
		{keyBoundary, f.Boundary != nil}, {keyBasis, len(f.Basis) > 0},
	} {
		if x.has {
			k |= x.key
		}
	}

	return k
}

// check checks f against its row of the fact table and the key rules of the tracepack corpus specification §5.11:
// the keys its reason gives and no other, hours strictly ascending, one hour but for a conflict,
// a defect with the block and offset §5.4 gives it,
// a barrier's boundary with ts and stop-unclean and its bases in table order,
// and a capture-boundary's boundary with only its four keys.
func (f *Fact) check() error {
	row, i := factRowOf(f.Reason)
	if i < 0 {
		return fmt.Errorf("corpus: fact reason %q is not defined", f.Reason)
	}
	if k := f.keys(); k&row.required != row.required || k&^(row.required|row.optional) != 0 {
		return fmt.Errorf("corpus: a %s fact holds keys %#x, not those of its row", f.Reason, uint16(k))
	}
	for j := 1; j < len(f.Hours); j++ {
		if f.Hours[j] <= f.Hours[j-1] {
			return fmt.Errorf("corpus: a %s fact's hours are not strictly ascending", f.Reason)
		}
	}
	if len(f.Hours) > 1 && f.Reason != FactConflict {
		return fmt.Errorf("corpus: a %s fact holds %d hours", f.Reason, len(f.Hours))
	}
	for _, p := range []*int{f.Pack, f.Block} {
		if p != nil && *p < 0 {
			return fmt.Errorf("corpus: a %s fact names a negative pack or block", f.Reason)
		}
	}
	if err := f.checkDefect(); err != nil {
		return err
	}

	return f.checkValues()
}

// checkDefect checks that a read fact's defect is defined and holds the block and offset §5.4 gives it.
func (f *Fact) checkDefect() error {
	switch f.Defect {
	case "":
		return nil
	case "truncated":
		if f.Block != nil {
			return fmt.Errorf("corpus: a truncated read fact names a block")
		}
	case "corrupt-block", "unknown-codec":
		if f.Block == nil || f.Offset == nil {
			return fmt.Errorf("corpus: a %s read fact needs its block and offset", f.Defect)
		}
	default:
		return fmt.Errorf("corpus: read defect %q is not defined", f.Defect)
	}

	return nil
}

// checkValues checks the coverage, boundary and basis of f.
func (f *Fact) checkValues() error {
	if f.Coverage != nil && f.Coverage.CaptureID != nil {
		if _, err := parseUUID(*f.Coverage.CaptureID); err != nil {
			return err
		}
	}
	if f.Boundary != nil {
		if err := f.Boundary.check(f.Reason == FactBarrier); err != nil {
			return err
		}
	}
	for j, b := range f.Basis {
		k := slices.Index(basisOrder, b)
		if k < 0 || (j > 0 && k <= slices.Index(basisOrder, f.Basis[j-1])) {
			return fmt.Errorf("corpus: barrier basis %v is not in the order %v", f.Basis, basisOrder)
		}
	}

	return nil
}

// check checks b, the boundary of a barrier fact when barrier, of a capture-boundary fact otherwise.
func (b *FactBoundary) check(barrier bool) error {
	if _, err := parseUUID(b.CaptureID); err != nil {
		return err
	}
	kind, ok := enumByName[tracepack.BoundaryKind](b.BoundaryKind)
	switch {
	case !ok:
		return fmt.Errorf("corpus: boundary_kind %q is not defined", b.BoundaryKind)
	case barrier && (b.TS == nil || kind != tracepack.BoundaryKindStopUnclean):
		return fmt.Errorf("corpus: a barrier's boundary needs ts and kind stop-unclean, not %s", b.BoundaryKind)
	case !barrier && (b.TS != nil || b.GapStart != nil || b.GapEnd != nil):
		return fmt.Errorf("corpus: a capture-boundary fact's boundary holds only capture_id, seq, boundary_kind and epoch")
	}

	return nil
}

// canonicalFacts returns fs checked, sorted by compareFacts, each distinct fact once
// (the tracepack corpus specification §5.11).
// fs itself is not reordered.
//
// Returns:
//   - []Fact: the facts, [] for none.
//   - error: a fact that check refuses.
func canonicalFacts(fs []Fact) ([]Fact, error) {
	out := make([]Fact, 0, len(fs))
	for i := range fs {
		if err := fs[i].check(); err != nil {
			return nil, err
		}
		out = append(out, fs[i])
	}
	slices.SortStableFunc(out, func(a, b Fact) int { return compareFacts(&a, &b) })

	return slices.CompactFunc(out, func(a, b Fact) bool { return compareFacts(&a, &b) == 0 }), nil
}

// compareFacts is the comparator of the tracepack corpus specification §5.11, total over facts check accepts:
// reason by the fact table; hours element by element, a prefix first;
// then pack, block, offset, seq, defect, coverage, boundary and basis, each absent before present, then by value.
func compareFacts(a, b *Fact) int {
	_, ra := factRowOf(a.Reason)
	_, rb := factRowOf(b.Reason)

	return cmp.Or(
		cmp.Compare(ra, rb),
		slices.Compare(a.Hours, b.Hours),
		compareOpt(a.Pack, b.Pack, cmp.Compare[int]),
		compareOpt(a.Block, b.Block, cmp.Compare[int]),
		compareOpt(a.Offset, b.Offset, cmp.Compare[U64]),
		compareOpt(a.Seq, b.Seq, cmp.Compare[U64]),
		compareOpt(optString(a.Defect), optString(b.Defect), byTable(defectOrder)),
		compareOpt(a.Coverage, b.Coverage, compareCoverage),
		compareOpt(a.Boundary, b.Boundary, compareBoundary),
		slices.CompareFunc(a.Basis, b.Basis, byTable(basisOrder)),
	)
}

// compareCoverage orders coverage objects by capture_id, seq_first, seq_last, time_start and time_end, each absent first,
// then by the unknown list entry by entry, a prefix first,
// an entry by tag, value_type, then its value's bytes, unsigned, a prefix first.
func compareCoverage(a, b Coverage) int {
	return cmp.Or(
		compareOpt(a.CaptureID, b.CaptureID, compareUUID),
		compareOpt(a.SeqFirst, b.SeqFirst, cmp.Compare[U64]),
		compareOpt(a.SeqLast, b.SeqLast, cmp.Compare[U64]),
		compareOpt(a.TimeStart, b.TimeStart, cmp.Compare[I64]),
		compareOpt(a.TimeEnd, b.TimeEnd, cmp.Compare[I64]),
		slices.CompareFunc(a.Unknown, b.Unknown, func(x, y UnknownEntry) int {
			return cmp.Or(cmp.Compare(x.Tag, y.Tag), cmp.Compare(x.ValueType, y.ValueType), bytes.Compare(x.Value, y.Value))
		}),
	)
}

// compareBoundary orders boundaries by capture_id, seq, boundary_kind by its numeric value,
// then epoch, ts, gap_start and gap_end, each absent first.
func compareBoundary(a, b FactBoundary) int {
	ka, _ := enumByName[tracepack.BoundaryKind](a.BoundaryKind)
	kb, _ := enumByName[tracepack.BoundaryKind](b.BoundaryKind)

	return cmp.Or(
		compareUUID(a.CaptureID, b.CaptureID),
		cmp.Compare(a.Seq, b.Seq),
		cmp.Compare(ka, kb),
		cmp.Compare(a.Epoch, b.Epoch),
		compareOpt(a.TS, b.TS, cmp.Compare[I64]),
		compareOpt(a.GapStart, b.GapStart, cmp.Compare[I64]),
		compareOpt(a.GapEnd, b.GapEnd, cmp.Compare[I64]),
	)
}

// compareUUID orders UUIDs in canonical form by their 16 bytes;
// a string that is no UUID, which check refuses, by its text.
func compareUUID(a, b string) int {
	ua, ea := parseUUID(a)
	ub, eb := parseUUID(b)
	if ea != nil || eb != nil {
		return cmp.Compare(a, b)
	}

	return bytes.Compare(ua[:], ub[:])
}

// compareOpt orders optional values: an absent one first, then two present ones by compare.
func compareOpt[T any](a, b *T, compare func(T, T) int) int {
	switch {
	case a == nil && b == nil:
		return 0
	case a == nil:
		return -1
	case b == nil:
		return 1
	default:
		return compare(*a, *b)
	}
}

// optString returns nil for "", s otherwise.
func optString(s string) *string {
	if s == "" {
		return nil
	}

	return &s
}

// byTable returns a comparator of names by their position in table.
func byTable(table []string) func(a, b string) int {
	return func(a, b string) int { return cmp.Compare(slices.Index(table, a), slices.Index(table, b)) }
}
