package tracepack

import (
	"bytes"
	"errors"
	"fmt"
	"io"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/arloliu/go-secs/tracepack/internal/format"
)

// errTestOutput is the failure of the output and sync test doubles.
var errTestOutput = errors.New("test output failure")

// failingOutput takes the bytes of every write into buf until limit bytes are written,
// then the bytes up to the limit and errTestOutput; a negative limit never fails.
type failingOutput struct {
	buf   bytes.Buffer
	limit int
}

func (o *failingOutput) Write(p []byte) (int, error) {
	if o.limit < 0 || o.buf.Len()+len(p) <= o.limit {
		return o.buf.Write(p)
	}
	n, _ := o.buf.Write(p[:o.limit-o.buf.Len()])

	return n, errTestOutput
}

// failingSyncer fails its sync numbered fail, counted from 1; 0 never fails.
type failingSyncer struct {
	calls, fail int
}

func (s *failingSyncer) Sync() error {
	s.calls++
	if s.calls == s.fail {
		return errTestOutput
	}

	return nil
}

// startNext starts the pack that continues the capture after a pack whose Close gave cont, as a capture writer does:
// at the seq Next reports, as the assigned seqs' start or as Meta.SeqStart.
// It refuses, writing nothing, when Next reports no continuation.
func startNext(out io.Writer, cont Continuation, meta *PackMeta, assignSeq bool) (*Writer, error) {
	next, ok := cont.Next()
	if !ok {
		return nil, fmt.Errorf("tracepack: the capture has no seq left: %w", ErrSeqOrder)
	}

	m := *meta
	opts := WriterOptions{Meta: &m, AssignSeq: assignSeq}
	if assignSeq {
		opts.NextSeq = next
	} else {
		m.SeqStart = next
	}

	return NewWriter(out, opts)
}

// lastSeqPack writes a pack whose one record has seq 2^63-1, the last a capture can hold,
// with the seq given or assigned, and returns its bytes and the result of its Close.
func lastSeqPack(t *testing.T, assignSeq bool) ([]byte, Continuation) {
	t.Helper()

	m := repairTestMeta()
	m.SeqStart = format.MaxU64
	var buf bytes.Buffer
	w, err := NewWriter(&buf, WriterOptions{Meta: m, AssignSeq: assignSeq, NextSeq: format.MaxU64})
	require.NoError(t, err)
	r := frameRecord(format.MaxU64, hsmsFrame(1, 1, nil))
	r.TSUTCNs = blockTestHour
	require.NoError(t, w.Append(&r))
	cont, err := w.Close()
	require.NoError(t, err, "a pack whose last record has the last seq is finalized")

	return buf.Bytes(), cont
}

