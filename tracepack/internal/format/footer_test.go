package format

import (
	"encoding/binary"
	"testing"

	"github.com/stretchr/testify/require"
)

// goldenFooterPrologue is the F-1 footer prologue of the tracepack format specification §10,
// built independently with Python's struct.pack('<HHIIIQQQQQQQ', ...).
// Every field except footer_layout_version and flags holds the bytes of its own offsets;
// flags holds footerPrologueFlagsMask, its two defined bits (§10).
var goldenFooterPrologue = []byte{
	0x01, 0x00, 0x03, 0x00, 0x04, 0x05, 0x06, 0x07, // 0
	0x08, 0x09, 0x0A, 0x0B, 0x0C, 0x0D, 0x0E, 0x0F, // 8
	0x10, 0x11, 0x12, 0x13, 0x14, 0x15, 0x16, 0x17, // 16
	0x18, 0x19, 0x1A, 0x1B, 0x1C, 0x1D, 0x1E, 0x1F, // 24
	0x20, 0x21, 0x22, 0x23, 0x24, 0x25, 0x26, 0x27, // 32
	0x28, 0x29, 0x2A, 0x2B, 0x2C, 0x2D, 0x2E, 0x2F, // 40
	0x30, 0x31, 0x32, 0x33, 0x34, 0x35, 0x36, 0x37, // 48
	0x38, 0x39, 0x3A, 0x3B, 0x3C, 0x3D, 0x3E, 0x3F, // 56
	0x40, 0x41, 0x42, 0x43, 0x44, 0x45, 0x46, 0x47, // 64
}

// goldenF2Entry is an F-2 block index entry of the tracepack format specification §10,
// built independently with Python's struct.pack('<QIIIIQQqqIIQIH2s', ...).
// Every field except reserved holds the bytes of its own offsets.
var goldenF2Entry = []byte{
	0x00, 0x01, 0x02, 0x03, 0x04, 0x05, 0x06, 0x07, // 0
	0x08, 0x09, 0x0A, 0x0B, 0x0C, 0x0D, 0x0E, 0x0F, // 8
	0x10, 0x11, 0x12, 0x13, 0x14, 0x15, 0x16, 0x17, // 16
	0x18, 0x19, 0x1A, 0x1B, 0x1C, 0x1D, 0x1E, 0x1F, // 24
	0x20, 0x21, 0x22, 0x23, 0x24, 0x25, 0x26, 0x27, // 32
	0x28, 0x29, 0x2A, 0x2B, 0x2C, 0x2D, 0x2E, 0x2F, // 40
	0x30, 0x31, 0x32, 0x33, 0x34, 0x35, 0x36, 0x37, // 48
	0x38, 0x39, 0x3A, 0x3B, 0x3C, 0x3D, 0x3E, 0x3F, // 56
	0x40, 0x41, 0x42, 0x43, 0x44, 0x45, 0x46, 0x47, // 64
	0x48, 0x49, 0x4A, 0x4B, 0x4C, 0x4D, 0x00, 0x00, // 72
}

// goldenF2EntryExtended is an 88-byte F-2 entry built the same way:
// goldenF2Entry with both reserved bytes set to 0xFF, followed by 8 bytes a longer f2_entry_len adds.
var goldenF2EntryExtended = []byte{
	0x00, 0x01, 0x02, 0x03, 0x04, 0x05, 0x06, 0x07, // 0
	0x08, 0x09, 0x0A, 0x0B, 0x0C, 0x0D, 0x0E, 0x0F, // 8
	0x10, 0x11, 0x12, 0x13, 0x14, 0x15, 0x16, 0x17, // 16
	0x18, 0x19, 0x1A, 0x1B, 0x1C, 0x1D, 0x1E, 0x1F, // 24
	0x20, 0x21, 0x22, 0x23, 0x24, 0x25, 0x26, 0x27, // 32
	0x28, 0x29, 0x2A, 0x2B, 0x2C, 0x2D, 0x2E, 0x2F, // 40
	0x30, 0x31, 0x32, 0x33, 0x34, 0x35, 0x36, 0x37, // 48
	0x38, 0x39, 0x3A, 0x3B, 0x3C, 0x3D, 0x3E, 0x3F, // 56
	0x40, 0x41, 0x42, 0x43, 0x44, 0x45, 0x46, 0x47, // 64
	0x48, 0x49, 0x4A, 0x4B, 0x4C, 0x4D, 0xFF, 0xFF, // 72
	0xEE, 0xEE, 0xEE, 0xEE, 0xEE, 0xEE, 0xEE, 0xEE, // 80
}

