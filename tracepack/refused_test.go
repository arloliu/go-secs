package tracepack

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"go/ast"
	"go/parser"
	"go/token"
	"io"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/arloliu/go-secs/tracepack/internal/format"
	"github.com/arloliu/go-secs/tracepack/internal/tlv"
)

// refusedCount returns the number of refusedError markers in err's chain, joined errors included.
func refusedCount(err error) int {
	if err == nil {
		return 0
	}
	n := 0
	if _, ok := err.(*refusedError); ok { //nolint:errorlint // counts this link of the chain, not the chain
		n++
	}
	switch u := err.(type) { //nolint:errorlint // walks the chain link by link
	case interface{ Unwrap() error }:
		n += refusedCount(u.Unwrap())
	case interface{ Unwrap() []error }:
		for _, e := range u.Unwrap() {
			n += refusedCount(e)
		}
	default:
		// The end of the chain.
	}

	return n
}

// requireRefused requires err to wrap ErrRefused and want, with exactly one marker,
// and to read as the error the marker wraps.
func requireRefused(t *testing.T, err, want error) {
	t.Helper()

	require.ErrorIs(t, err, ErrRefused)
	require.ErrorIs(t, err, want)
	require.Equal(t, 1, refusedCount(err), "one marker")
	var re *refusedError
	require.ErrorAs(t, err, &re)
	require.Equal(t, re.err.Error(), err.Error(), "the marker does not change the text")
}

// requireNotRefused requires err to be an error that neither matches ErrRefused nor holds a marker.
func requireNotRefused(t *testing.T, err error) {
	t.Helper()

	require.Error(t, err)
	require.NotErrorIs(t, err, ErrRefused)
	require.Zero(t, refusedCount(err), "no marker")
}

// refusalWriter returns a Writer over out for repairTestMeta, with opts changed by mutate when it is not nil.
func refusalWriter(t *testing.T, out io.Writer, mutate func(*WriterOptions)) *Writer {
	t.Helper()

	opts := WriterOptions{Meta: repairTestMeta()}
	if mutate != nil {
		mutate(&opts)
	}
	w, err := NewWriter(out, opts)
	require.NoError(t, err)

	return w
}

// newWriterErr returns the error of NewWriter for repairTestMeta changed by meta and options changed by mutate.
func newWriterErr(meta func(*PackMeta), mutate func(*WriterOptions)) error {
	m := repairTestMeta()
	if meta != nil {
		meta(m)
	}
	opts := WriterOptions{Meta: m}
	if mutate != nil {
		mutate(&opts)
	}
	_, err := NewWriter(io.Discard, opts)

	return err
}

// newSegErr returns the error of NewSegmentWriter for segOptions changed by mutate.
func newSegErr(t *testing.T, mutate func(*SegmentWriterOptions)) error {
	opts := segOptions(newMemSink())
	mutate(&opts)
	_, err := NewSegmentWriter(t.Context(), opts)

	return err
}

// truncateBlock is an encodedHook that damages the encoded block, so its validation fails.
func truncateBlock(enc []byte) []byte { return enc[:len(enc)-1] }

