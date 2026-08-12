// Fuzz targets for the public decoder entry points.
//
// decode_test.go covers valid and invalid fixtures deterministically,
// but the payload decoders had no fuzz coverage of their own:
// only hsmsss's FuzzMessageReader reached a decoder, and it goes through DecodeHSMSMessage.
// Both payload decoders accept raw network bytes with no length prefix.
//
// DecodeOwnedHSMSPayload is the one that most needs fuzzing.
// It adopts the caller's buffer zero-copy into a live DataMessage,
// so a length-validation slip there leaves a message pointing at memory it does not own,
// rather than merely panicking on a throwaway copy.
//
// These live in package hsms, as decode_test.go does,
// so the invariant helper can assert against decoder internals.
package hsms

import (
	"encoding/binary"
	"testing"

	"github.com/arloliu/go-secs/v2/secs2"
	"github.com/stretchr/testify/require"
)

// fuzzSeedPayloads returns header-plus-optional-body slices, without the four-byte length prefix
// that the payload decoders do not take.
//
// The valid frames are built from the same constructors decode_test.go uses.
// The malformed ones cover the shapes that file exercises deterministically:
// too short, undefined SType, non-zero PType, and a control frame carrying a body.
func fuzzSeedPayloads(t testing.TB) [][]byte {
	t.Helper()

	sb := [4]byte{0xDE, 0xAD, 0xBE, 0xEF}

	dataMsg, err := NewDataMessage(1, 1, true, 0x0001, [4]byte{0, 0, 0, 1}, secs2.NewASCIIItem("fuzz-seed"))
	require.NoError(t, err)

	emptyBodyMsg, err := NewDataMessage(1, 2, false, 0x0001, [4]byte{}, secs2.NewEmptyItem())
	require.NoError(t, err)

	linktestReq := NewLinktestReq(sb)
	selectReq := NewSelectReq(0x0001, sb)

	// controlWithBody: a valid control header with one trailing body byte appended — must be
	// rejected with ErrControlFrameWithBody (E37 §9.3.3.1: decode.go:126-132), never decoded.
	controlWithBody := append(append([]byte(nil), linktestReq.ToBytes()[4:]...), 0xFF)

	// undefinedSType: a valid data-message payload with header byte 5 (SType) patched to an
	// undefined value (decode.go:135-137).
	undefinedSType := append([]byte(nil), dataMsg.ToBytes()[4:]...)
	undefinedSType[5] = 8

	// nonZeroPType: a valid data-message payload with header byte 4 (PType) patched non-zero
	// (decode.go:116-118).
	nonZeroPType := append([]byte(nil), dataMsg.ToBytes()[4:]...)
	nonZeroPType[4] = 1

	return [][]byte{
		dataMsg.ToBytes()[4:],
		emptyBodyMsg.ToBytes()[4:],
		linktestReq.ToBytes()[4:],
		selectReq.ToBytes()[4:],
		controlWithBody,
		undefinedSType,
		nonZeroPType,
		make([]byte, 9),  // too short: below the 10-byte header minimum
		make([]byte, 10), // minimal header-only, all-zero (valid data message, empty body)
	}
}

// assertDecodedInvariants checks the guarantees decode.go documents for a successfully decoded
// message:
//
//   - SType is one defined by SEMI E37 — decodeOwnedFrame's switch rejects anything else before
//     returning a message (decode.go:120-137).
//   - PType is 0 (decode.go:116-118).
//   - The decoded header mirrors the first 10 input bytes exactly, including SessionID.
//   - For a data message, BodyLen equals len(payload)-10: there is no separate declared-length
//     field inside a payload (unlike DecodeHSMSMessage's 4-byte prefix) — the body is everything
//     after the header, so this can never diverge for a correct decoder.
//   - For a control message, the payload carries no body (enforced by decode.go:129-132; a
//     control message that reached this point already passed that check).
//   - The lazy SECS-II body decode (DataMessage.Item) must not panic on fuzzed bytes.
func assertDecodedInvariants(t *testing.T, msg Message, payload []byte) {
	t.Helper()

	require.GreaterOrEqual(t, len(payload), 10, "a successfully decoded message must have consumed at least the 10-byte header")
	require.True(t, IsValidSType(byte(msg.Type())), "decoded message must carry a defined SType, got %d", msg.Type())

	header := msg.HeaderBytes()
	require.Equal(t, payload[:10], header[:], "decoded header must mirror the first 10 input bytes exactly")
	require.Equal(t, byte(0), header[4], "PType must be 0 (decode.go rejects non-zero PType)")
	require.Equal(t, binary.BigEndian.Uint16(payload[0:2]), msg.SessionID(), "SessionID must match header bytes 0-1")

	switch m := msg.(type) {
	case *DataMessage:
		require.Equal(t, len(payload)-10, m.BodyLen(), "DataMessage body length must equal len(payload)-10")
		require.LessOrEqual(t, int(m.Stream()), 0x7F, "Stream must be masked to 7 bits (header[2] & 0x7F)")
		_, _ = m.Item() // must not panic on a malformed SECS-II body; an error is fine
	case *ControlMessage:
		require.Equal(t, 10, len(payload), "control frame must carry no body (enforced by decode.go:129-132)")
	default:
		t.Fatalf("unexpected decoded message type %T", msg)
	}
}

