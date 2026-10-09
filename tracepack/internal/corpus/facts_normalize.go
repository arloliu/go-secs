package corpus

import (
	"fmt"
	"slices"

	"github.com/arloliu/go-secs/tracepack"
)

// gapField is one field of a tracepack.TxGap that holds a value, a bit of a set of fields.
type gapField uint8

const (
	gapHours gapField = 1 << iota
	gapPack
	gapBlock
	gapOffset
	gapSeq
	gapDefect
	gapCoverage
	gapBarrier
)

// gapRule is how a gap of one reason normalizes into a fact:
// the fact's reason, the fields the gap must hold, and the fields it may hold that the fact drops.
// A field the fact's row of the fact table gives is projected; a field neither rule nor row names must be empty.
type gapRule struct {
	reason            string
	required, dropped gapField
}

// gapRules holds the rule of every reason a lookup reports, by the fact table of the tracepack corpus specification §5.11.
// NormalizeGaps handles an index gap without a block, the barriers and the capture-boundaries beyond these rules.
var gapRules = map[tracepack.TxGapReason]gapRule{
	tracepack.TxGapNoKey:       {reason: FactNoKey, dropped: gapHours | gapSeq | gapCoverage},
	tracepack.TxGapConflict:    {reason: FactConflict, required: gapHours | gapSeq},
	tracepack.TxGapIndex:       {reason: FactIndex, required: gapHours | gapPack | gapBlock | gapOffset | gapDefect, dropped: gapDefect},
	tracepack.TxGapScopeBreach: {reason: FactScopeBreach, required: gapHours | gapPack | gapBlock | gapSeq},
	tracepack.TxGapCold:        {reason: FactCold, required: gapHours},
	tracepack.TxGapConflicted:  {reason: FactConflicted, required: gapHours},
	tracepack.TxGapRead:        {reason: FactRead, required: gapHours | gapPack | gapDefect},
	tracepack.TxGapCoverage:    {reason: FactCoverage, required: gapHours | gapPack | gapCoverage},
	tracepack.TxGapUnevaluated: {reason: FactUnevaluated, required: gapHours | gapPack},
	tracepack.TxGapEvidence:    {reason: FactEvidence},
	tracepack.TxGapBarrier:     {reason: FactBarrier, required: gapBarrier},
	tracepack.TxGapCaptureBoundary: {
		reason: FactCaptureBoundary, required: gapBarrier | gapSeq, dropped: gapHours | gapPack | gapBlock | gapSeq,
	},
	tracepack.TxGapOrderingUncertain: {reason: FactOrderingUncertain, dropped: gapHours | gapPack | gapBlock | gapSeq},
	tracepack.TxGapCorrelation:       {reason: FactCorrelation, dropped: gapHours | gapSeq},
	tracepack.TxGapSeqGap:            {reason: FactSeqGap, dropped: gapSeq},
	tracepack.TxGapOpenWindow:        {reason: FactOpenWindow},
	tracepack.TxGapUnavailable:       {reason: FactUnavailable, required: gapHours | gapPack | gapBlock | gapSeq},
	tracepack.TxGapContradiction:     {reason: FactContradiction, required: gapSeq},
}

// readDefect is the reason of a read defect and its name.
type readDefect struct {
	reason tracepack.IncompleteReason
	name   string
}

// readDefects pairs each reason of a read defect a read fact may hold with its name, in the order of defectOrder
// (the tracepack corpus specification §5.4).
var readDefects = []readDefect{
	{tracepack.ReasonTruncated, "truncated"},
	{tracepack.ReasonCorruptBlock, "corrupt-block"},
	{tracepack.ReasonUnknownCodec, "unknown-codec"},
}

// boundaryKey identifies a boundary by every field, its gap bounds by value.
type boundaryKey struct {
	capture          tracepack.UUID
	seq              uint64
	kind             tracepack.BoundaryKind
	ts               int64
	epoch            uint32
	hasStart, hasEnd bool
	gapStart, gapEnd int64
}

// barrierGroup is the barrier gaps of one boundary: the boundary, its key, and the number of gaps.
type barrierGroup struct {
	key      boundaryKey
	boundary *tracepack.Boundary
	gaps     int
}

