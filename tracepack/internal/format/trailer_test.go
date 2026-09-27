package format

import (
	"encoding/binary"
	"testing"

	"github.com/stretchr/testify/require"
)

// goldenTrailer is the trailer of the tracepack format specification §11,
// built independently with Python's struct.pack('<QQQIIQQHBB', ...) + crc + magic and zlib.crc32.
// Every field except trailer_version, trailer_crc and magic holds the bytes of its own offsets;
// trailer_crc over bytes 0-51 is 0xC4BAB61D.
var goldenTrailer = []byte{
	0x00, 0x01, 0x02, 0x03, 0x04, 0x05, 0x06, 0x07, // 0
	0x08, 0x09, 0x0A, 0x0B, 0x0C, 0x0D, 0x0E, 0x0F, // 8
	0x10, 0x11, 0x12, 0x13, 0x14, 0x15, 0x16, 0x17, // 16
	0x18, 0x19, 0x1A, 0x1B, 0x1C, 0x1D, 0x1E, 0x1F, // 24
	0x20, 0x21, 0x22, 0x23, 0x24, 0x25, 0x26, 0x27, // 32
	0x28, 0x29, 0x2A, 0x2B, 0x2C, 0x2D, 0x2E, 0x2F, // 40
	0x01, 0x00, 0x32, 0x33, 0x1D, 0xB6, 0xBA, 0xC4, // 48
	0x54, 0x50, 0x4B, 0x45, 0x4E, 0x44, 0x0D, 0x0A, // 56
}

// goldenTrailerStruct is the decoded form of goldenTrailer.
func goldenTrailerStruct() Trailer {
	return Trailer{
		FooterOffset:          0x0706050403020100,
		FooterLen:             0x0F0E0D0C0B0A0908,
		FooterUncompressedLen: 0x1716151413121110,
		BlockCount:            0x1B1A1918,
		FooterCRC:             0x1F1E1D1C,
		RecordCount:           0x2726252423222120,
		LastSeq:               0x2F2E2D2C2B2A2928,
		TrailerVersion:        1,
		FooterCodec:           0x32,
		Flags:                 0x33,
		TrailerCRC:            0xC4BAB61D,
	}
}

func TestTrailerConstants(t *testing.T) {
	t.Parallel()

	require.Equal(t, 64, TrailerLen)
	require.Equal(t, "TPKEND\r\n", TrailerMagic)
	require.Equal(t, uint16(1), TrailerVersion)
}

func TestAppendTrailer_Golden(t *testing.T) {
	t.Parallel()

	tr := goldenTrailerStruct()
	tr.TrailerCRC = 0xDEADBEEF // ignored on write: Append computes the CRC

	require.Equal(t, goldenTrailer, AppendTrailer(nil, &tr))
}

func TestAppendTrailer_FieldOffsets(t *testing.T) {
	t.Parallel()

	tr := goldenTrailerStruct()
	b := AppendTrailer(nil, &tr)

	require.Len(t, b, TrailerLen)
	requireAscendingFields(t, b, []layoutField{
		{"footer_offset", 0, 8},
		{"footer_len", 8, 8},
		{"footer_uncompressed_len", 16, 8},
		{"block_count", 24, 4},
		{"footer_crc", 28, 4},
		{"record_count", 32, 8},
		{"last_seq", 40, 8},
		{"footer_codec", 50, 1},
		{"flags", 51, 1},
	})
	require.Equal(t, []byte{0x01, 0x00}, b[48:50], "trailer_version")
	require.Equal(t, uint32(0xC4BAB61D), binary.LittleEndian.Uint32(b[52:56]), "trailer_crc")
	require.Equal(t, []byte{0x54, 0x50, 0x4B, 0x45, 0x4E, 0x44, 0x0D, 0x0A}, b[56:64], "magic")
}

func TestAppendTrailer_Appends(t *testing.T) {
	t.Parallel()

	tr := goldenTrailerStruct()
	prefix := []byte{0xAA, 0xBB}

	b := AppendTrailer(cloneBytes(prefix), &tr)

	require.Equal(t, prefix, b[:2])
	require.Equal(t, goldenTrailer, b[2:])
}

func TestUnmarshalTrailer_Golden(t *testing.T) {
	t.Parallel()

	got, err := UnmarshalTrailer(goldenTrailer)
	require.NoError(t, err)
	require.Equal(t, goldenTrailerStruct(), got)
}