// goldenFooterPrologueStruct is the decoded form of goldenFooterPrologue.
func goldenFooterPrologueStruct() FooterPrologue {
	return FooterPrologue{
		FooterLayoutVersion: 1,
		Flags:               footerPrologueFlagsMask,
		BlockCount:          0x07060504,
		F2EntryLen:          0x0B0A0908,
		ExtractionVersion:   0x0F0E0D0C,
		F2Offset:            0x1716151413121110,
		F3Offset:            0x1F1E1D1C1B1A1918,
		F3Len:               0x2726252423222120,
		F4Offset:            0x2F2E2D2C2B2A2928,
		F4Len:               0x3736353433323130,
		F5Offset:            0x3F3E3D3C3B3A3938,
		F5Len:               0x4746454443424140,
	}
}

// goldenF2EntryStruct is the decoded form of goldenF2Entry.
func goldenF2EntryStruct() F2Entry {
	return F2Entry{
		Offset:          0x0706050403020100,
		OnDiskLen:       0x0B0A0908,
		UncompressedLen: 0x0F0E0D0C,
		RecordCount:     0x13121110,
		BodyCRC:         0x17161514,
		FirstSeq:        0x1F1E1D1C1B1A1918,
		LastSeq:         0x2726252423222120,
		TSMin:           0x2F2E2D2C2B2A2928,
		TSMax:           0x3736353433323130,
		EpochMin:        0x3B3A3938,
		EpochMax:        0x3F3E3D3C,
		SummaryOffset:   0x4746454443424140,
		SummaryLen:      0x4B4A4948,
		RecordHeaderLen: 0x4D4C,
	}
}

func TestFooterConstants(t *testing.T) {
	t.Parallel()

	require.Equal(t, 72, FooterPrologueLen)
	require.Equal(t, 80, F2EntryLen)
	require.Equal(t, uint16(1), FooterLayoutVersion)
}

func TestAppendFooterPrologue_Golden(t *testing.T) {
	t.Parallel()

	p := goldenFooterPrologueStruct()

	require.Equal(t, goldenFooterPrologue, AppendFooterPrologue(nil, &p))
}

// Flags bits 2-15 are reserved (§10): a caller that sets every bit gets back only bits 0 and 1,
// so the output is byte-identical to the golden vector.
func TestAppendFooterPrologue_FlagsReservedBitsZeroed(t *testing.T) {
	t.Parallel()

	p := goldenFooterPrologueStruct()
	p.Flags = 0xFFFF

	require.Equal(t, goldenFooterPrologue, AppendFooterPrologue(nil, &p))
}

func TestAppendFooterPrologue_FieldOffsets(t *testing.T) {
	t.Parallel()

	p := goldenFooterPrologueStruct()
	b := AppendFooterPrologue(nil, &p)

	require.Len(t, b, FooterPrologueLen)
	require.Equal(t, []byte{0x01, 0x00}, b[0:2], "footer_layout_version")
	require.Equal(t, footerPrologueFlagsMask, binary.LittleEndian.Uint16(b[2:4]), "flags")
	requireAscendingFields(t, b, []layoutField{
		{"block_count", 4, 4},
		{"f2_entry_len", 8, 4},
		{"extraction_version", 12, 4},
		{"f2_offset", 16, 8},
		{"f3_offset", 24, 8},
		{"f3_len", 32, 8},
		{"f4_offset", 40, 8},
		{"f4_len", 48, 8},
		{"f5_offset", 56, 8},
		{"f5_len", 64, 8},
	})
}

