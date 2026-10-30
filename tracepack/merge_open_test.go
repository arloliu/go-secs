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
	"math/rand/v2"
	"os"
	"slices"
	"strings"
	"testing"
	"testing/synctest"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// serialOpenInputs is mergePlan.openInputs as it was before Merge opened its inputs concurrently, kept unchanged as the tests' oracle:
// it opens each input once the one before it was checked.
func serialOpenInputs(p *mergePlan, ctx context.Context, inputs []MergeInput, opts ReaderOptions) error {
	seen := make(map[UUID]int, len(inputs))
	p.inputs = make([]*Reader, 0, len(inputs))
	p.thinned = make([]map[SF]struct{}, 0, len(inputs))
	for i, in := range inputs {
		r, err := openInput(ctx, i, in, opts)
		if err != nil {
			return err
		}
		id := UUID(r.hdr.PackID)
		if j, dup := seen[id]; dup {
			return inputError(i, id, fmt.Errorf("the pack_id of input %d", j))
		}
		seen[id] = i
		facts, err := p.checkInput(i, r)
		if err != nil {
			return inputError(i, id, err)
		}
		p.facts.AnyClassified = p.facts.AnyClassified || facts.AnyClassified
		p.facts.AnyOversized = p.facts.AnyOversized || facts.AnyOversized
		p.inputs = append(p.inputs, r)
		p.thinned = append(p.thinned, thinnedSFs(r.meta.Thinning))
	}

	return nil
}

// serialHooks makes a merge open its inputs with the oracle.
var serialHooks = mergeHooks{open: serialOpenInputs}

// mergeSentinels names every exported sentinel error of the package,
// the io and context sentinels a merge may wrap, and the tests' injected cause,
// so two outcomes are compared on each, present or absent.
var mergeSentinels = map[string]error{
	"ErrAnnotationText": ErrAnnotationText, "ErrBusMalformed": ErrBusMalformed, "ErrBusNotAccepted": ErrBusNotAccepted,
	"ErrBusPayloadMismatch": ErrBusPayloadMismatch, "ErrChecksum": ErrChecksum, "ErrClosed": ErrClosed,
	"ErrFieldLength": ErrFieldLength, "ErrFieldValidity": ErrFieldValidity, "ErrFieldValue": ErrFieldValue,
	"ErrInvalidFooter": ErrInvalidFooter, "ErrInvalidKey": ErrInvalidKey, "ErrInvalidQuery": ErrInvalidQuery,
	"ErrInvalidRecord": ErrInvalidRecord, "ErrMergeConflict": ErrMergeConflict, "ErrMergeInput": ErrMergeInput,
	"ErrMergeLimit": ErrMergeLimit, "ErrMetadataCommitment": ErrMetadataCommitment, "ErrNotPrimary": ErrNotPrimary,
	"ErrNotTracepack": ErrNotTracepack, "ErrPayloadTooLarge": ErrPayloadTooLarge, "ErrPublishUncertain": ErrPublishUncertain,
	"ErrReadLimit": ErrReadLimit, "ErrRedactionDefect": ErrRedactionDefect, "ErrRequiredTag": ErrRequiredTag,
	"ErrReservedTag": ErrReservedTag, "ErrScopeBreach": ErrScopeBreach, "ErrSegmentConflict": ErrSegmentConflict,
	"ErrSegmentExists": ErrSegmentExists, "ErrSeqOrder": ErrSeqOrder, "ErrUnsupportedFormat": ErrUnsupportedFormat,
	"ErrValidation": ErrValidation, "ErrWriterFailed": ErrWriterFailed,
	"io.EOF": io.EOF, "io.ErrUnexpectedEOF": io.ErrUnexpectedEOF,
	"context.Canceled": context.Canceled, "context.DeadlineExceeded": context.DeadlineExceeded,
	"injected": errInjected,
}

// mergeSentinels lists every exported variable of the package whose name starts with Err,
// so a sentinel added later is compared too.
func TestMergeSentinelsListed(t *testing.T) {
	t.Parallel()

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
	for _, n := range names {
		assert.Contains(t, mergeSentinels, n, "mergeSentinels lacks %s", n)
	}
}

