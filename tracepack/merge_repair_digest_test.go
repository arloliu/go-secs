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

// TestMergeAndRepairOutputDigests pins the bytes Merge and Repair write for fixed inputs, ids and options.
// Both write through the Writer, which does not validate their blocks and checks only their period, not their records' hours,
// so a change to the Writer that must leave their output alone shows here.
// A deliberate change to either output updates the digests,
// and so does an upgrade of the zstd encoder (github.com/klauspost/compress), which changes the zstd digests.
func TestMergeAndRepairOutputDigests(t *testing.T) {
	t.Parallel()

	repairID := UUID{0x4E, 0x9A}
	repairCapture := UUID{0xCA, 0x4E}
	repairInput := func(c Codec, open bool) *repairPack {
		p := writeRepairPack(t, c, nil, open, repairSteps(t))
		p.file = withIDs(t, p.file, seg0, repairCapture)

		return p
	}
	repaired := func(file []byte, c Codec) []byte {
		opts := repairOpts()
		opts.PackID, opts.Codec = repairID, c
		patch, _, err := repairBytes(t, file, opts)
		require.NoError(t, err)

		return patch
	}
	merged := func(c Codec, noCoalesce bool, files ...[]byte) []byte {
		v := mergeView(t, nil, files...)
		opts := mergeOpts()
		opts.Codec, opts.NoCoalesce = c, noCoalesce
		out, _, err := mergeBytes(t.Context(), v, inputsOf(t, v, files...), opts)
		require.NoError(t, err)

		return out
	}

	zstdPack := repairInput(CodecZstd, false)
	nonePack := repairInput(CodecNone, false)
	openPack := repairInput(CodecZstd, true)
	cut := openPack.file[:openPack.blocks[3].offset+50]

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
		{
			name: "repair, failed middle block, zstd",
			out:  func() []byte { return repaired(flipAll(zstdPack.file, zstdPack, 2), CodecZstd) },
			want: packDigests{
				file:   "d9bc7998868659ea7e1b1e2dc802f9b95d17e782e137bb4a546c295842ef9449",
				meta:   "dd0cc641e97a369f7f90e9200a6f89148b570a75d756a8ac30edd6f38d5ec045",
				blocks: "64f5d46f0e733ead33857f88b710f6d2f0bd756a24a6c4908140c352b61ede72",
				f3:     "8cac9ff124ee06e8edd2291563f835e696abedbf98d18de6f5ddee9ad742b355",
				footer: "c27db23e3c2473d16b3668ec988a169fda456ba07660b3b7f79c449973c46595",
			},
		},
		{
			name: "repair, invalid footer and two lost runs, none",
			out: func() []byte {
				return repaired(flipAll(invalidFooterFile(t, nonePack.file), nonePack, 1, 3), CodecNone)
			},
			want: packDigests{
				file:   "823f4fc7d89ec288cb584140b61d77a9d6d383f4a8ddce113bc6225a1c97ff18",
				meta:   "7a742584e7e758844e4886264547b5d2b06ac1f4d27dbce92b81c51f9eff2bcd",
				blocks: "6e9818fc6f32d7790a9a85bb83f6ed0eb04a299668de23072a92467182f4dd0a",
				f3:     "b9a3d05b0c619ac96b153d33754533c6b2bea6b9a7a83fb00c729b7503dd67b8",
				footer: "85758678b629319b79777a3dba300e87c824f250c96ff7c25e90ec5cf4d9209a",
			},
		},
		{
			name: "repair, unfinalized and cut inside a block, zstd",
			out:  func() []byte { return repaired(cut, CodecZstd) },
			want: packDigests{
				file:   "2aa20aff287a5035b5a754425f994d7a604baefebad5ba190d8b8a081560a1eb",
				meta:   "b72e1a5965f10970a7c0eb140f2a0498d16aa6d414ca30e3a3c633ee13be135c",
				blocks: "d92fbd6c5125d1feb407b2baf1361555746b80f354f98e8a4b46548e05015ebc",
				f3:     "2831ea8592212cd213d8e114ae2628e6ca8b3284d1608fe597442e04a21eafc3",
				footer: "821ba538c2bd24a6a09b0979f63644231a3ea8b1db20eb674a4c19cc47742c63",
			},
		},
	}

	for _, tt := range tests {
		got := digestsOf(t, tt.out())
		assert.Equal(t, tt.want, got, tt.name)
		assert.Equal(t, got, digestsOf(t, tt.out()), "%s: the same bytes written twice", tt.name)
	}
}
