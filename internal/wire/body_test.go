package wire

import (
	"bytes"
	"errors"
	"iter"
	"sync"
	"testing"

	"github.com/arloliu/go-secs/v2/secs2"
	"github.com/stretchr/testify/require"
)

// mockItem is a minimal secs2.Item that counts AppendTo invocations to verify memoization.
type mockItem struct {
	mu          sync.Mutex
	appendCount int
	data        []byte
}

func newMockItem(data []byte) *mockItem {
	return &mockItem{data: data}
}

func (m *mockItem) EncodedLen() int { return len(m.data) }

func (m *mockItem) AppendTo(dst []byte) []byte {
	m.mu.Lock()
	m.appendCount++
	m.mu.Unlock()

	return append(dst, m.data...)
}

func (m *mockItem) ToBytes() []byte                       { return append([]byte{}, m.data...) }
func (m *mockItem) Type() string                          { return secs2.BinaryType }
func (m *mockItem) Size() int                             { return len(m.data) }
func (m *mockItem) Error() error                          { return nil }
func (m *mockItem) IsEmpty() bool                         { return false }
func (m *mockItem) IsList() bool                          { return false }
func (m *mockItem) IsBinary() bool                        { return true }
func (m *mockItem) IsBoolean() bool                       { return false }
func (m *mockItem) IsASCII() bool                         { return false }
func (m *mockItem) IsJIS8() bool                          { return false }
func (m *mockItem) IsLocalizedStr() bool                  { return false }
func (m *mockItem) IsInt8() bool                          { return false }
func (m *mockItem) IsInt16() bool                         { return false }
func (m *mockItem) IsInt32() bool                         { return false }
func (m *mockItem) IsInt64() bool                         { return false }
func (m *mockItem) IsUint8() bool                         { return false }
func (m *mockItem) IsUint16() bool                        { return false }
func (m *mockItem) IsUint32() bool                        { return false }
func (m *mockItem) IsUint64() bool                        { return false }
func (m *mockItem) IsFloat32() bool                       { return false }
func (m *mockItem) IsFloat64() bool                       { return false }
func (m *mockItem) Get(...int) (secs2.Item, error)        { return nil, errors.ErrUnsupported }
func (m *mockItem) ToList() ([]secs2.Item, error)         { return nil, errors.ErrUnsupported }
func (m *mockItem) ToBinary() ([]byte, error)             { return m.data, nil }
func (m *mockItem) ToBoolean() ([]bool, error)            { return nil, errors.ErrUnsupported }
func (m *mockItem) ToASCII() (string, error)              { return "", errors.ErrUnsupported }
func (m *mockItem) ToJIS8() (string, error)               { return "", errors.ErrUnsupported }
func (m *mockItem) ToLocalizedStr() (string, error)       { return "", errors.ErrUnsupported }
func (m *mockItem) ToLocalizedStrHeader() (uint16, error) { return 0, errors.ErrUnsupported }
func (m *mockItem) ToInt() ([]int64, error)               { return nil, errors.ErrUnsupported }
func (m *mockItem) ToUint() ([]uint64, error)             { return nil, errors.ErrUnsupported }
func (m *mockItem) ToFloat() ([]float64, error)           { return nil, errors.ErrUnsupported }
func (m *mockItem) ItemAt(int) (secs2.Item, error)        { return nil, errors.ErrUnsupported }
func (m *mockItem) ByteAt(int) (byte, error)              { return 0, errors.ErrUnsupported }
func (m *mockItem) BoolAt(int) (bool, error)              { return false, errors.ErrUnsupported }
func (m *mockItem) IntAt(int) (int64, error)              { return 0, errors.ErrUnsupported }
func (m *mockItem) UintAt(int) (uint64, error)            { return 0, errors.ErrUnsupported }
func (m *mockItem) FloatAt(int) (float64, error)          { return 0, errors.ErrUnsupported }
func (m *mockItem) Items() iter.Seq[secs2.Item]           { return func(func(secs2.Item) bool) {} }
func (m *mockItem) Bools() iter.Seq[bool]                 { return func(func(bool) bool) {} }
func (m *mockItem) Ints() iter.Seq[int64]                 { return func(func(int64) bool) {} }
func (m *mockItem) Uints() iter.Seq[uint64]               { return func(func(uint64) bool) {} }
func (m *mockItem) Floats() iter.Seq[float64]             { return func(func(float64) bool) {} }
func (m *mockItem) AppendBinaryTo(dst []byte) []byte      { return append(dst, m.data...) }
func (m *mockItem) ToSML() string                         { return "" }

