package corpus

import (
	"bytes"
	"encoding/binary"
	"testing"
	"time"

	"github.com/stretchr/testify/require"

	"github.com/arloliu/go-secs/tracepack"
	"github.com/arloliu/go-secs/tracepack/internal/format"
)

// testHour is an hour-aligned time base, in nanoseconds since the Unix epoch.
const testHour int64 = 1_791_000_000_000_000_000

// Layout lengths of the tracepack format specification §4, §6, §7 and §11 that the tests compute offsets with.
const (
	testFileHeaderLen   = 80
	testEnvelopeLen     = 40
	testRecordHeaderLen = 44
	testTrailerLen      = 64
)

var (
	testPackID    = tracepack.UUID{0x01, 0x9a, 0x2b, 0x3c, 0x4d, 0x5e, 0x70, 0x01, 0x80, 0, 0, 0, 0, 0, 0, 0x01}
	testCaptureID = tracepack.UUID{0x01, 0x9a, 0x2b, 0x3c, 0x4d, 0x5e, 0x70, 0x02, 0x80, 0, 0, 0, 0, 0, 0, 0x02}
)

// testMeta returns the pack metadata of a generator segment of the hour testHour.
func testMeta() *tracepack.PackMeta {
	return &tracepack.PackMeta{
		ToolID:             "tool-01",
		Transport:          tracepack.TransportHSMSSS,
		CaptureMethod:      tracepack.CaptureMethodGenerator,
		Vantage:            tracepack.VantageNone,
		Recorder:           "corpus-test",
		Writer:             "corpus-test",
		TimeSource:         tracepack.TimeSourceGenerator,
		PeriodStart:        testHour,
		PeriodEnd:          testHour + int64(time.Minute),
		LifecycleCoverage:  tracepack.LifecycleCoverageNone,
		PackRole:           tracepack.PackRoleSegment,
		RecorderInstanceID: tracepack.UUID{0x01, 0x9a, 0x2b, 0x3c, 0x4d, 0x5e, 0x70, 0x03, 0x80, 0, 0, 0, 0, 0, 0, 0x03},
		ScopeGeneration:    new(uint64(0)),
	}
}

// writePack writes a pack of meta whose blocks hold blocks' records, one block per element,
// finalized when finalize is set.
func writePack(t *testing.T, meta *tracepack.PackMeta, blocks [][]tracepack.Record, finalize bool) []byte {
	t.Helper()

	var buf bytes.Buffer
	w, err := tracepack.NewWriter(&buf, tracepack.WriterOptions{
		Meta: meta, PackID: testPackID, CaptureID: testCaptureID,
		Now: func() time.Time { return time.Unix(0, testHour).UTC() },
	})
	require.NoError(t, err)
	for _, b := range blocks {
		for i := range b {
			require.NoError(t, w.Append(&b[i]))
		}
		require.NoError(t, w.Flush())
	}
	if finalize {
		_, err := w.Close()
		require.NoError(t, err)
	}

	return buf.Bytes()
}

// dataRecord returns a data record of seq at testHour + seq µs whose payload is a 14-byte HSMS header
// of SessionID 7, S1F1 with W, System Bytes 00 00 00 seq, followed by text, every field available.
func dataRecord(seq uint64, epoch uint32, text []byte) tracepack.Record {
	payload := binary.BigEndian.AppendUint32(nil, uint32(10+len(text)))
	payload = append(payload, 0x00, 0x07, 0x81, 0x01, 0x00, 0x00, 0x00, 0x00, 0x00, byte(seq))
	payload = append(payload, text...)
	r := tracepack.Record{
		Seq: seq, TSUTCNs: testHour + int64(seq)*int64(time.Microsecond), Epoch: epoch,
		Kind: tracepack.KindData, Dir: tracepack.DirHostToEquipment, Fidelity: tracepack.FidelityWireExact,
		Payload: payload,
	}
	r.SetCapturedFieldValidity()

	return r
}

// eventRecord returns a transport-event record of seq carrying ev.
func eventRecord(t *testing.T, seq uint64, epoch uint32, ev *tracepack.TransportEvent) tracepack.Record {
	t.Helper()

	payload, err := ev.MarshalBinary()
	require.NoError(t, err)

	return tracepack.Record{
		Seq: seq, TSUTCNs: testHour + int64(seq)*int64(time.Microsecond), Epoch: epoch,
		Kind: tracepack.KindTransportEvent, Dir: tracepack.DirLocal, Fidelity: tracepack.FidelitySynthesized,
		DecodeStatus: tracepack.DecodeStatusNotApplicable, Payload: payload,
	}
}

// metaEnd returns 80 + pack_metadata_len, read from pack's file header: where the first block starts.
func metaEnd(pack []byte) uint64 {
	return testFileHeaderLen + uint64(binary.LittleEndian.Uint32(pack[16:20]))
}

// blockLen returns the on-disk length of a codec none block of recs with 44-byte record headers:
// the envelope, the header section and the payloads (the tracepack format specification §6).
func blockLen(recs []tracepack.Record) uint64 {
	n := uint64(testEnvelopeLen)
	for i := range recs {
		n += testRecordHeaderLen + uint64(len(recs[i].Payload))
	}

	return n
}

// openPack opens pack.
func openPack(t *testing.T, pack []byte) *tracepack.Reader {
	t.Helper()

	r, err := tracepack.Open(t.Context(), bytes.NewReader(pack), int64(len(pack)), tracepack.ReaderOptions{})
	require.NoError(t, err)

	return r
}

// verifyPack verifies pack.
func verifyPack(t *testing.T, pack []byte) tracepack.VerifyReport {
	t.Helper()

	rep, err := tracepack.Verify(t.Context(), bytes.NewReader(pack), int64(len(pack)), tracepack.VerifyOptions{})
	require.NoError(t, err)

	return rep
}

// marshalJSON returns the canonical form of v.
func marshalJSON(t *testing.T, v any) string {
	t.Helper()

	b, err := Marshal(v)
	require.NoError(t, err)

	return string(b)
}

// patchTrailer rewrites pack's trailer through edit and recomputes its trailer_crc.
func patchTrailer(t *testing.T, pack []byte, edit func(*format.Trailer)) []byte {
	t.Helper()

	at := len(pack) - testTrailerLen
	tr, err := format.UnmarshalTrailer(pack[at:])
	require.NoError(t, err)
	edit(&tr)

	return format.AppendTrailer(bytes.Clone(pack[:at]), &tr)
}

// replaceFooter returns pack with its codec none footer replaced by footer:
// the trailer's footer_len, footer_uncompressed_len and footer_crc recomputed, every other byte kept.
func replaceFooter(t *testing.T, pack []byte, footer []byte) []byte {
	t.Helper()

	at := len(pack) - testTrailerLen
	tr, err := format.UnmarshalTrailer(pack[at:])
	require.NoError(t, err)
	tr.FooterLen, tr.FooterUncompressedLen, tr.FooterCRC = uint64(len(footer)), uint64(len(footer)), format.CRC(footer)

	out := append(bytes.Clone(pack[:tr.FooterOffset]), footer...)

	return format.AppendTrailer(out, &tr)
}

// storedFooter returns the decoded bytes of pack's codec none footer.
func storedFooter(t *testing.T, pack []byte) []byte {
	t.Helper()

	at := len(pack) - testTrailerLen
	tr, err := format.UnmarshalTrailer(pack[at:])
	require.NoError(t, err)
	require.Equal(t, uint8(0), tr.FooterCodec)

	return bytes.Clone(pack[tr.FooterOffset:at])
}
