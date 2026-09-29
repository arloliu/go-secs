package format

import (
	"encoding/binary"
	"testing"

	"github.com/stretchr/testify/require"
)

// goldenEnvelope is the block envelope of the tracepack format specification §6,
// built independently with Python's struct.pack('<4sBBHIIIIQ4sI', ...) and zlib.crc32.
// Every field except magic, reserved and envelope_crc holds the bytes of its own offsets;
// envelope_crc over bytes 0-35 is 0xA86E765F.
var goldenEnvelope = []byte{
	0x54, 0x50, 0x4B, 0x42, 0x04, 0x00, 0x06, 0x07, // 0
	0x08, 0x09, 0x0A, 0x0B, 0x0C, 0x0D, 0x0E, 0x0F, // 8
	0x10, 0x11, 0x12, 0x13, 0x14, 0x15, 0x16, 0x17, // 16
	0x18, 0x19, 0x1A, 0x1B, 0x1C, 0x1D, 0x1E, 0x1F, // 24
	0x00, 0x00, 0x00, 0x00, 0x5F, 0x76, 0x6E, 0xA8, // 32
}

// goldenEnvelopeReserved is goldenEnvelope with every reserved byte (5 and 32-35) set to 0xFF,
// built the same way; its envelope_crc is 0xD1875234.
var goldenEnvelopeReserved = []byte{
	0x54, 0x50, 0x4B, 0x42, 0x04, 0xFF, 0x06, 0x07, // 0
	0x08, 0x09, 0x0A, 0x0B, 0x0C, 0x0D, 0x0E, 0x0F, // 8
	0x10, 0x11, 0x12, 0x13, 0x14, 0x15, 0x16, 0x17, // 16
	0x18, 0x19, 0x1A, 0x1B, 0x1C, 0x1D, 0x1E, 0x1F, // 24
	0xFF, 0xFF, 0xFF, 0xFF, 0x34, 0x52, 0x87, 0xD1, // 32
}

// goldenEnvelopeStruct is the decoded form of goldenEnvelope.
func goldenEnvelopeStruct() BlockEnvelope {
	return BlockEnvelope{
		Codec:           0x04,
		RecordHeaderLen: 0x0706,
		BodyLen:         0x0B0A0908,
		UncompressedLen: 0x0F0E0D0C,
		RecordCount:     0x13121110,
		BodyCRC:         0x17161514,
		FirstSeq:        0x1F1E1D1C1B1A1918,
		EnvelopeCRC:     0xA86E765F,
	}
}

func TestEnvelopeConstants(t *testing.T) {
	t.Parallel()

	require.Equal(t, 40, EnvelopeLen)
	require.Equal(t, "TPKB", EnvelopeMagic)
}

func TestAppendBlockEnvelope_Golden(t *testing.T) {
	t.Parallel()

	e := goldenEnvelopeStruct()
	e.EnvelopeCRC = 0xDEADBEEF // ignored on write: Append computes the CRC

	require.Equal(t, goldenEnvelope, AppendBlockEnvelope(nil, &e))
}

func TestAppendBlockEnvelope_FieldOffsets(t *testing.T) {
	t.Parallel()

	e := goldenEnvelopeStruct()
	b := AppendBlockEnvelope(nil, &e)

	require.Len(t, b, EnvelopeLen)
	require.Equal(t, []byte("TPKB"), b[0:4], "magic")
	requireAscendingFields(t, b, []layoutField{
		{"codec", 4, 1},
		{"record_header_len", 6, 2},
		{"body_len", 8, 4},
		{"uncompressed_len", 12, 4},
		{"record_count", 16, 4},
		{"body_crc", 20, 4},
		{"first_seq", 24, 8},
	})
	require.Equal(t, byte(0), b[5], "reserved byte 5")
	require.Equal(t, make([]byte, 4), b[32:36], "reserved bytes 32-35")
	require.Equal(t, uint32(0xA86E765F), binary.LittleEndian.Uint32(b[36:40]), "envelope_crc")
}

func TestAppendBlockEnvelope_Appends(t *testing.T) {
	t.Parallel()

	e := goldenEnvelopeStruct()
	prefix := []byte{0xAA, 0xBB, 0xCC}

	b := AppendBlockEnvelope(cloneBytes(prefix), &e)

	require.Equal(t, prefix, b[:3])
	require.Equal(t, goldenEnvelope, b[3:])
}

func TestUnmarshalBlockEnvelope_Golden(t *testing.T) {
	t.Parallel()

	got, err := UnmarshalBlockEnvelope(goldenEnvelope)
	require.NoError(t, err)
	require.Equal(t, goldenEnvelopeStruct(), got)
}

func TestUnmarshalBlockEnvelope_ReservedIgnored(t *testing.T) {
	t.Parallel()

	got, err := UnmarshalBlockEnvelope(goldenEnvelopeReserved)
	require.NoError(t, err)

	want := goldenEnvelopeStruct()
	want.EnvelopeCRC = 0xD1875234
	require.Equal(t, want, got)
}