func TestAppendFooterPrologue_Appends(t *testing.T) {
	t.Parallel()

	p := goldenFooterPrologueStruct()
	prefix := []byte{0xAA}

	b := AppendFooterPrologue(cloneBytes(prefix), &p)

	require.Equal(t, prefix, b[:1])
	require.Equal(t, goldenFooterPrologue, b[1:])
}

func TestUnmarshalFooterPrologue_Golden(t *testing.T) {
	t.Parallel()

	got, err := UnmarshalFooterPrologue(goldenFooterPrologue)
	require.NoError(t, err)
	require.Equal(t, goldenFooterPrologueStruct(), got)
}

func TestFooterPrologue_RoundTrip(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name string
		p    FooterPrologue
	}{
		{name: "golden", p: goldenFooterPrologueStruct()},
		{name: "empty pack", p: FooterPrologue{FooterLayoutVersion: 1, Flags: 1, F2EntryLen: 80, F2Offset: 72, F3Offset: 72, F5Offset: 72}},
		{
			name: "values at their limits",
			p: FooterPrologue{
				FooterLayoutVersion: 1,
				Flags:               0xFFFF,
				BlockCount:          0xFFFFFFFF,
				F2EntryLen:          0xFFFFFFFF,
				ExtractionVersion:   0xFFFFFFFF,
				F2Offset:            MaxU64,
				F3Offset:            MaxU64,
				F3Len:               MaxU64,
				F4Offset:            MaxU64,
				F4Len:               MaxU64,
				F5Offset:            MaxU64,
				F5Len:               MaxU64,
			},
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()

			got, err := UnmarshalFooterPrologue(AppendFooterPrologue(nil, &tt.p))
			require.NoError(t, err)

			want := tt.p
			want.Flags &= footerPrologueFlagsMask // bits 2-15 are reserved (§10) and zeroed on write
			require.Equal(t, want, got)
		})
	}
}

func TestUnmarshalFooterPrologue_Errors(t *testing.T) {
	t.Parallel()

	withField := func(put func(b []byte)) func() []byte {
		return func() []byte {
			b := cloneBytes(goldenFooterPrologue)
			put(b)

			return b
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
		{name: "one byte short", b: func() []byte { return cloneBytes(goldenFooterPrologue[:71]) }, field: "length", off: 71, wantErr: ErrShort},
		{
			name: "layout version 2 before bad f2_entry_len",
			b: withField(func(b []byte) {
				b[0] = 2
				binary.LittleEndian.PutUint32(b[8:], 0)
			}),
			field:   "footer_layout_version",
			off:     0,
			wantErr: ErrSchema,
		},
		{name: "layout version 0", b: withField(func(b []byte) { b[0] = 0 }), field: "footer_layout_version", off: 0, wantErr: ErrSchema},
		{
			name:    "f2_entry_len 79",
			b:       withField(func(b []byte) { binary.LittleEndian.PutUint32(b[8:], 79) }),
			field:   "f2_entry_len",
			off:     8,
			wantErr: ErrCorrupt,
		},
		{
			name:    "f2_entry_len 0",
			b:       withField(func(b []byte) { binary.LittleEndian.PutUint32(b[8:], 0) }),
			field:   "f2_entry_len",
			off:     8,
			wantErr: ErrCorrupt,
		},
		{name: "f2_offset over 2^63-1", b: withField(func(b []byte) { b[23] = 0x80 }), field: "f2_offset", off: 16, wantErr: ErrLimit},
		{name: "f3_offset over 2^63-1", b: withField(func(b []byte) { b[31] = 0x80 }), field: "f3_offset", off: 24, wantErr: ErrLimit},
		{name: "f3_len over 2^63-1", b: withField(func(b []byte) { b[39] = 0x80 }), field: "f3_len", off: 32, wantErr: ErrLimit},
		{name: "f4_offset over 2^63-1", b: withField(func(b []byte) { b[47] = 0x80 }), field: "f4_offset", off: 40, wantErr: ErrLimit},
		{name: "f4_len over 2^63-1", b: withField(func(b []byte) { b[55] = 0x80 }), field: "f4_len", off: 48, wantErr: ErrLimit},
		{name: "f5_offset over 2^63-1", b: withField(func(b []byte) { b[63] = 0x80 }), field: "f5_offset", off: 56, wantErr: ErrLimit},
		{name: "f5_len over 2^63-1", b: withField(func(b []byte) { b[71] = 0x80 }), field: "f5_len", off: 64, wantErr: ErrLimit},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()

			got, err := UnmarshalFooterPrologue(tt.b())
			requireOffsetError(t, err, tt.field, tt.off, tt.wantErr)
			require.Equal(t, FooterPrologue{}, got)
		})
	}
}

