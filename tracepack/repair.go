package tracepack

import (
	"context"
	"errors"
	"fmt"
	"io"
	"math"

	"github.com/arloliu/go-secs/tracepack/internal/format"
)

var (
	// ErrRepairNotNeeded reports a finalized-consistent pack, which Repair leaves as it is and writes nothing for
	// (the tracepack format specification §13).
	ErrRepairNotNeeded = errors.New("tracepack: pack needs no repair")
	// ErrNotRepairable reports a pack for which no faithful and consistent patch can be made
	// (the tracepack format specification §13): a pack that is not stored, a period or a validated block outside one UTC hour, a seq-order or hour-span writer defect,
	// a lost run without a seq between its neighbours, or records that breach the pack metadata's commitments.
	// Repair writes nothing, and the damaged pack stays.
	ErrNotRepairable = errors.New("tracepack: pack cannot be repaired")
)

// RepairOptions configures Repair.
type RepairOptions struct {
	// Reader configures the bootstrap and the reader budgets, as for Verify.
	Reader ReaderOptions
	// Writer is the patch's writer tag, the implementation and version that writes it; required.
	Writer string
	// PatchBase is the replacement_set_id of the scope's current generation, which the patch is registered against
	// (the tracepack storage specification §2 and §5); nil when the scope has no generation.
	PatchBase *UUID
	// PackID identifies the patch; the zero UUID makes Repair generate a UUIDv7.
	// It must differ from the damaged pack's pack_id.
	PackID UUID
	// Codec compresses the patch's footer; the copied blocks keep their own codec.
	Codec Codec
	// Sync, when set, is called after every copied block and after the trailer, as WriterOptions.Sync is.
	Sync Syncer
}

// RepairReport is the result of Repair.
type RepairReport struct {
	// Verify is the damaged pack's VerifyReport.
	Verify VerifyReport
	// PackID is the patch's pack_id.
	PackID UUID
	// Blocks and Records count the blocks and records copied into the patch: every validated block of the damaged pack.
	Blocks  int
	Records uint64
	// Coverage holds the coverage entries the patch adds for the records the damaged pack lost;
	// the damaged pack's own entries, which the patch carries first, are not repeated.
	Coverage []Coverage
	// Size is the number of bytes written to dst.
	Size uint64
}

// Repair writes to dst a generation-0 repair patch of the pack in ra, as the tracepack format specification §13 builds it
// (the tracepack storage specification §6):
// every validated block copied byte for byte, the footer rebuilt from the copied records, supersedes naming the damaged pack, and the damaged pack's coverage followed by entries for the records it lost.
//
// Repair verifies the pack first, as Verify does, and decides every refusal before it writes a byte.
// It then reads each validated block again and copies it, requiring it to be the block the verification read.
// The source must not change while Repair runs.
//
// After any error the caller discards what was written to dst.
// ErrRepairNotNeeded, ErrNotRepairable and every other error before the patch's header leave dst untouched;
// an error before the footer leaves bytes that are not a finalized pack;
// an error of the footer-and-trailer write or of the final sync may leave bytes that form a finalized pack,
// which Repair still reports as a failure, since it cannot vouch for them.
//
// Parameters:
//   - ctx: cancels the repair between blocks and before the footer.
//   - ra: the damaged pack's bytes.
//   - size: the damaged pack's size in bytes.
//   - dst: the patch's output; bytes are written in file order and never rewritten.
//   - opts: see RepairOptions.
//
// Returns:
//   - RepairReport: what the patch holds; on ErrRepairNotNeeded and ErrNotRepairable only its Verify field, else zero on error.
//   - error: every error of Verify; ErrRepairNotNeeded; an error wrapping ErrNotRepairable, naming the cause;
//     an empty opts.Writer, or opts.PackID equal to the damaged pack's;
//     a *FieldError validating the patch's metadata; an invalid opts.Codec;
//     an error when a validated block fails or differs on the copy; ctx's error, wrapped;
//     a write or sync error of dst, or the Writer's error for a footer it cannot encode.
func Repair(ctx context.Context, ra io.ReaderAt, size int64, dst io.Writer, opts RepairOptions) (RepairReport, error) {
	r, err := Open(ctx, ra, size, opts.Reader)
	if err != nil {
		return RepairReport{}, err
	}

	a, err := r.analyze(ctx, true)
	if err != nil {
		return RepairReport{}, err
	}

	plan, err := r.planRepair(a, &opts)
	if err != nil {
		if errors.Is(err, ErrRepairNotNeeded) || errors.Is(err, ErrNotRepairable) {
			return RepairReport{Verify: a.report}, err
		}

		return RepairReport{}, err
	}
	if err := ctx.Err(); err != nil {
		return RepairReport{}, fmt.Errorf("tracepack: repair: %w", err)
	}

	return r.writePatch(ctx, a, plan, dst, &opts)
}

