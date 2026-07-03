package hsms_test

import (
	"sync"
	"testing"

	"github.com/arloliu/go-secs/v2/hsms"
	"github.com/arloliu/go-secs/v2/secs2"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// ────────────────────────────────────────────────────────────────
// Interface compile-time check
// ────────────────────────────────────────────────────────────────

// TestDataMessage_ImplementsMessageInterface verifies at compile-time that
// *DataMessage satisfies the Message interface.
func TestDataMessage_ImplementsMessageInterface(t *testing.T) {
	var _ hsms.Message = (*hsms.DataMessage)(nil)
}

// TestMessage_NoErrorMethod verifies that the reserved always-nil Error() method
// was removed from the Message interface and both concrete types. Reject outcomes
// surface as *RejectError; decode errors via DataMessage.DecodeErr.
func TestMessage_NoErrorMethod(t *testing.T) {
	type errorer interface{ Error() error }

	dm, err := hsms.NewDataMessage(1, 1, true, 0, [4]byte{}, secs2.NewEmptyItem())
	require.NoError(t, err)

	var dmAny any = dm
	_, ok := dmAny.(errorer)
	assert.False(t, ok, "*DataMessage must not expose Error() error")

	var cmAny any = hsms.NewSelectReq(0x0001, [4]byte{})
	_, ok = cmAny.(errorer)
	assert.False(t, ok, "*ControlMessage must not expose Error() error")

	// The Message interface itself must not carry Error(): a Message value must
	// not be assertable to errorer purely by the interface method set.
	var msg hsms.Message = dm
	_, ok = msg.(errorer)
	assert.False(t, ok, "Message must not expose Error() error")
}

// ────────────────────────────────────────────────────────────────
// Construction + accessors
// ────────────────────────────────────────────────────────────────

func TestNewDataMessage_EmptyItem(t *testing.T) {
	msg, err := hsms.NewDataMessage(0, 1, true, 123, [4]byte{}, secs2.NewEmptyItem())

	require.NoError(t, err)
	assert.Equal(t, hsms.DataMsgType, msg.Type())
	assert.Equal(t, uint8(0), msg.Stream())
	assert.Equal(t, uint8(1), msg.Function())
	assert.True(t, msg.WaitBit())
	assert.Equal(t, uint16(123), msg.SessionID())
	assert.Equal(t, [4]byte{}, msg.SystemBytes())
	assert.Equal(t, 0, msg.BodyLen())
	assert.Nil(t, msg.DecodeErr())
}

// TestNewDataMessage_Vectors ports the three known encoding vectors from v1.
func TestNewDataMessage_Vectors(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name          string
		stream        uint8
		function      uint8
		replyExpected bool
		sessionID     uint16
		systemBytes   [4]byte
		item          secs2.Item
		wantBytes     []byte
	}{
		{
			name:          "S1F1_W_ASCII",
			stream:        1,
			function:      1,
			replyExpected: true,
			sessionID:     1,
			systemBytes:   [4]byte{0, 0, 0, 1},
			item:          secs2.A("text"),
			// length=16, session=[0,1], stream=1|0x80=0x81, func=1, PType=0, SType=0,
			// sysBytes=[0,0,0,1], body=A("text")=[0x41,4,'t','e','x','t']
			wantBytes: []byte{
				0, 0, 0, 16,
				0, 1, 0x81, 1, 0, 0, 0, 0, 0, 1,
				0x41, 4, 0x74, 0x65, 0x78, 0x74,
			},
		},
		{
			name:          "S64F128_BOOLEAN",
			stream:        64,
			function:      128,
			replyExpected: false,
			sessionID:     256,
			systemBytes:   [4]byte{0x12, 0x34, 0x56, 0x78},
			item:          secs2.BOOLEAN(true, false),
			// length=14, session=[1,0], stream=0x40 (no W), func=0x80,
			// PType=0, SType=0, sysBytes=[0x12,0x34,0x56,0x78],
			// body=BOOLEAN(T,F)=[0x25,2,1,0]
			wantBytes: []byte{
				0, 0, 0, 14,
				0x01, 0x00, 0x40, 0x80, 0, 0, 0x12, 0x34, 0x56, 0x78,
				0x25, 2, 1, 0,
			},
		},
		{
			name:          "S127F255_W_NestedList",
			stream:        127,
			function:      255,
			replyExpected: true,
			sessionID:     0xFFFF,
			systemBytes:   [4]byte{0xf1, 0xf2, 0xf3, 0xf4},
			item:          secs2.L(secs2.L(), secs2.L(secs2.I1(64, 127))),
			// length=20, session=[0xff,0xff], stream=0xff (127|0x80), func=0xff,
			// PType=0, SType=0, sysBytes=[0xf1,0xf2,0xf3,0xf4],
			// body=L(L(),L(I1(64,127)))=[1,2,1,0,1,1,0x65,2,0x40,0x7f]
			wantBytes: []byte{
				0, 0, 0, 0x14,
				0xff, 0xff, 0xff, 0xff, 0, 0, 0xf1, 0xf2, 0xf3, 0xf4,
				0x01, 0x02, 0x01, 0x00, 0x01, 0x01, 0x65, 0x02, 0x40, 0x7f,
			},
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()

			msg, err := hsms.NewDataMessage(
				tc.stream, tc.function, tc.replyExpected,
				tc.sessionID, tc.systemBytes, tc.item,
			)
			require.NoError(t, err)
			assert.Equal(t, tc.wantBytes, msg.ToBytes())
		})
	}
}

