package tracepack

import (
	"crypto/sha256"
	"encoding/base64"
	"encoding/binary"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/arloliu/go-secs/tracepack/internal/tlv"
)

// fuzzDescriptorTags are the top-level tags a capture descriptor may hold
// (the tracepack storage specification §4, "Capture descriptor and pack metadata"),
// listed here independently of the codec under test.
var fuzzDescriptorTags = map[uint16]bool{
	0x0002: true, 0x0003: true, 0x0004: true, 0x0005: true, 0x0006: true, 0x0009: true,
	0x000A: true, 0x000B: true, 0x0012: true, 0x0013: true, 0x001B: true, 0x001C: true,
	0x001E: true, 0x001F: true, 0x0020: true, 0x0021: true, 0x0022: true, 0x0023: true,
	0x0024: true, 0x0025: true, 0x0026: true, 0x0028: true, 0x0033: true,
}

// fuzzDescriptorRefusable reports whether a well-framed descriptor must be refused, judged on its raw entries:
// a top-level tag outside the descriptor, a nested tag of thinning other than its three,
// or a thinning stream above 127 or function above 255.
func fuzzDescriptorRefusable(entries []tlv.Entry) bool {
	for _, e := range entries {
		if !fuzzDescriptorTags[e.Tag] {
			return true
		}
		if e.Tag != 0x0033 {
			continue
		}
		nested, err := tlv.Decode(e.Value)
		if err != nil {
			return true
		}
		for _, n := range nested {
			if n.Tag < 1 || n.Tag > 3 {
				return true
			}
			if n.Type != tlv.TypeU64 || len(n.Value) != 8 {
				continue
			}
			v := binary.LittleEndian.Uint64(n.Value)
			if (n.Tag == 1 && v > 127) || (n.Tag == 2 && v > 255) {
				return true
			}
		}
	}

	return false
}

func FuzzDecodeDescriptor(f *testing.F) {
	b, err := EncodeDescriptor(descriptorMeta())
	require.NoError(f, err)
	f.Add(b)

	entries, err := tlv.Decode(b)
	require.NoError(f, err)
	// Known entries reordered, and a nonzero reserved byte.
	reordered := encodeTLV(append(append([]tlv.Entry{}, entries[len(entries)/2:]...), entries[:len(entries)/2]...))
	f.Add(reordered)
	reserved := append([]byte{}, b...)
	reserved[3] = 0x5A
	f.Add(reserved)
	f.Add(encodeTLV(append(append([]tlv.Entry{}, entries...), tlv.U8Entry(0x7F00, 1))))
	f.Add(encodeTLV(append(append([]tlv.Entry{}, entries...), tlv.NestedEntry(tagThinning, []tlv.Entry{
		tlv.U64Entry(1, 128), tlv.U64Entry(2, 1), tlv.UTF8Entry(3, "s"),
	}))))
	f.Add([]byte{})

	f.Fuzz(func(t *testing.T, b []byte) {
		m, err := DecodeDescriptor(b)

		entries, framed := tlv.Decode(b)
		if framed == nil && fuzzDescriptorRefusable(entries) {
			require.Error(t, err, "an unknown tag or an out-of-range thinning entry is refused")
		}
		if err != nil {
			assert.Nil(t, m)

			return
		}
		require.NoError(t, framed)

		enc, err := EncodeDescriptor(m)
		require.NoError(t, err, "an accepted descriptor encodes")
		again, err := DecodeDescriptor(enc)
		require.NoError(t, err)
		assert.Equal(t, m, again, "decode, encode, decode gives equal values")
		enc2, err := EncodeDescriptor(again)
		require.NoError(t, err)
		assert.Equal(t, enc, enc2, "the encoding is canonical")
	})
}

// fuzzNormalized returns the body a consumer's normalization makes of a well-framed body:
// trailing_bytes, decode_status and field_validity zero, quality and record_flags kept to their defined bits.
func fuzzNormalized(body []byte) []byte {
	n := append([]byte{}, body...)
	copy(n[32:36], []byte{0, 0, 0, 0})
	binary.LittleEndian.PutUint16(n[36:38], binary.LittleEndian.Uint16(n[36:38])&0x00B7)
	n[41], n[42] = 0, 0
	n[43] &= 0x02

	return n
}

func FuzzCheckBusMessage(f *testing.F) {
	start := busStartRecord(f)
	startBody := busEncode(f, &start)
	startEnv := busEnvelope(f, 0)
	startEnv.Descriptor = base64.StdEncoding.EncodeToString(descriptorBytes(f))
	f.Add(startEnv.Layout, startEnv.MsgID, startEnv.Descriptor, "", "", startBody)

	data := Record{Seq: 42, TSUTCNs: 1, Epoch: 1, Kind: KindData, Fidelity: FidelityWireExact, Payload: busFrame(1, 3, []byte{1, 2})}
	dataBody := busEncode(f, &data)
	dataEnv := busEnvelope(f, 42)
	f.Add(dataEnv.Layout, dataEnv.MsgID, "", "", "", dataBody)

	ccBody, ccEnv := claimCheck(f, &start, "bucket")
	f.Add(ccEnv.Layout, ccEnv.MsgID, startEnv.Descriptor, ccEnv.PayloadRef, ccEnv.PayloadDigest, ccBody)

	// Reserved quality bits set, and redacted set on an ordinary seq.
	f.Add(dataEnv.Layout, dataEnv.MsgID, "", "", "", busRawBody(42, 0xFF5F, data.Payload))
	f.Add(dataEnv.Layout, dataEnv.MsgID, "", "", "", busRawBody(42, uint16(QualityRedacted), data.Payload))
	digest := sha256.Sum256(nil)
	f.Add("1", dataEnv.MsgID, "", "b/"+dataEnv.MsgID, "SHA-256="+base64.URLEncoding.EncodeToString(digest[:]), dataBody[:44])

	f.Fuzz(func(t *testing.T, layout, msgID, descriptor, ref, digest string, body []byte) {
		env := BusEnvelope{Layout: layout, MsgID: msgID, Descriptor: descriptor, PayloadRef: ref, PayloadDigest: digest}
		br, err := CheckBusMessage(env, body)
		if err != nil {
			assert.Equal(t, BusRecord{}, br)

			return
		}

		enc, err := EncodeBusBody(&br)
		if br.Record.Quality.HasAny(QualityRedacted) {
			require.ErrorIs(t, err, ErrBusMalformed, "the producer never sets redacted")

			return
		}
		require.NoError(t, err, "an accepted record a producer may send encodes")
		assert.Equal(t, fuzzNormalized(body), enc, "re-encoding gives the normalized body")

		again, err := CheckBusMessage(env, enc)
		require.NoError(t, err)
		assert.Equal(t, br, again, "decode, encode, decode gives equal values")
		enc2, err := EncodeBusBody(&again)
		require.NoError(t, err)
		assert.Equal(t, enc, enc2, "the encoding is canonical")
	})
}