// repairPlan is what the preflight of Repair decided: the patch's metadata and the facts it is validated against,
// and the coverage entries the patch adds.
type repairPlan struct {
	meta     *PackMeta
	facts    PackFacts
	coverage []Coverage
}

// planRepair decides, before anything is written, whether the pack analyzed as a can be repaired, and returns the patch's plan.
// It decides in this order: the pack's role, whether it needs repair, its period, the writer defects,
// the hours of the validated blocks, the metadata's commitments, the lost runs, then the options and the patch's metadata,
// so a refusal of the pack is never masked by an option error.
//
// Returns:
//   - *repairPlan: the plan; nil on error.
//   - error: ErrRepairNotNeeded, an error wrapping ErrNotRepairable, an empty opts.Writer, opts.PackID equal to the pack's,
//     or a *FieldError from validating or encoding the patch's metadata.
func (r *Reader) planRepair(a *analysis, opts *RepairOptions) (*repairPlan, error) {
	meta := r.meta
	if role := meta.PackRole; role != PackRoleSegment && role != PackRoleArchive && role != PackRoleRepair {
		return nil, fmt.Errorf("%w: pack_role %s is not a stored pack's", ErrNotRepairable, role)
	}
	if a.report.Outcome == OutcomeFinalizedConsistent {
		return nil, ErrRepairNotNeeded
	}

	if meta.PeriodStart >= meta.PeriodEnd || hourOf(meta.PeriodStart) != hourOf(meta.PeriodEnd-1) {
		return nil, fmt.Errorf("%w: period %d to %d is not inside one UTC hour", ErrNotRepairable, meta.PeriodStart, meta.PeriodEnd)
	}
	for _, d := range a.report.WriterDefects {
		if d.Kind == WriterDefectSeqOrder || d.Kind == WriterDefectHourSpan {
			return nil, fmt.Errorf("%w: the patch's own footer would reject the %s writer defect of block %d at offset %d",
				ErrNotRepairable, d.Kind, d.Block, d.Offset)
		}
	}

	scope := hourOf(meta.PeriodStart)
	for i := range a.blocks {
		if b := &a.blocks[i]; !b.failed && (hourOf(b.tsMin) != scope || hourOf(b.tsMax) != scope) {
			return nil, fmt.Errorf("%w: block %d at offset %d lies outside the UTC hour of the period", ErrNotRepairable, i, r.blocks[i].Offset)
		}
	}
	facts, err := r.checkRepairCommitments(a)
	if err != nil {
		return nil, err
	}

	coverage, err := r.newCoverage(a)
	if err != nil {
		return nil, err
	}

	if opts.Writer == "" {
		return nil, errors.New("tracepack: RepairOptions.Writer is empty")
	}

	p := &repairPlan{meta: r.patchMeta(a, opts, coverage), facts: facts, coverage: coverage}
	p.facts.CoverageLoss = len(p.meta.Coverage) > 0
	p.facts.ScopeHasGeneration = p.meta.PatchBase != nil
	if err := p.meta.Validate(p.facts); err != nil {
		return nil, err
	}
	if _, err := p.meta.MarshalBinary(); err != nil {
		return nil, err
	}
	if !opts.PackID.IsZero() && opts.PackID == UUID(r.hdr.PackID) {
		return nil, fmt.Errorf("tracepack: RepairOptions.PackID %s is the damaged pack's own pack_id", opts.PackID)
	}

	return p, nil
}

