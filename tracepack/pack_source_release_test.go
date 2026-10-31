package tracepack

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"runtime"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// releaseReaderOptions keep a Reader's retained bytes to the head and tail windows, raised to the least Open allows,
// so that every block of a test pack lies outside them and a read of it is a ReadAt after Open.
var releaseReaderOptions = ReaderOptions{WholeReadThreshold: 1, HeadWindow: 1, TailWindow: 1}

// errSealed fails a read of a releaseObject after it was sealed.
var errSealed = errors.New("read of an object after its hour was released")

// releaseSource is a PackSource over packs held in memory that checks when a lookup reads them,
// as a caller closing each hour's objects would.
// Each Readers call first seals every object opened before it, the objects of the hours read before,
// then opens a fresh Reader, with releaseReaderOptions, over a new releaseObject for each pack of its hour;
// sealAll, called once the lookup returned, seals the rest.
// A read started on a sealed object, and a read in flight on an object when it is sealed, are recorded on the object,
// never failing the test from the reading goroutine.
type releaseSource struct {
	files map[int64][][]byte
	// onRead, when set, runs on the reading goroutine at the start of every read after Open of an object not sealed,
	// with the read's offset; an error it returns fails the read.
	onRead func(o *releaseObject, off int64) error

	mu   sync.Mutex
	objs []*releaseObject
	// hours lists the hour of each Readers call, in order.
	hours []int64
}

// releaseObject is the object of one pack a releaseSource opened for one Readers call.
type releaseObject struct {
	src  *releaseSource
	hour int64
	r    *bytes.Reader
	// opened reports that Open returned, so every later read is the lookup's.
	opened atomic.Bool
	sealed atomic.Bool
	// inflight counts the reads running now.
	inflight atomic.Int64
	// lookupReads counts the reads after Open; late, those started once sealed;
	// inflightAtSeal, the reads in flight when it was sealed.
	lookupReads, late, inflightAtSeal atomic.Int64
}

var (
	_ PackSource  = (*releaseSource)(nil)
	_ io.ReaderAt = (*releaseObject)(nil)
)

// newReleaseSource returns a releaseSource giving files[i] for the hour memTestHour + i.
func newReleaseSource(files ...[][]byte) *releaseSource {
	s := &releaseSource{files: map[int64][][]byte{}}
	for i, fs := range files {
		s.files[memTestHour+int64(i)] = fs
	}

	return s
}

// releaseTestPacks returns the packs of three hours, each of several blocks,
// every block a ReadAt after Open under releaseReaderOptions:
// hour 0 holds the primaries 10 to 13 and the reply 14,
// hour 1 the reply 20 and the primary 21, and hour 2 the primary 30 and the reply 31.
func releaseTestPacks(t testing.TB) [][][]byte {
	t.Helper()

	steps := func(blocks ...[]footerTestStep) []footerTestStep {
		var out []footerTestStep
		for _, b := range blocks {
			out = append(out, b...)
		}

		return out
	}

	return [][][]byte{
		{txPack(t, seg0, nil, steps(txBlock(txSeqs(10, 11)...), txBlock(txSeqs(12, 13)...), txBlock(txReply(14, nil))))},
		{txPackIn(t, seg1, 1, nil, steps(txBlock(txReply(20, nil)), txBlock(txRecord(21, nil))))},
		{txPackIn(t, seg2, 2, nil, steps(txBlock(txRecord(30, nil)), txBlock(txReply(31, nil))))},
	}
}

// inPrefetch reports whether the calling goroutine is one MergeIterate's prefetch started to read a block ahead of its demand.
func inPrefetch() bool {
	pcs := make([]uintptr, 64)
	frames := runtime.CallersFrames(pcs[:runtime.Callers(2, pcs)])
	for {
		f, more := frames.Next()
		if strings.Contains(f.Function, ".(*prefetcher).issue") {
			return true
		}
		if !more {
			return false
		}
	}
}

