package corpus

import (
	"context"
	"errors"
	"fmt"
	"io"
	"slices"
	"testing"

	"github.com/stretchr/testify/require"

	"github.com/arloliu/go-secs/tracepack"
	"github.com/arloliu/go-secs/tracepack/internal/format"
	"github.com/arloliu/go-secs/tracepack/internal/tlv"
)

// Pack metadata tags the rejection tests edit (the tracepack format specification §5).
const (
	tagToolID              uint16 = 0x0002
	tagTransport           uint16 = 0x0003
	tagCaptureOriginUTCNs  uint16 = 0x000A
	tagCoverage            uint16 = 0x0016
	tagDeviceID            uint16 = 0x0025
	tagReplacementSetSize  uint16 = 0x002A
	tagRedactionPolicy     uint16 = 0x0031
	tagPolicyVersionNested uint16 = 0x0002
)

func TestRejectionCodeFromConstructedErrors(t *testing.T) {
	t.Parallel()

	entryErr := &tlv.EntryError{Tag: tagTransport, Offset: 40, Err: tlv.ErrType}
	nestedMissing := &tlv.EntryError{Tag: tagRedactionPolicy, Offset: 96, Err: &tlv.EntryError{Tag: 0x0001, Offset: -1, Err: tlv.ErrMissing}}
	tests := []struct {
		name string
		err  error
		want string
	}{
		{"short object", fmt.Errorf("x: %w: %w", tracepack.ErrNotTracepack, io.ErrUnexpectedEOF), RejectShortObject},
		{"bad magic", fmt.Errorf("x: %w: %w", tracepack.ErrNotTracepack, &format.OffsetError{Structure: "file header", Err: format.ErrBadMagic}), RejectBadMagic},
		{"header CRC", fmt.Errorf("x: %w: %w", tracepack.ErrChecksum, &format.OffsetError{Structure: "file header", Err: format.ErrCRC}), RejectHeaderCRC},
		{"unsupported version", fmt.Errorf("x: %w: %w", tracepack.ErrUnsupportedFormat, format.ErrSchema), RejectUnsupportedVersion},
		{"metadata past object", fmt.Errorf("x: %w", io.ErrUnexpectedEOF), RejectMetadataPastObject},
		{"metadata CRC", fmt.Errorf("x: %w", tracepack.ErrChecksum), RejectMetadataCRC},
		{"entry list", fmt.Errorf("x: %w", entryErr), RejectMetadataEntryList},
		{"nested missing tag", fmt.Errorf("x: %w", nestedMissing), RejectMetadataEntryList},
		{"top-level missing tag", fmt.Errorf("x: %w", &tlv.EntryError{Tag: tagToolID, Offset: -1, Err: tlv.ErrMissing}), RejectMetadataRequired},
		{"conditional tag", &tracepack.FieldError{Field: "capture_origin_utc_ns", Err: tracepack.ErrRequiredTag}, RejectMetadataRequired},
		{"replacement set size", &tracepack.FieldError{Field: "replacement_set_size", Err: tracepack.ErrFieldValue}, RejectReplacementSet},
		{"replacement set index", &tracepack.FieldError{Field: "replacement_set_index", Err: tracepack.ErrFieldValue}, RejectReplacementSet},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()

			got, err := RejectionCode(tt.err)
			require.NoError(t, err)
			require.Equal(t, tt.want, got)
		})
	}

	for _, err := range []error{
		fmt.Errorf("x: %w", tracepack.ErrReadLimit),
		fmt.Errorf("x: %w: %w", tracepack.ErrReadLimit, io.ErrUnexpectedEOF),
		fmt.Errorf("x: %w", context.Canceled),
		&tracepack.FieldError{Field: "pack_role", Err: tracepack.ErrFieldValue},
		errors.New("anything else"),
	} {
		_, got := RejectionCode(err)
		require.ErrorIs(t, got, ErrNotRejection, err.Error())
	}
}

