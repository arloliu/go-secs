package format

import (
	"encoding/binary"
	"testing"

	"github.com/stretchr/testify/require"
)

// goldenRecordHeader is the 56-byte record header of the tracepack format specification §7.1,
// built independently with Python's struct.pack('<QqqIII4sHHBBBBBBBBBB2s', ...).
// Every field except quality, field_validity, record_flags and reserved holds the bytes of its own offsets;
// quality, field_validity and record_flags each hold their own mask, their defined bits (§9).
var goldenRecordHeader = []byte{
	0x00, 0x01, 0x02, 0x03, 0x04, 0x05, 0x06, 0x07, // 0
	0x08, 0x09, 0x0A, 0x0B, 0x0C, 0x0D, 0x0E, 0x0F, // 8
	0x10, 0x11, 0x12, 0x13, 0x14, 0x15, 0x16, 0x17, // 16
	0x18, 0x19, 0x1A, 0x1B, 0x1C, 0x1D, 0x1E, 0x1F, // 24
	0x20, 0x21, 0x22, 0x23, 0x24, 0x25, 0x26, 0x27, // 32
	0x28, 0x29, 0x7F, 0x00, 0x2C, 0x2D, 0x2E, 0x2F, // 40
	0x30, 0x31, 0x32, 0x33, 0x3F, 0x03, 0x00, 0x00, // 48
}

// goldenRecordHeaderExtended is a 64-byte record header built the same way:
// goldenRecordHeader with both reserved bytes set to 0xFF, followed by 8 bytes a later minor version appended.
var goldenRecordHeaderExtended = []byte{
	0x00, 0x01, 0x02, 0x03, 0x04, 0x05, 0x06, 0x07, // 0
	0x08, 0x09, 0x0A, 0x0B, 0x0C, 0x0D, 0x0E, 0x0F, // 8
	0x10, 0x11, 0x12, 0x13, 0x14, 0x15, 0x16, 0x17, // 16
	0x18, 0x19, 0x1A, 0x1B, 0x1C, 0x1D, 0x1E, 0x1F, // 24
	0x20, 0x21, 0x22, 0x23, 0x24, 0x25, 0x26, 0x27, // 32
	0x28, 0x29, 0x7F, 0x00, 0x2C, 0x2D, 0x2E, 0x2F, // 40
	0x30, 0x31, 0x32, 0x33, 0x3F, 0x03, 0xFF, 0xFF, // 48
	0xE0, 0xE1, 0xE2, 0xE3, 0xE4, 0xE5, 0xE6, 0xE7, // 56
}

// goldenRecordHeaderStruct is the decoded form of goldenRecordHeader.
func goldenRecordHeaderStruct() RecordHeader {
	return RecordHeader{
		Seq:           0x0706050403020100,
		TSUTCNs:       0x0F0E0D0C0B0A0908,
		MonoNs:        0x1716151413121110,
		Epoch:         0x1B1A1918,
		PayloadLen:    0x1F1E1D1C,
		TrailingBytes: 0x23222120,
		SystemBytes:   [4]byte{0x24, 0x25, 0x26, 0x27},
		SessionID:     0x2928,
		Quality:       recordHeaderQualityMask,
		Stream:        0x2C,
		Function:      0x2D,
		PType:         0x2E,
		SType:         0x2F,
		Kind:          0x30,
		Dir:           0x31,
		Fidelity:      0x32,
		DecodeStatus:  0x33,
		FieldValidity: recordHeaderFieldValidityMask,
		RecordFlags:   recordHeaderRecordFlagsMask,
	}
}

func TestRecordHeaderConstants(t *testing.T) {
	t.Parallel()

	require.Equal(t, 56, RecordHeaderLen)
}

func TestAppendRecordHeader_Golden(t *testing.T) {
	t.Parallel()

	h := goldenRecordHeaderStruct()

	require.Equal(t, goldenRecordHeader, AppendRecordHeader(nil, &h))
}

// Quality bits 7-15, field_validity bits 6-7 and record_flags bits 2-7 are reserved (§9):
// a caller that sets every bit of each field gets back only its defined bits,
// so the output is byte-identical to the golden vector.
func TestAppendRecordHeader_BitFieldsReservedBitsZeroed(t *testing.T) {
	t.Parallel()

	h := goldenRecordHeaderStruct()
	h.Quality = 0xFFFF
	h.FieldValidity = 0xFF
	h.RecordFlags = 0xFF

	require.Equal(t, goldenRecordHeader, AppendRecordHeader(nil, &h))
}