// mergeOutcome is everything a merge's caller observes.
type mergeOutcome struct {
	out       []byte
	rep       MergeReport
	conflicts []Conflict
	err       string
	is        map[string]bool
}

// mergeNow is the clock of the merges whose outcomes are compared.
func mergeNow() time.Time { return time.Unix(0, blockTestHour+int64(time.Minute)).UTC() }

// runMerge merges the inputs mk returns with fixed identities and clock, through hooks, and returns its outcome.
func runMerge(t *testing.T, ctx context.Context, mk func() []MergeInput, opts MergeOptions, hooks mergeHooks) mergeOutcome {
	t.Helper()

	var o mergeOutcome
	opts = fixedIDs(opts)
	opts.Now = mergeNow
	opts.OnConflict = func(c Conflict) { o.conflicts = append(o.conflicts, c) }
	var out bytes.Buffer
	rep, err := mergeWith(ctx, &out, mk(), opts, hooks)
	o.out, o.rep = out.Bytes(), rep
	o.is = make(map[string]bool, len(mergeSentinels))
	if err != nil {
		o.err = err.Error()
		for name, s := range mergeSentinels {
			o.is[name] = errors.Is(err, s)
		}
	}

	return o
}

// requireSerialOutcome requires a merge of the inputs mk returns to observe what the oracle's merge of them observes.
func requireSerialOutcome(t *testing.T, mk func() []MergeInput, opts MergeOptions) {
	t.Helper()

	got := runMerge(t, t.Context(), mk, opts, mergeHooks{})
	want := runMerge(t, t.Context(), mk, opts, serialHooks)
	require.Equal(t, want, got)
}

// openInputKind is how one input of a property case differs from a valid segment.
type openInputKind int

const (
	inputValid openInputKind = iota
	inputBadMagic
	inputBadMetaCRC
	inputBadFooter
	inputShort
	inputOtherCapture
	inputDuplicateID
	inputFailingRead
	inputDeadlineRead
	inputConflicting
	openInputKinds
)

// openCaseInput is one input of a property case: its file, its declared size, and the ReaderAt behavior at offset 0.
type openCaseInput struct {
	file []byte
	size int64
	fail error
}

// openCase builds the inputs of one property case from r: n inputs of one scope,
// up to two of them, at random positions, of a random kind other than valid.
func openCase(t *testing.T, r *rand.Rand, codec Codec, n int) []openCaseInput {
	t.Helper()

	kinds := make([]openInputKind, n)
	for range r.IntN(3) {
		kinds[r.IntN(n)] = openInputKind(1 + r.IntN(int(openInputKinds)-1))
	}
	in := make([]openCaseInput, n)
	for i := range in {
		first := uint64(i*3 + 1)
		id := UUID{0x70, byte(i)}
		kind := kinds[i]
		steps := hourSteps(first, first+2, 2)
		capture := viewCapture
		switch kind {
		case inputOtherCapture:
			capture = UUID{0x7c}
		case inputDuplicateID:
			if i > 0 {
				id = UUID{0x70, byte(r.IntN(i))}
			}
		case inputConflicting:
			// The seqs of input 0, with other payloads.
			steps = hourSteps(1, 3, 2)
			for k := range steps {
				steps[k].rec.Payload = append(slices.Clone(steps[k].rec.Payload), 0x5a)
			}
		case inputValid, inputBadMagic, inputBadMetaCRC, inputBadFooter, inputShort, inputFailingRead, inputDeadlineRead, openInputKinds:
			// Written as a valid segment; the file or its reader is changed below.
		default:
			t.Fatalf("input kind %d", kind)
		}
		file := withIDs(t, writeRepairPack(t, codec, nil, false, steps).file, id, capture)
		c := openCaseInput{file: file, size: int64(len(file))}
		switch kind {
		case inputBadMagic:
			c.file = slices.Clone(file)
			c.file[0] ^= 0xff
		case inputBadMetaCRC:
			c.file = slices.Clone(file)
			c.file[90] ^= 0xff
		case inputBadFooter:
			c.file = slices.Clone(file)
			c.file[len(file)-80] ^= 0xff
		case inputShort:
			c.file = file[:len(file)-7]
		case inputFailingRead:
			c.fail = errInjected
		case inputDeadlineRead:
			c.fail = context.DeadlineExceeded
		case inputValid, inputOtherCapture, inputDuplicateID, inputConflicting, openInputKinds:
			// Changed above, if at all, when it was written.
		default:
			t.Fatalf("input kind %d", kind)
		}
		in[i] = c
	}

	return in
}

