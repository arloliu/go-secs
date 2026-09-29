// Fuzz targets for the decoders of the fixed byte layouts.
//
// The unit tests cover the golden vectors and one mutation per check deterministically;
// these targets feed arbitrary bytes to every Unmarshal function, to HeaderSectionLen and to ParseUUID.
// Each target asserts that the decoder never panics,
// that a failure is an *OffsetError wrapping one of the package's sentinel errors with the zero value alongside it,
// and that a successful decode re-encodes with the matching Append function to bytes that decode to an equal value.
// Equality is on the decoded struct, not on the bytes,
// because a decoder ignores reserved bytes that its Append function writes as zero.
//
// The file header, block envelope and trailer check their CRC before any other field,
// and a coverage-guided fuzzer does not find a matching CRC-32 by mutation.
// Their targets therefore decode each input twice: as given, which reaches the CRC mismatch path,
// and with the CRC recomputed over the mutated bytes, which reaches every check behind it.
package format

import (
	"encoding/binary"
	"errors"
	"math"
	"strings"
	"testing"

	"github.com/stretchr/testify/require"
)

// maxFuzzLayoutLen caps a fuzzed record_header_len or f2_entry_len,
// the largest value the u16 record_header_len field can declare.
const maxFuzzLayoutLen = math.MaxUint16

// capFuzzLayoutLen folds a fuzzed length above maxFuzzLayoutLen back into [0, maxFuzzLayoutLen],
// so a mutation of a large value still explores distinct lengths instead of collapsing onto the cap.
// A negative length is returned unchanged: the decoders must reject it themselves.
func capFuzzLayoutLen(n int) int {
	if n > maxFuzzLayoutLen {
		return n % (maxFuzzLayoutLen + 1)
	}

	return n
}

// sealedVariant returns a copy of golden with put applied and the CRC at crcOff recomputed,
// so a seed reaches the checks that follow the CRC.
func sealedVariant(golden []byte, crcOff int, put func(b []byte)) []byte {
	b := cloneBytes(golden)
	put(b)

	return reseal(b, crcOff)
}

// requireDecodeError asserts that err is an *OffsetError wrapping one of the package's sentinel errors.
func requireDecodeError(t *testing.T, err error) {
	t.Helper()

	var oe *OffsetError
	require.ErrorAs(t, err, &oe)

	sentinels := []error{ErrShort, ErrBadMagic, ErrCRC, ErrSchema, ErrLimit, ErrCorrupt}
	for _, s := range sentinels {
		if errors.Is(err, s) {
			return
		}
	}
	require.Failf(t, "unexpected error", "%v wraps none of the package's sentinel errors", err)
}

// fuzzResealed calls check on b as given and, when b holds at least n bytes,
// on a copy of its first n bytes with the CRC at crcOff recomputed.
func fuzzResealed(t *testing.T, b []byte, n, crcOff int, check func(t *testing.T, b []byte)) {
	t.Helper()

	check(t, b)
	if len(b) >= n {
		check(t, reseal(cloneBytes(b[:n]), crcOff))
	}
}

// FuzzUnmarshalFileHeader fuzzes UnmarshalFileHeader.
//
// On success, AppendFileHeader of the result decodes to an equal header, apart from header_crc,
// and reproduces bytes 0-63 of the input verbatim.
// FileHeader holds no slice, so there is no allocation to bound.
func FuzzUnmarshalFileHeader(f *testing.F) {
	f.Add(cloneBytes(goldenFileHeader))
	f.Add(cloneBytes(goldenFileHeaderReserved))
	f.Add(append(cloneBytes(goldenFileHeader), 0xEE, 0xEE))
	f.Add(cloneBytes(goldenFileHeader[:FileHeaderLen-1]))
	f.Add([]byte{})
	f.Add(sealedVariant(goldenFileHeader, fileHeaderCRCOff, func(b []byte) { b[0] ^= 0xFF }))
	f.Add(sealedVariant(goldenFileHeader, fileHeaderCRCOff, func(b []byte) { b[fileHeaderMajorOff] = 2 }))
	f.Add(sealedVariant(goldenFileHeader, fileHeaderCRCOff, func(b []byte) { b[fileHeaderMajorOff] = 0 }))
	f.Add(sealedVariant(goldenFileHeader, fileHeaderCRCOff, func(b []byte) {
		binary.LittleEndian.PutUint16(b[10:], math.MaxUint16)
		binary.LittleEndian.PutUint32(b[16:], math.MaxUint32)
	}))
	flipped := cloneBytes(goldenFileHeader)
	flipped[fileHeaderCRCOff] ^= 0x01
	f.Add(flipped)

	f.Fuzz(func(t *testing.T, b []byte) {
		fuzzResealed(t, b, FileHeaderLen, fileHeaderCRCOff, checkFileHeader)
	})
}