// checkRepairCommitments rejects validated records of the pack analyzed as a that its metadata cannot describe:
// a classified record without classifier, an oversized record without max_frame_len, or a redacted record, which no stored pack holds.
// It returns the facts of the validated records that the patch's metadata is validated against.
func (r *Reader) checkRepairCommitments(a *analysis) (PackFacts, error) {
	var facts PackFacts
	for i := range a.blocks {
		if b := &a.blocks[i]; !b.failed {
			facts.AnyClassified = facts.AnyClassified || b.classified
			facts.AnyOversized = facts.AnyOversized || b.oversized
			facts.AnyRedacted = facts.AnyRedacted || b.redacted
		}
	}

	switch {
	case facts.AnyClassified && r.meta.Classifier == nil:
		return PackFacts{}, fmt.Errorf("%w: a classified record without classifier in the pack metadata", ErrNotRepairable)
	case facts.AnyOversized && r.meta.MaxFrameLen == nil:
		return PackFacts{}, fmt.Errorf("%w: an oversized record without max_frame_len in the pack metadata", ErrNotRepairable)
	case facts.AnyRedacted:
		return PackFacts{}, fmt.Errorf("%w: a redacted record, which no stored pack holds", ErrNotRepairable)
	default:
		return facts, nil
	}
}

// newCoverage returns the coverage entries for the records the pack analyzed as a lost (the tracepack format specification §13):
// from a valid footer that no validated block disagrees with, each failed block's F-3 seq ranges within its F-2 time range;
// otherwise one entry per lost run, bounded by the seqs of its validated neighbours within the scope's UTC hour.
func (r *Reader) newCoverage(a *analysis) ([]Coverage, error) {
	if r.footer != nil && len(a.report.Disagreements) == 0 {
		if len(a.report.Lost) == 0 {
			return nil, nil
		}
		out := make([]Coverage, 0, len(a.report.Lost))
		for i := range a.report.Lost {
			out = append(out, *cloneCoverage(&a.report.Lost[i]))
		}

		return out, nil
	}

	return r.lostRuns(a)
}

// lostRuns returns one coverage entry per lost run of the pack analyzed as a:
// a maximal run of consecutive failed blocks, with a tail after the last located block when the pack is not finalized or the walk stopped.
// An entry's seqs lie between the last seq of the validated block before its run and the first seq of the one after it,
// from seq_start when none precedes it and without seq_last when none follows; its time interval is the scope's UTC hour.
// A run with no seq between its neighbours is ErrNotRepairable when it holds a failed block, since I-12 breaks inside it,
// and yields no entry when it is only the tail.
func (r *Reader) lostRuns(a *analysis) ([]Coverage, error) {
	capture := UUID(r.hdr.CaptureID)
	first, last := hourBounds(hourOf(r.meta.PeriodStart))

	var (
		out []Coverage
		// prev is the last seq of the latest validated block; valid iff hasPrev.
		prev    uint64
		hasPrev bool
		// inRun reports an open lost run, and runFailed whether it holds a failed block; runAt is the index of its first block.
		inRun, runFailed bool
		runAt            int
	)
	closeRun := func(next uint64, hasNext bool) error {
		c, ok := runCoverage(prev, hasPrev, next, hasNext, r.meta.SeqStart)
		if ok {
			c.CaptureID, c.TimeStart, c.TimeEnd = new(capture), new(first), new(last)
			out = append(out, c)
		} else if runFailed {
			return fmt.Errorf("%w: the lost run from block %d has no seq between its validated neighbours", ErrNotRepairable, runAt)
		}
		inRun, runFailed = false, false

		return nil
	}

	for i := range a.blocks {
		b := &a.blocks[i]
		if b.failed {
			if !inRun {
				inRun, runAt = true, i
			}
			runFailed = true

			continue
		}
		if inRun {
			if err := closeRun(b.firstSeq, true); err != nil {
				return nil, err
			}
		}
		prev, hasPrev = b.lastSeq, true
	}

	if !r.finalized || r.walkStop != nil {
		if !inRun {
			inRun, runAt = true, len(a.blocks)
		}
	}
	if inRun {
		if err := closeRun(0, false); err != nil {
			return nil, err
		}
	}

	return out, nil
}

// runCoverage returns the seq range of a lost run between the validated neighbours before and after it:
// from prev + 1, or seqStart without one, to next − 1, or unbounded without one.
// ok is false when no seq lies in the range: prev is 2^63−1, next is 0, or the first bound exceeds the last.
func runCoverage(prev uint64, hasPrev bool, next uint64, hasNext bool, seqStart uint64) (Coverage, bool) {
	seqFirst := seqStart
	if hasPrev {
		if prev >= format.MaxU64 {
			return Coverage{}, false
		}
		seqFirst = prev + 1
	}
	c := Coverage{SeqFirst: new(seqFirst)}
	if hasNext {
		if next == 0 || seqFirst > next-1 {
			return Coverage{}, false
		}
		c.SeqLast = new(next - 1)
	}

	return c, true
}