// caseInputs returns fresh ReaderAts over the inputs of a property case.
func caseInputs(in []openCaseInput) func() []MergeInput {
	return func() []MergeInput {
		out := make([]MergeInput, len(in))
		for i, c := range in {
			rr := newRoundReader(c.file)
			if c.fail != nil {
				rr.at(0).err = c.fail
			}
			out[i] = MergeInput{ReaderAt: rr, Size: c.size}
		}

		return out
	}
}

// Merge observes, for every input set, what opening the inputs one after another observes:
// the archive's bytes, the report, every conflict, the error's text and each sentinel it wraps,
// whatever MaxParallelOpens, with zstd and uncompressed inputs, so pooled decoders are reused across valid and malformed footers.
func TestMergeOpenOutcomeSerial(t *testing.T) {
	t.Parallel()

	r := rand.New(rand.NewPCG(11, 29))
	for k := range 300 {
		codec := CodecZstd
		if k%3 == 0 {
			codec = CodecNone
		}
		in := openCase(t, r, codec, 1+r.IntN(40))
		opts := mergeOpts()
		opts.MaxParallelOpens = []int{0, 1, 2, 5, 40}[r.IntN(5)]
		opts.Uncompressed = r.IntN(2) == 0
		opts.NoCoalesce = r.IntN(2) == 0
		if r.IntN(8) == 0 {
			opts.Reader.MaxPackMetadataLen = 32
		}
		t.Run(fmt.Sprint(k), func(t *testing.T) {
			t.Parallel()

			requireSerialOutcome(t, caseInputs(in), opts)
		})
	}
}

// The comparison of TestMergeOpenOutcomeSerial sees a sentinel lost behind an unchanged text.
func TestMergeOutcomeSeesLostCause(t *testing.T) {
	t.Parallel()

	file := mergePackCodec(t, CodecNone, UUID{0x70}, hourSteps(1, 3, 2))
	mk := caseInputs([]openCaseInput{{file: file, size: int64(len(file)), fail: errInjected}})
	opts := mergeOpts()
	want := runMerge(t, t.Context(), mk, opts, serialHooks)
	stripped := mergeHooks{open: func(p *mergePlan, ctx context.Context, inputs []MergeInput, o ReaderOptions) error {
		if err := p.openInputs(ctx, inputs, o); err != nil {
			return errors.New(err.Error())
		}

		return nil
	}}
	got := runMerge(t, t.Context(), mk, opts, stripped)
	require.Equal(t, want.err, got.err, "the text is kept")
	require.NotEqual(t, want, got, "the lost cause is seen")
}

// openTestFiles returns n valid uncompressed segments of one scope, with distinct pack_ids and seqs.
func openTestFiles(t *testing.T, n int) [][]byte {
	t.Helper()

	files := make([][]byte, n)
	for i := range files {
		first := uint64(i*3 + 1)
		files[i] = mergePackCodec(t, CodecNone, UUID{0x70, byte(i)}, hourSteps(first, first+2, 2))
	}

	return files
}

// gatedInputs returns a roundReader per file, as merge inputs.
func gatedInputs(files [][]byte) ([]*roundReader, []MergeInput) {
	rs := make([]*roundReader, len(files))
	in := make([]MergeInput, len(files))
	for i, f := range files {
		rs[i] = newRoundReader(f)
		in[i] = MergeInput{ReaderAt: rs[i], Size: int64(len(f))}
	}

	return rs, in
}