// TestNewDataMessage_Accessors verifies all accessor methods.
func TestNewDataMessage_Accessors(t *testing.T) {
	sb := [4]byte{0xAA, 0xBB, 0xCC, 0xDD}
	item := secs2.A("hello")
	msg, err := hsms.NewDataMessage(5, 3, true, 0x1234, sb, item)

	require.NoError(t, err)
	assert.Equal(t, hsms.DataMsgType, msg.Type())
	assert.Equal(t, uint8(5), msg.Stream())
	assert.Equal(t, uint8(3), msg.Function())
	assert.True(t, msg.WaitBit())
	assert.Equal(t, uint16(0x1234), msg.SessionID())
	assert.Equal(t, sb, msg.SystemBytes())

	wantHeader := [10]byte{0x12, 0x34, 0x85, 0x03, 0x00, 0x00, 0xAA, 0xBB, 0xCC, 0xDD}

	assert.Equal(t, wantHeader, msg.HeaderBytes())
}

// TestNewDataMessage_HeaderBytes_Independence ensures HeaderBytes returns a value copy
// (mutating the returned array must not affect the message).
func TestNewDataMessage_HeaderBytes_Independence(t *testing.T) {
	msg, err := hsms.NewDataMessage(1, 1, false, 10, [4]byte{1, 2, 3, 4}, secs2.NewEmptyItem())
	require.NoError(t, err)

	hdr := msg.HeaderBytes()
	hdr[0] = 0xFF // mutate the local copy — must not affect msg

	assert.Equal(t, byte(0xFF), hdr[0], "local copy must reflect the write")
	assert.Equal(t, uint16(10), msg.SessionID(), "original sessionID must be unchanged after mutating copy")
}

// TestNewDataMessage_SystemBytes_Independence ensures SystemBytes returns a value copy.
func TestNewDataMessage_SystemBytes_Independence(t *testing.T) {
	orig := [4]byte{0x01, 0x02, 0x03, 0x04}
	msg, err := hsms.NewDataMessage(1, 1, false, 0, orig, secs2.NewEmptyItem())
	require.NoError(t, err)

	sb := msg.SystemBytes()
	sb[0] = 0xFF // mutate the local copy — must not affect msg

	assert.Equal(t, byte(0xFF), sb[0], "local copy must reflect the write")
	assert.Equal(t, orig, msg.SystemBytes(), "original systemBytes must be unchanged")
}