func checkFileHeader(t *testing.T, b []byte) {
	t.Helper()

	h, err := UnmarshalFileHeader(b)
	if err != nil {
		requireDecodeError(t, err)
		require.Equal(t, FileHeader{}, h)

		return
	}

	enc := AppendFileHeader(nil, &h)
	require.Len(t, enc, FileHeaderLen)
	require.Equal(t, b[:12], enc[:12], "every named field before flags is written verbatim")
	require.Equal(t, h.Flags&fileHeaderFlagsMask, binary.LittleEndian.Uint32(enc[12:16]), "flags keeps only its defined bit (§4)")
	require.Equal(t, b[16:64], enc[16:64], "every named field after flags is written verbatim")

	got, err := UnmarshalFileHeader(enc)
	require.NoError(t, err)
	require.Equal(t, CRC(enc[:fileHeaderCRCOff]), got.HeaderCRC)

	h.HeaderCRC, got.HeaderCRC = 0, 0
	h.Flags &= fileHeaderFlagsMask
	require.Equal(t, h, got)
}

// FuzzUnmarshalBlockEnvelope fuzzes UnmarshalBlockEnvelope.
//
// On success, AppendBlockEnvelope of the result decodes to an equal envelope, apart from envelope_crc,
// and reproduces bytes 0-4 and 6-31 of the input verbatim.
// BlockEnvelope holds no slice, so there is no allocation to bound.
func FuzzUnmarshalBlockEnvelope(f *testing.F) {
	f.Add(cloneBytes(goldenEnvelope))
	f.Add(cloneBytes(goldenEnvelopeReserved))
	f.Add(append(cloneBytes(goldenEnvelope), 0xEE))
	f.Add(cloneBytes(goldenEnvelope[:EnvelopeLen-1]))
	f.Add([]byte{})
	f.Add(sealedVariant(goldenEnvelope, envelopeCRCOff, func(b []byte) { b[3] = 'X' }))
	f.Add(sealedVariant(goldenEnvelope, envelopeCRCOff, func(b []byte) {
		binary.LittleEndian.PutUint16(b[envelopeRecordHeaderLenOff:], RecordHeaderLen-1)
	}))
	f.Add(sealedVariant(goldenEnvelope, envelopeCRCOff, func(b []byte) {
		binary.LittleEndian.PutUint16(b[envelopeRecordHeaderLenOff:], RecordHeaderLen)
	}))
	f.Add(sealedVariant(goldenEnvelope, envelopeCRCOff, func(b []byte) {
		binary.LittleEndian.PutUint32(b[envelopeRecordCountOff:], 0)
	}))
	f.Add(sealedVariant(goldenEnvelope, envelopeCRCOff, func(b []byte) {
		binary.LittleEndian.PutUint32(b[envelopeBodyLenOff:], MaxLen32+1)
	}))
	f.Add(sealedVariant(goldenEnvelope, envelopeCRCOff, func(b []byte) {
		binary.LittleEndian.PutUint32(b[envelopeUncompressedLenOff:], MaxLen32)
	}))
	f.Add(sealedVariant(goldenEnvelope, envelopeCRCOff, func(b []byte) {
		binary.LittleEndian.PutUint64(b[envelopeFirstSeqOff:], MaxU64+1)
	}))
	flipped := cloneBytes(goldenEnvelope)
	flipped[envelopeCRCOff] ^= 0x01
	f.Add(flipped)

	f.Fuzz(func(t *testing.T, b []byte) {
		fuzzResealed(t, b, EnvelopeLen, envelopeCRCOff, checkBlockEnvelope)
	})
}