// entered reports whether input r has had a ReadAt call.
func entered(r *roundReader) bool { return r.calls.Load() > 0 }

// mergeAsync runs a merge of inputs on a goroutine of its own and sends its error, or the value it panicked with, once it returns.
func mergeAsync(ctx context.Context, inputs []MergeInput, opts MergeOptions, hooks mergeHooks) <-chan any {
	out := make(chan any, 1)
	go func() {
		defer func() {
			if v := recover(); v != nil {
				out <- v
			}
		}()
		var dst bytes.Buffer
		_, err := mergeWith(ctx, &dst, inputs, opts, hooks)
		out <- err
	}()

	return out
}

// requirePending requires the merge whose outcome comes on got not to have returned.
func requirePending(t *testing.T, got <-chan any) {
	t.Helper()

	select {
	case v := <-got:
		t.Fatalf("Merge returned %v while an open was in flight", v)
	default:
	}
}

func openTestOpts() MergeOptions {
	opts := mergeOpts()
	opts.Uncompressed = true

	return opts
}

// The input an error names is the first, in input order, whose open or checks fail, whichever open finishes first.
func TestMergeOpenOrderUnderTiming(t *testing.T) {
	t.Parallel()

	files := openTestFiles(t, 4)
	for _, tt := range []struct {
		name      string
		firstFail bool
		want      string
	}{
		{"input 0 succeeds late", false, "input 3"},
		{"input 0 fails late", true, "input 0"},
	} {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()

			synctest.Test(t, func(t *testing.T) {
				rs, in := gatedInputs(files)
				release := make(chan struct{})
				rs[0].at(0).after = release
				if tt.firstFail {
					rs[0].at(0).err = errInjected
				}
				rs[3].at(0).err = errInjected
				got := mergeAsync(t.Context(), in, openTestOpts(), mergeHooks{})
				synctest.Wait()
				requirePending(t, got)
				close(release)
				err, _ := (<-got).(error)
				require.ErrorIs(t, err, errInjected)
				assert.Contains(t, err.Error(), tt.want+":")
			})
		})
	}
}

// Once an input fails, Merge returns only after every open it launched returned, and no ReadAt comes after it returns,
// whether the input failed, its open panicked, or a panic came from the checks.
func TestMergeOpenJoin(t *testing.T) {
	t.Parallel()

	files := openTestFiles(t, 8)
	for _, tt := range []struct {
		name  string
		setup func(rs []*roundReader, hooks *mergeHooks)
		panic bool
	}{
		{"failed open", func(rs []*roundReader, _ *mergeHooks) { rs[0].at(0).err = errInjected }, false},
		{"panicking open", func(rs []*roundReader, _ *mergeHooks) { rs[0].at(0).panic = "input 0" }, true},
		{"panicking checks", func(_ []*roundReader, h *mergeHooks) {
			h.admitted = func(i int) {
				if i == 0 {
					panic("input 0")
				}
			}
		}, true},
	} {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()

			synctest.Test(t, func(t *testing.T) {
				rs, in := gatedInputs(files)
				var hooks mergeHooks
				tt.setup(rs, &hooks)
				first, peer := make(chan struct{}), make(chan struct{})
				rs[0].at(0).after = first
				rs[5].at(0).after = peer
				got := mergeAsync(t.Context(), in, openTestOpts(), hooks)
				<-rs[5].at(0).entered
				close(first)
				synctest.Wait()
				requirePending(t, got)
				close(peer)
				v := <-got
				if tt.panic {
					assert.Equal(t, "input 0", v)
				} else {
					err, ok := v.(error)
					require.True(t, ok, "Merge returned %v", v)
					require.ErrorIs(t, err, errInjected)
				}
				calls := make([]int64, len(rs))
				for i, r := range rs {
					calls[i] = r.calls.Load()
				}
				synctest.Wait()
				for i, r := range rs {
					assert.Equal(t, calls[i], r.calls.Load(), "no ReadAt of input %d after Merge returned", i)
				}
			})
		})
	}
}