// ────────────────────────────────────────────────────────────────
// Item()
// ────────────────────────────────────────────────────────────────

// TestDataMessage_Item_TreePath verifies that Item() returns the original constructed item.
func TestDataMessage_Item_TreePath(t *testing.T) {
	item := secs2.A("value")
	msg, err := hsms.NewDataMessage(1, 1, false, 0, [4]byte{}, item)
	require.NoError(t, err)

	got, err := msg.Item()
	require.NoError(t, err)
	assert.NotNil(t, got)

	// The returned item should encode identically to the original.
	assert.Equal(t, item.ToBytes(), got.ToBytes())
}

// TestDataMessage_Item_EmptyBody verifies that a message with EmptyItem body
// returns (NewEmptyItem(), nil) from Item().
func TestDataMessage_Item_EmptyBody(t *testing.T) {
	msg, err := hsms.NewDataMessage(1, 1, false, 0, [4]byte{}, secs2.NewEmptyItem())
	require.NoError(t, err)

	got, decErr := msg.Item()
	assert.NoError(t, decErr)
	assert.NotNil(t, got)
	assert.True(t, got.IsEmpty())
	assert.Equal(t, 0, msg.BodyLen())
}

// ────────────────────────────────────────────────────────────────
// WithSessionID / WithSystemBytes
// ────────────────────────────────────────────────────────────────

// TestDataMessage_WithSessionID verifies immutability and body sharing.
func TestDataMessage_WithSessionID(t *testing.T) {
	item := secs2.L(secs2.A("abc"), secs2.I1(1, 2))
	orig, err := hsms.NewDataMessage(3, 5, true, 0x0001, [4]byte{1, 2, 3, 4}, item)
	require.NoError(t, err)

	stamped := orig.WithSessionID(0xABCD)

	// New message has the new session ID.
	assert.Equal(t, uint16(0xABCD), stamped.SessionID())
	// Original is unchanged.
	assert.Equal(t, uint16(0x0001), orig.SessionID())

	// Non-header fields are unchanged.
	assert.Equal(t, orig.Stream(), stamped.Stream())
	assert.Equal(t, orig.Function(), stamped.Function())
	assert.Equal(t, orig.WaitBit(), stamped.WaitBit())
	assert.Equal(t, orig.SystemBytes(), stamped.SystemBytes())

	// Body is shared: same length and same encoded bytes.
	assert.Equal(t, orig.BodyLen(), stamped.BodyLen())
	assert.Equal(t, orig.AppendBodyTo(nil), stamped.AppendBodyTo(nil))

	// ToBytes reflect only the session ID difference.
	ob := orig.ToBytes()
	sb := stamped.ToBytes()
	assert.Equal(t, len(ob), len(sb))
	// Bytes 4-5 of the wire frame (header[0:2]) should differ.
	assert.Equal(t, byte(0x00), ob[4])
	assert.Equal(t, byte(0x01), ob[5])
	assert.Equal(t, byte(0xAB), sb[4])
	assert.Equal(t, byte(0xCD), sb[5])
	// Body bytes (frame[14:]) must be identical.
	assert.Equal(t, ob[14:], sb[14:])
}

// TestDataMessage_WithSystemBytes verifies immutability and body sharing.
func TestDataMessage_WithSystemBytes(t *testing.T) {
	origSB := [4]byte{0x10, 0x20, 0x30, 0x40}
	newSB := [4]byte{0xAA, 0xBB, 0xCC, 0xDD}
	item := secs2.A("hi")
	orig, err := hsms.NewDataMessage(1, 3, true, 5, origSB, item)
	require.NoError(t, err)

	stamped := orig.WithSystemBytes(newSB)

	assert.Equal(t, newSB, stamped.SystemBytes())
	assert.Equal(t, origSB, orig.SystemBytes())

	// Body is shared.
	assert.Equal(t, orig.BodyLen(), stamped.BodyLen())
	assert.Equal(t, orig.AppendBodyTo(nil), stamped.AppendBodyTo(nil))
}