// NormalizeGaps returns the canonical facts of run's gaps (the tracepack corpus specification §5.11, Facts),
// each distinct fact once, in the order of the comparator.
// It drops each gap's Err and names each pack by its number;
// it drops a block index gap without a block, which reports records above the primary that arrived before it,
// when the lookup also has an index gap of a block of the primary's scope;
// it groups the barrier gaps by boundary, every field compared,
// and gives each group's fact the bases whose predicates hold:
// epoch when the boundary is of the primary's capture and no kept version above the primary,
// without a conflict and labelled closing, is of the primary's epoch;
// time when its gap interval meets the hours scheduled;
// it keeps of a capture-boundary gap only the boundary's capture, seq, kind and epoch,
// and of a no-key, correlation, ordering-uncertain or seq-gap gap only its reason.
// It reads only fields that the result holds.
//
// Returns:
//   - []Fact: the facts; [] for none.
//   - error: a gap of a reason no rule names;
//     a gap missing a field its rule requires or holding one its rule does not allow;
//     a pack_id no source pack has; a read defect without a name; a block-less index gap without an index gap of a block of the primary's scope;
//     a barrier group whose number of gaps is not its number of bases;
//     a capture-boundary gap of another capture, another epoch or another seq than its boundary's;
//     the hours scheduled out of the range of a lookup; or a fact that the fact table refuses.
func NormalizeGaps(run *LookupRun) ([]Fact, error) {
	if run.MaxScopes < 1 || run.Key.Hour < tracepack.MinTxHour || run.Key.Hour > tracepack.MaxTxHour ||
		int64(run.MaxScopes) > tracepack.MaxTxHour+1-run.Key.Hour {
		return nil, fmt.Errorf("corpus: hour %d and max_scopes %d are out of the range of a lookup", run.Key.Hour, run.MaxScopes)
	}

	var n gapNormalizer
	for i := range run.Result.Gaps {
		if err := n.add(run, &run.Result.Gaps[i]); err != nil {
			return nil, fmt.Errorf("corpus: gap %d: %w", i, err)
		}
	}
	if n.blockless > 0 && n.blocky == 0 {
		return nil, fmt.Errorf("corpus: an index gap without a block, and no index gap of a block of the primary's scope")
	}
	if err := n.addBarriers(run); err != nil {
		return nil, err
	}

	return canonicalFacts(n.facts)
}

// gapNormalizer holds the facts of the gaps NormalizeGaps has met so far,
// the barrier groups, and the counts of index gaps without and with a block.
type gapNormalizer struct {
	facts             []Fact
	barriers          []barrierGroup
	blockless, blocky int
}

// add normalizes the gap g of run's result.
func (n *gapNormalizer) add(run *LookupRun, g *tracepack.TxGap) error {
	rule, ok := gapRules[g.Reason]
	if !ok {
		return fmt.Errorf("reason %v is not defined", g.Reason)
	}
	got := gapFieldsOf(g)
	if g.Reason == tracepack.TxGapIndex && g.Pack == nil && g.Block < 0 {
		if got&^(gapHours|gapSeq) != 0 {
			return fmt.Errorf("an index gap without a block holds fields %#x", uint8(got))
		}
		n.blockless++

		return nil
	}
	row, _ := factRowOf(rule.reason)
	projected := fieldsOfKeys(row.required | row.optional)
	if got&rule.required != rule.required || got&^(rule.required|rule.dropped|projected) != 0 {
		return fmt.Errorf("a %v gap holds fields %#x", g.Reason, uint8(got))
	}
	if g.Reason == tracepack.TxGapBarrier {
		n.barriers = addBarrier(n.barriers, g.Barrier)

		return nil
	}
	if g.Reason == tracepack.TxGapIndex {
		if g.Defect != tracepack.ReasonIndexMismatch {
			return fmt.Errorf("an index gap of defect %v", g.Defect)
		}
		// Records above the primary that arrive before it come from the primary's scope read alone,
		// so only an index gap of a block of that scope accounts for them.
		if len(g.Hours) > 0 && g.Hours[0] == run.Key.Hour {
			n.blocky++
		}
	}
	if b := g.Barrier; g.Reason == tracepack.TxGapCaptureBoundary &&
		(b.Capture != run.Key.Capture || b.Epoch != run.Result.Epoch || *g.Seq != b.Seq) {
		return fmt.Errorf("a capture-boundary of capture %s, epoch %d, seq %d", b.Capture, b.Epoch, *g.Seq)
	}
	f, err := projectGap(g, rule.reason, got&projected, run.Numbers)
	if err != nil {
		return err
	}
	n.facts = append(n.facts, f)

	return nil
}

// addBarriers adds the fact of each barrier group, with the bases whose predicates hold on run's result:
// epoch when the boundary is of the primary's capture and no kept version above the primary,
// without a conflict and labelled closing, is of the primary's epoch;
// time when its gap interval meets the hours scheduled, which NormalizeGaps checked fit.
// A group must hold one gap per basis.
func (n *gapNormalizer) addBarriers(run *LookupRun) error {
	res := &run.Result
	from, to := run.Key.Hour*hourNs, (run.Key.Hour+int64(run.MaxScopes))*hourNs
	closed := slices.ContainsFunc(res.Records, func(v tracepack.TxRecord) bool {
		return v.Record.Seq > run.Key.Seq && !v.Conflict && v.Class.Has(tracepack.TxClosing) && v.Record.Epoch == res.Epoch
	})
	for i := range n.barriers {
		g := &n.barriers[i]
		var basis []string
		if g.key.capture == run.Key.Capture && !closed {
			basis = append(basis, BasisEpoch)
		}
		if meets(&g.key, from, to) {
			basis = append(basis, BasisTime)
		}
		if len(basis) != g.gaps {
			return fmt.Errorf("corpus: the barrier at seq %d of capture %s is in %d gaps, but %d bases hold",
				g.key.seq, g.key.capture, g.gaps, len(basis))
		}
		n.facts = append(n.facts, Fact{Reason: FactBarrier, Boundary: barrierBoundary(g.boundary), Basis: basis})
	}

	return nil
}