// A pack whose last record has seq 2^63-1 closes successfully and reports no continuation,
// a result distinct from an empty pack at seq 0 and from a failure (the tracepack storage specification §4):
// no seq follows the last one, and a capture never restarts at seq 0.
func TestWriterTerminalClose(t *testing.T) {
	t.Parallel()

	for _, assignSeq := range []bool{false, true} {
		t.Run(fmt.Sprintf("AssignSeq %t", assignSeq), func(t *testing.T) {
			t.Parallel()

			file, cont := lastSeqPack(t, assignSeq)
			seq, ok := cont.Next()
			assert.False(t, ok, "no continuation")
			assert.Zero(t, seq)
			r := mustOpen(t, file, ReaderOptions{})
			require.NoError(t, r.Header().FooterErr, "the pack is finalized")
			run := iterate(t, r, Query{})
			assert.Equal(t, []uint64{format.MaxU64}, run.seqs())

			var out bytes.Buffer
			for _, assign := range []bool{false, true} {
				w, err := startNext(&out, cont, repairTestMeta(), assign)
				require.ErrorIs(t, err, ErrSeqOrder, "AssignSeq %t: no next pack", assign)
				assert.Nil(t, w)
			}
			assert.Zero(t, out.Len(), "nothing written")

			// A caller that ignores ok and starts from the seq after the last is refused too.
			for _, opts := range []WriterOptions{
				{Meta: func() *PackMeta { m := repairTestMeta(); m.SeqStart = format.MaxU64 + 1; return m }()},
				{Meta: repairTestMeta(), AssignSeq: true, NextSeq: format.MaxU64 + 1},
			} {
				_, err := NewWriter(&out, opts)
				require.ErrorIs(t, err, ErrSeqOrder)
			}
			assert.Zero(t, out.Len(), "nothing written")
		})
	}

	t.Run("an empty pack at seq 0", func(t *testing.T) {
		t.Parallel()

		var buf bytes.Buffer
		w, err := NewWriter(&buf, WriterOptions{Meta: repairTestMeta()})
		require.NoError(t, err)
		cont, err := w.Close()
		require.NoError(t, err)
		seq, ok := cont.Next()
		assert.True(t, ok, "an empty pack continues at its seq_start")
		assert.Zero(t, seq)
		require.NoError(t, mustOpen(t, buf.Bytes(), ReaderOptions{}).Header().FooterErr)

		var out bytes.Buffer
		next, err := startNext(&out, cont, repairTestMeta(), false)
		require.NoError(t, err)
		r := frameRecord(0, hsmsFrame(1, 1, nil))
		require.NoError(t, next.Append(&r))
		cont, err = next.Close()
		require.NoError(t, err)
		seq, ok = cont.Next()
		assert.Equal(t, [2]any{uint64(1), true}, [2]any{seq, ok})
	})

	t.Run("a failed footer write or sync", func(t *testing.T) {
		t.Parallel()

		for _, fail := range []struct {
			name  string
			limit int
			sync  int
		}{{"footer write", 0, 0}, {"trailer sync", -1, 1}} {
			out := &failingOutput{limit: -1}
			syncer := &failingSyncer{fail: fail.sync}
			w, err := NewWriter(out, WriterOptions{Meta: repairTestMeta(), Sync: syncer})
			require.NoError(t, err)
			if fail.limit >= 0 {
				out.limit = out.buf.Len() + fail.limit
			}
			cont, err := w.Close()
			require.ErrorIs(t, err, errTestOutput, fail.name)
			_, ok := cont.Next()
			assert.False(t, ok, "%s: no continuation with an error", fail.name)
			assert.Equal(t, Continuation{}, cont, "%s: the zero Continuation", fail.name)
		}
	})
}

// A SegmentWriter whose capture reaches the last seq finalizes the segment holding it,
// and then refuses every record, opening no segment, without resetting its seq counter.
func TestSegmentWriterLastSeq(t *testing.T) {
	t.Parallel()

	w, sink := newSeg(t, nil)
	require.NoError(t, w.Rotate(t.Context()))
	w.nextSeq = format.MaxU64
	last := segMonoRecord(time.Hour, time.Hour)
	require.NoError(t, w.Append(t.Context(), &last))
	require.NoError(t, w.Rotate(t.Context()), "the segment holding the last seq is finalized")
	assert.Equal(t, format.MaxU64+1, w.nextSeq, "the counter stays past the last seq")

	creates := sink.calls[sinkCreate]
	next := segMonoRecord(time.Hour, time.Hour)
	require.ErrorIs(t, w.Append(t.Context(), &next), ErrSeqOrder)
	require.ErrorIs(t, w.AppendFrame(t.Context(), segAt(time.Hour), DirHostToEquipment, 1, blockTestFrame, 0), ErrSeqOrder)
	assert.Equal(t, creates, sink.calls[sinkCreate], "no segment opened")
	assert.Equal(t, format.MaxU64+1, w.nextSeq)
	assert.Equal(t, [][]uint64{{0}, {format.MaxU64}}, segSeqs(sink.records(t)), "no record at seq 0 again")
}