// ────────────────────────────────────────────────────────────────
// Q3 validation
// ────────────────────────────────────────────────────────────────

// TestNewDataMessage_Q3_ItemError verifies that NewDataMessage rejects an item
// whose Error() is non-nil.
func TestNewDataMessage_Q3_ItemError(t *testing.T) {
	// NewIntItem(3) has invalid byte size → Error() != nil.
	badItem := secs2.NewIntItem(3)
	require.Error(t, badItem.Error(), "precondition: item must carry an error")

	_, err := hsms.NewDataMessage(1, 1, false, 0, [4]byte{}, badItem)
	assert.Error(t, err)
}

// TestNewDataMessage_Q3_NestedListError verifies that aggregate errors from list
// children propagate through item.Error().
func TestNewDataMessage_Q3_NestedListError(t *testing.T) {
	// A list containing an invalid-byte-size IntItem has Error() != nil.
	badItem := secs2.L(secs2.NewIntItem(3))
	require.Error(t, badItem.Error(), "precondition: list must carry aggregate error")

	_, err := hsms.NewDataMessage(1, 1, false, 0, [4]byte{}, badItem)
	assert.Error(t, err)
}

// TestNewDataMessage_NilItem verifies that a nil secs2.Item is treated as an
// empty item (no panic) and yields a valid empty-body message. Before the guard,
// NewDataMessage called item.Error() on the nil interface and panicked.
func TestNewDataMessage_NilItem(t *testing.T) {
	require.NotPanics(t, func() {
		msg, err := hsms.NewDataMessage(1, 13, true, 0, [4]byte{}, nil)
		require.NoError(t, err)
		require.NotNil(t, msg)
		assert.Equal(t, 0, msg.BodyLen(), "nil item must produce an empty body")

		item, derr := msg.Item()
		require.NoError(t, derr)
		require.NotNil(t, item)
		assert.True(t, item.IsEmpty(), "nil item must decode to an empty item")
	})
}

// TestDataMessageBuilder_NilItem verifies that Derive().WithItem(nil).Build()
// is treated as an empty item (no panic).
func TestDataMessageBuilder_NilItem(t *testing.T) {
	base, err := hsms.NewDataMessage(1, 1, true, 0, [4]byte{}, secs2.NewEmptyItem())
	require.NoError(t, err)

	require.NotPanics(t, func() {
		msg, berr := base.Derive().WithItem(nil).WithFunction(13).Build()
		require.NoError(t, berr)
		require.NotNil(t, msg)
		assert.Equal(t, 0, msg.BodyLen())
	})
}

// TestNewDataMessage_Q3_WBitOnEvenFunction verifies that W=1 on an even function is rejected.
func TestNewDataMessage_Q3_WBitOnEvenFunction(t *testing.T) {
	_, err := hsms.NewDataMessage(1, 2, true, 0, [4]byte{}, secs2.NewEmptyItem())
	assert.ErrorIs(t, err, hsms.ErrInvalidRspMsg)
}

// TestNewDataMessage_Q3_StreamTooLarge verifies that stream > 127 is rejected.
func TestNewDataMessage_Q3_StreamTooLarge(t *testing.T) {
	_, err := hsms.NewDataMessage(128, 1, false, 0, [4]byte{}, secs2.NewEmptyItem())
	assert.ErrorIs(t, err, hsms.ErrInvalidStreamCode)
}

// TestNewDataMessage_Q3_Valid verifies that valid combinations construct without error.
func TestNewDataMessage_Q3_Valid(t *testing.T) {
	// stream=127 (max), function=255 (odd → W=1 OK), W=true
	msg, err := hsms.NewDataMessage(127, 255, true, 0, [4]byte{}, secs2.NewEmptyItem())
	require.NoError(t, err)
	assert.NotNil(t, msg)

	// stream=0, function=2 (even), W=false → OK
	msg2, err := hsms.NewDataMessage(0, 2, false, 0, [4]byte{}, secs2.NewEmptyItem())
	require.NoError(t, err)
	assert.NotNil(t, msg2)
}

