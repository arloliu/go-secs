package tracepack_test

import (
	"encoding/hex"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/arloliu/go-secs/tracepack"
	"github.com/arloliu/go-secs/tracepack/internal/tlv"
)

// Golden transport-event and annotation payloads are hand-built from the entry layout of the tracepack format specification §5
// and the tag registries of §8.
// Each is the hex of python3 struct.pack('<HBBI', tag, value_type, 0, len(value)) + value, concatenated in tag order.
const (
	// teMinHex holds only the required event tag (event = state-transition).
	teMinHex = "010001000100000001"
	// teFullHex sets every transport-event tag once.
	teFullHex = "010001000100000001020001000100000001030001000100000003040001000100000003050006000800000076656e646f723a58" +
		"06000100010000000007000100010000000108000200010000000109000400080000002a000000000000000a00040008000000010" +
		"0000000000000" +
		"0b0004000800000003000000000000000c00070004000000010203040d00010001000000010e000400080000000a0000000000000" +
		"00f0004000800000014000000000000001000030008000000fbffffffffffffff1100030008000000050000000000000012000600" +
		"04000000" +
		"6e6f7465130003000800000040420f0000000000"
	// teUnknownHex is teMinHex plus one entry of an unknown tag (0x7000, utf8, "x").
	teUnknownHex = teMinHex + "007006000100000078"
	// teBadPrimarySystemBytesHex sets primary_system_bytes to 3 bytes instead of the required 4.
	teBadPrimarySystemBytesHex = "0100010001000000010c00070003000000010203"

	// annTextHex is annotation_kind = note with text = "hello".
	annTextHex = "010001000100000001050006000500000068656c6c6f"
	// annRawHex is annotation_kind = skipped-bytes with raw = 0xDEAD.
	annRawHex = "0100010001000000030600070002000000dead"
	// annRawEmptyHex is annotation_kind = skipped-bytes with raw present but zero-length:
	// exactly one of text/raw, valid per §8.
	annRawEmptyHex = "0100010001000000030600070000000000"
	// annTextAndEmptyRawHex sets text = "hello" and a zero-length raw: both present, invalid.
	annTextAndEmptyRawHex = "010001000100000001050006000500000068656c6c6f0600070000000000"
	// annNeitherHex sets annotation_kind alone: neither text nor raw.
	annNeitherHex = "010001000100000001"
	// annBothHex sets both text and raw.
	annBothHex = annTextHex + "060007000100000001"
	// annUnknownHex is annTextHex plus one entry of a private tag (0x9000, bytes, 0x09 0x09).
	annUnknownHex = annTextHex + "00900700020000000909"
)

func TestTransportEventMarshalMinimalGolden(t *testing.T) {
	t.Parallel()

	te := &tracepack.TransportEvent{Event: tracepack.EventStateTransition}
	got, err := te.MarshalBinary()
	require.NoError(t, err)
	assert.Equal(t, teMinHex, hex.EncodeToString(got))
}

func TestTransportEventMarshalFullGolden(t *testing.T) {
	t.Parallel()

	te := &tracepack.TransportEvent{
		Event:                     tracepack.EventStateTransition,
		PrevState:                 new(tracepack.StateNotConnected),
		CurState:                  new(tracepack.StateSelected),
		Cause:                     new(tracepack.CauseSelectAccepted),
		CauseRaw:                  new("vendor:X"),
		Timer:                     new(tracepack.TimerNone),
		SocketRole:                new(tracepack.SocketRolePassive),
		Inferred:                  new(true),
		PrimarySessionID:          new(uint64(42)),
		PrimaryStream:             new(uint64(1)),
		PrimaryFunction:           new(uint64(3)),
		PrimarySystemBytes:        [4]byte{1, 2, 3, 4},
		PrimarySystemBytesPresent: true,
		BoundaryKind:              new(tracepack.BoundaryKindStart),
		BoundarySeqFirst:          new(uint64(10)),
		BoundarySeqLast:           new(uint64(20)),
		GapStart:                  new(int64(-5)),
		GapEnd:                    new(int64(5)),
		Detail:                    new("note"),
		ClockStepNs:               new(int64(1000000)),
	}

	got, err := te.MarshalBinary()
	require.NoError(t, err)
	assert.Equal(t, teFullHex, hex.EncodeToString(got))
}