func TestAppendF2Entry_Golden(t *testing.T) {
	t.Parallel()

	e := goldenF2EntryStruct()

	require.Equal(t, goldenF2Entry, AppendF2Entry(nil, &e))
}

func TestAppendF2Entry_FieldOffsets(t *testing.T) {
	t.Parallel()

	e := goldenF2EntryStruct()
	b := AppendF2Entry(nil, &e)

	require.Len(t, b, F2EntryLen)
	requireAscendingFields(t, b, []layoutField{
		{"offset", 0, 8},
		{"on_disk_len", 8, 4},
		{"uncompressed_len", 12, 4},
		{"record_count", 16, 4},
		{"body_crc", 20, 4},
		{"first_seq", 24, 8},
		{"last_seq", 32, 8},
		{"ts_min", 40, 8},
		{"ts_max", 48, 8},
		{"epoch_min", 56, 4},
		{"epoch_max", 60, 4},
		{"summary_offset", 64, 8},
		{"summary_len", 72, 4},
		{"record_header_len", 76, 2},
	})
	require.Equal(t, []byte{0, 0}, b[78:80], "reserved")
}

func TestAppendF2Entry_Appends(t *testing.T) {
	t.Parallel()

	e := goldenF2EntryStruct()
	prefix := []byte{0xAA, 0xBB}

	b := AppendF2Entry(cloneBytes(prefix), &e)

	require.Equal(t, prefix, b[:2])
	require.Equal(t, goldenF2Entry, b[2:])
}

func TestUnmarshalF2Entry_Golden(t *testing.T) {
	t.Parallel()

	got, err := UnmarshalF2Entry(goldenF2Entry, F2EntryLen)
	require.NoError(t, err)
	require.Equal(t, goldenF2EntryStruct(), got)
}

// A longer f2_entry_len adds bytes the reader skips (§10), and nonzero reserved bytes are ignored (§1).
func TestUnmarshalF2Entry_Extended(t *testing.T) {
	t.Parallel()

	got, err := UnmarshalF2Entry(goldenF2EntryExtended, len(goldenF2EntryExtended))
	require.NoError(t, err)
	require.Equal(t, goldenF2EntryStruct(), got)
}

// Entries are read at a stride of f2_entry_len from the F-2 section; each decodes independently.
func TestUnmarshalF2Entry_Stride(t *testing.T) {
	t.Parallel()

	const stride = 88

	second := goldenF2EntryStruct()
	second.Offset++
	second.FirstSeq++

	section := cloneBytes(goldenF2EntryExtended)
	section = AppendF2Entry(section, &second)
	section = append(section, make([]byte, stride-F2EntryLen)...)
	require.Len(t, section, 2*stride)

	first, err := UnmarshalF2Entry(section[0:], stride)
	require.NoError(t, err)
	require.Equal(t, goldenF2EntryStruct(), first)

	got, err := UnmarshalF2Entry(section[stride:], stride)
	require.NoError(t, err)
	require.Equal(t, second, got)
}

