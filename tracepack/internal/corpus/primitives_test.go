package corpus

import (
	"crypto/hmac"
	"crypto/sha256"
	"encoding/binary"
	"encoding/hex"
	"slices"
	"strings"
	"testing"

	"github.com/stretchr/testify/require"

	"github.com/arloliu/go-secs/tracepack"
	"github.com/arloliu/go-secs/tracepack/internal/format"
	"github.com/arloliu/go-secs/tracepack/internal/siv"
	"github.com/arloliu/go-secs/tracepack/redact"
)

func TestPrimitives(t *testing.T) {
	t.Parallel()

	header := format.AppendFileHeader(nil, &format.FileHeader{FormatMajor: 1, PackMetadataLen: 8})
	envelope := format.AppendBlockEnvelope(nil, &format.BlockEnvelope{RecordCount: 1, RecordHeaderLen: 44})
	low := tracepack.UUID{0x00, 0x11, 0x22, 0x33, 0x44, 0x55, 0x66, 0x77, 0x88, 0x99, 0xaa, 0xbb, 0xcc, 0xdd, 0xee, 0xff}
	high := tracepack.UUID{0xf0, 0x01}
	p, err := NewPrimitives(header, envelope, []PrimitiveUUID{{UUID: high}, {UUID: low, Pack: "basic-uuid-byte-order"}})
	require.NoError(t, err)
	b, err := p.Marshal()
	require.NoError(t, err)

	// The rows of the two structures carry their own stored CRCs, which NewPrimitives checks against their bytes.
	headerRow := marshalJSON(t, CRCRow{Name: CRCFileHeader, Input: header[:76], CRC: format.CRC(header[:76])})
	envelopeRow := marshalJSON(t, CRCRow{Name: CRCBlockEnvelope, Input: envelope[:36], CRC: format.CRC(envelope[:36])})
	indent := func(s string) string {
		return "    " + strings.ReplaceAll(strings.TrimSuffix(s, "\n"), "\n", "\n    ")
	}
	require.Equal(t, `{
  "crc": [
    {
      "name": "check-value",
      "input": "MTIzNDU2Nzg5",
      "crc": 3421780262
    },
`+indent(headerRow)+`,
`+indent(envelopeRow)+`
  ],
  "uuids": [
    {
      "bytes": "ABEiM0RVZneImaq7zN3u/w==",
      "string": "00112233-4455-6677-8899-aabbccddeeff",
      "pack": "basic-uuid-byte-order"
    },
    {
      "bytes": "8AEAAAAAAAAAAAAAAAAAAA==",
      "string": "f0010000-0000-0000-0000-000000000000"
    }
  ],
  "digest_keys": [],
  "encryption_keys": [],
  "hmac": [],
  "aes_siv": []
}
`, string(b))

	bad := append([]byte{}, header...)
	bad[76] ^= 1
	_, err = NewPrimitives(bad, envelope, nil)
	require.Error(t, err, "a stored CRC that does not match its bytes")
	_, err = NewPrimitives(header, envelope[:39], nil)
	require.Error(t, err, "a short structure")
}