func (s *releaseSource) Readers(ctx context.Context, hour int64) ([]*Reader, error) {
	s.mu.Lock()
	for _, o := range s.objs {
		o.seal()
	}
	s.hours = append(s.hours, hour)
	files := s.files[hour]
	s.mu.Unlock()

	out := make([]*Reader, len(files))
	for i, f := range files {
		o := &releaseObject{src: s, hour: hour, r: bytes.NewReader(f)}
		r, err := Open(ctx, o, int64(len(f)), releaseReaderOptions)
		if err != nil {
			return nil, err
		}
		o.opened.Store(true)
		s.mu.Lock()
		s.objs = append(s.objs, o)
		s.mu.Unlock()
		out[i] = r
	}

	return out, nil
}

// sealAll seals every object, as a caller closing them all once the lookup returned does.
func (s *releaseSource) sealAll() {
	s.mu.Lock()
	defer s.mu.Unlock()

	for _, o := range s.objs {
		o.seal()
	}
}

// requireReleased requires that no read of an object was in flight when it was sealed, nor started after;
// and, when everyRead is set, that the lookup read every object after Open, else at least one.
func (s *releaseSource) requireReleased(t *testing.T, everyRead bool) {
	t.Helper()

	s.mu.Lock()
	defer s.mu.Unlock()

	require.NotEmpty(t, s.objs)
	read := 0
	for i, o := range s.objs {
		require.True(t, o.sealed.Load(), "object %d, hour %d: sealed", i, o.hour-memTestHour)
		assert.Zero(t, o.inflightAtSeal.Load(), "object %d, hour %d: no read in flight at its hour's release", i, o.hour-memTestHour)
		assert.Zero(t, o.late.Load(), "object %d, hour %d: no read after its hour's release", i, o.hour-memTestHour)
		if o.lookupReads.Load() > 0 {
			read++
		} else if everyRead {
			t.Errorf("object %d, hour %d: read after Open, not from retained bytes", i, o.hour-memTestHour)
		}
	}
	require.Positive(t, read, "some block read after Open")
}

// ReadAt reads from the pack, counting the read and checking it against the object's release.
// The count of reads in flight is taken before sealed is read, and seal stores sealed before it reads the count,
// so a read either sees the object sealed or is counted in flight by seal.
func (o *releaseObject) ReadAt(p []byte, off int64) (int, error) {
	o.inflight.Add(1)
	defer o.inflight.Add(-1)

	if o.sealed.Load() {
		o.late.Add(1)
		return 0, errSealed
	}
	if o.opened.Load() {
		o.lookupReads.Add(1)
		if o.src.onRead != nil {
			if err := o.src.onRead(o, off); err != nil {
				return 0, err
			}
		}
	}

	return o.r.ReadAt(p, off)
}

// seal marks the object released, recording the reads in flight on it then; sealing it again does nothing.
func (o *releaseObject) seal() {
	if o.sealed.Swap(true) {
		return
	}
	o.inflightAtSeal.Store(o.inflight.Load())
}

// blockSecondBlock sets s so that the lookup it serves is cancelled while a prefetch read is in flight that the lookup never waits for:
// the prefetch read of block 1 of the pack of hour, the only one, blocks until release is closed, closing entered once it blocks;
// the read of block 0 then waits for entered and calls cancel before it returns,
// so the lookup sees ctx done at the load of block 1, before it would wait for that block's read.
func (s *releaseSource) blockSecondBlock(t testing.TB, hour int64, cancel context.CancelFunc) (entered, release chan struct{}) {
	t.Helper()

	files := s.files[hour]
	require.Len(t, files, 1)
	blocks := mustOpen(t, files[0], ReaderOptions{}).blocks
	require.GreaterOrEqual(t, len(blocks), 2)
	first, second := int64(blocks[0].Offset), int64(blocks[1].Offset)

	entered, release = make(chan struct{}), make(chan struct{})
	var once sync.Once
	s.onRead = func(o *releaseObject, off int64) error {
		if o.hour != hour {
			return nil
		}
		switch {
		case off == second && inPrefetch():
			once.Do(func() { close(entered) })
			<-release
		case off == first:
			select {
			case <-entered:
			case <-time.After(txParWatchdog):
				return errors.New("the read of block 1 did not start")
			}
			cancel()
		default:
		}

		return nil
	}

	return entered, release
}

