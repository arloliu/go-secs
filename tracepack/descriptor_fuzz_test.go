package tracepack

import (
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