// gapFieldsOf returns the set of fields g holds a value in; Err is not one.
func gapFieldsOf(g *tracepack.TxGap) gapField {
	var f gapField
	for _, x := range []struct {
		field gapField
		has   bool
	}{
		{gapHours, len(g.Hours) > 0}, {gapPack, g.Pack != nil}, {gapBlock, g.Block >= 0}, {gapOffset, g.Offset >= 0},
		{gapSeq, g.Seq != nil}, {gapDefect, g.Defect != 0}, {gapCoverage, g.Coverage != nil}, {gapBarrier, g.Barrier != nil},
	} {
		if x.has {
			f |= x.field
		}
	}

	return f
}

// fieldsOfKeys returns the gap fields that project to the fact keys k.
func fieldsOfKeys(k factKey) gapField {
	var f gapField
	for _, x := range []struct {
		key   factKey
		field gapField
	}{
		{keyHours, gapHours}, {keyPack, gapPack}, {keyBlock, gapBlock}, {keyOffset, gapOffset},
		{keySeq, gapSeq}, {keyDefect, gapDefect}, {keyCoverage, gapCoverage},
	} {
		if k&x.key != 0 {
			f |= x.field
		}
	}

	return f
}

// projectGap returns the fact of reason holding the fields of g that fields names, every pack by its number in numbers,
// and, for a capture-boundary, its boundary's capture, seq, kind and epoch.
func projectGap(g *tracepack.TxGap, reason string, fields gapField, numbers map[tracepack.UUID]int) (Fact, error) {
	f := Fact{Reason: reason}
	if fields&gapHours != 0 {
		for _, h := range g.Hours {
			f.Hours = append(f.Hours, I64(h))
		}
	}
	if fields&gapPack != 0 {
		n, ok := numbers[*g.Pack]
		if !ok {
			return Fact{}, fmt.Errorf("pack %s is no source pack", *g.Pack)
		}
		f.Pack = new(n)
	}
	if fields&gapBlock != 0 {
		f.Block = new(g.Block)
	}
	if fields&gapOffset != 0 {
		f.Offset = new(U64(g.Offset))
	}
	if fields&gapSeq != 0 {
		f.Seq = new(U64(*g.Seq))
	}
	if fields&gapDefect != 0 {
		k := slices.IndexFunc(readDefects, func(d readDefect) bool { return d.reason == g.Defect })
		if k < 0 {
			return Fact{}, fmt.Errorf("a read gap of defect %v", g.Defect)
		}
		f.Defect = readDefects[k].name
	}
	if fields&gapCoverage != 0 {
		f.Coverage = new(CoverageFrom(g.Coverage))
	}
	if reason == FactCaptureBoundary {
		b := g.Barrier
		f.Boundary = &FactBoundary{CaptureID: b.Capture.String(), Seq: U64(b.Seq), BoundaryKind: b.Kind.String(), Epoch: b.Epoch}
	}

	return f, nil
}

// addBarrier counts b in the group of barriers equal to it in every field, adding the group when it is new.
func addBarrier(groups []barrierGroup, b *tracepack.Boundary) []barrierGroup {
	k := boundaryKey{capture: b.Capture, seq: b.Seq, kind: b.Kind, ts: b.TS, epoch: b.Epoch}
	if b.GapStart != nil {
		k.hasStart, k.gapStart = true, *b.GapStart
	}
	if b.GapEnd != nil {
		k.hasEnd, k.gapEnd = true, *b.GapEnd
	}
	if i := slices.IndexFunc(groups, func(g barrierGroup) bool { return g.key == k }); i >= 0 {
		groups[i].gaps++

		return groups
	}

	return append(groups, barrierGroup{key: k, boundary: b, gaps: 1})
}

// meets reports whether the gap interval of the boundary k, [gap_start, gap_end],
// unbounded on a side whose bound is absent, meets the half-open range [from, to);
// an interval whose gap_start exceeds its gap_end meets every range.
func meets(k *boundaryKey, from, to int64) bool {
	if k.hasStart && k.hasEnd && k.gapStart > k.gapEnd {
		return true
	}

	return (!k.hasEnd || k.gapEnd >= from) && (!k.hasStart || k.gapStart < to)
}

// barrierBoundary renders b as the boundary of a barrier fact.
func barrierBoundary(b *tracepack.Boundary) *FactBoundary {
	out := &FactBoundary{CaptureID: b.Capture.String(), Seq: U64(b.Seq), BoundaryKind: b.Kind.String(), TS: new(I64(b.TS)), Epoch: b.Epoch}
	if b.GapStart != nil {
		out.GapStart = new(I64(*b.GapStart))
	}
	if b.GapEnd != nil {
		out.GapEnd = new(I64(*b.GapEnd))
	}

	return out
}