func checkBlockEnvelope(t *testing.T, b []byte) {
	t.Helper()

	e, err := UnmarshalBlockEnvelope(b)
	if err != nil {
		requireDecodeError(t, err)
		require.Equal(t, BlockEnvelope{}, e)

		return
	}

	require.GreaterOrEqual(t, int(e.RecordHeaderLen), RecordHeaderLen)
	require.NotZero(t, e.RecordCount)
	require.LessOrEqual(t, e.BodyLen, MaxLen32)
	require.LessOrEqual(t, e.UncompressedLen, MaxLen32)
	require.LessOrEqual(t, e.FirstSeq, MaxU64)

	enc := AppendBlockEnvelope(nil, &e)
	require.Len(t, enc, EnvelopeLen)
	require.Equal(t, b[:5], enc[:5], "magic and codec are written verbatim")
	require.Equal(t, b[6:32], enc[6:32], "every named field is written verbatim")

	got, err := UnmarshalBlockEnvelope(enc)
	require.NoError(t, err)
	require.Equal(t, CRC(enc[:envelopeCRCOff]), got.EnvelopeCRC)

	e.EnvelopeCRC, got.EnvelopeCRC = 0, 0
	require.Equal(t, e, got)
}

// FuzzUnmarshalRecordHeader fuzzes UnmarshalRecordHeader with a fuzzed record_header_len,
// capped at 65535 and passed through when negative.
//
// On success, Extra holds exactly the bytes from offset 44 to record_header_len, is nil when there are none,
// and never aliases the input;
// AppendRecordHeader of the result is record_header_len bytes long, reproduces every byte but the retired and reserved bits,
// and decodes to an equal header.
// Allocation bound: cap(Extra) ≤ 2×len(input), since the copy is rounded up to an allocator size class.
func FuzzUnmarshalRecordHeader(f *testing.F) {
	f.Add(cloneBytes(goldenRecordHeader), RecordHeaderLen)
	f.Add(cloneBytes(goldenRecordHeaderExtended), len(goldenRecordHeaderExtended))
	f.Add(cloneBytes(goldenRecordHeaderExtended), RecordHeaderLen)
	f.Add(cloneBytes(goldenRecordHeader), RecordHeaderLen-1)
	f.Add(cloneBytes(goldenRecordHeader), RecordHeaderLen+1)
	f.Add(cloneBytes(goldenRecordHeader), -1)
	f.Add(cloneBytes(goldenRecordHeader), math.MinInt)
	f.Add(cloneBytes(goldenRecordHeader), math.MaxInt)
	f.Add(cloneBytes(goldenRecordHeader[:RecordHeaderLen-1]), RecordHeaderLen)
	f.Add([]byte{}, 0)

	long := make([]byte, 4096)
	copy(long, goldenRecordHeader)
	f.Add(long, len(long))

	seqLimit := cloneBytes(goldenRecordHeader)
	binary.LittleEndian.PutUint64(seqLimit[recordHeaderSeqOff:], MaxU64+1)
	f.Add(seqLimit, RecordHeaderLen)

	payloadLimit := cloneBytes(goldenRecordHeader)
	binary.LittleEndian.PutUint32(payloadLimit[recordHeaderPayloadLenOff:], MaxLen32+1)
	f.Add(payloadLimit, RecordHeaderLen)

	f.Fuzz(func(t *testing.T, b []byte, recordHeaderLen int) {
		recordHeaderLen = capFuzzLayoutLen(recordHeaderLen)

		h, err := UnmarshalRecordHeader(b, recordHeaderLen)
		if err != nil {
			requireDecodeError(t, err)
			require.Equal(t, RecordHeader{}, h)

			return
		}

		require.GreaterOrEqual(t, recordHeaderLen, RecordHeaderLen)
		require.LessOrEqual(t, h.Seq, MaxU64)
		require.LessOrEqual(t, h.PayloadLen, MaxLen32)

		extraLen := recordHeaderLen - RecordHeaderLen
		if extraLen == 0 {
			require.Nil(t, h.Extra)
		} else {
			require.Equal(t, b[RecordHeaderLen:recordHeaderLen], h.Extra)
			require.NotSame(t, &b[RecordHeaderLen], &h.Extra[0], "Extra must not alias the input")
			require.LessOrEqual(t, cap(h.Extra), 2*len(b), "cap(Extra) must stay within 2×len(input)")
		}

		enc := AppendRecordHeader(nil, &h)
		require.Len(t, enc, recordHeaderLen)
		require.Equal(t, b[:36], enc[:36], "every named field before quality is written verbatim")
		require.Equal(t, h.Quality&recordHeaderQualityMask, binary.LittleEndian.Uint16(enc[36:38]), "quality keeps only its defined bits (§9)")
		require.Equal(t, b[38:42], enc[38:42], "every named field between quality and field_validity is written verbatim")
		require.Equal(t, h.FieldValidity&recordHeaderFieldValidityMask, enc[42], "field_validity keeps only its defined bits (§9)")
		require.Equal(t, h.RecordFlags&recordHeaderRecordFlagsMask, enc[43], "record_flags keeps only its defined bits (§9)")
		require.Equal(t, b[RecordHeaderLen:recordHeaderLen], enc[RecordHeaderLen:], "Extra is written verbatim")

		got, err := UnmarshalRecordHeader(enc, recordHeaderLen)
		require.NoError(t, err)

		h.Quality &= recordHeaderQualityMask
		h.FieldValidity &= recordHeaderFieldValidityMask
		h.RecordFlags &= recordHeaderRecordFlagsMask
		require.Equal(t, h, got)
	})
}