// runReleaseLookup runs lookup, seals every object of s once it returned, as a caller closing them then would,
// and sends its error on done.
// Its name marks the goroutine running the lookup in a stack dump (awaitPrefetchJoin).
func runReleaseLookup(s *releaseSource, lookup func() error, done chan<- error) {
	err := lookup()
	s.sealAll()
	done <- err
}

// awaitPrefetchJoin waits until the goroutine running runReleaseLookup waits in a prefetch's close for its reads in flight,
// or until the lookup returned, which it then reports with the lookup's error.
func awaitPrefetchJoin(t *testing.T, done <-chan error) (returned bool, err error) {
	t.Helper()

	buf := make([]byte, 1<<20)
	require.Eventually(t, func() bool {
		select {
		case err = <-done:
			returned = true
			return true
		default:
		}
		n := runtime.Stack(buf, true)
		for n == len(buf) {
			buf = make([]byte, 2*len(buf))
			n = runtime.Stack(buf, true)
		}
		for g := range strings.SplitSeq(string(buf[:n]), "\n\n") {
			if strings.Contains(g, ".runReleaseLookup(") && strings.Contains(g, ".(*prefetcher).close(") {
				return true
			}
		}

		return false
	}, txParWatchdog, time.Millisecond, "the lookup waits for its prefetch reads, or returns")

	return returned, err
}

// The test packs hold no block the Reader retained: a read over them issues ReadAt calls after Open,
// so the release tests see the reads a lookup issues.
func TestReleaseSourceReadsAfterOpen(t *testing.T) {
	t.Parallel()

	s := newReleaseSource(releaseTestPacks(t)...)
	readers, err := s.Readers(t.Context(), memTestHour)
	require.NoError(t, err)
	_, err = MergeIterate(t.Context(), readers, Query{Payloads: true}, MergeIterateOptions{Order: OrderCapture}, func(*Item) error { return nil })
	require.NoError(t, err)
	s.sealAll()
	s.requireReleased(t, true)
}

// FindTransaction reads each hour's packs only during that hour's read:
// at each later Readers call and at its return no read of an earlier hour's objects is in flight, and none starts afterwards,
// whether it succeeds, fails reading a block, or is cancelled while a prefetch read of an hour is blocked.
func TestFindTransactionReleasesHours(t *testing.T) {
	t.Parallel()

	key, opts := txKeyAt(12), TxOptions{MaxHours: 3}

	t.Run("success", func(t *testing.T) {
		t.Parallel()

		s := newReleaseSource(releaseTestPacks(t)...)
		res, err := FindTransaction(t.Context(), s, key, opts)
		s.sealAll()
		require.NoError(t, err)
		assert.Len(t, res.Searched, 3)
		assert.Equal(t, []int64{memTestHour, memTestHour + 1, memTestHour + 2}, s.hours)
		s.requireReleased(t, true)
	})

	t.Run("read error", func(t *testing.T) {
		t.Parallel()

		s := newReleaseSource(releaseTestPacks(t)...)
		s.onRead = func(o *releaseObject, _ int64) error {
			if o.hour == memTestHour+1 {
				return errInjected
			}

			return nil
		}
		_, err := FindTransaction(t.Context(), s, key, opts)
		s.sealAll()
		require.ErrorIs(t, err, errInjected)
		assert.Equal(t, []int64{memTestHour, memTestHour + 1}, s.hours)
		s.requireReleased(t, false)
	})

	t.Run("cancelled with a prefetch read blocked", func(t *testing.T) {
		t.Parallel()

		s := newReleaseSource(releaseTestPacks(t)...)
		ctx, cancel := context.WithCancel(t.Context())
		defer cancel()
		entered, release := s.blockSecondBlock(t, memTestHour+1, cancel)
		done := make(chan error, 1)
		go runReleaseLookup(s, func() error {
			_, err := FindTransaction(ctx, s, key, opts)
			return err
		}, done)
		requireCancelledAfterRead(t, entered, release, done)
		assert.Equal(t, []int64{memTestHour, memTestHour + 1}, s.hours)
		s.requireReleased(t, false)
	})
}