// ────────────────────────────────────────────────────────────────
// AppendBodyTo
// ────────────────────────────────────────────────────────────────

// TestDataMessage_AppendBodyTo verifies that AppendBodyTo appends body bytes to dst.
func TestDataMessage_AppendBodyTo(t *testing.T) {
	item := secs2.A("abc")
	msg, err := hsms.NewDataMessage(1, 1, false, 0, [4]byte{}, item)
	require.NoError(t, err)

	got := msg.AppendBodyTo(nil)
	assert.Equal(t, item.ToBytes(), got)

	prefix := []byte{0xFF}
	got2 := msg.AppendBodyTo(prefix)
	assert.Equal(t, append([]byte{0xFF}, item.ToBytes()...), got2)
}

// ────────────────────────────────────────────────────────────────
// Derive().Build()
// ────────────────────────────────────────────────────────────────

// TestDataMessageBuilder_Build_Basic verifies that Derive().Build() produces a new
// message reflecting the builder overrides.
func TestDataMessageBuilder_Build_Basic(t *testing.T) {
	orig, err := hsms.NewDataMessage(1, 1, true, 0x1000, [4]byte{1, 2, 3, 4}, secs2.A("x"))
	require.NoError(t, err)

	derived, err := orig.Derive().
		WithStream(3).
		WithFunction(7).
		WithWaitBit(false).
		WithItem(secs2.A("derived")).
		Build()
	require.NoError(t, err)

	assert.Equal(t, uint8(3), derived.Stream())
	assert.Equal(t, uint8(7), derived.Function())
	assert.False(t, derived.WaitBit())

	// Session ID and system bytes come from the source message.
	assert.Equal(t, uint16(0x1000), derived.SessionID())
	assert.Equal(t, [4]byte{1, 2, 3, 4}, derived.SystemBytes())

	// Body reflects the new item.
	assert.Equal(t, secs2.A("derived").ToBytes(), derived.AppendBodyTo(nil))
}

// TestDataMessageBuilder_Build_Q3_WBitOnEvenFunction verifies that Build() re-runs
// the full Q3 validation and rejects W=1 on an even function.
func TestDataMessageBuilder_Build_Q3_WBitOnEvenFunction(t *testing.T) {
	orig, err := hsms.NewDataMessage(1, 1, true, 0, [4]byte{}, secs2.NewEmptyItem())
	require.NoError(t, err)

	// Change function to even but keep W=true → must fail Q3.
	_, err = orig.Derive().WithFunction(2).Build()
	assert.ErrorIs(t, err, hsms.ErrInvalidRspMsg)
}

// TestDataMessageBuilder_Build_Q3_ItemError verifies that Build() rejects errored items.
func TestDataMessageBuilder_Build_Q3_ItemError(t *testing.T) {
	orig, err := hsms.NewDataMessage(1, 1, false, 0, [4]byte{}, secs2.NewEmptyItem())
	require.NoError(t, err)

	_, err = orig.Derive().WithItem(secs2.NewIntItem(3)).Build()
	assert.Error(t, err)
}

// ────────────────────────────────────────────────────────────────
// Fan-out concurrency (-race)
// ────────────────────────────────────────────────────────────────

// TestDataMessage_FanOutConcurrency verifies that concurrent reads, With*
// calls, and Item() / ToBytes() / AppendBodyTo() on shared messages are
// race-free under -race.
func TestDataMessage_FanOutConcurrency(t *testing.T) {
	item := secs2.L(secs2.A("concurrent"), secs2.I1(10, 20, 30))
	msg, err := hsms.NewDataMessage(2, 5, true, 0x0100, [4]byte{0, 0, 0, 1}, item)
	require.NoError(t, err)

	const n = 64

	var wg sync.WaitGroup

	wg.Add(n)

	for i := range n {
		go func(id int) {
			defer wg.Done()

			_ = msg.ToBytes()
			_ = msg.AppendBodyTo(nil)
			_ = msg.BodyLen()

			gotItem, itemErr := msg.Item()
			assert.NoError(t, itemErr)
			assert.NotNil(t, gotItem)

			_ = msg.DecodeErr()

			m2 := msg.WithSessionID(uint16(id))
			_ = m2.ToBytes()
			_ = m2.HeaderBytes()

			m3 := msg.WithSystemBytes([4]byte{byte(id), 0, 0, 0})
			_ = m3.ToBytes()
			_ = m3.SystemBytes()
		}(i)
	}

	wg.Wait()
}