// refusalProvocations provokes each sentinel ErrRefused marks through one public writing call.
var refusalProvocations = map[string]func(t *testing.T) error{
	"ErrSeqOrder": func(t *testing.T) error {
		r := testDataRecord(5, blockTestHour+1, 1)
		return refusalWriter(t, io.Discard, nil).Append(&r)
	},
	"ErrPayloadTooLarge": func(t *testing.T) error {
		r := testDataRecord(0, blockTestHour+1, 1)
		r.Payload, r.FieldValidity = hugePayload(t), 0
		return refusalWriter(t, io.Discard, nil).Append(&r)
	},
	"ErrFieldValidity": func(t *testing.T) error {
		r := testDataRecord(0, blockTestHour+1, 1)
		r.Payload = blockTestFrame[:4]
		return refusalWriter(t, io.Discard, nil).Append(&r)
	},
	"ErrMetadataCommitment": func(t *testing.T) error {
		r := testDataRecord(0, blockTestHour+1, 1)
		r.DecodeStatus = DecodeStatusOversized // the pack has no max_frame_len
		return refusalWriter(t, io.Discard, nil).Append(&r)
	},
	"ErrValidation": func(t *testing.T) error {
		w := refusalWriter(t, io.Discard, nil)
		w.encodedHook = truncateBlock
		r := testDataRecord(0, blockTestHour+1, 1)
		require.NoError(t, w.Append(&r))
		return w.Flush()
	},
	"ErrScopeBreach": func(t *testing.T) error {
		r := testDataRecord(0, blockTestHour+hourNs+1, 1)
		return refusalWriter(t, io.Discard, nil).Append(&r)
	},
	"ErrInvalidRecord": func(t *testing.T) error {
		w, _ := newSeg(t, nil)
		return w.Append(t.Context(), nil)
	},
	"ErrRequiredTag": func(*testing.T) error {
		return newWriterErr(func(m *PackMeta) { m.ScopeGeneration = nil }, nil)
	},
	"ErrFieldValue": func(*testing.T) error {
		return newWriterErr(func(m *PackMeta) { m.PackRole = PackRoleRepair }, nil)
	},
	"ErrFieldLength": func(t *testing.T) error {
		// A transport event whose primary_system_bytes holds 3 bytes does not decode.
		var p []byte
		p = tlv.AppendEntry(p, tlv.U8Entry(tagEvent, uint8(EventStateTransition)))
		p = tlv.AppendEntry(p, tlv.BytesEntry(tagPrimarySystemBytes, []byte{1, 2, 3}))
		r := Record{TSUTCNs: segAt(time.Second).UnixNano(), Kind: KindTransportEvent, Dir: DirLocal, Payload: p}
		w, _ := newSeg(t, nil)

		return w.Append(t.Context(), &r)
	},
	"ErrAnnotationText": func(t *testing.T) error {
		// An annotation with neither text nor raw does not decode.
		p := tlv.AppendEntry(nil, tlv.U8Entry(tagAnnotationKind, uint8(AnnotationKindNote)))
		r := Record{TSUTCNs: segAt(time.Second).UnixNano(), Kind: KindAnnotation, Dir: DirLocal, Payload: p}
		w, _ := newSeg(t, nil)
		return w.Append(t.Context(), &r)
	},
	"ErrReservedTag": func(*testing.T) error {
		return newWriterErr(func(m *PackMeta) {
			m.Unknown = []RawEntry{{Tag: tagToolID, Type: uint8(tlv.TypeUTF8), Value: []byte("x")}}
		}, nil)
	},
}

// exportedSentinels returns the name of every exported variable of the package whose name starts with Err.
func exportedSentinels(t *testing.T) []string {
	t.Helper()

	entries, err := os.ReadDir(".")
	require.NoError(t, err)
	var names []string
	fset := token.NewFileSet()
	for _, e := range entries {
		if e.IsDir() || !strings.HasSuffix(e.Name(), ".go") || strings.HasSuffix(e.Name(), "_test.go") {
			continue
		}
		f, err := parser.ParseFile(fset, e.Name(), nil, parser.SkipObjectResolution)
		require.NoError(t, err)
		for _, d := range f.Decls {
			g, ok := d.(*ast.GenDecl)
			if !ok || g.Tok != token.VAR {
				continue
			}
			for _, s := range g.Specs {
				vs, ok := s.(*ast.ValueSpec)
				if !ok {
					continue
				}
				for _, n := range vs.Names {
					if n.IsExported() && strings.HasPrefix(n.Name, "Err") {
						names = append(names, n.Name)
					}
				}
			}
		}
	}
	require.NotEmpty(t, names)

	return names
}

// TestErrRefusedSentinels provokes, through a public writing call, each sentinel refuse marks,
// and requires the inventory of refusalSentinels to be exactly those of the package's sentinels this test provokes.
func TestErrRefusedSentinels(t *testing.T) {
	t.Parallel()

	var marked []string
	for _, name := range exportedSentinels(t) {
		s, ok := mergeSentinels[name]
		require.True(t, ok, "mergeSentinels lacks %s", name)
		if slices.Contains(refusalSentinels[:], s) {
			marked = append(marked, name)
		}
	}
	require.Len(t, marked, len(refusalSentinels), "every sentinel refuse marks is exported")
	require.Len(t, refusalProvocations, len(refusalSentinels), "one provocation per sentinel")

	for _, name := range marked {
		provoke, ok := refusalProvocations[name]
		require.True(t, ok, "no provocation of %s", name)
		t.Run(name, func(t *testing.T) {
			t.Parallel()

			requireRefused(t, provoke(t), mergeSentinels[name])
		})
	}
}