func TestAppendRecordHeader_FieldOffsets(t *testing.T) {
	t.Parallel()

	h := goldenRecordHeaderStruct()
	b := AppendRecordHeader(nil, &h)

	require.Len(t, b, RecordHeaderLen)
	requireAscendingFields(t, b, []layoutField{
		{"seq", 0, 8},
		{"ts_utc_ns", 8, 8},
		{"mono_ns", 16, 8},
		{"epoch", 24, 4},
		{"payload_len", 28, 4},
		{"trailing_bytes", 32, 4},
		{"system_bytes", 36, 4},
		{"session_id", 40, 2},
		{"stream", 44, 1},
		{"function", 45, 1},
		{"ptype", 46, 1},
		{"stype", 47, 1},
		{"kind", 48, 1},
		{"dir", 49, 1},
		{"fidelity", 50, 1},
		{"decode_status", 51, 1},
	})
	require.Equal(t, recordHeaderQualityMask, binary.LittleEndian.Uint16(b[42:44]), "quality")
	require.Equal(t, recordHeaderFieldValidityMask, b[52], "field_validity")
	require.Equal(t, recordHeaderRecordFlagsMask, b[53], "record_flags")
	require.Equal(t, []byte{0, 0}, b[54:56], "reserved")
}

// Extra follows the 56 known bytes verbatim, and the reserved bytes are written as zero.
func TestAppendRecordHeader_Extra(t *testing.T) {
	t.Parallel()

	h := goldenRecordHeaderStruct()
	h.Extra = []byte{0xE0, 0xE1, 0xE2, 0xE3, 0xE4, 0xE5, 0xE6, 0xE7}

	b := AppendRecordHeader(nil, &h)

	require.Len(t, b, 64)
	require.Equal(t, goldenRecordHeaderExtended[:54], b[:54])
	require.Equal(t, []byte{0, 0}, b[54:56], "reserved")
	require.Equal(t, goldenRecordHeaderExtended[56:], b[56:])
}

func TestAppendRecordHeader_Appends(t *testing.T) {
	t.Parallel()

	h := goldenRecordHeaderStruct()
	prefix := []byte{0xAA}

	b := AppendRecordHeader(cloneBytes(prefix), &h)

	require.Equal(t, prefix, b[:1])
	require.Equal(t, goldenRecordHeader, b[1:])
}

func TestUnmarshalRecordHeader_Golden(t *testing.T) {
	t.Parallel()

	got, err := UnmarshalRecordHeader(goldenRecordHeader, RecordHeaderLen)
	require.NoError(t, err)
	require.Equal(t, goldenRecordHeaderStruct(), got)
	require.Nil(t, got.Extra, "no bytes behind offset 56")
}

// A longer record_header_len keeps the unknown bytes behind offset 56 (§7.1),
// and nonzero reserved bytes are ignored (§1).
func TestUnmarshalRecordHeader_Extended(t *testing.T) {
	t.Parallel()

	got, err := UnmarshalRecordHeader(goldenRecordHeaderExtended, len(goldenRecordHeaderExtended))
	require.NoError(t, err)

	want := goldenRecordHeaderStruct()
	want.Extra = []byte{0xE0, 0xE1, 0xE2, 0xE3, 0xE4, 0xE5, 0xE6, 0xE7}
	require.Equal(t, want, got)
}

// Extra is a copy: changing the input afterwards does not change the decoded header.
func TestUnmarshalRecordHeader_ExtraIsCopy(t *testing.T) {
	t.Parallel()

	b := cloneBytes(goldenRecordHeaderExtended)

	got, err := UnmarshalRecordHeader(b, len(b))
	require.NoError(t, err)

	b[56] = 0x00
	require.Equal(t, byte(0xE0), got.Extra[0])
}

// Only recordHeaderLen bytes belong to the header; the next header or the payload section follows.
func TestUnmarshalRecordHeader_LongerBuffer(t *testing.T) {
	t.Parallel()

	got, err := UnmarshalRecordHeader(goldenRecordHeaderExtended, RecordHeaderLen)
	require.NoError(t, err)

	want := goldenRecordHeaderStruct()
	require.Equal(t, want, got)
	require.Nil(t, got.Extra)
}

