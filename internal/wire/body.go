// Package wire provides the zero-copy message-body bridge used by the immutable HSMS
// message layer (task 2a) and the SECS-I / writev transports (tasks 2b / 2c).
//
// internal/wire is a true leaf: it imports only secs2, net, and sync from the standard
// library. It must never appear in an exported signature of any public package.
package wire

import (
	"net"
	"sync"

	"github.com/arloliu/go-secs/v2/secs2"
)

// Body is the zero-copy view of an HSMS message body.
// Implementations are immutable: no method exposes a writable []byte.
// Body values are safe for concurrent use after construction.
//
// The slices returned by [Body.Buffers] are an internal zero-copy bridge for writev
// transports (task 2c). Callers must not mutate them.
type Body interface {
	// Len returns the encoded byte length of the body.
	Len() int

	// AppendTo appends the body bytes into dst and returns the extended slice.
	AppendTo(dst []byte) []byte

	// Buffers returns the body as one or more sub-slices for writev use (task 2c).
	// The returned slices are shared and must not be mutated by callers.
	Buffers() net.Buffers

	// Chunk returns a sub-view of body[off : off+n] for SECS-I per-block use (task 2b).
	Chunk(off, n int) Chunk
}

// Chunk is a minimal sub-view of a [Body], returned by [Body.Chunk] for SECS-I
// per-block transmission. Full SECS-I semantics are added in task 2b.
type Chunk struct{ b []byte }

// Len returns the number of bytes in the chunk.
func (c Chunk) Len() int { return len(c.b) }

// AppendTo appends the chunk bytes into dst and returns the extended slice.
func (c Chunk) AppendTo(dst []byte) []byte { return append(dst, c.b...) }

// ChunkOf wraps an already-owned byte slice as a Chunk, zero-copy. Used by the SECS-I block
// parser to view a received block body as a sub-slice of the read buffer without copying. The
// caller owns b; ChunkOf neither copies it nor retains it beyond the returned Chunk.
func ChunkOf(b []byte) Chunk { return Chunk{b: b} }

// chunkView returns body[off : off+n] as a Chunk, panicking with a descriptive message on
// out-of-range indices. Callers (the SECS-I splitter) compute valid offsets; this guards a
// programming error, not untrusted input (received blocks are length-validated in parseBlock).
func chunkView(body []byte, off, n int) Chunk {
	if off < 0 || n < 0 || off > len(body) || n > len(body)-off {
		panic("wire: Chunk offset/length out of range")
	}
	return Chunk{b: body[off : off+n]}
}

// rawFrameBody wraps already-owned body bytes without copying.
// It uses value receivers because it has no sync.Once field.
type rawFrameBody struct{ body []byte }

// AdoptBody wraps already-owned body bytes zero-copy.
// The caller — typically the HSMS decode layer — owns the backing buffer;
// rawFrameBody neither copies nor frees it.
func AdoptBody(body []byte) Body { return rawFrameBody{body: body} }

// Len returns the byte length of the raw frame body.
func (r rawFrameBody) Len() int { return len(r.body) }

// AppendTo appends the raw body bytes into dst and returns the extended slice.
func (r rawFrameBody) AppendTo(dst []byte) []byte { return append(dst, r.body...) }

// Buffers returns the raw body as a single-element net.Buffers.
// The returned slice is shared and must not be mutated by callers.
func (r rawFrameBody) Buffers() net.Buffers { return net.Buffers{r.body} }

// Chunk returns a sub-view of raw body[off : off+n].
func (r rawFrameBody) Chunk(off, n int) Chunk { return chunkView(r.body, off, n) }

// OwnedBytes returns the retained backing slice of a body adopted via
// AdoptBody (a raw-frame body), and true. For any other Body implementation
// (e.g. a tree body) it returns nil, false. Used by the recv decode path to
// run secs2.DecodeOwned over the frame body with no extra copy.
func OwnedBytes(b Body) ([]byte, bool) {
	if r, ok := b.(rawFrameBody); ok {
		return r.body, true
	}

	return nil, false
}

// treeBody owns a constructed [secs2.Item]; the wire encoding is memoized once via
// [sync.Once] so that repeat AppendTo / Buffers / Chunk calls — e.g. SECS-I per-block
// re-emit (task 2b) or repeated ToBytes — encode the item tree exactly once.
//
// All methods use pointer receivers: copying a sync.Once field is undefined behaviour
// and is caught by go vet (copylocks).
type treeBody struct {
	item secs2.Item
	once sync.Once
	enc  []byte
}

// FromItem wraps a constructed [secs2.Item] as a [Body].
// It returns *treeBody (pointer) so that the embedded sync.Once is never copied
// when the Body interface value is stored or passed.
func FromItem(it secs2.Item) Body { return &treeBody{item: it} }

// encoded lazily encodes the item exactly once and returns the cached bytes.
func (t *treeBody) encoded() []byte {
	t.once.Do(func() {
		t.enc = t.item.AppendTo(make([]byte, 0, t.item.EncodedLen()))
	})

	return t.enc
}

// Len returns the wire byte length of the body without triggering encoding.
func (t *treeBody) Len() int { return t.item.EncodedLen() }

// AppendTo appends the memoized encoding into dst and returns the extended slice.
func (t *treeBody) AppendTo(dst []byte) []byte { return append(dst, t.encoded()...) }

// Buffers returns the memoized encoding as a single-element net.Buffers for writev use.
// The returned slice is shared and must not be mutated by callers.
func (t *treeBody) Buffers() net.Buffers { return net.Buffers{t.encoded()} }

// Chunk returns a sub-view of the memoized encoding for SECS-I blocking.
func (t *treeBody) Chunk(off, n int) Chunk { return chunkView(t.encoded(), off, n) }