func TestBlockEnvelope_RoundTrip(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name string
		e    BlockEnvelope
	}{
		{name: "golden", e: goldenEnvelopeStruct()},
		{name: "minimal", e: BlockEnvelope{RecordHeaderLen: 44, RecordCount: 1}},
		{
			name: "every value at its limit",
			e: BlockEnvelope{
				Codec:           0xFF,
				RecordHeaderLen: 0xFFFF,
				BodyLen:         MaxLen32,
				UncompressedLen: MaxLen32,
				RecordCount:     0xFFFFFFFF,
				BodyCRC:         0xFFFFFFFF,
				FirstSeq:        MaxU64,
			},
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()

			b := AppendBlockEnvelope(nil, &tt.e)
			got, err := UnmarshalBlockEnvelope(b)
			require.NoError(t, err)

			want := tt.e
			want.EnvelopeCRC = binary.LittleEndian.Uint32(b[36:40])
			require.Equal(t, want, got)
		})
	}
}

// An unknown codec is not an envelope error (§2): the value is preserved and the block is reported unreadable later.
func TestUnmarshalBlockEnvelope_UnknownCodec(t *testing.T) {
	t.Parallel()

	b := cloneBytes(goldenEnvelope)
	b[4] = 0xFF

	got, err := UnmarshalBlockEnvelope(reseal(b, 36))
	require.NoError(t, err)
	require.Equal(t, uint8(0xFF), got.Codec)
}

func TestUnmarshalBlockEnvelope_Errors(t *testing.T) {
	t.Parallel()

	// withField returns goldenEnvelope with put applied and envelope_crc recomputed.
	withField := func(put func(b []byte)) func() []byte {
		return func() []byte {
			b := cloneBytes(goldenEnvelope)
			put(b)

			return reseal(b, 36)
		}
	}

	tests := []struct {
		name    string
		b       func() []byte
		field   string
		off     int
		wantErr error
	}{
		{name: "empty", b: func() []byte { return nil }, field: "length", off: 0, wantErr: ErrShort},
		{name: "one byte short", b: func() []byte { return cloneBytes(goldenEnvelope[:39]) }, field: "length", off: 39, wantErr: ErrShort},
		{
			name: "bad magic before bad CRC",
			b: func() []byte {
				b := cloneBytes(goldenEnvelope)
				b[3] = 'C'
				b[36] ^= 0xFF

				return b
			},
			field:   "magic",
			off:     0,
			wantErr: ErrBadMagic,
		},
		{
			name: "bad CRC before bad record_header_len",
			b: func() []byte {
				b := cloneBytes(goldenEnvelope)
				binary.LittleEndian.PutUint16(b[6:], 43)

				return b
			},
			field:   "envelope_crc",
			off:     36,
			wantErr: ErrCRC,
		},
		{
			name: "reserved byte flipped without resealing",
			b: func() []byte {
				b := cloneBytes(goldenEnvelope)
				b[33] = 0x01

				return b
			},
			field:   "envelope_crc",
			off:     36,
			wantErr: ErrCRC,
		},
		{
			name:    "record_header_len 43",
			b:       withField(func(b []byte) { binary.LittleEndian.PutUint16(b[6:], 43) }),
			field:   "record_header_len",
			off:     6,
			wantErr: ErrCorrupt,
		},
		{
			name:    "record_header_len 0",
			b:       withField(func(b []byte) { binary.LittleEndian.PutUint16(b[6:], 0) }),
			field:   "record_header_len",
			off:     6,
			wantErr: ErrCorrupt,
		},
		{
			name:    "record_count 0",
			b:       withField(func(b []byte) { binary.LittleEndian.PutUint32(b[16:], 0) }),
			field:   "record_count",
			off:     16,
			wantErr: ErrCorrupt,
		},
		{
			name:    "body_len over 2^31-1",
			b:       withField(func(b []byte) { binary.LittleEndian.PutUint32(b[8:], 1<<31) }),
			field:   "body_len",
			off:     8,
			wantErr: ErrLimit,
		},
		{
			name:    "uncompressed_len over 2^31-1",
			b:       withField(func(b []byte) { binary.LittleEndian.PutUint32(b[12:], 0xFFFFFFFF) }),
			field:   "uncompressed_len",
			off:     12,
			wantErr: ErrLimit,
		},
		{
			name:    "first_seq over 2^63-1",
			b:       withField(func(b []byte) { binary.LittleEndian.PutUint64(b[24:], 1<<63) }),
			field:   "first_seq",
			off:     24,
			wantErr: ErrLimit,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()

			got, err := UnmarshalBlockEnvelope(tt.b())
			requireOffsetError(t, err, tt.field, tt.off, tt.wantErr)
			require.Equal(t, BlockEnvelope{}, got)
		})
	}
}

func TestBlockEnvelope_Allocs(t *testing.T) {
	e := goldenEnvelopeStruct()
	dst := make([]byte, 0, EnvelopeLen)

	appendAllocs := testing.AllocsPerRun(100, func() {
		dst = AppendBlockEnvelope(dst[:0], &e)
	})
	require.Zero(t, appendAllocs, "AppendBlockEnvelope into a buffer with capacity")

	unmarshalAllocs := testing.AllocsPerRun(100, func() {
		if _, err := UnmarshalBlockEnvelope(goldenEnvelope); err != nil {
			t.Fatal(err)
		}
	})
	require.Zero(t, unmarshalAllocs, "UnmarshalBlockEnvelope")
}