// ────────────────────────────────────────────────────────────────
// Alloc checks
// ────────────────────────────────────────────────────────────────

// TestDataMessage_ToBytes_OneAlloc verifies that ToBytes allocates exactly one buffer.
func TestDataMessage_ToBytes_OneAlloc(t *testing.T) {
	msg, err := hsms.NewDataMessage(1, 1, true, 1, [4]byte{0, 0, 0, 1}, secs2.A("alloc-check"))
	require.NoError(t, err)

	// Warm up: fire treeBody encoding cache so the first measured run is clean.
	_ = msg.ToBytes()

	allocs := testing.AllocsPerRun(50, func() {
		_ = msg.ToBytes()
	})
	assert.Equal(t, float64(1), allocs, "ToBytes must allocate exactly one buffer")
}

// TestDataMessage_AppendBodyTo_ZeroAlloc verifies that AppendBodyTo into a
// pre-allocated buffer allocates nothing after the encoding is cached.
func TestDataMessage_AppendBodyTo_ZeroAlloc(t *testing.T) {
	msg, err := hsms.NewDataMessage(1, 1, false, 0, [4]byte{}, secs2.A("zero-alloc"))
	require.NoError(t, err)

	// Warm up: cache the treeBody encoding.
	_ = msg.AppendBodyTo(nil)

	buf := make([]byte, 0, msg.BodyLen())
	allocs := testing.AllocsPerRun(50, func() {
		buf = msg.AppendBodyTo(buf[:0])
	})
	assert.Equal(t, float64(0), allocs, "AppendBodyTo into pre-sized buffer must allocate nothing")
}

// TestDataMessage_With_BodyNotCopied verifies the O(header) property of With*:
// the number of allocations is at most 1 (the new struct) regardless of body size,
// proving the body is shared and not re-encoded. The result is used to prevent
// the compiler from eliding the allocation via escape analysis.
func TestDataMessage_With_BodyNotCopied(t *testing.T) {
	// Use a non-trivial body so we can observe that it isn't re-encoded.
	item := secs2.L(secs2.A("body-shared-check"), secs2.I1(1, 2, 3, 4, 5))
	msg, err := hsms.NewDataMessage(1, 1, false, 0, [4]byte{1, 2, 3, 4}, item)
	require.NoError(t, err)

	// Force encoding cache warm-up so subsequent calls are stable.
	_ = msg.AppendBodyTo(nil)

	var sink []byte

	allocsID := testing.AllocsPerRun(50, func() {
		m := msg.WithSessionID(0x1234)
		// Use the result to prevent the compiler from eliding the allocation.
		sink = m.AppendBodyTo(sink[:0])
	})

	// At most 1 alloc: the new *DataMessage struct (body is shared, not re-encoded).
	assert.LessOrEqual(t, allocsID, float64(1), "WithSessionID must not allocate proportional to body size")
	// The body content must match the original.
	assert.Equal(t, msg.AppendBodyTo(nil), sink)

	allocsSB := testing.AllocsPerRun(50, func() {
		m := msg.WithSystemBytes([4]byte{0xAA, 0xBB, 0xCC, 0xDD})
		sink = m.AppendBodyTo(sink[:0])
	})

	assert.LessOrEqual(t, allocsSB, float64(1), "WithSystemBytes must not allocate proportional to body size")
	assert.Equal(t, msg.AppendBodyTo(nil), sink)
}
