// Cross-cutting success-criteria tests for the hsms message layer (spec §11).
// Tests in this file span DataMessage and ControlMessage together and close minor
// gaps from earlier task reviews.
package hsms_test

import (
	"encoding/binary"
	"testing"

	"github.com/arloliu/go-secs/v2/hsms"
	"github.com/arloliu/go-secs/v2/secs2"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// ────────────────────────────────────────────────────────────────
// Comprehensive round-trip table
// ────────────────────────────────────────────────────────────────

// TestMessageRoundTrip_AllTypes verifies DecodeHSMSMessage(m.ToBytes()) for a
// DataMessage with a non-trivial nested item body and for every defined control
// SType. Assertions are non-vacuous: header bytes, body bytes, and the decoded
// SECS-II item are all compared after the full encode→decode round-trip.
func TestMessageRoundTrip_AllTypes(t *testing.T) {
	t.Parallel()

	sb := [4]byte{0xCA, 0xFE, 0xBA, 0xBE}

	// DataMessage with a non-trivial nested item.
	nestedItem := secs2.L(secs2.A("roundtrip"), secs2.I1(1, 2, 3), secs2.BOOLEAN(true))
	dataMsg, err := hsms.NewDataMessage(5, 3, true, 0x0ABC, sb, nestedItem)
	require.NoError(t, err)

	t.Run("DataMessage", func(t *testing.T) {
		t.Parallel()

		frame := dataMsg.ToBytes()
		decoded, decErr := hsms.DecodeHSMSMessage(frame)
		require.NoError(t, decErr)

		dm, ok := decoded.(*hsms.DataMessage)
		require.True(t, ok, "decoded message must be *DataMessage")

		// HeaderBytes must survive encode→decode intact.
		assert.Equal(t, dataMsg.HeaderBytes(), dm.HeaderBytes(), "header bytes must round-trip")

		// Body bytes must survive encode→decode (wire-level equality, independent of item struct).
		assert.Equal(t, dataMsg.AppendBodyTo(nil), dm.AppendBodyTo(nil), "body bytes must round-trip")

		// Item() must decode to the same wire representation (non-vacuous: decItem goes
		// through secs2.Decode and is a structurally distinct object from the original).
		origItem, origItemErr := dataMsg.Item()
		require.NoError(t, origItemErr)
		decItem, decItemErr := dm.Item()
		require.NoError(t, decItemErr)
		assert.Equal(t, origItem.ToBytes(), decItem.ToBytes(),
			"decoded SECS-II item must encode identically to the original")
	})

	// Build all control message types.
	selectReq := hsms.NewSelectReq(0x0001, sb)
	selectRsp, err := hsms.NewSelectRsp(selectReq, hsms.SelectStatusSuccess)
	require.NoError(t, err)
	deselReq := hsms.NewDeselectReq(0x0002, sb)
	deselRsp, err := hsms.NewDeselectRsp(deselReq, hsms.DeselectStatusSuccess)
	require.NoError(t, err)
	linktestReq := hsms.NewLinktestReq(sb)
	linktestRsp, err := hsms.NewLinktestRsp(linktestReq)
	require.NoError(t, err)
	rejectReq := hsms.NewRejectReqRaw(0x0003, 0, byte(hsms.SeparateReqType), sb, hsms.RejectSTypeNotSupported)
	separateReq := hsms.NewSeparateReq(0x0004, sb)

	controlCases := []struct {
		name string
		msg  *hsms.ControlMessage
	}{
		{"SelectReq", selectReq},
		{"SelectRsp", selectRsp},
		{"DeselectReq", deselReq},
		{"DeselectRsp", deselRsp},
		{"LinktestReq", linktestReq},
		{"LinktestRsp", linktestRsp},
		{"RejectReq", rejectReq},
		{"SeparateReq", separateReq},
	}

	for _, tc := range controlCases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()

			frame := tc.msg.ToBytes()
			decoded, decErr := hsms.DecodeHSMSMessage(frame)
			require.NoError(t, decErr)

			cm, ok := decoded.(*hsms.ControlMessage)
			require.True(t, ok, "decoded control message must be *ControlMessage")

			// HeaderBytes must survive encode→decode intact.
			assert.Equal(t, tc.msg.HeaderBytes(), cm.HeaderBytes(),
				"header bytes must round-trip for %s", tc.name)
			// SType must be preserved.
			assert.Equal(t, tc.msg.Type(), cm.Type(),
				"SType must match after round-trip for %s", tc.name)
		})
	}
}

// ────────────────────────────────────────────────────────────────
// dec-sharing: decode-at-most-once across restamped copies
// ────────────────────────────────────────────────────────────────

