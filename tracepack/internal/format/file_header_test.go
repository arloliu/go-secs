package format

import (
	"encoding/binary"
	"testing"

	"github.com/stretchr/testify/require"
)

// goldenFileHeader is the file header of the tracepack format specification §4,
// built independently with Python's struct.pack('<8sHHIIIq16s16s12sI', ...) and zlib.crc32.
// Every field except magic, format_major, flags, reserved and header_crc holds the bytes of its own offsets;
// flags holds fileHeaderFlagsMask, its only defined bit (§4);
// header_crc over bytes 0-75 is 0x0BE26CDD.
var goldenFileHeader = []byte{
	0x89, 0x54, 0x50, 0x4B, 0x0D, 0x0A, 0x1A, 0x0A, // 0
	0x01, 0x00, 0x0A, 0x0B, 0x01, 0x00, 0x00, 0x00, // 8
	0x10, 0x11, 0x12, 0x13, 0x14, 0x15, 0x16, 0x17, // 16
	0x18, 0x19, 0x1A, 0x1B, 0x1C, 0x1D, 0x1E, 0x1F, // 24
	0x20, 0x21, 0x22, 0x23, 0x24, 0x25, 0x26, 0x27, // 32
	0x28, 0x29, 0x2A, 0x2B, 0x2C, 0x2D, 0x2E, 0x2F, // 40
	0x30, 0x31, 0x32, 0x33, 0x34, 0x35, 0x36, 0x37, // 48
	0x38, 0x39, 0x3A, 0x3B, 0x3C, 0x3D, 0x3E, 0x3F, // 56
	0x00, 0x00, 0x00, 0x00, 0x00, 0x00, 0x00, 0x00, // 64
	0x00, 0x00, 0x00, 0x00, 0xDD, 0x6C, 0xE2, 0x0B, // 72
}

// goldenFileHeaderReserved is goldenFileHeader with every reserved byte (64-75) set to 0xFF,
// built the same way; its header_crc is 0xCBAE5538.
var goldenFileHeaderReserved = []byte{
	0x89, 0x54, 0x50, 0x4B, 0x0D, 0x0A, 0x1A, 0x0A, // 0
	0x01, 0x00, 0x0A, 0x0B, 0x01, 0x00, 0x00, 0x00, // 8
	0x10, 0x11, 0x12, 0x13, 0x14, 0x15, 0x16, 0x17, // 16
	0x18, 0x19, 0x1A, 0x1B, 0x1C, 0x1D, 0x1E, 0x1F, // 24
	0x20, 0x21, 0x22, 0x23, 0x24, 0x25, 0x26, 0x27, // 32
	0x28, 0x29, 0x2A, 0x2B, 0x2C, 0x2D, 0x2E, 0x2F, // 40
	0x30, 0x31, 0x32, 0x33, 0x34, 0x35, 0x36, 0x37, // 48
	0x38, 0x39, 0x3A, 0x3B, 0x3C, 0x3D, 0x3E, 0x3F, // 56
	0xFF, 0xFF, 0xFF, 0xFF, 0xFF, 0xFF, 0xFF, 0xFF, // 64
	0xFF, 0xFF, 0xFF, 0xFF, 0x38, 0x55, 0xAE, 0xCB, // 72
}

// goldenFileHeaderStruct is the decoded form of goldenFileHeader.
func goldenFileHeaderStruct() FileHeader {
	return FileHeader{
		FormatMajor:      1,
		FormatMinor:      0x0B0A,
		Flags:            fileHeaderFlagsMask,
		PackMetadataLen:  0x13121110,
		PackMetadataCRC:  0x17161514,
		WriterStartUTCNs: 0x1F1E1D1C1B1A1918,
		PackID: UUID{
			0x20, 0x21, 0x22, 0x23, 0x24, 0x25, 0x26, 0x27,
			0x28, 0x29, 0x2A, 0x2B, 0x2C, 0x2D, 0x2E, 0x2F,
		},
		CaptureID: UUID{
			0x30, 0x31, 0x32, 0x33, 0x34, 0x35, 0x36, 0x37,
			0x38, 0x39, 0x3A, 0x3B, 0x3C, 0x3D, 0x3E, 0x3F,
		},
		HeaderCRC: 0x0BE26CDD,
	}
}

