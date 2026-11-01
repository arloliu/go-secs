package tracepack

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/arloliu/go-secs/tracepack/internal/format"
)

// mergeDecision is what a caller of Merge does with its error, the first match deciding.
type mergeDecision string

const (
	// decideMergeCtx: the merge's ctx is done; stop.
	decideMergeCtx mergeDecision = "ctx"
	// decideMergeSource: an input's ReadAt failed or did not return, while opening it or reading a block; retry by the source's policy.
	decideMergeSource mergeDecision = "source"
	// decideMergeFixOptions: an option or identity Merge cannot take; fix the options.
	decideMergeFixOptions mergeDecision = "fix the options"
	// decideMergeOperator: a budget or a limit of the format; raise the budget, or an operator decides.
	decideMergeOperator mergeDecision = "operator"
	// decideMergeConflict: records or redaction entries that differ; act as OnConflict decided.
	decideMergeConflict mergeDecision = "conflict"
	// decideMergeInput: an input Merge refuses; the cause beside the sentinel says why.
	decideMergeInput mergeDecision = "input unusable"
	// decideMergeTransient: a block read of an input cut short, or the destination, failed; it may be transient.
	decideMergeTransient mergeDecision = "transient"
)

// decideMerge returns the decision a caller of Merge takes for err, in the documented order.
func decideMerge(err error) mergeDecision {
	switch {
	case errors.Is(err, context.Canceled), errors.Is(err, context.DeadlineExceeded):
		return decideMergeCtx
	case errors.Is(err, ErrSourceRead):
		return decideMergeSource
	case errors.Is(err, ErrInvalidArgument):
		return decideMergeFixOptions
	case errors.Is(err, ErrMergeLimit), errors.Is(err, ErrReadLimit):
		return decideMergeOperator
	case errors.Is(err, ErrMergeConflict):
		return decideMergeConflict
	case errors.Is(err, ErrMergeInput):
		return decideMergeInput
	default:
		return decideMergeTransient
	}
}

// TestMergeDecision drives each row of Merge's decision table through Merge:
// an option it cannot take, a budget or a limit of the format, a conflict, an input it refuses with each cause beside it,
// and a failed read of an input, whether Merge was opening it or reading a block, which is no refusal of the input.
func TestMergeDecision(t *testing.T) {
	t.Parallel()

	file, clean := readFaultPack(t)
	l := layoutOf(t, file)
	block := clean.Blocks()[1]
	merge := func(edit func(*MergeOptions), files ...[]byte) func(t *testing.T) error {
		return func(t *testing.T) error {
			opts := mergeOpts()
			if edit != nil {
				edit(&opts)
			}
			_, err := Merge(t.Context(), io.Discard, inputsOf(files...), opts)

			return err
		}
	}
	failing := func(fail func(off int64, n int) bool) func(t *testing.T) error {
		return func(t *testing.T) error {
			g := newGatedReader(file)
			g.fail = fail
			_, err := Merge(t.Context(), io.Discard, []MergeInput{{ReaderAt: g, Size: int64(len(file))}}, uncachedMergeOpts())

			return err
		}
	}
	a := mergePack(t, seg0, nil, hourSteps(10, 13, 4))
	b := mergePack(t, seg1, nil, hourSteps(10, 13, 4))

	tests := []struct {
		name string
		err  func(t *testing.T) error
		want mergeDecision
		// also holds the sentinels of the cause beside the deciding one, which the first row overrides.
		also []error
		// never holds sentinels the error must not match.
		never []error
	}{
		{name: "ctx cancelled", want: decideMergeCtx, err: func(t *testing.T) error {
			ctx, cancel := context.WithCancel(t.Context())
			cancel()
			_, err := Merge(ctx, io.Discard, inputsOf(file), mergeOpts())

			return err
		}},
		{name: "no inputs", want: decideMergeFixOptions, err: merge(nil)},
		{
			name: "a writer tag the pack metadata cannot hold", want: decideMergeFixOptions, also: []error{ErrFieldValue},
			err: merge(func(o *MergeOptions) { o.WriterTag = "\xff" }, file),
		},
		{name: "an archive footer over its budget", want: decideMergeOperator, also: []error{ErrMergeLimit},
			err: merge(func(o *MergeOptions) { o.MaxFooterLen = 1 }, file)},
		{
			name: "an input footer over a reader budget", want: decideMergeOperator, also: []error{ErrReadLimit}, never: []error{ErrMergeInput},
			err: merge(func(o *MergeOptions) { o.Reader.MaxFooterLen = 1 }, file),
		},
		{
			name: "records that differ", want: decideMergeConflict,
			err: merge(func(o *MergeOptions) { o.Uncompressed = true }, a, widenFile(t, b, allBlocks)),
		},
		{
			name: "an input not finalized", want: decideMergeInput, also: []error{ErrNotFinalized},
			err: merge(nil, file[:l.tr.FooterOffset]),
		},
		{
			name: "an input with a corrupt block", want: decideMergeInput, also: []error{ErrChecksum},
			err: merge(nil, flipByte(file, block.Offset+format.EnvelopeLen+3)),
		},
		{name: "an input that is not a pack", want: decideMergeInput, also: []error{ErrNotTracepack},
			err: merge(nil, bytes.Repeat([]byte{0x5A}, len(file)))},
		{
			// A ReaderAt reporting io.EOF reports the object's end: the object is shorter than its Size, and Merge refuses it.
			name: "an input shorter than its size, its ReaderAt reporting io.EOF", want: decideMergeInput,
			also: []error{io.ErrUnexpectedEOF}, never: []error{ErrSourceRead},
			err: func(t *testing.T) error {
				_, err := Merge(t.Context(), io.Discard, []MergeInput{{ReaderAt: bytes.NewReader(file), Size: int64(len(file)) + 10}}, mergeOpts())
				return err
			},
		},
		{
			name: "a ReadAt error while opening an input", want: decideMergeSource, also: []error{errInjected},
			never: []error{ErrMergeInput, ErrCorrupt, ErrNotFinalized, ErrNotTracepack},
			err:   failing(func(int64, int) bool { return true }),
		},
		{
			name: "a ReadAt error reading an input's block", want: decideMergeSource, also: []error{errInjected},
			never: []error{ErrMergeInput, ErrCorrupt},
			err:   failing(func(off int64, _ int) bool { return off == int64(block.Offset) }),
		},
		{
			// The source's own error keeps its chain, a module sentinel in it included; the source row decides first.
			name: "a ReadAt error that wraps ErrCorrupt itself", want: decideMergeSource, also: []error{ErrCorrupt},
			never: []error{ErrMergeInput},
			err: func(t *testing.T) error {
				g := newGatedReader(file)
				g.fail = func(off int64, _ int) bool { return off == int64(block.Offset) }
				g.failErr = fmt.Errorf("the source's own check: %w", ErrCorrupt)
				_, err := Merge(t.Context(), io.Discard, []MergeInput{{ReaderAt: g, Size: int64(len(file))}}, uncachedMergeOpts())

				return err
			},
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()

			err := tt.err(t)
			require.Error(t, err)
			assert.Equal(t, tt.want, decideMerge(err), "%v", err)
			for _, s := range tt.also {
				assert.ErrorIs(t, err, s, "the cause stays matchable")
			}
			for _, s := range tt.never {
				assert.NotErrorIs(t, err, s)
			}
		})
	}
}