func TestTrailer_RoundTrip(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name string
		tr   Trailer
	}{
		{name: "golden", tr: goldenTrailerStruct()},
		{name: "empty pack", tr: Trailer{FooterOffset: 80, TrailerVersion: 1}},
		{
			name: "values at their limits",
			tr: Trailer{
				FooterOffset:          MaxU64,
				FooterLen:             MaxU64,
				FooterUncompressedLen: MaxU64,
				BlockCount:            0xFFFFFFFF,
				FooterCRC:             0xFFFFFFFF,
				RecordCount:           MaxU64,
				LastSeq:               MaxU64,
				TrailerVersion:        1,
				FooterCodec:           0xFF,
				Flags:                 0xFF,
			},
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()

			b := AppendTrailer(nil, &tt.tr)
			got, err := UnmarshalTrailer(b)
			require.NoError(t, err)

			want := tt.tr
			want.TrailerCRC = binary.LittleEndian.Uint32(b[52:56])
			require.Equal(t, want, got)
		})
	}
}

func TestUnmarshalTrailer_Errors(t *testing.T) {
	t.Parallel()

	// withField returns goldenTrailer with put applied and trailer_crc recomputed.
	withField := func(put func(b []byte)) func() []byte {
		return func() []byte {
			b := cloneBytes(goldenTrailer)
			put(b)

			return reseal(b, 52)
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
		{name: "one byte short", b: func() []byte { return cloneBytes(goldenTrailer[:63]) }, field: "length", off: 63, wantErr: ErrShort},
		{
			name: "bad magic before bad CRC",
			b: func() []byte {
				b := cloneBytes(goldenTrailer)
				b[63] = 0x00
				b[52] ^= 0xFF

				return b
			},
			field:   "magic",
			off:     56,
			wantErr: ErrBadMagic,
		},
		{
			name: "bad CRC before bad version",
			b: func() []byte {
				b := cloneBytes(goldenTrailer)
				b[48] = 2

				return b
			},
			field:   "trailer_crc",
			off:     52,
			wantErr: ErrCRC,
		},
		{
			name:    "trailer_version 2",
			b:       withField(func(b []byte) { b[48] = 2 }),
			field:   "trailer_version",
			off:     48,
			wantErr: ErrSchema,
		},
		{
			name:    "trailer_version 0",
			b:       withField(func(b []byte) { b[48] = 0 }),
			field:   "trailer_version",
			off:     48,
			wantErr: ErrSchema,
		},
		{
			name:    "footer_offset over 2^63-1",
			b:       withField(func(b []byte) { binary.LittleEndian.PutUint64(b[0:], 1<<63) }),
			field:   "footer_offset",
			off:     0,
			wantErr: ErrLimit,
		},
		{
			name:    "footer_len over 2^63-1",
			b:       withField(func(b []byte) { binary.LittleEndian.PutUint64(b[8:], 1<<63) }),
			field:   "footer_len",
			off:     8,
			wantErr: ErrLimit,
		},
		{
			name:    "footer_uncompressed_len over 2^63-1",
			b:       withField(func(b []byte) { binary.LittleEndian.PutUint64(b[16:], 1<<63) }),
			field:   "footer_uncompressed_len",
			off:     16,
			wantErr: ErrLimit,
		},
		{
			name:    "record_count over 2^63-1",
			b:       withField(func(b []byte) { binary.LittleEndian.PutUint64(b[32:], 1<<64-1) }),
			field:   "record_count",
			off:     32,
			wantErr: ErrLimit,
		},
		{
			name:    "last_seq over 2^63-1",
			b:       withField(func(b []byte) { binary.LittleEndian.PutUint64(b[40:], 1<<63) }),
			field:   "last_seq",
			off:     40,
			wantErr: ErrLimit,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()

			got, err := UnmarshalTrailer(tt.b())
			requireOffsetError(t, err, tt.field, tt.off, tt.wantErr)
			require.Equal(t, Trailer{}, got)
		})
	}
}

func TestTrailer_Allocs(t *testing.T) {
	tr := goldenTrailerStruct()
	dst := make([]byte, 0, TrailerLen)

	appendAllocs := testing.AllocsPerRun(100, func() {
		dst = AppendTrailer(dst[:0], &tr)
	})
	require.Zero(t, appendAllocs, "AppendTrailer into a buffer with capacity")

	unmarshalAllocs := testing.AllocsPerRun(100, func() {
		if _, err := UnmarshalTrailer(goldenTrailer); err != nil {
			t.Fatal(err)
		}
	})
	require.Zero(t, unmarshalAllocs, "UnmarshalTrailer")
}