// FuzzUnmarshalFooterPrologue fuzzes UnmarshalFooterPrologue.
//
// The prologue has neither reserved bytes nor a CRC,
// so on success AppendFooterPrologue of the result reproduces the first 72 input bytes exactly
// and decodes to an equal prologue.
// FooterPrologue holds no slice, so there is no allocation to bound.
func FuzzUnmarshalFooterPrologue(f *testing.F) {
	f.Add(cloneBytes(goldenFooterPrologue))
	f.Add(append(cloneBytes(goldenFooterPrologue), 0xEE))
	f.Add(cloneBytes(goldenFooterPrologue[:FooterPrologueLen-1]))
	f.Add([]byte{})

	withField := func(put func(b []byte)) []byte {
		b := cloneBytes(goldenFooterPrologue)
		put(b)

		return b
	}
	f.Add(withField(func(b []byte) { binary.LittleEndian.PutUint16(b, 2) }))
	f.Add(withField(func(b []byte) { binary.LittleEndian.PutUint32(b[prologueF2EntryLenOff:], F2EntryLen-1) }))
	f.Add(withField(func(b []byte) { binary.LittleEndian.PutUint32(b[prologueF2EntryLenOff:], F2EntryLen) }))
	f.Add(withField(func(b []byte) { binary.LittleEndian.PutUint64(b[prologueF2OffsetOff:], MaxU64+1) }))
	f.Add(withField(func(b []byte) { binary.LittleEndian.PutUint64(b[prologueF5LenOff:], MaxU64) }))

	f.Fuzz(func(t *testing.T, b []byte) {
		p, err := UnmarshalFooterPrologue(b)
		if err != nil {
			requireDecodeError(t, err)
			require.Equal(t, FooterPrologue{}, p)

			return
		}

		require.Equal(t, FooterLayoutVersion, p.FooterLayoutVersion)
		require.GreaterOrEqual(t, p.F2EntryLen, uint32(F2EntryLen))

		enc := AppendFooterPrologue(nil, &p)
		require.Equal(t, b[:2], enc[:2], "footer_layout_version is written verbatim")
		require.Equal(t, p.Flags&footerPrologueFlagsMask, binary.LittleEndian.Uint16(enc[2:4]), "flags keeps only its defined bits (§10)")
		require.Equal(t, b[4:FooterPrologueLen], enc[4:FooterPrologueLen], "every field after flags is written verbatim")

		got, err := UnmarshalFooterPrologue(enc)
		require.NoError(t, err)

		p.Flags &= footerPrologueFlagsMask
		require.Equal(t, p, got)
	})
}