func TestF2Entry_RoundTrip(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name string
		e    F2Entry
	}{
		{name: "golden", e: goldenF2EntryStruct()},
		{name: "minimal", e: F2Entry{RecordCount: 1, SummaryLen: 1, RecordHeaderLen: 44}},
		{
			name: "values at their limits",
			e: F2Entry{
				Offset:          MaxU64,
				OnDiskLen:       EnvelopeLen + MaxLen32,
				UncompressedLen: MaxLen32,
				RecordCount:     0xFFFFFFFF,
				BodyCRC:         0xFFFFFFFF,
				FirstSeq:        MaxU64,
				LastSeq:         MaxU64,
				TSMin:           -1 << 63,
				TSMax:           1<<63 - 1,
				EpochMin:        0xFFFFFFFF,
				EpochMax:        0xFFFFFFFF,
				SummaryOffset:   MaxU64,
				SummaryLen:      0xFFFFFFFF,
				RecordHeaderLen: 0xFFFF,
			},
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()

			got, err := UnmarshalF2Entry(AppendF2Entry(nil, &tt.e), F2EntryLen)
			require.NoError(t, err)
			require.Equal(t, tt.e, got)
		})
	}
}

func TestUnmarshalF2Entry_Errors(t *testing.T) {
	t.Parallel()

	withField := func(put func(b []byte)) func() []byte {
		return func() []byte {
			b := cloneBytes(goldenF2Entry)
			put(b)

			return b
		}
	}

	tests := []struct {
		name       string
		b          func() []byte
		f2EntryLen int
		field      string
		off        int
		wantErr    error
	}{
		{name: "f2_entry_len 79", b: withField(func([]byte) {}), f2EntryLen: 79, field: "f2_entry_len", off: 0, wantErr: ErrCorrupt},
		{name: "f2_entry_len 0 on an empty buffer", b: func() []byte { return nil }, f2EntryLen: 0, field: "f2_entry_len", off: 0, wantErr: ErrCorrupt},
		{name: "negative f2_entry_len", b: withField(func([]byte) {}), f2EntryLen: -80, field: "f2_entry_len", off: 0, wantErr: ErrCorrupt},
		{
			name:       "one byte short of 80",
			b:          func() []byte { return cloneBytes(goldenF2Entry[:79]) },
			f2EntryLen: F2EntryLen,
			field:      "length",
			off:        79,
			wantErr:    ErrShort,
		},
		{
			name:       "known bytes present but entry short",
			b:          func() []byte { return cloneBytes(goldenF2EntryExtended[:84]) },
			f2EntryLen: 88,
			field:      "length",
			off:        84,
			wantErr:    ErrShort,
		},
		{
			name:       "record_header_len 43",
			b:          withField(func(b []byte) { binary.LittleEndian.PutUint16(b[76:], 43) }),
			f2EntryLen: F2EntryLen,
			field:      "record_header_len",
			off:        76,
			wantErr:    ErrCorrupt,
		},
		{
			name:       "record_header_len 0",
			b:          withField(func(b []byte) { binary.LittleEndian.PutUint16(b[76:], 0) }),
			f2EntryLen: F2EntryLen,
			field:      "record_header_len",
			off:        76,
			wantErr:    ErrCorrupt,
		},
		{name: "offset over 2^63-1", b: withField(func(b []byte) { b[7] = 0x80 }), f2EntryLen: F2EntryLen, field: "offset", off: 0, wantErr: ErrLimit},
		{
			name:       "on_disk_len over the envelope plus 2^31-1",
			b:          withField(func(b []byte) { binary.LittleEndian.PutUint32(b[8:], EnvelopeLen+MaxLen32+1) }),
			f2EntryLen: F2EntryLen,
			field:      "on_disk_len",
			off:        8,
			wantErr:    ErrLimit,
		},
		{name: "uncompressed_len over 2^31-1", b: withField(func(b []byte) { b[15] = 0x80 }), f2EntryLen: F2EntryLen, field: "uncompressed_len", off: 12, wantErr: ErrLimit},
		{name: "first_seq over 2^63-1", b: withField(func(b []byte) { b[31] = 0x80 }), f2EntryLen: F2EntryLen, field: "first_seq", off: 24, wantErr: ErrLimit},
		{name: "last_seq over 2^63-1", b: withField(func(b []byte) { b[39] = 0x80 }), f2EntryLen: F2EntryLen, field: "last_seq", off: 32, wantErr: ErrLimit},
		{name: "summary_offset over 2^63-1", b: withField(func(b []byte) { b[71] = 0x80 }), f2EntryLen: F2EntryLen, field: "summary_offset", off: 64, wantErr: ErrLimit},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()

			got, err := UnmarshalF2Entry(tt.b(), tt.f2EntryLen)
			requireOffsetError(t, err, tt.field, tt.off, tt.wantErr)
			require.Equal(t, F2Entry{}, got)
		})
	}
}