// TestErrRefusedArguments requires every invalid argument or option of a writing call
// that wraps no refusal sentinel of its own to be marked.
func TestErrRefusedArguments(t *testing.T) {
	t.Parallel()

	maskedAssign := func(m *PackMeta) {
		m.RedactionPolicy, m.Redaction = encryptedPolicy(), maskEntries(false, 0)
	}
	writeClose := func(edit func(*CloseOptions)) func(*testing.T) error {
		return func(*testing.T) error {
			opts := closeOptions(closeMeta(1000, false, false), nil)
			edit(&opts)
			_, err := WriteClose(io.Discard, opts)

			return err
		}
	}
	tests := []struct {
		name    string
		provoke func(t *testing.T) error
		msg     string
	}{
		{"nil WriterOptions.Meta", func(*testing.T) error {
			_, err := NewWriter(io.Discard, WriterOptions{})
			return err
		}, "WriterOptions.Meta is nil"},
		{"an undefined WriterOptions.CompressionLevel", func(*testing.T) error {
			return newWriterErr(nil, func(o *WriterOptions) { o.CompressionLevel = CompressionBest + 1 })
		}, "WriterOptions.CompressionLevel"},
		{"a negative WriterOptions.BlockThreshold", func(*testing.T) error {
			return newWriterErr(nil, func(o *WriterOptions) { o.BlockThreshold = -1 })
		}, "WriterOptions.BlockThreshold"},
		{"AssignSeq in a pack masked at write", func(*testing.T) error {
			return newWriterErr(maskedAssign, func(o *WriterOptions) { o.AssignSeq = true })
		}, "pack masked at write"},
		{"DetectClockSteps without AssignSeq", func(*testing.T) error {
			return newWriterErr(nil, func(o *WriterOptions) { o.DetectClockSteps = true })
		}, "requires AssignSeq"},
		{"DetectClockSteps without capture-clock", func(*testing.T) error {
			return newWriterErr(nil, func(o *WriterOptions) { o.DetectClockSteps, o.AssignSeq = true, true })
		}, "requires time_source capture-clock"},
		{"an F-3 list over summary_len at Close", func(t *testing.T) error {
			w := refusalWriter(t, io.Discard, nil)
			w.maxF3ListLen = 1
			r := testDataRecord(0, blockTestHour+1, 1)
			require.NoError(t, w.Append(&r))
			_, err := w.Close()

			return err
		}, "exceeds the u32 summary_len"},
		{"nil CloseOptions.Meta", writeClose(func(o *CloseOptions) { o.Meta = nil }), "CloseOptions.Meta is nil"},
		{"nil CloseOptions.CaptureID", writeClose(func(o *CloseOptions) { o.CaptureID = UUID{} }), "CaptureID is the nil UUID"},
		{"a close of another time source", writeClose(func(o *CloseOptions) { o.Meta.TimeSource = TimeSourceGenerator }), "not capture-clock"},
		{"a close without tolerance", writeClose(func(o *CloseOptions) { o.Meta.ClockStepToleranceNs = nil }), "without clock_step_tolerance_ns"},
		{"a close trigger not representable", writeClose(func(o *CloseOptions) { o.Trigger = time.Date(2300, 1, 1, 0, 0, 0, 0, time.UTC) }),
			"not representable as int64 nanoseconds"},
		{"nil SegmentWriterOptions.Sink", func(t *testing.T) error {
			return newSegErr(t, func(o *SegmentWriterOptions) { o.Sink = nil })
		}, "Sink is nil"},
		{"an undefined SegmentWriterOptions.CompressionLevel", func(t *testing.T) error {
			return newSegErr(t, func(o *SegmentWriterOptions) { o.CompressionLevel = CompressionBest + 1 })
		}, "SegmentWriterOptions.CompressionLevel"},
		{"a FlushInterval not dividing the hour", func(t *testing.T) error {
			return newSegErr(t, func(o *SegmentWriterOptions) { o.FlushInterval = 7 * time.Minute })
		}, "does not divide one hour"},
		{"a negative MaxSegmentBytes", func(t *testing.T) error {
			return newSegErr(t, func(o *SegmentWriterOptions) { o.MaxSegmentBytes = -1 })
		}, "MaxSegmentBytes -1 is negative"},
		{"a CaptureOrigin not representable", func(t *testing.T) error {
			return newSegErr(t, func(o *SegmentWriterOptions) { o.Capture.CaptureOrigin = time.Date(2300, 1, 1, 0, 0, 0, 0, time.UTC) })
		}, "CaptureOrigin"},
		{"a ToolID no key can hold", func(t *testing.T) error {
			return newSegErr(t, func(o *SegmentWriterOptions) { o.Capture.ToolID = ".." })
		}, "tool_id"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()

			err := tt.provoke(t)
			requireRefused(t, err, errArgument)
			require.ErrorContains(t, err, tt.msg)
		})
	}
}