// At most MaxParallelOpens inputs open, or wait to be checked, at once, the one checked next included:
// an input is launched only once the input that many before it was checked, not when an open completes.
func TestMergeOpenWindow(t *testing.T) {
	t.Parallel()

	files := openTestFiles(t, 40)
	for _, tt := range []struct {
		opens, want int
	}{{0, 16}, {1, 1}, {3, 3}, {16, 16}} {
		t.Run(fmt.Sprint(tt.opens), func(t *testing.T) {
			t.Parallel()

			synctest.Test(t, func(t *testing.T) {
				rs, in := gatedInputs(files)
				release := make(chan struct{})
				rs[0].at(0).after = release
				opts := openTestOpts()
				opts.MaxParallelOpens = tt.opens
				got := mergeAsync(t.Context(), in, opts, mergeHooks{})
				synctest.Wait()
				for i, r := range rs {
					assert.Equal(t, i < tt.want, entered(r), "input %d opened while input 0 is held", i)
				}
				close(release)
				require.Nil(t, <-got)
				for i, r := range rs {
					assert.True(t, entered(r), "input %d opened once input 0 was checked", i)
				}
			})
		})
	}
}

// A failure stops the launches: with the default window, no input beyond the failing one plus 15 is ever read,
// at the refill boundaries too, and every input before the failing one was read.
func TestMergeOpenEarlyStop(t *testing.T) {
	t.Parallel()

	files := openTestFiles(t, 40)
	for _, fail := range []int{0, 15, 16, 31, 32} {
		t.Run(fmt.Sprint(fail), func(t *testing.T) {
			t.Parallel()

			synctest.Test(t, func(t *testing.T) {
				rs, in := gatedInputs(files)
				rs[fail].at(0).err = errInjected
				err, _ := (<-mergeAsync(t.Context(), in, openTestOpts(), mergeHooks{})).(error)
				require.ErrorIs(t, err, errInjected)
				assert.Contains(t, err.Error(), fmt.Sprintf("input %d:", fail))
				for i, r := range rs {
					switch {
					case i <= fail:
						assert.True(t, entered(r), "input %d read", i)
					case i > fail+15:
						assert.False(t, entered(r), "input %d never read", i)
					default:
					}
				}
			})
		})
	}
}

// A panic of an input after a failing one is discarded with its open, as opening one after another never opens that input,
// in a round of one request or of two.
func TestMergeOpenSpeculativePanic(t *testing.T) {
	t.Parallel()

	files := openTestFiles(t, 3)
	for _, tt := range []struct {
		name   string
		reader ReaderOptions
	}{{"one request", ReaderOptions{}}, {"two requests", roundOptions}} {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()

			synctest.Test(t, func(t *testing.T) {
				rs, in := gatedInputs(files)
				release := make(chan struct{})
				rs[0].at(0).after = release
				rs[0].at(0).err = errInjected
				rs[1].at(0).panic = "input 1"
				opts := openTestOpts()
				opts.Reader = tt.reader
				got := mergeAsync(t.Context(), in, opts, mergeHooks{})
				<-rs[1].at(0).done
				close(release)
				err, _ := (<-got).(error)
				require.ErrorIs(t, err, errInjected)
				assert.Contains(t, err.Error(), "input 0:")
			})
		})
	}
}

// A panic of an admitted input is raised on Merge's caller, with its value, once every open returned,
// in a round of one request or of two.
func TestMergeOpenAdmittedPanic(t *testing.T) {
	t.Parallel()

	files := openTestFiles(t, 4)
	for _, tt := range []struct {
		name   string
		reader ReaderOptions
	}{{"one request", ReaderOptions{}}, {"two requests", roundOptions}} {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()

			synctest.Test(t, func(t *testing.T) {
				rs, in := gatedInputs(files)
				rs[0].at(0).panic = "input 0"
				peer := make(chan struct{})
				rs[3].at(0).after = peer
				// Input 0 panics only once input 3's read is in flight.
				rs[0].at(0).after = rs[3].at(0).entered
				opts := openTestOpts()
				opts.Reader = tt.reader
				got := mergeAsync(t.Context(), in, opts, mergeHooks{})
				<-rs[3].at(0).entered
				synctest.Wait()
				requirePending(t, got)
				close(peer)
				assert.Equal(t, "input 0", <-got)
			})
		})
	}
}

