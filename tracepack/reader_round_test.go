package tracepack

import (
	"bytes"
	"encoding/binary"
	"errors"
	"fmt"
	"sync"
	"sync/atomic"
	"testing"
	"testing/synctest"

	"github.com/arloliu/go-secs/tracepack/internal/format"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// roundReader serves a pack and gives the ReadAt calls at chosen offsets their own behavior.
// Any other offset is read normally.
type roundReader struct {
	data []byte
	acts map[int64]*roundAct
	// calls counts every ReadAt call, at any offset.
	calls atomic.Int64
}

// roundAct is the behavior of the ReadAt call at one offset; a test sets it up before Open runs.
type roundAct struct {
	// entered is closed when the call starts.
	entered chan struct{}
	// after, when not nil, is waited on before the call acts.
	after <-chan struct{}
	// err fails the call; panic, when not nil, makes it panic instead.
	err   error
	panic any
	// n is the length the call was asked for.
	n int
	// done is closed once the call has returned, normally or by a panic.
	done chan struct{}
	once sync.Once
}

func newRoundReader(data []byte) *roundReader {
	return &roundReader{data: data, acts: make(map[int64]*roundAct)}
}

// at returns the behavior of the call at off, creating it.
func (r *roundReader) at(off int64) *roundAct {
	a := r.acts[off]
	if a == nil {
		a = &roundAct{entered: make(chan struct{}), done: make(chan struct{})}
		r.acts[off] = a
	}

	return a
}

func (r *roundReader) ReadAt(p []byte, off int64) (int, error) {
	r.calls.Add(1)
	a := r.acts[off]
	if a == nil {
		return bytes.NewReader(r.data).ReadAt(p, off)
	}
	defer a.once.Do(func() { close(a.done) })
	a.n = len(p)
	close(a.entered)
	if a.after != nil {
		<-a.after
	}
	if a.panic != nil {
		panic(a.panic)
	}
	if a.err != nil {
		return 0, a.err
	}

	return bytes.NewReader(r.data).ReadAt(p, off)
}

// returned reports whether the call at off has returned.
func (r *roundReader) returned(off int64) bool {
	select {
	case <-r.acts[off].done:
		return true
	default:
		return false
	}
}

// roundPack returns an uncompressed pack whose bootstrap, under roundOptions, reads in two rounds of two requests each,
// and the offsets of those requests:
// the head and the suffix, then the rest of the pack metadata and the rest of the footer.
func roundPack(t *testing.T) (file []byte, first, second [2]int64) {
	t.Helper()

	file = writeReaderPack(t, readerPackConfig{codec: CodecNone}, hourRecords(3, 40)).file
	size := int64(len(file))
	footerAt := int64(binary.LittleEndian.Uint64(file[size-format.TrailerLen:]))
	require.Less(t, footerAt, size-format.TrailerLen, "the footer lies before the suffix read")

	return file, [2]int64{0, size - format.TrailerLen}, [2]int64{format.FileHeaderLen, footerAt}
}

// roundOptions makes Open read the 80-byte file header and the 64-byte trailer first,
// so the rest of the pack metadata and of the footer form a second round.
var roundOptions = ReaderOptions{WholeReadThreshold: 1, HeadWindow: 1, TailWindow: 1}

var (
	errLowRequest  = errors.New("the lower request's failure")
	errHighRequest = errors.New("the higher request's failure")
)

// A round returns the error of its lower request, whichever request fails first, once both calls have returned.
func TestReadRoundErrorOrder(t *testing.T) {
	t.Parallel()

	file, first, second := roundPack(t)
	for _, tt := range []struct {
		name string
		offs [2]int64
	}{{"first round", first}, {"second round", second}} {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()

			r := newRoundReader(file)
			low, high := r.at(tt.offs[0]), r.at(tt.offs[1])
			low.err, high.err = errLowRequest, errHighRequest
			// The lower request fails only once the higher one has failed.
			low.after = high.done

			_, err := Open(t.Context(), r, int64(len(file)), roundOptions)
			require.ErrorIs(t, err, errLowRequest)
			require.NotErrorIs(t, err, errHighRequest)
			assert.EqualError(t, err, fmt.Sprintf("tracepack: read %d bytes at offset %d: %v", low.n, tt.offs[0], errLowRequest))
			assert.True(t, r.returned(tt.offs[0]) && r.returned(tt.offs[1]), "both calls returned before Open")
		})
	}
}

// openRecovered runs Open on a goroutine of its own and sends the value it panicked with, or nil, once it returns.
func openRecovered(t *testing.T, r *roundReader, size int) <-chan any {
	t.Helper()

	out := make(chan any, 1)
	go func() {
		defer func() { out <- recover() }()
		_, _ = Open(t.Context(), r, int64(size), roundOptions)
	}()

	return out
}

// A ReadAt panic on one of a round's goroutines is raised on Open's caller, with its value, once every call of the round has returned.
func TestReadRoundPanicAfterJoin(t *testing.T) {
	t.Parallel()

	file, first, _ := roundPack(t)
	synctest.Test(t, func(t *testing.T) {
		r := newRoundReader(file)
		head, tail := r.at(first[0]), r.at(first[1])
		head.panic = "head read"
		release := make(chan struct{})
		tail.after = release

		got := openRecovered(t, r, len(file))
		synctest.Wait()
		select {
		case v := <-got:
			t.Fatalf("Open returned with %v while a call of its round was in flight", v)
		default:
		}
		close(release)
		assert.Equal(t, "head read", <-got)
		assert.True(t, r.returned(first[1]))
	})
}

// A panic outweighs an error in the same round, and the lower request's panic outweighs a higher one's, whichever comes first.
func TestReadRoundPanicPrecedence(t *testing.T) {
	t.Parallel()

	file, first, _ := roundPack(t)
	for _, tt := range []struct {
		name     string
		lowErr   error
		lowPanic any
		want     any
	}{
		{"lower error, higher panic", errLowRequest, nil, "higher panic"},
		{"two panics", nil, "lower panic", "lower panic"},
	} {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()

			synctest.Test(t, func(t *testing.T) {
				r := newRoundReader(file)
				low, high := r.at(first[0]), r.at(first[1])
				low.err, low.panic = tt.lowErr, tt.lowPanic
				high.panic = "higher panic"
				low.after = high.done

				assert.Equal(t, tt.want, <-openRecovered(t, r, len(file)))
				assert.True(t, r.returned(first[0]) && r.returned(first[1]))
			})
		})
	}
}