// TestErrRefusedExcluded requires the errors of writing calls that their input does not determine to be unmarked.
func TestErrRefusedExcluded(t *testing.T) {
	t.Parallel()

	t.Run("Now() at Close not representable", func(t *testing.T) {
		t.Parallel()

		w, _ := newSeg(t, func(o *SegmentWriterOptions) { o.Now = nowAt(time.Date(2300, 1, 1, 0, 0, 0, 0, time.UTC)) })
		err := w.Close(t.Context())
		requireNotRefused(t, err)
		require.ErrorContains(t, err, "Now() at Close")
	})
	t.Run("a Create returning no file", func(t *testing.T) {
		t.Parallel()

		_, err := NewSegmentWriter(t.Context(), segOptions(nilFileSink{}))
		requireNotRefused(t, err)
		require.ErrorContains(t, err, "returned no file")
		require.ErrorIs(t, err, errExternal)
	})
	t.Run("a done ctx", func(t *testing.T) {
		t.Parallel()

		ctx, cancel := context.WithCancel(t.Context())
		cancel()
		w, _ := newSeg(t, nil)
		requireNotRefused(t, w.Rotate(ctx))
		_, err := NewSegmentWriter(ctx, segOptions(newMemSink()))
		requireNotRefused(t, err)
	})
}

// TestMergeNeverRefuses requires Merge's own refusals to be unmarked,
// and an ErrRefused its destination returns to pass through without a marker.
func TestMergeNeverRefuses(t *testing.T) {
	t.Parallel()

	t.Run("ErrMergeInput", func(t *testing.T) {
		t.Parallel()

		_, err := Merge(t.Context(), io.Discard, inputsOf([]byte("not a pack")), mergeOpts())
		require.ErrorIs(t, err, ErrMergeInput)
		requireNotRefused(t, err)
	})
	t.Run("ErrMergeConflict", func(t *testing.T) {
		t.Parallel()

		steps := hourSteps(10, 13, 4)
		a, b := mergePack(t, seg0, nil, steps), mergePack(t, seg1, nil, changed(steps, 1, 12))
		_, _, err := mergeBytes(t.Context(), inputsOf(a, b), mergeOpts())
		require.ErrorIs(t, err, ErrMergeConflict)
		requireNotRefused(t, err)
	})
	t.Run("ErrMergeLimit of a block", func(t *testing.T) {
		t.Parallel()

		opts := mergeOpts()
		opts.MaxFooterLen = 8
		_, _, err := mergeBytes(t.Context(), inputsOf(mergePack(t, seg0, nil, hourSteps(1, 4, 4))), opts)
		require.ErrorIs(t, err, ErrMergeLimit)
		requireNotRefused(t, err)
	})
	t.Run("ErrMergeLimit at close", func(t *testing.T) {
		t.Parallel()

		// As TestMergeFooterBudgetExceededByF5: the block is admitted, and the footer exceeds the budget when the merge closes.
		var seqs [2][]uint64
		for seq := uint64(0); seq < 400; seq += 2 {
			seqs[seq/200] = append(seqs[seq/200], seq)
		}
		a := mergePack(t, seg0, nil, seqBlocks(seqs[0], seqs[1]))
		opts := mergeOpts()
		opts.Uncompressed = true
		full, _ := mustMerge(t, inputsOf(a), opts)
		opts.MaxFooterLen = int64(layoutOf(t, full).tr.FooterLen - 1)
		_, _, err := mergeBytes(t.Context(), inputsOf(a), opts)
		require.ErrorIs(t, err, ErrMergeLimit)
		require.ErrorContains(t, err, "exceeds the budget")
		requireNotRefused(t, err)
	})
	t.Run("a destination returning ErrRefused", func(t *testing.T) {
		t.Parallel()

		a := mergePack(t, seg0, nil, hourSteps(1, 4, 4))
		_, err := Merge(t.Context(), &failingWriter{err: ErrRefused}, inputsOf(a), mergeOpts())
		require.ErrorIs(t, err, ErrRefused, "passed through")
		require.Zero(t, refusedCount(err), "no marker")
	})
}