// TestDataMessage_DecShareAcrossWithSessionID verifies that WithSessionID shares the
// decodeState with the original message: Item() on both the original and the stamped
// copy returns the same secs2.Item pointer, proving that secs2.Decode fires at most
// once across all copies produced by With*.
func TestDataMessage_DecShareAcrossWithSessionID(t *testing.T) {
	t.Parallel()

	item := secs2.A("dec-share-proof")
	msg, err := hsms.NewDataMessage(1, 1, false, 0x0001, [4]byte{1, 2, 3, 4}, item)
	require.NoError(t, err)

	stamped := msg.WithSessionID(0xABCD)

	origItem, origErr := msg.Item()
	require.NoError(t, origErr)
	require.NotNil(t, origItem)

	stItem, stErr := stamped.Item()
	require.NoError(t, stErr)
	require.NotNil(t, stItem)

	// Both must return the same underlying secs2.Item pointer.
	// The shared decodeState ensures the item is decoded at most once and cached
	// for every copy produced by WithSessionID / WithSystemBytes.
	require.Same(t, origItem, stItem,
		"WithSessionID must share decodeState: Item() must return the same pointer")
}

// TestDataMessage_DecShareRawFrame verifies decode-at-most-once across copies of a
// raw-frame DataMessage (one produced by DecodeHSMSMessage, whose dec.once is unfired
// at construction time).
//
// Sequence:
//  1. Build m0 (tree-path) and encode it to wire bytes.
//  2. Decode those bytes into m (raw-frame); m.dec.once is unfired.
//  3. WithSessionID(0x1234) → stamped (shares m.dec).
//  4. Call stamped.Item() first — fires the shared once and caches the item.
//  5. Call m.Item() — once is already fired; returns the cached item.
//  6. require.Same proves a single decode, not two independent ones.
func TestDataMessage_DecShareRawFrame(t *testing.T) {
	t.Parallel()

	// Step 1: build a tree-path message and encode it.
	m0Item := secs2.L(secs2.A("raw-frame-proof"), secs2.U2(42))
	m0, err := hsms.NewDataMessage(3, 5, true, 0x0099, [4]byte{0xDE, 0xAD, 0xBE, 0xEF}, m0Item)
	require.NoError(t, err)

	frame := m0.ToBytes()

	// Step 2: decode to a raw-frame DataMessage (dec.once unfired).
	decoded, err := hsms.DecodeHSMSMessage(frame)
	require.NoError(t, err)

	m, ok := decoded.(*hsms.DataMessage)
	require.True(t, ok, "decoded message must be *DataMessage")

	// Step 3: restamp — shares the same *decodeState.
	stamped := m.WithSessionID(0x1234)

	// Step 4: call stamped.Item() first to fire the shared dec.once.
	stItem, stErr := stamped.Item()
	require.NoError(t, stErr)
	require.NotNil(t, stItem)

	// Step 5: call m.Item() — once already fired; must return the cached item.
	origItem, origErr := m.Item()
	require.NoError(t, origErr)
	require.NotNil(t, origItem)

	// Step 6: same pointer proves decode fired exactly once on the shared dec.
	require.Same(t, stItem, origItem,
		"WithSessionID must share decodeState: Item() must return the same pointer (raw-frame decode-at-most-once)")

	// Value-equality: the decoded item must encode identically to m0's item.
	m0OrigItem, m0Err := m0.Item()
	require.NoError(t, m0Err)
	assert.Equal(t, m0OrigItem.ToBytes(), origItem.ToBytes(),
		"decoded item must encode identically to the source message item")
}

// ────────────────────────────────────────────────────────────────
// Decode malformed: msgLen = 9 reaches the second length guard
// ────────────────────────────────────────────────────────────────

// TestDecodeHSMSMessage_MsgLen9ReachesSecondGuard verifies that a 14-byte frame
// whose msgLen field is 9 is rejected by the msgLen<10 guard — not the len(data)<14
// guard. This closes the boundary gap: len(data)=14 passes the first guard; msgLen=9
// triggers the second guard and must return an error without panicking.
func TestDecodeHSMSMessage_MsgLen9ReachesSecondGuard(t *testing.T) {
	// Build a 14-byte buffer so len(data)=14 >= 14 (minFrame=4+10) — passes guard 1.
	// Set msgLen=9 < 10 so guard 2 fires.
	data := make([]byte, 14)
	binary.BigEndian.PutUint32(data[0:4], 9) // msgLen = 9

	msg, err := hsms.DecodeHSMSMessage(data)
	assert.Error(t, err, "msgLen=9 must be rejected as below the 10-byte minimum")
	assert.Nil(t, msg, "no message must be returned on error")
}