// FuzzDecodeHSMSPayload fuzzes DecodeHSMSPayload (decode.go:72-83): [10-byte header || optional
// body], no length prefix, defensive copy.
//
// Invariants: never panics; err == nil implies a non-nil message satisfying
// assertDecodedInvariants.
func FuzzDecodeHSMSPayload(f *testing.F) {
	for _, seed := range fuzzSeedPayloads(f) {
		f.Add(seed)
	}

	f.Fuzz(func(t *testing.T, payload []byte) {
		msg, err := DecodeHSMSPayload(payload)
		if err != nil {
			require.Nil(t, msg)

			return
		}

		require.NotNil(t, msg, "DecodeHSMSPayload: nil error must yield a non-nil message")
		assertDecodedInvariants(t, msg, payload)
	})
}

// FuzzDecodeOwnedHSMSPayload fuzzes DecodeOwnedHSMSPayload (decode.go:85-97): same shape as
// DecodeHSMSPayload but WITHOUT the defensive copy — for a data message the body aliases the
// caller's buffer zero-copy (decode.go:99-104, newRawFrameDataMessage -> wire.AdoptBody).
//
// Invariants: never panics; err == nil implies a non-nil message satisfying
// assertDecodedInvariants; AND, because this path aliases the input, mutating the input buffer
// after a successful decode must not corrupt the header or drift BodyLen out of range — that
// would be the signature of a length-validation bug letting the message reference out-of-bounds
// or stale memory.
func FuzzDecodeOwnedHSMSPayload(f *testing.F) {
	for _, seed := range fuzzSeedPayloads(f) {
		f.Add(seed)
	}

	f.Fuzz(func(t *testing.T, payload []byte) {
		// DecodeOwnedHSMSPayload takes ownership of its argument; decode into an independent
		// copy so the corpus's backing slice is never mutated by the aliasing check below.
		owned := append([]byte(nil), payload...)

		msg, err := DecodeOwnedHSMSPayload(owned)
		if err != nil {
			require.Nil(t, msg)

			return
		}

		require.NotNil(t, msg, "DecodeOwnedHSMSPayload: nil error must yield a non-nil message")
		assertDecodedInvariants(t, msg, owned)

		// The decoder copies the header into a value array, so mutating the input afterwards
		// must not disturb it.
		// The body is adopted zero-copy, so its bytes are expected to follow the mutation while
		// its length stays put: a length that drifted would mean the decoder captured a bad
		// length or an out-of-bounds reference.
		//
		// This runs on a second, untouched decode.
		// Item caches its result on first use, so a body read before the mutation would answer
		// from that cache and prove nothing.
		aliased := append([]byte(nil), payload...)

		second, secondErr := DecodeOwnedHSMSPayload(aliased)
		if secondErr != nil {
			return
		}

		headerBefore := second.HeaderBytes()

		dm, isDataMsg := second.(*DataMessage)

		var (
			bodyLenBefore int
			bodyBefore    []byte
		)

		if isDataMsg {
			bodyLenBefore = dm.BodyLen()
			bodyBefore = dm.AppendBodyTo(nil)
		}

		for i := range aliased {
			aliased[i] ^= 0xFF
		}

		require.Equal(t, headerBefore, second.HeaderBytes(),
			"the header is copied by value, so mutating the input after decode must not change it")

		if isDataMsg {
			require.Equal(t, bodyLenBefore, dm.BodyLen(),
				"the body length must not drift when only the input bytes change")

			if bodyLenBefore > 0 {
				require.NotEqual(t, bodyBefore, dm.AppendBodyTo(nil),
					"the body is adopted zero-copy, so mutating the input must be visible through it")
			}

			_, _ = dm.Item() // first decode of this message, against the mutated body: must not panic
		}
	})
}

// FuzzDecodeHSMSMessage fuzzes DecodeHSMSMessage (decode.go:41-67) directly within package hsms:
// [4-byte big-endian length prefix || 10-byte header || optional body]. hsmsss/fuzz_test.go's
// FuzzMessageReader already chains through this entry point via transport.readFrame, but this
// target additionally covers the length-prefix parsing (short/zero/oversized/mismatched msgLen)
// without requiring a net.Pipe round trip.
//
// Invariants: never panics; err == nil implies a non-nil message satisfying
// assertDecodedInvariants against data[4:].
func FuzzDecodeHSMSMessage(f *testing.F) {
	dataMsg, err := NewDataMessage(1, 1, true, 0x0001, [4]byte{0, 0, 0, 1}, secs2.NewASCIIItem("fuzz-seed"))
	require.NoError(f, err)
	linktestReq := NewLinktestReq([4]byte{0xDE, 0xAD, 0xBE, 0xEF})

	f.Add(dataMsg.ToBytes())
	f.Add(linktestReq.ToBytes())
	f.Add([]byte{0x00, 0x00, 0x00, 0x00})                // shorter than minFrame
	f.Add([]byte{0, 0, 0, 9, 0, 0, 0, 0, 0, 0, 0, 0, 0}) // msgLen below minimum (10)
	f.Add(append(dataMsg.ToBytes(), 0xFF))               // length mismatch: extra byte

	oversizedLen := make([]byte, 4)
	binary.BigEndian.PutUint32(oversizedLen, 0xFFFFFFFF)
	f.Add(oversizedLen) // oversized length field, must be rejected before allocation

	f.Fuzz(func(t *testing.T, data []byte) {
		msg, err := DecodeHSMSMessage(data)
		if err != nil {
			require.Nil(t, msg)

			return
		}

		require.NotNil(t, msg, "DecodeHSMSMessage: nil error must yield a non-nil message")
		assertDecodedInvariants(t, msg, data[4:])
	})
}
