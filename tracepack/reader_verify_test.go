package tracepack

import (
	"context"
	"sync"
	"testing"

	"github.com/arloliu/go-secs/tracepack/internal/format"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// TestReaderVerifyMatchesVerify requires (*Reader).Verify to give the report Verify gives, on clean and damaged packs,
// and to leave the Reader usable: Iterate after it yields what it yields on a fresh Reader.
func TestReaderVerifyMatchesVerify(t *testing.T) {
	t.Parallel()

	// CodecNone keeps every defect's error text deterministic, so the reports compare whole.
	recs := hourRecords(3, 40)
	closed := writeReaderPack(t, readerPackConfig{codec: CodecNone}, recs)
	open := writeReaderPack(t, readerPackConfig{codec: CodecNone, open: true}, recs)
	l := layoutOf(t, closed.file)

	tests := []struct {
		name string
		file []byte
		want VerifyOutcome
	}{
		{name: "consistent", file: closed.file, want: VerifyOutcomeFinalizedConsistent},
		{name: "middle block body", file: flipByte(closed.file, closed.blocks[1].offset+format.EnvelopeLen+1), want: VerifyOutcomeCorruptMiddle},
		{name: "footer CRC, walked", file: flipByte(closed.file, l.tr.FooterOffset+1), want: VerifyOutcomeUnfinalized},
		{name: "unfinalized", file: open.file, want: VerifyOutcomeUnfinalized},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()

			want := mustVerify(t, tt.file)
			require.Equal(t, tt.want, want.Outcome)

			r := mustOpen(t, tt.file, ReaderOptions{})
			got, err := r.Verify(t.Context())
			require.NoError(t, err)
			assert.Equal(t, want, got)

			after, afterRes := iterateAll(t, r)
			fresh, freshRes := iterateAll(t, mustOpen(t, tt.file, ReaderOptions{}))
			assert.Equal(t, fresh, after)
			assert.Equal(t, freshRes, afterRes)
		})
	}
}

// TestReaderVerifyErrors requires (*Reader).Verify to return the errors Verify returns after its Open, under the Reader's options.
func TestReaderVerifyErrors(t *testing.T) {
	t.Parallel()

	p := writeReaderPack(t, readerPackConfig{codec: CodecZstd}, hourRecords(3, 40))
	l := layoutOf(t, p.file)

	t.Run("block over MaxBlockLen", func(t *testing.T) {
		t.Parallel()

		_, err := mustOpen(t, p.file, ReaderOptions{MaxBlockLen: 16}).Verify(t.Context())
		require.ErrorIs(t, err, ErrReadLimit)
	})

	t.Run("footer over MaxFooterLen", func(t *testing.T) {
		t.Parallel()

		_, err := mustOpen(t, p.file, ReaderOptions{MaxFooterLen: int64(l.tr.FooterLen) - 1}).Verify(t.Context())
		require.ErrorIs(t, err, ErrReadLimit)
	})

	t.Run("ctx done before", func(t *testing.T) {
		t.Parallel()

		ctx, cancel := context.WithCancel(t.Context())
		cancel()
		r := mustOpen(t, p.file, ReaderOptions{})
		_, err := r.Verify(ctx)
		require.ErrorIs(t, err, context.Canceled)

		rep, err := r.Verify(t.Context())
		require.NoError(t, err, "a cancelled Verify leaves the Reader usable")
		assert.Equal(t, VerifyOutcomeFinalizedConsistent, rep.Outcome)
	})
}

// TestReaderVerifyCancelDuring cancels ctx while (*Reader).Verify reads a block:
// it returns ctx's error before reading the next block, and the Reader stays usable.
func TestReaderVerifyCancelDuring(t *testing.T) {
	t.Parallel()

	p := writeReaderPack(t, readerPackConfig{codec: CodecZstd}, hourRecords(3, 40))
	rr := newRoundReader(p.file)
	r, err := Open(t.Context(), rr, int64(len(p.file)), uncachedWindows())
	require.NoError(t, err)

	first, second := rr.at(int64(p.blocks[0].offset)), rr.at(int64(p.blocks[1].offset))
	release := make(chan struct{})
	first.after = release
	ctx, cancel := context.WithCancel(t.Context())
	go func() {
		<-first.entered
		cancel()
		close(release)
	}()
	_, err = r.Verify(ctx)
	require.ErrorIs(t, err, context.Canceled)
	select {
	case <-second.entered:
		t.Fatal("the next block was read after ctx was done")
	default:
	}

	// The acts fire once; the next calls at those offsets read normally.
	delete(rr.acts, int64(p.blocks[0].offset))
	delete(rr.acts, int64(p.blocks[1].offset))
	rep, err := r.Verify(t.Context())
	require.NoError(t, err)
	assert.Equal(t, VerifyOutcomeFinalizedConsistent, rep.Outcome)
}

// TestReaderVerifyOneRead requires Open followed by (*Reader).Verify of a pack within WholeReadThreshold to make one ReadAt:
// the blocks are verified from the bytes Open kept.
func TestReaderVerifyOneRead(t *testing.T) {
	t.Parallel()

	p := writeReaderPack(t, readerPackConfig{codec: CodecZstd}, hourRecords(3, 40))
	require.Less(t, int64(len(p.file)), int64(DefaultWholeReadThreshold))

	rr := newRoundReader(p.file)
	r, err := Open(t.Context(), rr, int64(len(p.file)), ReaderOptions{})
	require.NoError(t, err)
	rep, err := r.Verify(t.Context())
	require.NoError(t, err)
	assert.Equal(t, VerifyOutcomeFinalizedConsistent, rep.Outcome)
	assert.Equal(t, int64(1), rr.calls.Load())
}

// TestReaderVerifyConcurrent runs Verify and Iterate on one Reader at once, for the race detector.
func TestReaderVerifyConcurrent(t *testing.T) {
	t.Parallel()

	p := writeReaderPack(t, readerPackConfig{codec: CodecZstd}, hourRecords(3, 40))
	r := mustOpen(t, p.file, ReaderOptions{WholeReadThreshold: 1})
	want := mustVerify(t, p.file)

	var wg sync.WaitGroup
	for range 4 {
		wg.Go(func() {
			got, err := r.Verify(t.Context())
			assert.NoError(t, err)
			assert.Equal(t, want, got)
		})
		wg.Go(func() {
			_, err := r.Iterate(t.Context(), Query{Payloads: true}, func(*Item) error { return nil })
			assert.NoError(t, err)
		})
	}
	wg.Wait()
}