func TestFileHeaderConstants(t *testing.T) {
	t.Parallel()

	require.Equal(t, 80, FileHeaderLen)
	require.Equal(t, "\x89TPK\r\n\x1a\n", FileHeaderMagic)
}

func TestAppendFileHeader_Golden(t *testing.T) {
	t.Parallel()

	h := goldenFileHeaderStruct()
	h.HeaderCRC = 0xDEADBEEF // ignored on write: Append computes the CRC

	require.Equal(t, goldenFileHeader, AppendFileHeader(nil, &h))
}

// Flags bits 1-31 are reserved (§4): a caller that sets every bit gets back only bit 0,
// and header_crc is computed over the zeroed bytes, so the output is byte-identical to the golden vector.
func TestAppendFileHeader_FlagsReservedBitsZeroed(t *testing.T) {
	t.Parallel()

	h := goldenFileHeaderStruct()
	h.Flags = 0xFFFFFFFF

	require.Equal(t, goldenFileHeader, AppendFileHeader(nil, &h))
}

func TestAppendFileHeader_FieldOffsets(t *testing.T) {
	t.Parallel()

	h := goldenFileHeaderStruct()
	b := AppendFileHeader(nil, &h)

	require.Len(t, b, FileHeaderLen)
	require.Equal(t, []byte{0x89, 0x54, 0x50, 0x4B, 0x0D, 0x0A, 0x1A, 0x0A}, b[0:8], "magic")
	require.Equal(t, []byte{0x01, 0x00}, b[8:10], "format_major")
	require.Equal(t, fileHeaderFlagsMask, binary.LittleEndian.Uint32(b[12:16]), "flags")
	requireAscendingFields(t, b, []layoutField{
		{"format_minor", 10, 2},
		{"pack_metadata_len", 16, 4},
		{"pack_metadata_crc", 20, 4},
		{"writer_start_utc_ns", 24, 8},
		{"pack_id", 32, 16},
		{"capture_id", 48, 16},
	})
	require.Equal(t, make([]byte, 12), b[64:76], "reserved")
	require.Equal(t, uint32(0x0BE26CDD), binary.LittleEndian.Uint32(b[76:80]), "header_crc")
}

// AppendFileHeader appends to dst without touching the bytes already there.
func TestAppendFileHeader_Appends(t *testing.T) {
	t.Parallel()

	h := goldenFileHeaderStruct()
	prefix := []byte{0xAA, 0xBB}

	b := AppendFileHeader(cloneBytes(prefix), &h)

	require.Equal(t, prefix, b[:2])
	require.Equal(t, goldenFileHeader, b[2:])
}

func TestUnmarshalFileHeader_Golden(t *testing.T) {
	t.Parallel()

	got, err := UnmarshalFileHeader(goldenFileHeader)
	require.NoError(t, err)
	require.Equal(t, goldenFileHeaderStruct(), got)
}

// Reserved bytes are ignored on read (§1): a nonzero reserved range with a matching CRC decodes to the same fields.
func TestUnmarshalFileHeader_ReservedIgnored(t *testing.T) {
	t.Parallel()

	got, err := UnmarshalFileHeader(goldenFileHeaderReserved)
	require.NoError(t, err)

	want := goldenFileHeaderStruct()
	want.HeaderCRC = 0xCBAE5538
	require.Equal(t, want, got)
}

// Only the first 80 bytes are the header; the pack metadata that follows is not read.
func TestUnmarshalFileHeader_LongerBuffer(t *testing.T) {
	t.Parallel()

	b := append(cloneBytes(goldenFileHeader), 0xEE, 0xEE, 0xEE)

	got, err := UnmarshalFileHeader(b)
	require.NoError(t, err)
	require.Equal(t, goldenFileHeaderStruct(), got)
}