// TestPrimitiveKeys checks the published test keys and their cases (the tracepack corpus specification §5.1):
// the key ids and lengths, every array ascending, the label case first with the 53 bytes of associated data
// the tracepack semantics specification §8 Encryption defines,
// every digest recomputed by HMAC-SHA-256 here and through redact,
// and every encryption case opened through internal/siv and restored through redact.
func TestPrimitiveKeys(t *testing.T) {
	t.Parallel()

	var p Primitives
	require.NoError(t, p.AddKeys())

	require.Equal(t, []string{DigestKeyID1, DigestKeyID2}, keyIDs(p.DigestKeys))
	require.Equal(t, []string{EncryptionKeyID1, EncryptionKeyID2}, keyIDs(p.EncryptionKeys))
	for _, k := range p.DigestKeys {
		sum := sha256.Sum256([]byte("tracepack-corpus test key/" + k.KeyID + "/0"))
		require.Equal(t, sum[:], k.Key, k.KeyID)
	}
	for _, k := range p.EncryptionKeys {
		a := sha256.Sum256([]byte("tracepack-corpus test key/" + k.KeyID + "/0"))
		b := sha256.Sum256([]byte("tracepack-corpus test key/" + k.KeyID + "/1"))
		require.Equal(t, slices.Concat(a[:], b[:]), k.Key, k.KeyID)
	}
	key := func(rows []KeyRow, id string) []byte {
		k := slices.IndexFunc(rows, func(r KeyRow) bool { return r.KeyID == id })
		require.GreaterOrEqual(t, k, 0, id)

		return rows[k].Key
	}

	require.NotEmpty(t, p.HMAC)
	require.True(t, slices.IsSortedFunc(p.HMAC, func(a, b HMACRow) int { return strings.Compare(a.Name, b.Name) }))
	for _, c := range p.HMAC {
		mac := hmac.New(sha256.New, key(p.DigestKeys, c.KeyID))
		mac.Write(binary.LittleEndian.AppendUint16(nil, uint16(len(c.Domain))))
		mac.Write([]byte(c.Domain))
		mac.Write(c.Input)
		require.Equal(t, mac.Sum(nil), c.Digest, c.Name)
		d, err := redact.Digest(key(p.DigestKeys, c.KeyID), c.Domain, c.Input)
		require.NoError(t, err)
		require.Equal(t, d, c.Digest, c.Name)
	}

	require.NotEmpty(t, p.AESSIV)
	require.Equal(t, "label", p.AESSIV[0].Name, "the first aes_siv case")
	require.True(t, slices.IsSortedFunc(p.AESSIV, func(a, b AESSIVRow) int { return strings.Compare(a.Name, b.Name) }))
	label := must(hex.DecodeString("74726163657061636b2d6f726967696e616c2d7631" + // tracepack-original-v1
		"00112233445566778899aabbccddeeff" + "0100000000000000" + "1800000000000000"))(t)
	require.Len(t, label, 53)
	require.Equal(t, label, p.AESSIV[0].AssociatedData, "the label case's associated data, byte for byte")
	for _, c := range p.AESSIV {
		capture, err := tracepack.ParseUUID(c.CaptureID)
		require.NoError(t, err)
		ad := siv.AssociatedData(capture, uint64(c.Seq), uint64(c.FirstOffset))
		require.Equal(t, ad[:], c.AssociatedData, c.Name)
		k := key(p.EncryptionKeys, c.KeyID)
		require.Len(t, c.Output, siv.SIVSize+len(c.Plaintext), c.Name)
		plain, err := siv.Open(k, c.AssociatedData, c.Output[:siv.SIVSize], c.Output[siv.SIVSize:])
		require.NoError(t, err, c.Name)
		require.Equal(t, c.Plaintext, plain, c.Name)
		require.Equal(t, c.Plaintext, restoreCase(t, capture, &c, k), c.Name)
	}
}

// keyIDs returns the key ids of rows, in order.
func keyIDs(rows []KeyRow) []string {
	ids := make([]string, 0, len(rows))
	for _, r := range rows {
		ids = append(ids, r.KeyID)
	}

	return ids
}

// restoreCase restores, through redact.Restore, a data record whose payload holds c's ciphertext at c's first offset,
// under one entry with c's siv, and returns the restored bytes of the range.
func restoreCase(t *testing.T, capture tracepack.UUID, c *AESSIVRow, key []byte) []byte {
	t.Helper()

	off, n := uint64(c.FirstOffset), uint64(len(c.Plaintext))
	payload := make([]byte, off+n)
	copy(payload[off:], c.Output[siv.SIVSize:])
	r := tracepack.Record{Seq: uint64(c.Seq), Kind: tracepack.KindData, Quality: tracepack.QualityRedacted, Payload: payload}
	entry := tracepack.RedactionEntry{
		Seq: r.Seq, MaskedRanges: []tracepack.MaskedRange{{Offset: off, Length: n}}, Domain: "case",
		Digest: make([]byte, redact.DigestSize), SIV: c.Output[:siv.SIVSize], HasSIV: true,
	}
	alg, keyID := tracepack.EncryptionAlgorithmAESSIVCMAC512, c.KeyID
	policy := tracepack.RedactionPolicy{
		PolicyID: "case", PolicyVersion: 1, KeyID: DigestKeyID1, DigestAlgorithm: tracepack.DigestAlgorithmHMACSHA256,
		EncryptionAlgorithm: &alg, EncryptionKeyID: &keyID,
	}
	rr := &tracepack.RecordRedaction{Entries: []tracepack.RedactionEntry{entry}}
	restored, err := redact.Restore(capture, &r, rr, &policy, key)
	require.NoError(t, err)

	return restored[off:]
}