// errSyncer is a Syncer whose every Sync fails with err.
type errSyncer struct {
	err error
}

// Sync fails.
func (s *errSyncer) Sync() error { return s.err }

// externalFaults are the errors a supplied implementation returns in the external-origin tests:
// a plain error, one impersonating a refusal, and ErrRefused itself.
func externalFaults() []struct {
	name string
	err  error
} {
	return []struct {
		name string
		err  error
	}{
		{"a plain error", errInjected},
		{"an error wrapping ErrSeqOrder", fmt.Errorf("impersonation: %w", ErrSeqOrder)},
		{"ErrRefused itself", ErrRefused},
	}
}

// requireExternal requires err, the result of a writing call whose supplied implementation failed with fault,
// to hold fault and no marker: it matches ErrRefused only when fault itself does.
func requireExternal(t *testing.T, err, fault error) {
	t.Helper()

	require.ErrorIs(t, err, fault)
	require.ErrorIs(t, err, errExternal)
	require.Zero(t, refusedCount(err), "no marker")
	require.Equal(t, errors.Is(fault, ErrRefused), errors.Is(err, ErrRefused), "ErrRefused only by passthrough")
}

// TestErrRefusedExternal requires the errors of a failing io.Writer, Syncer, SegmentSink and SegmentFile
// to be returned unmarked, even when they wrap a refusal sentinel;
// an ErrRefused they return passes through.
func TestErrRefusedExternal(t *testing.T) {
	t.Parallel()

	for _, f := range externalFaults() {
		t.Run(f.name, func(t *testing.T) {
			t.Parallel()

			// The destination, at NewWriter's header write and at WriteClose's.
			_, err := NewWriter(&failingWriter{err: f.err}, WriterOptions{Meta: repairTestMeta()})
			requireExternal(t, err, f.err)
			_, err = WriteClose(&failingWriter{err: f.err}, closeOptions(closeMeta(1000, false, false), nil))
			requireExternal(t, err, f.err)

			// The Syncer, after a block.
			w := refusalWriter(t, io.Discard, func(o *WriterOptions) { o.Sync = &errSyncer{err: f.err} })
			r := testDataRecord(0, blockTestHour+1, 1)
			require.NoError(t, w.Append(&r))
			requireExternal(t, w.Flush(), f.err)

			// The sink's Create, and a segment file's Commit.
			sink := newMemSink()
			sink.fault = func(op sinkOp, _ int) error {
				if op == sinkCreate {
					return f.err
				}

				return nil
			}
			_, err = NewSegmentWriter(t.Context(), segOptions(sink))
			requireExternal(t, err, f.err)

			sw, sink := newSeg(t, nil)
			sink.fault = func(op sinkOp, _ int) error {
				if op == sinkCommit {
					return f.err
				}

				return nil
			}
			requireExternal(t, sw.Rotate(t.Context()), f.err)
		})
	}
}

// TestErrRefusedMixedOrigin requires a refusal joined with a failed Abort to be unmarked,
// and the same refusal with an Abort that succeeds to be marked.
func TestErrRefusedMixedOrigin(t *testing.T) {
	t.Parallel()

	abortErr := errors.New("abort failed")
	appendRefused := func(t *testing.T, abort error) error {
		t.Helper()

		w, sink := newSeg(t, func(o *SegmentWriterOptions) { o.BlockThreshold = 1 })
		sink.fault = func(op sinkOp, _ int) error {
			if op == sinkAbort {
				return abort
			}

			return nil
		}
		w.seg.w.encodedHook = truncateBlock

		return w.AppendFrame(t.Context(), segAt(time.Second), DirHostToEquipment, 1, blockTestFrame, 0)
	}

	err := appendRefused(t, abortErr)
	require.ErrorIs(t, err, ErrValidation)
	require.ErrorIs(t, err, abortErr)
	requireNotRefused(t, err)

	requireRefused(t, appendRefused(t, nil), ErrValidation)

	t.Run("ctx done after the segment's last sync", func(t *testing.T) {
		t.Parallel()

		// closeExhausted closes a SegmentWriter with no seq left for the stop boundary,
		// so Close finalizes the segment without it and joins that refusal with finalize's error;
		// cancelAt is the sync of Close, counted from 0, at which ctx is cancelled, or -1 for none.
		// It returns the count of syncs Close made and Close's error.
		closeExhausted := func(t *testing.T, cancelAt int) (int, error) {
			t.Helper()

			w, sink := newSeg(t, nil)
			w.nextSeq = format.MaxU64 + 1
			ctx, cancel := context.WithCancel(t.Context())
			defer cancel()
			base := sink.calls[sinkSync]
			sink.fault = func(op sinkOp, n int) error {
				if op == sinkSync && n-base == cancelAt {
					cancel()
				}

				return nil
			}
			err := w.Close(ctx)

			return sink.calls[sinkSync] - base, err
		}

		syncs, err := closeExhausted(t, -1)
		require.Positive(t, syncs, "Close syncs the segment")
		requireRefused(t, err, ErrSeqOrder)

		// Cancelled at the trailer's sync, which succeeds, ctx is first seen done after the pack is closed.
		_, err = closeExhausted(t, syncs-1)
		require.ErrorIs(t, err, ErrSeqOrder)
		require.ErrorIs(t, err, context.Canceled)
		requireNotRefused(t, err)
	})
}