// Writer output decodes back to the same header.
// The reverse, byte identity of arbitrary input, does not hold: reserved bytes are ignored on read and written as zero.
func TestRecordHeader_RoundTrip(t *testing.T) {
	t.Parallel()

	extended := goldenRecordHeaderStruct()
	extended.Extra = []byte{1, 2, 3}

	tests := []struct {
		name string
		h    RecordHeader
	}{
		{name: "golden", h: goldenRecordHeaderStruct()},
		{name: "zero", h: RecordHeader{}},
		{name: "with extra bytes", h: extended},
		{
			name: "values at their limits",
			h:    RecordHeader{Seq: MaxU64, TSUTCNs: -1, MonoNs: -1 << 63, PayloadLen: MaxLen32, Epoch: 0xFFFFFFFF, TrailingBytes: 0xFFFFFFFF},
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()

			b := AppendRecordHeader(nil, &tt.h)
			got, err := UnmarshalRecordHeader(b, len(b))
			require.NoError(t, err)
			require.Equal(t, tt.h, got)
		})
	}
}

func TestUnmarshalRecordHeader_Errors(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name            string
		b               func() []byte
		recordHeaderLen int
		field           string
		off             int
		wantErr         error
	}{
		{
			name:            "record_header_len 55",
			b:               func() []byte { return cloneBytes(goldenRecordHeader) },
			recordHeaderLen: 55,
			field:           "record_header_len",
			off:             0,
			wantErr:         ErrCorrupt,
		},
		{
			name:            "record_header_len 0 on an empty buffer",
			b:               func() []byte { return nil },
			recordHeaderLen: 0,
			field:           "record_header_len",
			off:             0,
			wantErr:         ErrCorrupt,
		},
		{
			name:            "negative record_header_len",
			b:               func() []byte { return cloneBytes(goldenRecordHeader) },
			recordHeaderLen: -1,
			field:           "record_header_len",
			off:             0,
			wantErr:         ErrCorrupt,
		},
		{
			name:            "one byte short of 56",
			b:               func() []byte { return cloneBytes(goldenRecordHeader[:55]) },
			recordHeaderLen: RecordHeaderLen,
			field:           "length",
			off:             55,
			wantErr:         ErrShort,
		},
		{
			name:            "known bytes present but extension short",
			b:               func() []byte { return cloneBytes(goldenRecordHeaderExtended[:60]) },
			recordHeaderLen: 64,
			field:           "length",
			off:             60,
			wantErr:         ErrShort,
		},
		{
			name: "seq over 2^63-1",
			b: func() []byte {
				b := cloneBytes(goldenRecordHeader)
				binary.LittleEndian.PutUint64(b[0:], 1<<63)

				return b
			},
			recordHeaderLen: RecordHeaderLen,
			field:           "seq",
			off:             0,
			wantErr:         ErrLimit,
		},
		{
			name: "payload_len over 2^31-1",
			b: func() []byte {
				b := cloneBytes(goldenRecordHeader)
				binary.LittleEndian.PutUint32(b[28:], 1<<31)

				return b
			},
			recordHeaderLen: RecordHeaderLen,
			field:           "payload_len",
			off:             28,
			wantErr:         ErrLimit,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()

			got, err := UnmarshalRecordHeader(tt.b(), tt.recordHeaderLen)
			requireOffsetError(t, err, tt.field, tt.off, tt.wantErr)
			require.Equal(t, RecordHeader{}, got)
		})
	}
}

// Decoding a 56-byte header allocates nothing; a longer one allocates only the copy of Extra.
func TestRecordHeader_Allocs(t *testing.T) {
	h := goldenRecordHeaderStruct()
	dst := make([]byte, 0, RecordHeaderLen)

	appendAllocs := testing.AllocsPerRun(100, func() {
		dst = AppendRecordHeader(dst[:0], &h)
	})
	require.Zero(t, appendAllocs, "AppendRecordHeader into a buffer with capacity")

	knownAllocs := testing.AllocsPerRun(100, func() {
		if _, err := UnmarshalRecordHeader(goldenRecordHeader, RecordHeaderLen); err != nil {
			t.Fatal(err)
		}
	})
	require.Zero(t, knownAllocs, "UnmarshalRecordHeader of 56 bytes")

	extendedAllocs := testing.AllocsPerRun(100, func() {
		if _, err := UnmarshalRecordHeader(goldenRecordHeaderExtended, len(goldenRecordHeaderExtended)); err != nil {
			t.Fatal(err)
		}
	})
	require.InDelta(t, 1.0, extendedAllocs, 0, "UnmarshalRecordHeader of 64 bytes copies Extra once")
}
