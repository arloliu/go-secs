// Package hsms — in-package test for DataMessage.decode's copy-fallback branch.
//
// The fallback (data_msg.go, inside decode()) only runs when msg.body is NOT a raw-frame body —
// normally a treeBody, whose sync.Once is always pre-fired by NewDataMessage before any caller can
// observe it, and which always encodes exactly one item (so it can never itself carry trailing
// bytes). Reaching the fallback with a multi-item payload therefore requires a whitebox Body stand-in
// that (a) is not wire's rawFrameBody (so wire.OwnedBytes returns false, taking the fallback branch)
// and (b) can hold arbitrary bytes.
// This file builds a *DataMessage by hand with such a Body and an unfired decodeState
// to reach that branch directly.
package hsms

import (
	"net"
	"testing"

	"github.com/arloliu/go-secs/v2/internal/wire"
	"github.com/arloliu/go-secs/v2/secs2"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// arbitraryBody is a minimal wire.Body over an arbitrary byte slice.
// Unlike rawFrameBody it is not recognized by wire.OwnedBytes,
// so DataMessage.decode takes the copy-fallback branch for it.
// Unlike treeBody it is not tied to a single secs2.Item,
// so it can carry more than one item's worth of bytes — the shape the fallback test needs.
type arbitraryBody struct{ b []byte }

func (a arbitraryBody) Len() int                    { return len(a.b) }
func (a arbitraryBody) AppendTo(dst []byte) []byte  { return append(dst, a.b...) }
func (a arbitraryBody) Buffers() net.Buffers        { return net.Buffers{a.b} }
func (a arbitraryBody) Chunk(off, n int) wire.Chunk { return wire.ChunkOf(a.b[off : off+n]) }

var _ wire.Body = arbitraryBody{}

// TestDataMessage_decode_CopyFallback_TrailingBytes proves the copy-fallback branch
// (data_msg.go's decode(), the `else` of the wire.OwnedBytes check) counts trailing bytes the same
// way the raw-frame branch does: routed through the same secs2.DecodeOwnedFrame entry point via
// framecodec.AdoptSECS2Body(msg.body.AppendTo(nil)), so there is exactly one counting
// implementation for both body shapes.
func TestDataMessage_decode_CopyFallback_TrailingBytes(t *testing.T) {
	item1 := secs2.NewASCIIItem("hi")
	item2 := secs2.NewUintItem(1, 1, 2, 3)
	body := append(item1.ToBytes(), item2.ToBytes()...)

	msg := &DataMessage{
		header: [10]byte{0, 1, 1, 1, 0, 0, 0, 0, 0, 1},
		body:   arbitraryBody{b: body},
		dec:    &decodeState{},
	}

	// Confirm the fallback branch is actually the one exercised: wire.OwnedBytes must reject this
	// Body, or the test would be silently covering the raw-frame branch instead.
	_, ok := wire.OwnedBytes(msg.body)
	require.False(t, ok, "arbitraryBody must NOT be recognized by wire.OwnedBytes")

	got, decErr := msg.Item()
	require.NoError(t, decErr)
	assert.Equal(t, item1.ToBytes(), got.ToBytes(), "the copy-fallback path must decode only the first item")
	assert.Nil(t, msg.DecodeErr())
	assert.Equal(t, len(item2.ToBytes()), msg.TrailingBytes(), "the copy-fallback path must count trailing bytes the same as the raw-frame path")
}