// FuzzUnmarshalF2Entry fuzzes UnmarshalF2Entry with a fuzzed f2_entry_len,
// capped at 65535 and passed through when negative.
//
// On success, AppendF2Entry of the result reproduces bytes 0-77 of the input verbatim
// and decodes, at the layout 1 length, to an equal entry.
// F2Entry holds no slice, so there is no allocation to bound.
func FuzzUnmarshalF2Entry(f *testing.F) {
	f.Add(cloneBytes(goldenF2Entry), F2EntryLen)
	f.Add(cloneBytes(goldenF2EntryExtended), len(goldenF2EntryExtended))
	f.Add(cloneBytes(goldenF2EntryExtended), F2EntryLen)
	f.Add(cloneBytes(goldenF2Entry), F2EntryLen-1)
	f.Add(cloneBytes(goldenF2Entry), F2EntryLen+1)
	f.Add(cloneBytes(goldenF2Entry), -1)
	f.Add(cloneBytes(goldenF2Entry), math.MinInt)
	f.Add(cloneBytes(goldenF2Entry), math.MaxInt)
	f.Add(cloneBytes(goldenF2Entry[:F2EntryLen-1]), F2EntryLen)
	f.Add([]byte{}, 0)

	withField := func(put func(b []byte)) []byte {
		b := cloneBytes(goldenF2Entry)
		put(b)

		return b
	}
	f.Add(withField(func(b []byte) {
		binary.LittleEndian.PutUint16(b[f2RecordHeaderLenOff:], RecordHeaderLen-1)
	}), F2EntryLen)
	f.Add(withField(func(b []byte) { binary.LittleEndian.PutUint64(b[f2OffsetOff:], MaxU64+1) }), F2EntryLen)
	f.Add(withField(func(b []byte) { binary.LittleEndian.PutUint64(b[f2LastSeqOff:], MaxU64+1) }), F2EntryLen)
	f.Add(withField(func(b []byte) { binary.LittleEndian.PutUint32(b[f2OnDiskLenOff:], MaxLen32+1) }), F2EntryLen)
	f.Add(withField(func(b []byte) {
		binary.LittleEndian.PutUint32(b[f2UncompressedLenOff:], MaxLen32)
	}), F2EntryLen)

	f.Fuzz(func(t *testing.T, b []byte, f2EntryLen int) {
		f2EntryLen = capFuzzLayoutLen(f2EntryLen)

		e, err := UnmarshalF2Entry(b, f2EntryLen)
		if err != nil {
			requireDecodeError(t, err)
			require.Equal(t, F2Entry{}, e)

			return
		}

		require.GreaterOrEqual(t, f2EntryLen, F2EntryLen)
		require.GreaterOrEqual(t, int(e.RecordHeaderLen), RecordHeaderLen)

		enc := AppendF2Entry(nil, &e)
		require.Len(t, enc, F2EntryLen)
		require.Equal(t, b[:78], enc[:78], "every named field is written verbatim")

		got, err := UnmarshalF2Entry(enc, F2EntryLen)
		require.NoError(t, err)
		require.Equal(t, e, got)
	})
}

// FuzzUnmarshalTrailer fuzzes UnmarshalTrailer.
//
// The trailer has no reserved bytes and its CRC has matched,
// so on success AppendTrailer of the result reproduces the first 64 input bytes exactly
// and decodes to an equal trailer.
// Trailer holds no slice, so there is no allocation to bound.
func FuzzUnmarshalTrailer(f *testing.F) {
	f.Add(cloneBytes(goldenTrailer))
	f.Add(append(cloneBytes(goldenTrailer), 0xEE))
	f.Add(cloneBytes(goldenTrailer[:TrailerLen-1]))
	f.Add([]byte{})
	f.Add(sealedVariant(goldenTrailer, trailerCRCOff, func(b []byte) { b[trailerMagicOff] = 'X' }))
	f.Add(sealedVariant(goldenTrailer, trailerCRCOff, func(b []byte) {
		binary.LittleEndian.PutUint16(b[trailerVersionOff:], 2)
	}))
	f.Add(sealedVariant(goldenTrailer, trailerCRCOff, func(b []byte) {
		binary.LittleEndian.PutUint64(b[trailerFooterOffsetOff:], MaxU64+1)
	}))
	f.Add(sealedVariant(goldenTrailer, trailerCRCOff, func(b []byte) {
		binary.LittleEndian.PutUint64(b[trailerRecordCountOff:], 0)
		binary.LittleEndian.PutUint64(b[trailerLastSeqOff:], MaxU64+1)
	}))
	f.Add(sealedVariant(goldenTrailer, trailerCRCOff, func(b []byte) {
		binary.LittleEndian.PutUint64(b[trailerFooterUncompressedLenOff:], MaxU64)
	}))
	flipped := cloneBytes(goldenTrailer)
	flipped[trailerCRCOff] ^= 0x01
	f.Add(flipped)

	f.Fuzz(func(t *testing.T, b []byte) {
		fuzzResealed(t, b, TrailerLen, trailerCRCOff, checkTrailer)
	})
}