// TestErrRefusedNestedOnce requires WriteClose, whose nested NewWriter refuses its metadata, to mark its error once.
func TestErrRefusedNestedOnce(t *testing.T) {
	t.Parallel()

	m := closeMeta(1000, true, false)
	m.Redaction = maskEntries(false, format.MaxU64) // entries without the redaction-present flag
	_, err := WriteClose(io.Discard, closeOptions(m, nil))
	requireRefused(t, err, ErrMetadataCommitment)
}

// TestErrRefusedPriorState requires a failure the input determined to be marked when it happens,
// and the error of a later call on the failed writer, which reports the prior state, to be unmarked.
func TestErrRefusedPriorState(t *testing.T) {
	t.Parallel()

	t.Run("Writer", func(t *testing.T) {
		t.Parallel()

		var out bytes.Buffer
		w := refusalWriter(t, &out, nil)
		w.encodedHook = truncateBlock
		r := testDataRecord(0, blockTestHour+1, 1)
		require.NoError(t, w.Append(&r))
		requireRefused(t, w.Flush(), ErrValidation)

		r = testDataRecord(1, blockTestHour+2, 1)
		err := w.Append(&r)
		require.ErrorIs(t, err, ErrWriterFailed)
		require.ErrorIs(t, err, ErrValidation)
		requireNotRefused(t, err)
		_, err = w.Close()
		require.ErrorIs(t, err, ErrWriterFailed)
		requireNotRefused(t, err)
	})
	// The failure is stored by the segment's Writer when a record closes a block of one byte's threshold (Append),
	// or when the segment's Writer closes the open block (Rotate).
	fails := map[string]struct {
		threshold int
		fail      func(ctx context.Context, w *SegmentWriter) error
	}{
		"Append": {1, func(ctx context.Context, w *SegmentWriter) error {
			return w.AppendFrame(ctx, segAt(time.Second), DirHostToEquipment, 1, blockTestFrame, 0)
		}},
		"Rotate": {0, func(ctx context.Context, w *SegmentWriter) error { return w.Rotate(ctx) }},
	}
	for name, f := range fails {
		t.Run("SegmentWriter "+name, func(t *testing.T) {
			t.Parallel()

			w, _ := newSeg(t, func(o *SegmentWriterOptions) { o.BlockThreshold = f.threshold })
			w.seg.w.encodedHook = truncateBlock
			requireRefused(t, f.fail(t.Context(), w), ErrValidation)

			r := segMonoRecord(time.Second, time.Second)
			err := w.Append(t.Context(), &r)
			require.ErrorIs(t, err, ErrWriterFailed)
			require.ErrorIs(t, err, ErrValidation)
			requireNotRefused(t, err)
			err = w.Close(t.Context())
			require.ErrorIs(t, err, ErrValidation)
			requireNotRefused(t, err)
		})
	}
	// A Close that fails is marked as any call is; a later Close returns its result as the prior state, unmarked.
	t.Run("SegmentWriter Close", func(t *testing.T) {
		t.Parallel()

		w, _ := newSeg(t, nil)
		r := segMonoRecord(time.Second, time.Second)
		require.NoError(t, w.Append(t.Context(), &r))
		w.seg.w.encodedHook = truncateBlock
		requireRefused(t, w.Close(t.Context()), ErrValidation)

		err := w.Close(t.Context())
		require.ErrorIs(t, err, ErrValidation)
		requireNotRefused(t, err)
	})
}

