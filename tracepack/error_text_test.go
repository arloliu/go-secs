package tracepack

import (
	"errors"
	"fmt"
	"io"
	"io/fs"
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// requireNamespaceOnce requires each line of err's text to name errNamespace at most once,
// and a text of one line to name it exactly once.
func requireNamespaceOnce(t *testing.T, err error) {
	t.Helper()

	require.Error(t, err)
	lines := strings.Split(err.Error(), "\n")
	for i, line := range lines {
		n := strings.Count(line, errNamespace)
		if len(lines) == 1 {
			assert.Equal(t, 1, n, "the namespace once: %q", line)
		} else {
			assert.LessOrEqual(t, n, 1, "line %d names the namespace at most once: %q", i, line)
		}
	}
}

// TestErrorTextNamespaceOnce drives errors the module builds over other errors of its own through public calls,
// and requires each line of their text to name the namespace once,
// while errors.Is and errors.As find the same causes as before.
func TestErrorTextNamespaceOnce(t *testing.T) {
	t.Parallel()

	file, clean := readFaultPack(t)
	l := layoutOf(t, file)
	blocks := clean.Blocks()
	capture := UUID(clean.hdr.CaptureID)

	t.Run("a ReadAt failure through FindTransactions", func(t *testing.T) {
		t.Parallel()

		g := newGatedReader(file)
		g.fail = func(off int64, _ int) bool { return off == int64(blocks[1].Offset) }
		r, err := Open(t.Context(), g, int64(len(file)), uncachedWindows())
		require.NoError(t, err)
		src, err := NewReaderSource([]*Reader{r})
		require.NoError(t, err)
		span := TxRange{CaptureID: capture, From: time.Unix(0, blocks[0].TSMin), To: time.Unix(0, blocks[2].TSMax+1)}
		_, err = FindTransactions(t.Context(), src, span, TxRangeOptions{}, func(*TxPairing) error { return nil })
		requireNamespaceOnce(t, err)
		require.True(t, strings.HasPrefix(err.Error(), "tracepack: find transactions: hour "), err.Error())
		require.True(t, strings.HasSuffix(err.Error(), ": "+errInjected.Error()), "the caller's text kept: %s", err)
		require.ErrorIs(t, err, errInjected)
		requireNotCorrupt(t, err)
	})

	t.Run("a footer over the budget through ExportJSONL", func(t *testing.T) {
		t.Parallel()

		r := mustOpen(t, file, ReaderOptions{MaxFooterLen: 8})
		_, err := ExportJSONL(t.Context(), r, io.Discard)
		require.EqualError(t, err, fmt.Sprintf("tracepack: export JSONL: footer at offset %d: footer_len %d exceeds MaxFooterLen 8: structure exceeds a reader budget",
			l.tr.FooterOffset, l.tr.FooterLen))
		requireNamespaceOnce(t, err)
		require.ErrorIs(t, err, ErrReadLimit)
	})

	t.Run("a FieldError through NewWriter", func(t *testing.T) {
		t.Parallel()

		err := newWriterErr(func(m *PackMeta) { m.Writer = "\xff" }, nil)
		require.EqualError(t, err,
			"tracepack: encode pack metadata: writer: field value not allowed: tlv: tag 0x0007 at offset 0: tlv: utf8 value is not valid UTF-8")
		requireNamespaceOnce(t, err)
		requireRefused(t, err, ErrFieldValue)
		require.ErrorIs(t, err, ErrInvalidArgument)
		var fe *FieldError
		require.ErrorAs(t, err, &fe)
		require.Equal(t, "writer", fe.Field)
	})

	t.Run("a failed Writer's later Append", func(t *testing.T) {
		t.Parallel()

		w := refusalWriter(t, io.Discard, nil)
		w.encodedHook = truncateBlock
		r := testDataRecord(0, blockTestHour+1, 1)
		require.NoError(t, w.Append(&r))
		failure := w.Flush()
		requireNamespaceOnce(t, failure)

		r = testDataRecord(1, blockTestHour+2, 1)
		err := w.Append(&r)
		require.Equal(t, "tracepack: writer failed: "+strings.TrimPrefix(failure.Error(), errNamespace), err.Error())
		requireNamespaceOnce(t, err)
		require.ErrorIs(t, err, ErrWriterFailed)
		require.ErrorIs(t, err, ErrValidation)
		requireNotRefused(t, err)
	})

	t.Run("a damaged trailer's FooterErr", func(t *testing.T) {
		t.Parallel()

		r := mustOpen(t, flipByte(file, uint64(l.size-30)), ReaderOptions{})
		err := r.Header().FooterErr
		require.True(t, strings.HasPrefix(err.Error(), "tracepack: no valid trailer: pack not finalized: checksum mismatch: trailer: "), err.Error())
		requireNamespaceOnce(t, err)
		require.ErrorIs(t, err, ErrNotFinalized)
		require.ErrorIs(t, err, ErrChecksum)

		pe := PackError{Pack: 2, Err: err}
		require.Equal(t, "tracepack: pack 2: "+strings.TrimPrefix(err.Error(), errNamespace), pe.Error())
		requireNamespaceOnce(t, pe)

		te := TxPackError{Hour: 497500, Pack: capture, Err: err}
		require.Equal(t, "tracepack: hour 497500 pack "+capture.String()+": "+strings.TrimPrefix(err.Error(), errNamespace), te.Error(),
			"the hour and pack_id kept")
		requireNamespaceOnce(t, te)
		var got TxPackError
		require.ErrorAs(t, wrapf("tracepack: find transaction: %w", te), &got)
		require.Equal(t, te, got)
		require.ErrorIs(t, te, ErrNotFinalized)
	})

	t.Run("a caller's error whose text begins with the namespace", func(t *testing.T) {
		t.Parallel()

		callerErr := errors.New("tracepack: caller fault")
		_, err := Open(t.Context(), errReaderAt{err: callerErr}, int64(len(file)), ReaderOptions{})
		require.Regexp(t, `^tracepack: read \d+ bytes at offset 0: caller fault$`, err.Error(), "the caller's namespace trimmed")
		requireNamespaceOnce(t, err)
		require.ErrorIs(t, err, callerErr, "the error value is unchanged")
		requireNotCorrupt(t, err)
	})
}

// TestErrorTextJoined requires the joined errors of a SegmentWriter to name the namespace once on each line,
// in the order of their causes, with an Abort that succeeds adding no line.
func TestErrorTextJoined(t *testing.T) {
	t.Parallel()

	abortErr := errors.New("tracepack: abort failed")

	t.Run("a failed Commit after publication, then Abort", func(t *testing.T) {
		t.Parallel()

		// commit fails the Commit after publication and Abort with abort, nil for an Abort that succeeds.
		commit := func(t *testing.T, abort error) error {
			t.Helper()

			w, sink := newSeg(t, nil)
			segFrame(t, w, time.Second)
			sink.fault = func(op sinkOp, n int) error {
				if op == sinkAbort {
					return abort
				}

				return failAt(sinkPublished, 0)(op, n)
			}

			return w.AppendFrame(t.Context(), segAt(time.Hour), DirHostToEquipment, 1, blockTestFrame, 0)
		}

		const first = "tracepack: commit segment: segment published, durability uncertain: published 0: injected read failure"
		err := commit(t, abortErr)
		require.EqualError(t, err, first+"\ntracepack: abort segment: abort failed")
		requireNamespaceOnce(t, err)
		var joined interface{ Unwrap() []error }
		require.ErrorAs(t, err, &joined)
		parts := joined.Unwrap()
		require.Len(t, parts, 2)
		require.ErrorIs(t, parts[0], ErrPublishUncertain, "the cause first")
		require.ErrorIs(t, parts[1], abortErr, "the Abort error after it")
		require.ErrorIs(t, err, errInjected)
		require.ErrorIs(t, err, errExternal)
		requireNotRefused(t, err)

		err = commit(t, nil)
		require.EqualError(t, err, first, "an Abort that succeeds joins nothing")
		requireNamespaceOnce(t, err)
		require.ErrorIs(t, err, ErrPublishUncertain)
	})

	t.Run("a refusal joined with a failed Abort", func(t *testing.T) {
		t.Parallel()

		w, sink := newSeg(t, func(o *SegmentWriterOptions) { o.BlockThreshold = 1 })
		sink.fault = func(op sinkOp, _ int) error {
			if op == sinkAbort {
				return abortErr
			}

			return nil
		}
		w.seg.w.encodedHook = truncateBlock
		err := w.AppendFrame(t.Context(), segAt(time.Second), DirHostToEquipment, 1, blockTestFrame, 0)
		lines := strings.Split(err.Error(), "\n")
		require.Len(t, lines, 2, err.Error())
		require.True(t, strings.HasPrefix(lines[0], "tracepack: block failed validation: "), lines[0])
		require.Equal(t, "tracepack: abort segment: abort failed", lines[1])
		requireNamespaceOnce(t, err)
		require.ErrorIs(t, err, ErrValidation)
		require.ErrorIs(t, err, abortErr)
		requireNotRefused(t, err)
	})

	t.Run("a DirSink error through the SegmentWriter", func(t *testing.T) {
		t.Parallel()

		w, sink, _ := newDirSeg(t)
		ds, ok := sink.SegmentSink.(*DirSink)
		require.True(t, ok)
		require.NoError(t, ds.Close())
		err := w.AppendFrame(t.Context(), segAt(time.Hour), DirHostToEquipment, 1, blockTestFrame, 0)
		lines := strings.Split(err.Error(), "\n")
		require.Len(t, lines, 2, err.Error())
		require.True(t, strings.HasPrefix(lines[0], "tracepack: commit segment: DirSink: Commit of segment "), lines[0])
		require.True(t, strings.HasPrefix(lines[1], "tracepack: abort segment: DirSink: remove segment "), lines[1])
		requireNamespaceOnce(t, err)
		require.ErrorIs(t, err, fs.ErrClosed)
		require.ErrorIs(t, err, errExternal)
		requireNotRefused(t, err)
	})
}

// TestErrorTypesText requires FieldError, PackError and TxPackError to name the namespace once before their context.
func TestErrorTypesText(t *testing.T) {
	t.Parallel()

	fe := &FieldError{Field: "redaction_policy.policy_id", Err: ErrFieldValue}
	require.EqualError(t, fe, "tracepack: redaction_policy.policy_id: field value not allowed")
	require.ErrorIs(t, fe, ErrFieldValue)

	pack, err := ParseUUID("01a192fc-276d-7f41-a6a1-33ced22ad79d")
	require.NoError(t, err)
	cause := wrapf("tracepack: block 1 at offset 9: %w", ErrNotFinalized)
	require.EqualError(t, PackError{Pack: 3, Err: cause}, "tracepack: pack 3: block 1 at offset 9: pack not finalized")
	require.EqualError(t, TxPackError{Hour: 497500, Pack: pack, Err: cause},
		"tracepack: hour 497500 pack 01a192fc-276d-7f41-a6a1-33ced22ad79d: block 1 at offset 9: pack not finalized")
	require.EqualError(t, PackError{Pack: 0, Err: errors.New("plain")}, "tracepack: pack 0: plain")
	require.ErrorIs(t, TxPackError{Err: cause}, ErrNotFinalized)

	require.EqualError(t, &entryConflictError{n: 2}, "tracepack: merge: 2 conflicts of redaction entries: merge found conflicting records")
}