func TestFileHeader_RoundTrip(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name string
		h    FileHeader
	}{
		{name: "golden", h: goldenFileHeaderStruct()},
		{name: "minimal", h: FileHeader{FormatMajor: 1}},
		{name: "negative start time", h: FileHeader{FormatMajor: 1, FormatMinor: 7, WriterStartUTCNs: -1}},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()

			b := AppendFileHeader(nil, &tt.h)
			got, err := UnmarshalFileHeader(b)
			require.NoError(t, err)

			want := tt.h
			want.HeaderCRC = binary.LittleEndian.Uint32(b[76:80])
			require.Equal(t, want, got)
		})
	}
}

// Readers accept any minor within their major (§14).
func TestUnmarshalFileHeader_AnyMinor(t *testing.T) {
	t.Parallel()

	b := cloneBytes(goldenFileHeader)
	binary.LittleEndian.PutUint16(b[10:], 0xFFFF)

	got, err := UnmarshalFileHeader(reseal(b, 76))
	require.NoError(t, err)
	require.Equal(t, uint16(0xFFFF), got.FormatMinor)
}

// A reader checks magic, then header_crc, then the version (§4);
// each case breaks one check and every check after it, so only the first failing check may report.
func TestUnmarshalFileHeader_Errors(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name    string
		b       func() []byte
		field   string
		off     int
		wantErr error
	}{
		{
			name:    "empty",
			b:       func() []byte { return nil },
			field:   "length",
			off:     0,
			wantErr: ErrShort,
		},
		{
			name:    "one byte short",
			b:       func() []byte { return cloneBytes(goldenFileHeader[:79]) },
			field:   "length",
			off:     79,
			wantErr: ErrShort,
		},
		{
			name: "bad magic before bad CRC",
			b: func() []byte {
				b := cloneBytes(goldenFileHeader)
				b[0] = 0x88
				b[8] = 2

				return b
			},
			field:   "magic",
			off:     0,
			wantErr: ErrBadMagic,
		},
		{
			name: "text-mode damage to magic",
			b: func() []byte {
				b := cloneBytes(goldenFileHeader)
				b[4] = '\n'

				return reseal(b, 76)
			},
			field:   "magic",
			off:     0,
			wantErr: ErrBadMagic,
		},
		{
			name: "bad CRC before bad version",
			b: func() []byte {
				b := cloneBytes(goldenFileHeader)
				b[8] = 2

				return b
			},
			field:   "header_crc",
			off:     76,
			wantErr: ErrCRC,
		},
		{
			name: "reserved byte flipped without resealing",
			b: func() []byte {
				b := cloneBytes(goldenFileHeader)
				b[70] = 0x01

				return b
			},
			field:   "header_crc",
			off:     76,
			wantErr: ErrCRC,
		},
		{
			name: "stored CRC flipped",
			b: func() []byte {
				b := cloneBytes(goldenFileHeader)
				b[79] ^= 0x80

				return b
			},
			field:   "header_crc",
			off:     76,
			wantErr: ErrCRC,
		},
		{
			name: "format major 2",
			b: func() []byte {
				b := cloneBytes(goldenFileHeader)
				b[8] = 2

				return reseal(b, 76)
			},
			field:   "format_major",
			off:     8,
			wantErr: ErrSchema,
		},
		{
			name: "format major 0",
			b: func() []byte {
				b := cloneBytes(goldenFileHeader)
				b[8] = 0

				return reseal(b, 76)
			},
			field:   "format_major",
			off:     8,
			wantErr: ErrSchema,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()

			got, err := UnmarshalFileHeader(tt.b())
			requireOffsetError(t, err, tt.field, tt.off, tt.wantErr)
			require.Equal(t, FileHeader{}, got)
		})
	}
}

func TestFileHeader_Allocs(t *testing.T) {
	h := goldenFileHeaderStruct()
	dst := make([]byte, 0, FileHeaderLen)

	appendAllocs := testing.AllocsPerRun(100, func() {
		dst = AppendFileHeader(dst[:0], &h)
	})
	require.Zero(t, appendAllocs, "AppendFileHeader into a buffer with capacity")

	unmarshalAllocs := testing.AllocsPerRun(100, func() {
		if _, err := UnmarshalFileHeader(goldenFileHeader); err != nil {
			t.Fatal(err)
		}
	})
	require.Zero(t, unmarshalAllocs, "UnmarshalFileHeader")
}