// hourBounds returns the first and last nanosecond of UTC hour h, as hourOf numbers it, clamped to the int64 range.
func hourBounds(h int64) (first, last int64) {
	first, last = math.MinInt64, math.MaxInt64
	// Go's division truncates toward zero, so these are the extreme hours whose bounds do not overflow.
	if h >= math.MinInt64/hourNs {
		first = h * hourNs
	}
	if h < math.MaxInt64/hourNs {
		last = (h+1)*hourNs - 1
	}

	return first, last
}

// patchMeta returns the patch's pack metadata (the tracepack storage specification §6):
// the damaged pack's, with pack_role repair, scope_generation 0, no publisher_epoch or replacement-set tags,
// supersedes naming the damaged pack, the damaged pack's coverage followed by coverage, the caller's patch_base and writer,
// and seq_start at the first copied record, or the damaged pack's seq_start when no record is copied.
func (r *Reader) patchMeta(a *analysis, opts *RepairOptions, coverage []Coverage) *PackMeta {
	// Header decodes the pack metadata afresh, so m is the patch's own.
	m := r.Header().Meta
	m.PackRole = PackRoleRepair
	m.ScopeGeneration = new(uint64(0))
	m.PublisherEpoch = nil
	m.ReplacementSetID, m.ReplacementSetSize, m.ReplacementSetIndex = nil, nil, nil
	m.Supersedes = []UUID{UUID(r.hdr.PackID)}
	m.Coverage = append(m.Coverage, coverage...)
	m.PatchBase = nil
	if opts.PatchBase != nil {
		m.PatchBase = new(*opts.PatchBase)
	}
	m.Writer = opts.Writer
	for i := range a.blocks {
		if !a.blocks[i].failed {
			m.SeqStart = a.blocks[i].firstSeq

			break
		}
	}

	return m
}

// writePatch writes the patch planned as p:
// its header and metadata, every validated block of the pack analyzed as a, and its footer.
func (r *Reader) writePatch(ctx context.Context, a *analysis, p *repairPlan, dst io.Writer, opts *RepairOptions) (RepairReport, error) {
	w, err := startWriter(dst, WriterOptions{
		Meta: p.meta, Facts: p.facts, Codec: opts.Codec, Sync: opts.Sync,
		PackID: opts.PackID, CaptureID: UUID(r.hdr.CaptureID),
	}, true)
	if err != nil {
		return RepairReport{}, err
	}

	rep := RepairReport{Verify: a.report, PackID: w.PackID(), Coverage: p.coverage}
	var buf blockBuf
	for i := range a.blocks {
		if a.blocks[i].failed {
			continue
		}
		if err := ctx.Err(); err != nil {
			return RepairReport{}, fmt.Errorf("tracepack: repair: %w", err)
		}

		s, err := r.copyBlock(i, &a.blocks[i], &buf)
		if err != nil {
			return RepairReport{}, err
		}
		if err := w.appendBlock(buf.raw, &s); err != nil {
			return RepairReport{}, err
		}
		rep.Blocks++
		rep.Records += uint64(s.recordCount)
	}

	if err := ctx.Err(); err != nil {
		return RepairReport{}, fmt.Errorf("tracepack: repair: %w", err)
	}
	if _, err := w.Close(); err != nil {
		return RepairReport{}, err
	}
	rep.Size = w.offset

	return rep, nil
}

// copyBlock reads block i again into buf and returns the summary of its records,
// requiring it to be the validated block its analysis b describes: the same envelope CRC and the same first and last seq.
func (r *Reader) copyBlock(i int, b *blockAnalysis, buf *blockBuf) (blockSummary, error) {
	info := &r.blocks[i]
	d, def, err := r.readBlock(i, buf)
	if err != nil {
		return blockSummary{}, err
	}
	if d == nil {
		return blockSummary{}, fmt.Errorf("tracepack: repair: block %d at offset %d validated, then failed on the copy: %w", i, info.Offset, def.Err)
	}

	s := summaryOf(d)
	if d.env.EnvelopeCRC != b.envelopeCRC || s.firstSeq != b.firstSeq || s.lastSeq != b.lastSeq {
		return blockSummary{}, fmt.Errorf("tracepack: repair: block %d at offset %d changed between the verification and the copy", i, info.Offset)
	}

	return s, nil
}