func TestRejectionOf(t *testing.T) {
	t.Parallel()

	meta, err := testMeta().MarshalBinary()
	require.NoError(t, err)
	header := format.FileHeader{
		FormatMajor: format.FormatMajor, WriterStartUTCNs: testHour, PackID: format.UUID(testPackID), CaptureID: format.UUID(testCaptureID),
	}
	withMeta := func(m []byte) []byte { return must(PackWithMeta(header, m))(t) }
	withU64 := func(m []byte, tag uint16, v uint64) []byte {
		out := slices.Clone(m)
		require.NoError(t, SetU64(out, Section{Len: len(out)}, tag, v))
		return out
	}
	base := withMeta(meta)
	_, err = RejectionOf(t.Context(), base)
	require.ErrorIs(t, err, ErrNotRejection, "the base pack opens")

	clockMeta := testMeta()
	clockMeta.TimeSource = tracepack.TimeSourceCaptureClock
	clockMeta.CaptureOriginUTCNs, clockMeta.CaptureOriginMonoNs, clockMeta.ClockStepToleranceNs = new(testHour), new(int64(0)), new(uint64(1000))
	clockRaw, err := clockMeta.MarshalBinary()
	require.NoError(t, err)

	setMeta := testMeta()
	setMeta.ReplacementSetID, setMeta.ReplacementSetSize, setMeta.ReplacementSetIndex = &testPackID, new(uint64(1)), new(uint64(0))
	setRaw, err := setMeta.MarshalBinary()
	require.NoError(t, err)

	tests := []struct {
		name string
		pack []byte
		want string
	}{
		{"short object", base[:testFileHeaderLen-1], RejectShortObject},
		{"bad magic", must(FlipByte(base, 0))(t), RejectBadMagic},
		{"header CRC", must(FlipByte(base, 76))(t), RejectHeaderCRC},
		{"unsupported version", must(PatchHeader(base, func(h []byte) { h[8] = 2 }))(t), RejectUnsupportedVersion},
		{"metadata past object", must(PatchHeader(base, func(h []byte) { h[headerPackMetadataLenOff]++ }))(t), RejectMetadataPastObject},
		{"metadata CRC", must(FlipByte(base, testFileHeaderLen+8))(t), RejectMetadataCRC},
		{"entry framing", withMeta(append(slices.Clone(meta), 0x02, 0x00, 0x06)), RejectMetadataEntryList},
		{"entry type", withMeta(tlv.AppendEntry(slices.Clone(meta), tlv.Entry{Tag: tagTransport, Type: tlv.TypeUTF8, Value: []byte("x")})), RejectMetadataEntryList},
		{"entry value", withMeta(tlv.AppendEntry(slices.Clone(meta), tlv.U64Entry(tagDeviceID, 1<<63))), RejectMetadataEntryList},
		{"entry repetition", withMeta(tlv.AppendEntry(slices.Clone(meta), tlv.UTF8Entry(tagToolID, "again"))), RejectMetadataEntryList},
		{"entry nested", withMeta(tlv.AppendEntry(slices.Clone(meta), tlv.Entry{Tag: tagCoverage, Type: tlv.TypeTLV, Value: []byte{0x02, 0x00, 0x04}})), RejectMetadataEntryList},
		{"nested required tag", withMeta(tlv.AppendEntry(slices.Clone(meta), tlv.NestedEntry(tagRedactionPolicy, []tlv.Entry{
			tlv.U64Entry(tagPolicyVersionNested, 1), tlv.UTF8Entry(0x0003, "k"), tlv.U8Entry(0x0004, 1),
		}))), RejectMetadataEntryList},
		{"required tag", withMeta(must(DropEntries(meta, tagToolID))(t)), RejectMetadataRequired},
		{"conditional tag", withMeta(must(DropEntries(clockRaw, tagCaptureOriginUTCNs))(t)), RejectMetadataRequired},
		{"replacement set", withMeta(withU64(setRaw, tagReplacementSetSize, 2)), RejectReplacementSet},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()

			got, err := RejectionOf(t.Context(), tt.pack)
			require.NoError(t, err)
			require.Equal(t, tt.want, got)

			r := Rejection{Rejection: got}
			b, err := r.Marshal()
			require.NoError(t, err)
			require.Equal(t, "{\n  \"rejection\": \""+tt.want+"\"\n}\n", string(b))
		})
	}
}

func TestRejectionMarshalRefusesUnknownCode(t *testing.T) {
	t.Parallel()

	r := Rejection{Rejection: "metadata"}
	_, err := r.Marshal()
	require.Error(t, err)
}