// The expected products are plain arithmetic: 2^31-1 is prime, so (2^31-1) × 1 is the only product exactly at the limit,
// and 65535 × 32768 = 2147450880 and 65535 × 32769 = 2147516415 bracket it with the largest record_header_len.
func TestHeaderSectionLen(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name            string
		recordCount     uint32
		recordHeaderLen uint16
		want            int
		wantErr         bool
	}{
		{name: "no records", recordCount: 0, recordHeaderLen: 44, want: 0},
		{name: "one record", recordCount: 1, recordHeaderLen: 44, want: 44},
		{name: "typical block", recordCount: 1000, recordHeaderLen: 44, want: 44000},
		{name: "exactly at the limit", recordCount: 2147483647, recordHeaderLen: 1, want: 2147483647},
		{name: "largest header just under", recordCount: 65535, recordHeaderLen: 32768, want: 2147450880},
		{name: "largest header just over", recordCount: 65535, recordHeaderLen: 32769, wantErr: true},
		{name: "44-byte headers just under", recordCount: 48806446, recordHeaderLen: 44, want: 2147483624},
		{name: "44-byte headers just over", recordCount: 48806447, recordHeaderLen: 44, wantErr: true},
		{name: "both at their type maximum", recordCount: 0xFFFFFFFF, recordHeaderLen: 0xFFFF, wantErr: true},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()

			got, err := HeaderSectionLen(tt.recordCount, tt.recordHeaderLen)
			if tt.wantErr {
				requireOffsetError(t, err, "record_count", 16, ErrLimit)
				require.Zero(t, got)

				return
			}

			require.NoError(t, err)
			require.Equal(t, tt.want, got)
		})
	}
}

func TestFooter_Allocs(t *testing.T) {
	p := goldenFooterPrologueStruct()
	e := goldenF2EntryStruct()
	dst := make([]byte, 0, F2EntryLen)

	appendAllocs := testing.AllocsPerRun(100, func() {
		dst = AppendFooterPrologue(dst[:0], &p)
		dst = AppendF2Entry(dst[:0], &e)
	})
	require.Zero(t, appendAllocs, "AppendFooterPrologue and AppendF2Entry into a buffer with capacity")

	unmarshalAllocs := testing.AllocsPerRun(100, func() {
		if _, err := UnmarshalFooterPrologue(goldenFooterPrologue); err != nil {
			t.Fatal(err)
		}

		if _, err := UnmarshalF2Entry(goldenF2EntryExtended, len(goldenF2EntryExtended)); err != nil {
			t.Fatal(err)
		}

		if _, err := HeaderSectionLen(1000, 44); err != nil {
			t.Fatal(err)
		}
	})
	require.Zero(t, unmarshalAllocs, "UnmarshalFooterPrologue, UnmarshalF2Entry and HeaderSectionLen")
}