func TestUnmarshalTransportEventFullRoundTrip(t *testing.T) {
	t.Parallel()

	te, err := tracepack.UnmarshalTransportEvent(mustHexBytes(t, teFullHex))
	require.NoError(t, err)
	assert.Equal(t, tracepack.EventStateTransition, te.Event)
	require.NotNil(t, te.CauseRaw)
	assert.Equal(t, "vendor:X", *te.CauseRaw)
	assert.True(t, te.PrimarySystemBytesPresent)
	assert.Equal(t, [4]byte{1, 2, 3, 4}, te.PrimarySystemBytes)
	assert.Empty(t, te.Unknown)

	again, err := te.MarshalBinary()
	require.NoError(t, err)
	assert.Equal(t, teFullHex, hex.EncodeToString(again))
}

func TestUnmarshalTransportEventRejectsShortPrimarySystemBytes(t *testing.T) {
	t.Parallel()

	_, err := tracepack.UnmarshalTransportEvent(mustHexBytes(t, teBadPrimarySystemBytesHex))
	require.ErrorIs(t, err, tracepack.ErrFieldLength)
}

func TestTransportEventUnknownEntryRoundTrip(t *testing.T) {
	t.Parallel()

	te, err := tracepack.UnmarshalTransportEvent(mustHexBytes(t, teUnknownHex))
	require.NoError(t, err)
	require.Len(t, te.Unknown, 1)
	assert.Equal(t, uint16(0x7000), te.Unknown[0].Tag)
	assert.Equal(t, []byte("x"), te.Unknown[0].Value)

	again, err := te.MarshalBinary()
	require.NoError(t, err)
	assert.Equal(t, teUnknownHex, hex.EncodeToString(again))
}

func TestTransportEventMarshalRejectsUnknownEntryNamingKnownTag(t *testing.T) {
	t.Parallel()

	te := &tracepack.TransportEvent{
		Event:   tracepack.EventStateTransition,
		Unknown: []tracepack.RawEntry{{Tag: 0x0002, Type: 1, Value: []byte{1}}},
	}
	_, err := te.MarshalBinary()
	require.ErrorIs(t, err, tracepack.ErrReservedTag)
}

func TestAnnotationMarshalTextGolden(t *testing.T) {
	t.Parallel()

	a := &tracepack.Annotation{AnnotationKind: tracepack.AnnotationKindNote, Text: new("hello")}
	got, err := a.MarshalBinary()
	require.NoError(t, err)
	assert.Equal(t, annTextHex, hex.EncodeToString(got))
}

func TestAnnotationMarshalRawGolden(t *testing.T) {
	t.Parallel()

	a := &tracepack.Annotation{AnnotationKind: tracepack.AnnotationKindSkippedBytes, Raw: []byte{0xDE, 0xAD}}
	got, err := a.MarshalBinary()
	require.NoError(t, err)
	assert.Equal(t, annRawHex, hex.EncodeToString(got))
}

func TestUnmarshalAnnotationTextRoundTrip(t *testing.T) {
	t.Parallel()

	a, err := tracepack.UnmarshalAnnotation(mustHexBytes(t, annTextHex))
	require.NoError(t, err)
	assert.Equal(t, tracepack.AnnotationKindNote, a.AnnotationKind)
	require.NotNil(t, a.Text)
	assert.Equal(t, "hello", *a.Text)
	assert.Nil(t, a.Raw)

	again, err := a.MarshalBinary()
	require.NoError(t, err)
	assert.Equal(t, annTextHex, hex.EncodeToString(again))
}

func TestUnmarshalAnnotationRawRoundTrip(t *testing.T) {
	t.Parallel()

	a, err := tracepack.UnmarshalAnnotation(mustHexBytes(t, annRawHex))
	require.NoError(t, err)
	assert.Equal(t, tracepack.AnnotationKindSkippedBytes, a.AnnotationKind)
	assert.Nil(t, a.Text)
	assert.Equal(t, []byte{0xDE, 0xAD}, a.Raw)

	again, err := a.MarshalBinary()
	require.NoError(t, err)
	assert.Equal(t, annRawHex, hex.EncodeToString(again))
}

func TestUnmarshalAnnotationEmptyRawIsPresent(t *testing.T) {
	t.Parallel()

	a, err := tracepack.UnmarshalAnnotation(mustHexBytes(t, annRawEmptyHex))
	require.NoError(t, err)
	assert.Nil(t, a.Text)
	require.NotNil(t, a.Raw)
	assert.Empty(t, a.Raw)

	again, err := a.MarshalBinary()
	require.NoError(t, err)
	assert.Equal(t, annRawEmptyHex, hex.EncodeToString(again))
}