// A panic of an admitted input's open on a goroutine of its own is raised on Merge's caller, with its value, when its input is checked,
// in a round of one request or of two.
func TestMergeOpenAdmittedWorkerPanic(t *testing.T) {
	t.Parallel()

	files := openTestFiles(t, 3)
	for _, tt := range []struct {
		name   string
		reader ReaderOptions
	}{{"one request", ReaderOptions{}}, {"two requests", roundOptions}} {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()

			synctest.Test(t, func(t *testing.T) {
				rs, in := gatedInputs(files)
				release := make(chan struct{})
				rs[0].at(0).after = release
				rs[1].at(0).panic = "input 1"
				opts := openTestOpts()
				opts.Reader = tt.reader
				got := mergeAsync(t.Context(), in, opts, mergeHooks{})
				// Input 1 panicked on its own goroutine while input 0 is held.
				<-rs[1].at(0).done
				synctest.Wait()
				requirePending(t, got)
				close(release)
				assert.Equal(t, "input 1", <-got)
			})
		})
	}
}

// Cancelling ctx fails the merge with ctx's error, before anything is written, whenever it comes, and every open is waited for.
func TestMergeOpenCancelled(t *testing.T) {
	t.Parallel()

	files := openTestFiles(t, 20)
	t.Run("before Merge", func(t *testing.T) {
		t.Parallel()

		synctest.Test(t, func(t *testing.T) {
			rs, in := gatedInputs(files)
			ctx, cancel := context.WithCancel(t.Context())
			cancel()
			err, _ := (<-mergeAsync(ctx, in, openTestOpts(), mergeHooks{})).(error)
			require.ErrorIs(t, err, context.Canceled)
			for i, r := range rs {
				assert.False(t, entered(r), "input %d read after ctx was cancelled", i)
			}
		})
	})
	t.Run("while opens wait", func(t *testing.T) {
		t.Parallel()

		synctest.Test(t, func(t *testing.T) {
			rs, in := gatedInputs(files)
			release := make(chan struct{})
			rs[0].at(0).after = release
			ctx, cancel := context.WithCancel(t.Context())
			got := mergeAsync(ctx, in, openTestOpts(), mergeHooks{})
			synctest.Wait()
			cancel()
			synctest.Wait()
			requirePending(t, got)
			close(release)
			err, _ := (<-got).(error)
			require.ErrorIs(t, err, context.Canceled)
		})
	})
	t.Run("an earlier check failure beats the cancellation", func(t *testing.T) {
		t.Parallel()

		// Input 1 is of another capture and opened before ctx is cancelled;
		// opening one after another would have begun it after the cancellation and failed on ctx.
		other := slices.Clone(files)
		other[1] = withIDs(t, other[1], UUID{0x70, 1}, UUID{0x7c})
		synctest.Test(t, func(t *testing.T) {
			rs, in := gatedInputs(other)
			release := make(chan struct{})
			rs[0].at(0).after = release
			ctx, cancel := context.WithCancel(t.Context())
			got := mergeAsync(ctx, in, openTestOpts(), mergeHooks{})
			synctest.Wait()
			cancel()
			close(release)
			err, _ := (<-got).(error)
			require.ErrorIs(t, err, ErrMergeInput)
			assert.Contains(t, err.Error(), "input 1")
		})
	})
}

// A negative MaxParallelOpens fails Merge before it reads any input.
func TestMergeParallelOpensNegative(t *testing.T) {
	t.Parallel()

	rs, in := gatedInputs(openTestFiles(t, 2))
	opts := openTestOpts()
	opts.MaxParallelOpens = -1
	var dst bytes.Buffer
	_, err := Merge(t.Context(), &dst, in, opts)
	require.EqualError(t, err, "tracepack: MergeOptions.MaxParallelOpens -1 is negative")
	assert.Zero(t, dst.Len())
	for _, r := range rs {
		assert.False(t, entered(r))
	}
}
