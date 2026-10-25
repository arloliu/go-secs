package tracepack

import (
	"bytes"
	"context"
	"errors"
	"maps"
	"sync"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// memSource is a PackSource over packs held in memory, for the lookup tests:
// the packs given for each UTC hour, of any capture, in the order added.
// Every Readers call opens a fresh Reader for each pack of the hour,
// so a test can change the source between the calls a lookup makes.
//
// Its helpers lock mu, so beforeReaders, which runs without it held, can change the source.
type memSource struct {
	mu    sync.Mutex
	hours map[int64][][]byte
	// beforeReaders, when set, runs before Readers answers; an error it returns is returned instead of the answer.
	beforeReaders func(ctx context.Context, hour int64) error
	// calls counts the calls of Readers by hour.
	calls map[int64]int
}

var _ PackSource = (*memSource)(nil)

// newMemSource returns an empty memSource.
func newMemSource() *memSource {
	return &memSource{hours: map[int64][][]byte{}, calls: map[int64]int{}}
}

// addPack gives file for hour, after the packs given for it before.
func (s *memSource) addPack(t testing.TB, hour int64, file []byte) {
	t.Helper()

	s.mu.Lock()
	defer s.mu.Unlock()
	s.hours[hour] = append(s.hours[hour], bytes.Clone(file))
}

// counts returns the calls of Readers so far, by hour.
func (s *memSource) counts() map[int64]int {
	s.mu.Lock()
	defer s.mu.Unlock()

	return maps.Clone(s.calls)
}

// Readers opens a fresh Reader for each pack given for hour, in the order given.
func (s *memSource) Readers(ctx context.Context, hour int64) ([]*Reader, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	s.mu.Lock()
	s.calls[hour]++
	hook := s.beforeReaders
	s.mu.Unlock()
	if hook != nil {
		if err := hook(ctx, hour); err != nil {
			return nil, err
		}
	}

	return s.open(ctx, hour)
}

// memView is a memSource read without counting its calls or running its hook,
// so a check beside a test does not change what the test counts.
type memView struct {
	s *memSource
}

// view returns s read without counting or hook.
func (s *memSource) view() PackSource {
	return memView{s: s}
}

func (v memView) Readers(ctx context.Context, hour int64) ([]*Reader, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}

	return v.s.open(ctx, hour)
}

// open opens a fresh Reader for each pack given for hour, in the order given.
func (s *memSource) open(ctx context.Context, hour int64) ([]*Reader, error) {
	s.mu.Lock()
	files := s.hours[hour]
	s.mu.Unlock()

	out := make([]*Reader, len(files))
	for i, f := range files {
		r, err := Open(ctx, bytes.NewReader(f), int64(len(f)), ReaderOptions{})
		if err != nil {
			return nil, err
		}
		out[i] = r
	}

	return out, nil
}

// memTestHour is the UTC hour of blockTestHour, the hour of the test packs' records.
var memTestHour = hourOf(blockTestHour)

// packIDs returns the pack_id of each reader, in order.
func packIDs(readers []*Reader) []UUID {
	out := make([]UUID, len(readers))
	for i, r := range readers {
		out[i] = UUID(r.hdr.PackID)
	}

	return out
}

// TestMemSourceReaders gives packs of two captures in one hour and one in the next:
// Readers opens fresh Readers of an hour's packs in the order given, of any capture,
// none for an hour without packs, counts its calls by hour, and returns the hook's error and ctx's error.
func TestMemSourceReaders(t *testing.T) {
	t.Parallel()

	s := newMemSource()
	s.addPack(t, memTestHour, planTestPack(t, seg1, captureLow, false, blockAt(0, 1, 2)))
	s.addPack(t, memTestHour, planTestPack(t, seg0, captureHigh, false, blockAt(0, 1, 2)))
	s.addPack(t, memTestHour+1, txPackIn(t, seg2, 1, nil, txBlock(txSeqs(3)...)))

	first, err := s.Readers(t.Context(), memTestHour)
	require.NoError(t, err)
	assert.Equal(t, []UUID{seg1, seg0}, packIDs(first))
	again, err := s.Readers(t.Context(), memTestHour)
	require.NoError(t, err)
	assert.NotSame(t, first[0], again[0], "a fresh Reader each call")
	next, err := s.Readers(t.Context(), memTestHour+1)
	require.NoError(t, err)
	assert.Equal(t, []UUID{seg2}, packIDs(next))
	none, err := s.Readers(t.Context(), memTestHour+2)
	require.NoError(t, err)
	assert.Empty(t, none)
	assert.Equal(t, map[int64]int{memTestHour: 2, memTestHour + 1: 1, memTestHour + 2: 1}, s.counts())

	boom := errors.New("boom")
	s.beforeReaders = func(context.Context, int64) error { return boom }
	_, err = s.Readers(t.Context(), memTestHour)
	require.ErrorIs(t, err, boom)

	ctx, cancel := context.WithCancel(t.Context())
	cancel()
	_, err = s.Readers(ctx, memTestHour)
	require.ErrorIs(t, err, context.Canceled)
}