func checkTrailer(t *testing.T, b []byte) {
	t.Helper()

	tr, err := UnmarshalTrailer(b)
	if err != nil {
		requireDecodeError(t, err)
		require.Equal(t, Trailer{}, tr)

		return
	}

	require.Equal(t, TrailerVersion, tr.TrailerVersion)

	enc := AppendTrailer(nil, &tr)
	require.Equal(t, b[:51], enc[:51], "every field before flags is written verbatim")
	require.Zero(t, enc[51], "flags is fully reserved (§11) and written as zero")

	got, err := UnmarshalTrailer(enc)
	require.NoError(t, err)

	tr.Flags &= trailerFlagsMask
	tr.TrailerCRC, got.TrailerCRC = 0, 0
	require.Equal(t, tr, got)
}

// FuzzHeaderSectionLen fuzzes HeaderSectionLen over the whole u32 × u16 input space.
//
// It succeeds with the exact product exactly when that product is at most MaxLen32,
// and reports ErrLimit with a zero length otherwise.
// HeaderSectionLen returns no slice and allocates only its error.
func FuzzHeaderSectionLen(f *testing.F) {
	f.Add(uint32(1), uint16(RecordHeaderLen))
	f.Add(uint32(0), uint16(RecordHeaderLen))
	f.Add(uint32(math.MaxUint32), uint16(math.MaxUint16))
	f.Add(MaxLen32, uint16(1))
	f.Add(MaxLen32, uint16(2))
	f.Add(uint32(38347922), uint16(RecordHeaderLen))
	f.Add(uint32(38347923), uint16(RecordHeaderLen))

	f.Fuzz(func(t *testing.T, recordCount uint32, recordHeaderLen uint16) {
		n, err := HeaderSectionLen(recordCount, recordHeaderLen)
		want := uint64(recordCount) * uint64(recordHeaderLen)
		if want > uint64(MaxLen32) {
			require.ErrorIs(t, err, ErrLimit)
			requireDecodeError(t, err)
			require.Zero(t, n)

			return
		}

		require.NoError(t, err)
		require.Equal(t, want, uint64(n))
	})
}

// FuzzParseUUID fuzzes ParseUUID.
//
// On success, String renders the input in lowercase and parses back to the same UUID;
// on error, the result is the nil UUID.
// UUID is an array, so there is no slice to bound.
func FuzzParseUUID(f *testing.F) {
	f.Add("f47ac10b-58cc-4372-a567-0e02b2c3d479")
	f.Add("F47AC10B-58CC-4372-A567-0E02B2C3D479")
	f.Add("00000000-0000-0000-0000-000000000000")
	f.Add("ffffffff-ffff-ffff-ffff-ffffffffffff")
	f.Add("f47ac10b58cc4372a5670e02b2c3d479")
	f.Add("f47ac10b-58cc-4372-a567-0e02b2c3d47")
	f.Add("f47ac10b-58cc-4372-a567-0e02b2c3d4790")
	f.Add("f47ac10b-58cc-4372-a567-0e02b2c3d47g")
	f.Add("f47ac10b-58cc-4372-a5670-e02b2c3d479")
	f.Add("+47ac10b-58cc-4372-a567-0e02b2c3d479")
	f.Add("f47ac10b-58cc-4372-a567-0e02b2c3d4é")
	f.Add("")

	f.Fuzz(func(t *testing.T, s string) {
		u, err := ParseUUID(s)
		if err != nil {
			require.True(t, u.IsZero(), "a failed parse must return the nil UUID")

			return
		}

		require.Equal(t, strings.ToLower(s), u.String())

		got, err := ParseUUID(u.String())
		require.NoError(t, err)
		require.Equal(t, u, got)
	})
}
