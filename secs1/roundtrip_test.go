package secs1

import (
	"strconv"
	"testing"

	"github.com/arloliu/go-secs/v2/internal/wire"
	"github.com/arloliu/go-secs/v2/secs2"
	"github.com/stretchr/testify/require"
)

// TestSECS1Framing_FullRoundTrip exercises framing over ARBITRARY bytes (including the empty /
// header-only body, size 0) — byte-exact reproduction across block boundaries.
func TestSECS1Framing_FullRoundTrip(t *testing.T) {
	t.Parallel()
	sizes := []int{0, 1, 243, 244, 245, 488, 489, 244*7 + 3}
	hdr := messageHeader{deviceID: 0x2AAA, rBit: true, stream: 100, function: 200, waitBit: true, systemBytes: [4]byte{0x11, 0x22, 0x33, 0x44}}
	for _, size := range sizes {
		t.Run(strconv.Itoa(size), func(t *testing.T) {
			t.Parallel()
			var raw []byte
			if size > 0 {
				raw = make([]byte, size)
				for i := range raw {
					raw[i] = byte(i * 7)
				}
			}
			parsed, _ := splitParse(t, wire.AdoptBody(raw), hdr)
			gotHdr, gotBody, err := assembleBlocks(parsed)
			require.NoError(t, err)
			require.Equal(t, hdr, gotHdr)
			require.Equal(t, raw, gotBody.AppendTo(nil))
			// last block carries the E-bit; all earlier do not
			for i, b := range parsed {
				require.Equal(t, i == len(parsed)-1, b.eBit())
			}
		})
	}
}

// TestSECS1Framing_ItemRoundTrip is the spec §11 item round-trip: a real secs2.Item through
// wire.FromItem (tree-backed body) -> split -> appendTo -> parseBlock -> assembleBlocks ->
// secs2.Decode, asserting the decoded item is value-equal to the original. Item-encoded sizes
// straddle the 244-byte block boundary and the 1-vs-2 length-byte boundary (>255 data bytes), so
// this covers single-block, two-block, and many-block messages.
func TestSECS1Framing_ItemRoundTrip(t *testing.T) {
	t.Parallel()
	dataLens := []int{1, 242, 243, 485, 486, 2000}
	hdr := messageHeader{deviceID: 0x2AAA, rBit: true, stream: 100, function: 200, waitBit: true, systemBytes: [4]byte{0x11, 0x22, 0x33, 0x44}}
	for _, dl := range dataLens {
		t.Run(strconv.Itoa(dl), func(t *testing.T) {
			t.Parallel()
			item, err := secs2.Decode(asciiItemEncoding(dl))
			require.NoError(t, err)
			encoded := item.ToBytes() // canonical encoding — the bytes that get split (source of truth)
			body := wire.FromItem(item)

			parsed, _ := splitParse(t, body, hdr)
			gotHdr, gotBody, err := assembleBlocks(parsed)
			require.NoError(t, err)
			require.Equal(t, hdr, gotHdr)
			require.Equal(t, encoded, gotBody.AppendTo(nil))

			decoded, err := secs2.Decode(gotBody.AppendTo(nil))
			require.NoError(t, err)
			require.Equal(t, encoded, decoded.ToBytes()) // item value-equality round-trip
		})
	}
}

// asciiItemEncoding returns a valid SECS-II ASCII item (format code 16) carrying n data bytes,
// using the minimal number of length bytes (1 for n<=255, else 2) so secs2 round-trips it
// canonically. Used only to construct test items of controlled size.
func asciiItemEncoding(n int) []byte {
	data := make([]byte, n)
	for i := range data {
		data[i] = byte('A' + i%26)
	}
	if n <= 0xFF {
		return append([]byte{0x41, byte(n)}, data...) // 0x41 = (16<<2)|1 length byte
	}

	return append([]byte{0x42, byte(n >> 8), byte(n)}, data...) // 0x42 = (16<<2)|2 length bytes
}
