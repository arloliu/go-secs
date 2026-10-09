package corpus

import (
	"strings"
	"testing"

	"github.com/stretchr/testify/require"

	"github.com/arloliu/go-secs/tracepack"
	"github.com/arloliu/go-secs/tracepack/internal/format"
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
  ]
}
`, string(b))

	bad := append([]byte{}, header...)
	bad[76] ^= 1
	_, err = NewPrimitives(bad, envelope, nil)
	require.Error(t, err, "a stored CRC that does not match its bytes")
	_, err = NewPrimitives(header, envelope[:39], nil)
	require.Error(t, err, "a short structure")
}