// TestTreeBodyLen verifies Len() returns EncodedLen() without triggering item encoding.
func TestTreeBodyLen(t *testing.T) {
	data := []byte{0x01, 0x02, 0x03, 0x04}
	item := newMockItem(data)
	b := FromItem(item)

	if got, want := b.Len(), item.EncodedLen(); got != want {
		t.Errorf("Len() = %d, want %d", got, want)
	}

	item.mu.Lock()
	count := item.appendCount
	item.mu.Unlock()

	if count != 0 {
		t.Errorf("Len() triggered %d AppendTo calls, want 0 (no encoding)", count)
	}
}

// TestTreeBodyAppendTo verifies AppendTo returns the item's encoded bytes.
func TestTreeBodyAppendTo(t *testing.T) {
	data := []byte{0x01, 0x02, 0x03, 0x04}
	b := FromItem(newMockItem(data))

	got := b.AppendTo(nil)
	if !bytes.Equal(got, data) {
		t.Errorf("AppendTo(nil) = %v, want %v", got, data)
	}
}

// TestTreeBodyBuffers verifies Buffers() concatenates to the full encoded bytes.
func TestTreeBodyBuffers(t *testing.T) {
	data := []byte{0x01, 0x02, 0x03, 0x04}
	b := FromItem(newMockItem(data))

	bufs := b.Buffers()
	if len(bufs) == 0 {
		t.Fatal("Buffers() returned empty net.Buffers")
	}

	all := make([]byte, 0, b.Len())
	for _, buf := range bufs {
		all = append(all, buf...)
	}

	if !bytes.Equal(all, data) {
		t.Errorf("Buffers() concatenation = %v, want %v", all, data)
	}
}

// TestTreeBodyEncodeOnce verifies the underlying item is encoded exactly once
// under repeated AppendTo / Buffers / Chunk calls.
func TestTreeBodyEncodeOnce(t *testing.T) {
	data := []byte{0x01, 0x02, 0x03, 0x04}
	item := newMockItem(data)
	b := FromItem(item)

	for range 5 {
		_ = b.AppendTo(nil)
		_ = b.Buffers()
		_ = b.Chunk(0, len(data))
	}

	item.mu.Lock()
	count := item.appendCount
	item.mu.Unlock()

	if count != 1 {
		t.Errorf("item.AppendTo called %d times, want 1 (memoized)", count)
	}
}

// TestTreeBodyEncodeOnceRace verifies memoization is race-free under concurrent access.
func TestTreeBodyEncodeOnceRace(t *testing.T) {
	data := []byte{0x01, 0x02, 0x03, 0x04}
	item := newMockItem(data)
	b := FromItem(item)

	var wg sync.WaitGroup

	for range 16 {
		wg.Go(func() {
			_ = b.AppendTo(nil)
			_ = b.Buffers()
		})
	}

	wg.Wait()

	item.mu.Lock()
	count := item.appendCount
	item.mu.Unlock()

	if count != 1 {
		t.Errorf("item.AppendTo called %d times concurrently, want 1", count)
	}
}

// TestAdoptBodyLen verifies Len() == len(original slice).
func TestAdoptBodyLen(t *testing.T) {
	original := []byte{0x0A, 0x0B, 0x0C}
	body := AdoptBody(original)

	if got, want := body.Len(), len(original); got != want {
		t.Errorf("Len() = %d, want %d", got, want)
	}
}

// TestAdoptBodyAppendTo verifies AppendTo(nil) matches the original and returns a fresh copy.
func TestAdoptBodyAppendTo(t *testing.T) {
	original := []byte{0x0A, 0x0B, 0x0C}
	body := AdoptBody(original)

	got := body.AppendTo(nil)
	if !bytes.Equal(got, original) {
		t.Errorf("AppendTo(nil) = %v, want %v", got, original)
	}

	// Output must be a fresh copy — mutating it must not change the original.
	got[0] = 0xFF
	if original[0] == 0xFF {
		t.Error("AppendTo output aliases the original slice (expected fresh copy)")
	}
}