func TestAnnotationRejectsTextRawViolations(t *testing.T) {
	t.Parallel()

	// Each row breaks the exactly-one-of text and raw rule: hex on decode, a on encode.
	tests := []struct {
		name string
		hex  string
		a    *tracepack.Annotation
	}{
		{name: "unmarshal text and empty raw", hex: annTextAndEmptyRawHex},
		{name: "unmarshal neither text nor raw", hex: annNeitherHex},
		{name: "unmarshal both text and raw", hex: annBothHex},
		{name: "marshal neither text nor raw", a: &tracepack.Annotation{AnnotationKind: tracepack.AnnotationKindNote}},
		{name: "marshal both text and raw", a: &tracepack.Annotation{AnnotationKind: tracepack.AnnotationKindNote, Text: new("x"), Raw: []byte{1}}},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()

			var err error
			if tt.a != nil {
				_, err = tt.a.MarshalBinary()
			} else {
				_, err = tracepack.UnmarshalAnnotation(mustHexBytes(t, tt.hex))
			}
			require.ErrorIs(t, err, tracepack.ErrAnnotationText)
		})
	}
}

func TestAnnotationUnknownEntryRoundTrip(t *testing.T) {
	t.Parallel()

	a, err := tracepack.UnmarshalAnnotation(mustHexBytes(t, annUnknownHex))
	require.NoError(t, err)
	require.Len(t, a.Unknown, 1)
	assert.Equal(t, uint16(0x9000), a.Unknown[0].Tag)
	assert.Equal(t, []byte{0x09, 0x09}, a.Unknown[0].Value)

	again, err := a.MarshalBinary()
	require.NoError(t, err)
	assert.Equal(t, annUnknownHex, hex.EncodeToString(again))
}

func TestAnnotationMarshalRejectsUnknownEntryNamingKnownTag(t *testing.T) {
	t.Parallel()

	a := &tracepack.Annotation{
		AnnotationKind: tracepack.AnnotationKindNote,
		Text:           new("x"),
		Unknown:        []tracepack.RawEntry{{Tag: 0x0005, Type: 6, Value: []byte("y")}},
	}
	_, err := a.MarshalBinary()
	require.ErrorIs(t, err, tracepack.ErrReservedTag)
}

func TestTransportEventMarshalRejectsValuesItsDecoderRejects(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name string
		ev   tracepack.TransportEvent
		want error
	}{
		{"cause_raw not valid UTF-8", tracepack.TransportEvent{Event: tracepack.EventStateTransition, CauseRaw: new("\xff")}, tlv.ErrUTF8},
		{"primary_session_id above 2^63-1", tracepack.TransportEvent{
			Event: tracepack.EventStateTransition, PrimarySessionID: new(uint64(1) << 63),
		}, tlv.ErrLimit},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()

			b, err := tt.ev.MarshalBinary()
			require.ErrorIs(t, err, tt.want)
			assert.Nil(t, b)
		})
	}
}

func TestTransportEventMarshalLimitValueRoundTrip(t *testing.T) {
	t.Parallel()

	ev := &tracepack.TransportEvent{Event: tracepack.EventStateTransition, PrimarySessionID: new(uint64(1)<<63 - 1)}
	b, err := ev.MarshalBinary()
	require.NoError(t, err)

	got, err := tracepack.UnmarshalTransportEvent(b)
	require.NoError(t, err)
	assert.Equal(t, ev, got)
}

func TestAnnotationMarshalRejectsValuesItsDecoderRejects(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name string
		ann  tracepack.Annotation
		want error
	}{
		{"text not valid UTF-8", tracepack.Annotation{AnnotationKind: tracepack.AnnotationKindNote, Text: new("\xff")}, tlv.ErrUTF8},
		{"ref_seq_first above 2^63-1", tracepack.Annotation{
			AnnotationKind: tracepack.AnnotationKindNote, Text: new("note"), RefSeqFirst: new(uint64(1) << 63),
		}, tlv.ErrLimit},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()

			b, err := tt.ann.MarshalBinary()
			require.ErrorIs(t, err, tt.want)
			assert.Nil(t, b)
		})
	}
}