// externalCallHelpers names, for each call on a supplied implementation or the random source,
// the functions that may make it: each marks the error as external, or passes it to one that does.
var externalCallHelpers = map[string][]string{
	"Write":          {"writeFull", "segmentOutput.Write"},
	"Sync":           {"syncOut", "segmentOutput.Sync"},
	"Create":         {"SegmentWriter.createSegment"},
	"Commit":         {"commitSegment"},
	"Abort":          {"abortSegment"},
	"GenerateUUIDv7": {"newUUIDv7"},
	"rand":           {"newUUIDv7"},
}

// TestErrRefusedExternalCallSites parses the writing calls' sources and requires every call on a destination, a Syncer,
// a sink, a segment file or the random source to be made by externalCallHelpers, so a new call cannot bypass the external marker;
// and each helper that is not a passthrough to calls external.
func TestErrRefusedExternalCallSites(t *testing.T) {
	t.Parallel()

	seen := map[string]bool{}
	fset := token.NewFileSet()
	for _, name := range []string{"writer.go", "segment_writer.go", "close_writer.go"} {
		f, err := parser.ParseFile(fset, name, nil, parser.SkipObjectResolution)
		require.NoError(t, err)
		for _, d := range f.Decls {
			fn, ok := d.(*ast.FuncDecl)
			if !ok || fn.Body == nil {
				continue
			}
			encl := funcName(fn)
			callsExternal, calls := false, false
			check := func(key string, sel *ast.SelectorExpr) {
				if allowed, watched := externalCallHelpers[key]; watched {
					assert.Contains(t, allowed, encl, "%s: %s.%s outside its marking helper", fset.Position(sel.Pos()), exprString(sel.X), sel.Sel.Name)
					calls = true
				}
			}
			ast.Inspect(fn.Body, func(n ast.Node) bool {
				switch x := n.(type) {
				case *ast.SelectorExpr:
					// Any use of the random source counts, a call or an argument such as rand.Reader.
					if id, ok := x.X.(*ast.Ident); ok && id.Name == "rand" {
						check("rand", x)
					}
				case *ast.CallExpr:
					if id, ok := x.Fun.(*ast.Ident); ok && id.Name == "external" {
						callsExternal = true
					}
					if sel, ok := x.Fun.(*ast.SelectorExpr); ok {
						check(sel.Sel.Name, sel)
					}
				default:
					// Only selectors and calls name a supplied implementation or the random source.
				}

				return true
			})
			if calls {
				seen[encl] = true
				if !strings.HasPrefix(encl, "segmentOutput.") {
					assert.True(t, callsExternal, "%s marks the error it returns as external", encl)
				}
			}
		}
	}
	for _, helpers := range externalCallHelpers {
		for _, h := range helpers {
			assert.True(t, seen[h], "%s makes its call", h)
		}
	}
}

// funcName returns fn's name, prefixed with its receiver's type for a method.
func funcName(fn *ast.FuncDecl) string {
	if fn.Recv == nil || len(fn.Recv.List) == 0 {
		return fn.Name.Name
	}
	typ := fn.Recv.List[0].Type
	if star, ok := typ.(*ast.StarExpr); ok {
		typ = star.X
	}

	return exprString(typ) + "." + fn.Name.Name
}

// exprString returns the source form of a selector chain of identifiers, such as w.seg.file.
func exprString(e ast.Expr) string {
	switch x := e.(type) {
	case *ast.Ident:
		return x.Name
	case *ast.SelectorExpr:
		return exprString(x.X) + "." + x.Sel.Name
	case *ast.CallExpr:
		return exprString(x.Fun) + "()"
	default:
		return fmt.Sprintf("%T", e)
	}
}

// TestErrRefusedSegmentWriterCalls requires the refusals of AppendEvent and Tick to be marked at their own boundary.
// Tick refuses nothing of its own input, now deciding only whether the segment rolls:
// it returns the refusal of the segment's Writer closing the open block.
func TestErrRefusedSegmentWriterCalls(t *testing.T) {
	t.Parallel()

	t.Run("AppendEvent", func(t *testing.T) {
		t.Parallel()

		w, _ := newSeg(t, nil)
		requireRefused(t, w.AppendEvent(t.Context(), segAt(time.Second), 0, nil), ErrInvalidRecord)
	})
	t.Run("Tick", func(t *testing.T) {
		t.Parallel()

		w, _ := newSeg(t, nil)
		require.NotNil(t, w.seg, "a segment is open")
		w.seg.w.encodedHook = truncateBlock
		requireRefused(t, w.Tick(t.Context(), time.Unix(0, w.seg.end)), ErrValidation)
	})
}

