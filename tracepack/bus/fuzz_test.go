package bus

import (
	"crypto/sha256"
	"encoding/base64"
	"encoding/binary"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/arloliu/go-secs/tracepack"
)

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

func FuzzCheckMessage(f *testing.F) {
	start := busStartRecord(f)
	startBody := busEncode(f, &start)
	startEnv := busEnvelope(f, 0)
	startEnv.Descriptor = base64.StdEncoding.EncodeToString(descriptorBytes(f))
	f.Add(startEnv.Layout, startEnv.MsgID, startEnv.Descriptor, "", "", startBody)

	data := tracepack.Record{Seq: 42, TSUTCNs: 1, Epoch: 1, Kind: tracepack.KindData, Fidelity: tracepack.FidelityWireExact, Payload: busFrame(1, 3, []byte{1, 2})}
	dataBody := busEncode(f, &data)
	dataEnv := busEnvelope(f, 42)
	f.Add(dataEnv.Layout, dataEnv.MsgID, "", "", "", dataBody)

	ccBody, ccEnv := claimCheck(f, &start, "bucket")
	f.Add(ccEnv.Layout, ccEnv.MsgID, startEnv.Descriptor, ccEnv.PayloadRef, ccEnv.PayloadDigest, ccBody)

	// Reserved quality bits set, and redacted set on an ordinary seq.
	f.Add(dataEnv.Layout, dataEnv.MsgID, "", "", "", busRawBody(42, 0xFF5F, data.Payload))
	f.Add(dataEnv.Layout, dataEnv.MsgID, "", "", "", busRawBody(42, uint16(tracepack.QualityRedacted), data.Payload))
	digest := sha256.Sum256(nil)
	f.Add("1", dataEnv.MsgID, "", "b/"+dataEnv.MsgID, "SHA-256="+base64.URLEncoding.EncodeToString(digest[:]), dataBody[:44])

	f.Fuzz(func(t *testing.T, layout, msgID, descriptor, ref, digest string, body []byte) {
		env := Envelope{Layout: layout, MsgID: msgID, Descriptor: descriptor, PayloadRef: ref, PayloadDigest: digest}
		br, err := CheckMessage(env, body)
		if err != nil {
			assert.Equal(t, Record{}, br)

			return
		}

		enc, err := EncodeBody(&br)
		if br.Record.Quality.HasAny(tracepack.QualityRedacted) {
			require.ErrorIs(t, err, ErrMalformed, "the producer never sets redacted")

			return
		}
		require.NoError(t, err, "an accepted record a producer may send encodes")
		assert.Equal(t, fuzzNormalized(body), enc, "re-encoding gives the normalized body")

		again, err := CheckMessage(env, enc)
		require.NoError(t, err)
		assert.Equal(t, br, again, "decode, encode, decode gives equal values")
		enc2, err := EncodeBody(&again)
		require.NoError(t, err)
		assert.Equal(t, enc, enc2, "the encoding is canonical")
	})
}
