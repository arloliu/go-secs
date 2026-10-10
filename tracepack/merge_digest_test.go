package tracepack

import (
	"crypto/sha256"
	"encoding/binary"
	"encoding/hex"
	"hash"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/arloliu/go-secs/tracepack/internal/format"
)

// packDigests are the SHA-256 digests of a pack's parts, each in hex.
type packDigests struct {
	// file is the whole file with its file header normalized:
	// writer_start_utc_ns, the one value a Writer takes from the time it runs, set to 0, and header_crc recomputed.
	file string
	// meta is the pack metadata.
	meta string
	// blocks are the blocks, envelope and on-disk body, in file order, each prefixed by its length.
	blocks string
	// f3 are the blocks' F-3 entry lists as the footer stores them, in file order, each prefixed by its length.
	f3 string
	// footer is the footer on disk and the trailer.
	footer string
}

// digestsOf returns the digests of the parts of file, a finalized pack.
func digestsOf(t testing.TB, file []byte) packDigests {
	t.Helper()

	sum := func(h hash.Hash) string { return hex.EncodeToString(h.Sum(nil)) }
	one := func(b []byte) string {
		h := sha256.New()
		h.Write(b)

		return sum(h)
	}
	each := func(parts [][]byte) string {
		h := sha256.New()
		for _, p := range parts {
			h.Write(binary.BigEndian.AppendUint64(nil, uint64(len(p))))
			h.Write(p)
		}

		return sum(h)
	}

	l := layoutOf(t, file)
	normalized := patchHeader(t, file, func(h *format.FileHeader) { h.WriterStartUTCNs = 0 })

	return packDigests{
		file:   one(normalized),
		meta:   one(file[format.FileHeaderLen:l.blocksStart]),
		blocks: each(blockBytes(t, file, allBlocks)),
		f3:     each(f3ListsOf(t, file)),
		footer: one(file[l.tr.FooterOffset:]),
	}
}

// TestMergeOutputDigests pins the bytes Merge writes for fixed inputs, ids and options.
// Merge writes through the Writer, which does not validate its blocks and checks only its period, not its records' hours,
// so a change to the Writer that must leave its output alone shows here.
// A deliberate change to the output updates the digests,
// and so does an upgrade of the zstd encoder (github.com/klauspost/compress), which changes the zstd digests.
func TestMergeOutputDigests(t *testing.T) {
	t.Parallel()

	merged := func(c Codec, noCoalesce bool, files ...[]byte) []byte {
		opts := mergeOpts()
		opts.Uncompressed, opts.NoCoalesce = c == CodecNone, noCoalesce
		out, _, err := mergeBytes(t.Context(), inputsOf(files...), opts)
		require.NoError(t, err)

		return out
	}

	tests := []struct {
		name string
		out  func() []byte
		want packDigests
	}{
		{
			name: "merge, segments copied, none",
			out: func() []byte {
				return merged(CodecNone, true,
					mergePackCodec(t, CodecNone, seg0, hourSteps(30, 49, 5)),
					mergePackCodec(t, CodecNone, seg1, hourSteps(10, 29, 7)))
			},
			want: packDigests{
				file:   "9330739bf6cfc4ca9c7e54a076574b7ac1ad3839d3d4d907f50bbae9ed9f0a1b",
				meta:   "b0b091f7a38e7e08301582df414c15f45077b43ab07cbe4da290d2f29115f3c4",
				blocks: "87aecc42b58d04895e4195394a3a2030608750fb964e014359836d541c2be216",
				f3:     "b808d601759d0929a20285aa6d9c82d449323cd2d0e6d48339568ae4f19a35fd",
				footer: "fc6431b88bf93cd05e5e20033db0ab77e5acd5de72c42fdbf3c981c98bd4e4b7",
			},
		},
		{
			name: "merge, overlaps resolved and blocks coalesced, zstd",
			out: func() []byte {
				return merged(CodecZstd, false,
					mergePack(t, seg0, nil, repairSteps(t)),
					mergePack(t, seg1, nil, hourSteps(40, 60, 6)),
					mergePack(t, seg2, nil, hourSteps(50, 70, 4)))
			},
			want: packDigests{
				file:   "43bcd538d113fd62fa45ac8e396311c039b8476693564ab18cc86e51aebbb742",
				meta:   "a73271497be671ecc8d5bd2bb2528415eff07bbc638d455820a312a468348672",
				blocks: "eed817cb24700258a3d0cd2acfc00d2d7569371df707544fcdb94a4fed7ccb92",
				f3:     "3e9423553a1475d4364b9352264f7ac1927218b2f3da2d1c8699cb1fd01b057a",
				footer: "7e59ebb31c29445915a1e276e2e503c3effee6f63a225d3f21990c44a7dabc1e",
			},
		},
	}

	for _, tt := range tests {
		got := digestsOf(t, tt.out())
		assert.Equal(t, tt.want, got, tt.name)
		assert.Equal(t, got, digestsOf(t, tt.out()), "%s: the same bytes written twice", tt.name)
	}
}