// overflowArgumentSites names, in writer.go, the functions holding a check of a length against a u32 field
// whose input cannot be built in a test, the pack metadata's length and the block count.
var overflowArgumentSites = []string{"encodeHead", "Writer.finalize"}

// TestErrRefusedOverflowArguments parses writer.go and requires each check of overflowArgumentSites
// to return its error marked as an argument, as TestErrRefusedArguments requires of the checks a test can reach:
// each function holds exactly one comparison with math.MaxUint32, whose body returns argument(...).
func TestErrRefusedOverflowArguments(t *testing.T) {
	t.Parallel()

	fset := token.NewFileSet()
	f, err := parser.ParseFile(fset, "writer.go", nil, parser.SkipObjectResolution)
	require.NoError(t, err)
	found := map[string]bool{}
	for _, d := range f.Decls {
		fn, ok := d.(*ast.FuncDecl)
		if !ok || fn.Body == nil || !slices.Contains(overflowArgumentSites, funcName(fn)) {
			continue
		}
		name := funcName(fn)
		found[name] = true
		var checks []*ast.IfStmt
		ast.Inspect(fn.Body, func(n ast.Node) bool {
			if s, ok := n.(*ast.IfStmt); ok {
				if c, ok := s.Cond.(*ast.BinaryExpr); ok && c.Op == token.GTR && exprString(c.Y) == "math.MaxUint32" {
					checks = append(checks, s)
				}
			}

			return true
		})
		require.Len(t, checks, 1, "%s holds one u32 check", name)
		body := checks[0].Body.List
		require.NotEmpty(t, body, "%s: the u32 check returns", name)
		ret, ok := body[len(body)-1].(*ast.ReturnStmt)
		require.True(t, ok, "%s: the u32 check returns", name)
		require.NotEmpty(t, ret.Results)
		call, ok := ret.Results[len(ret.Results)-1].(*ast.CallExpr)
		require.True(t, ok, "%s: %s returns a call", name, fset.Position(ret.Pos()))
		id, ok := call.Fun.(*ast.Ident)
		assert.True(t, ok && id.Name == "argument", "%s: %s returns its error marked by argument", name, fset.Position(ret.Pos()))
	}
	for _, name := range overflowArgumentSites {
		assert.True(t, found[name], "writer.go declares %s", name)
	}
}

// internalBoundaryCalls lists, by their receiver's source form, the calls named Append, Flush or Close
// that TestErrRefusedInternalBoundary allows: calls on a value that is not a Writer.
// It is empty.
var internalBoundaryCalls = map[string]bool{}

// TestErrRefusedInternalBoundary parses Merge's and the SegmentWriter's sources and requires them to make no call
// named Append, Flush or Close, so they reach a Writer only through its unexported implementations:
// a Merge error never passes a wrapper that adds ErrRefused, and the failure a SegmentWriter stores carries no marker.
func TestErrRefusedInternalBoundary(t *testing.T) {
	t.Parallel()

	files, err := filepath.Glob("merge*.go")
	require.NoError(t, err)
	files = slices.DeleteFunc(files, func(name string) bool { return strings.HasSuffix(name, "_test.go") })
	require.Contains(t, files, "merge_write.go")
	files = append(files, "segment_writer.go")

	fset := token.NewFileSet()
	for _, name := range files {
		f, err := parser.ParseFile(fset, name, nil, parser.SkipObjectResolution)
		require.NoError(t, err)
		ast.Inspect(f, func(n ast.Node) bool {
			call, ok := n.(*ast.CallExpr)
			if !ok {
				return true
			}
			sel, ok := call.Fun.(*ast.SelectorExpr)
			if !ok {
				return true
			}
			switch sel.Sel.Name {
			case "Append", "Flush", "Close":
				recv := exprString(sel.X)
				assert.True(t, internalBoundaryCalls[recv], "%s: %s.%s calls a public writing method; call its unexported implementation",
					fset.Position(sel.Pos()), recv, sel.Sel.Name)
			default:
				// Any other call is outside the boundary.
			}

			return true
		})
	}
}