// TestAdoptBodyBuffersAlias verifies Buffers()[0] shares the original backing array (zero-copy).
func TestAdoptBodyBuffersAlias(t *testing.T) {
	original := []byte{0x0A, 0x0B, 0x0C}
	body := AdoptBody(original)

	bufs := body.Buffers()
	if len(bufs) == 0 || len(bufs[0]) == 0 {
		t.Fatal("Buffers() returned empty")
	}

	if &bufs[0][0] != &original[0] {
		t.Error("Buffers()[0] does not alias the original slice (zero-copy violated)")
	}
}

// TestChunk verifies Chunk returns a sub-view with the correct length and content.
func TestChunk(t *testing.T) {
	original := []byte{0x01, 0x02, 0x03, 0x04, 0x05}
	body := AdoptBody(original)

	ch := body.Chunk(1, 3)

	if got, want := ch.Len(), 3; got != want {
		t.Errorf("Chunk.Len() = %d, want %d", got, want)
	}

	got := ch.AppendTo(nil)
	if !bytes.Equal(got, original[1:4]) {
		t.Errorf("Chunk.AppendTo(nil) = %v, want %v", got, original[1:4])
	}
}

// TestAppendToZeroAllocs verifies AdoptBody.AppendTo into a pre-sized buffer allocates nothing.
func TestAppendToZeroAllocs(t *testing.T) {
	b := []byte{0x01, 0x02, 0x03, 0x04}
	body := AdoptBody(b)
	dst := make([]byte, 0, len(b))

	allocs := testing.AllocsPerRun(100, func() {
		dst = dst[:0]
		dst = body.AppendTo(dst)
	})

	if allocs != 0 {
		t.Errorf("rawFrameBody.AppendTo allocs = %.0f, want 0", allocs)
	}
}

// TestTreeBodyAppendToZeroAllocs verifies FromItem.AppendTo is alloc-free after memoization warmup.
func TestTreeBodyAppendToZeroAllocs(t *testing.T) {
	data := []byte{0x01, 0x02, 0x03, 0x04}
	b := FromItem(newMockItem(data))

	// Warm up: trigger the once.Do encoding allocation.
	_ = b.AppendTo(nil)

	dst := make([]byte, 0, len(data))

	allocs := testing.AllocsPerRun(100, func() {
		dst = dst[:0]
		dst = b.AppendTo(dst)
	})

	if allocs != 0 {
		t.Errorf("treeBody.AppendTo allocs = %.0f, want 0", allocs)
	}
}

func TestChunkOf_PointerAlias(t *testing.T) {
	t.Parallel()
	buf := make([]byte, 4)
	c := ChunkOf(buf)
	require.Equal(t, 4, c.Len())
	if &c.b[0] != &buf[0] { // white-box: ChunkOf must alias, not copy
		t.Fatal("ChunkOf must wrap the slice zero-copy")
	}
}

func TestBodyChunk_BoundsGuard(t *testing.T) {
	t.Parallel()
	raw := AdoptBody([]byte{1, 2, 3, 4, 5})
	require.Equal(t, []byte{2, 3}, raw.Chunk(1, 2).AppendTo(nil)) // in range
	assertChunkPanics(t, func() { raw.Chunk(3, 5) })              // off+n > len
	assertChunkPanics(t, func() { raw.Chunk(6, 0) })              // off > len
	assertChunkPanics(t, func() { raw.Chunk(-1, 2) })             // off < 0
	assertChunkPanics(t, func() { raw.Chunk(2, -1) })             // n < 0

	// Same descriptive guard on the tree-backed body (its Chunk routes through chunkView too).
	item, err := secs2.Decode([]byte{0x41, 0x02, 'H', 'I'}) // ASCII "HI", encoded length 4
	require.NoError(t, err)
	tree := FromItem(item)
	require.Equal(t, 4, tree.Len())
	assertChunkPanics(t, func() { tree.Chunk(2, 5) })
}

// assertChunkPanics asserts fn panics with the descriptive wire.Chunk guard message — proving the
// chunkView guard ran, not a bare runtime slice-bounds panic (which would not be a string
// containing "wire: Chunk").
func assertChunkPanics(t *testing.T, fn func()) {
	t.Helper()
	defer func() {
		r := recover()
		require.NotNil(t, r, "expected a panic")
		msg, ok := r.(string)
		require.True(t, ok, "panic value should be a string")
		require.Contains(t, msg, "wire: Chunk")
	}()
	fn()
}