// FindTransactions reads each hour's packs once, during that hour's read, and replays it to the lookups of its primaries:
// at each later Readers call and at its return no read of an earlier hour's objects is in flight, and none starts afterwards,
// whether it succeeds with the lookups on several workers, its callback fails, a lookup fails on a worker,
// or it is cancelled while a prefetch read of an hour is blocked.
func TestFindTransactionsReleasesHours(t *testing.T) {
	t.Parallel()

	r := TxRange{CaptureID: captureLow, From: txBatchHour(0).From, To: txBatchHour(1).To}
	opts := TxRangeOptions{Lookup: TxOptions{MaxHours: 2}}
	all := []int64{memTestHour, memTestHour + 1, memTestHour + 2}

	for _, workers := range txWorkerCounts {
		t.Run(fmt.Sprintf("success on %d workers", workers), func(t *testing.T) {
			t.Parallel()

			s := newReleaseSource(releaseTestPacks(t)...)
			var keys []TxKey
			res, err := findTransactionsWith(t.Context(), s, r, opts, func(p *TxPairing) error {
				keys = append(keys, p.Key)
				return nil
			}, txWorkers(workers))
			s.sealAll()
			require.NoError(t, err, "workers %d", workers)
			assert.Equal(t, TxRangeResult{HoursRead: 3, Primaries: 5}, res, "workers %d", workers)
			assert.Equal(t, []TxKey{txKeyAt(10), txKeyAt(11), txKeyAt(12), txKeyAt(13), {CaptureID: captureLow, Seq: 21, Hour: memTestHour + 1}},
				keys, "workers %d", workers)
			assert.Equal(t, all, s.hours, "workers %d", workers)
			s.requireReleased(t, true)
		})
	}

	t.Run("callback error", func(t *testing.T) {
		t.Parallel()

		s := newReleaseSource(releaseTestPacks(t)...)
		errStop := errors.New("stop")
		_, err := findTransactionsWith(t.Context(), s, r, opts, func(*TxPairing) error { return errStop }, txWorkers(4))
		s.sealAll()
		require.ErrorIs(t, err, errStop)
		assert.Equal(t, all[:2], s.hours)
		s.requireReleased(t, true)
	})

	t.Run("worker error", func(t *testing.T) {
		t.Parallel()

		s := newReleaseSource(releaseTestPacks(t)...)
		failing := opts
		failing.Lookup.MaxStateBytes = 1
		_, err := findTransactionsWith(t.Context(), s, r, failing, func(*TxPairing) error { return nil }, txWorkers(4))
		s.sealAll()
		require.ErrorIs(t, err, ErrReadLimit)
		assert.Contains(t, err.Error(), "primary seq 10 of hour", "the first lookup's error, from a worker")
		assert.Equal(t, all[:2], s.hours)
		s.requireReleased(t, true)
	})

	t.Run("cancelled with a prefetch read blocked", func(t *testing.T) {
		t.Parallel()

		s := newReleaseSource(releaseTestPacks(t)...)
		ctx, cancel := context.WithCancel(t.Context())
		defer cancel()
		entered, release := s.blockSecondBlock(t, memTestHour+1, cancel)
		done := make(chan error, 1)
		go runReleaseLookup(s, func() error {
			_, err := findTransactionsWith(ctx, s, r, opts, func(*TxPairing) error { return nil }, txWorkers(4))
			return err
		}, done)
		requireCancelledAfterRead(t, entered, release, done)
		assert.Equal(t, all[:2], s.hours)
		s.requireReleased(t, false)
	})
}

// requireCancelledAfterRead waits for the read blockSecondBlock blocks, as entered reports,
// then for the lookup, which reports its error on done, to wait for that read in its prefetch's close,
// and requires that the lookup does not return before release lets the read end, then that it returns ctx's error.
func requireCancelledAfterRead(t *testing.T, entered, release chan struct{}, done <-chan error) {
	t.Helper()

	released := false
	defer func() {
		if !released { // a failed check leaves no read blocked behind it
			close(release)
		}
	}()
	recv(t, entered, "the blocked prefetch read")
	returned, err := awaitPrefetchJoin(t, done)
	require.False(t, returned, "the lookup returned while a read was in flight: %v", err)
	close(release)
	released = true
	require.ErrorIs(t, recv(t, done, "the lookup"), context.Canceled)
}
