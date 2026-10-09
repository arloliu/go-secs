package corpus

import (
	"github.com/arloliu/go-secs/tracepack"
	"github.com/arloliu/go-secs/tracepack/internal/format"
)

// truncationSeed is the identity seed of the truncation vectors' base packs, shared by both codecs.
const truncationSeed = "verify-truncation"

// truncationVectors returns the truncation vectors of the verify group (the tracepack corpus specification §9.4):
// the base pack of truncationBasePack under codec none and codec zstd, with the expectation of every cut.
func truncationVectors() []Recipe {
	return []Recipe{
		{
			ID: "verify-truncation-none", Title: "a pack of two blocks, codec none, cut at every byte offset",
			Cites: []string{"FMT §16", "FMT §13", "CORPUS §5.7"}, Class: ClassTruncation, IdentitySeed: truncationSeed,
			Build: func(seed string) (*Built, error) {
				pack, err := truncationBasePack(seed, tracepack.CodecNone)

				return &Built{Pack: pack}, err
			},
			Expect: truncationExpect(),
		},
		{
			ID: "verify-truncation-zstd", Title: "the records of verify-truncation-none, blocks and footer of codec zstd, cut at every byte offset",
			Cites: []string{"FMT §16", "FMT §13", "CORPUS §5.7", "CORPUS §6"}, Class: ClassTruncation, IdentitySeed: truncationSeed,
			Codec: CodecZstd, EncoderMade: true,
			Build: func(seed string) (*Built, error) {
				pack, err := truncationBasePack(seed, tracepack.CodecZstd)

				return &Built{Pack: pack}, err
			},
			Expect: truncationExpect(),
		},
	}
}

// truncationBasePack writes a finalized pack of two blocks of epoch 1 under codec c:
// a socket-connect (seq 0) and an S1F1 (seq 1), then the S1F2 reply (seq 2) and a socket-close (seq 3).
func truncationBasePack(seed string, c tracepack.Codec) ([]byte, error) {
	connect, err := newSocketEvent(0, 1, tracepack.EventSocketConnect)
	if err != nil {
		return nil, err
	}
	closed, err := newSocketEvent(3, 1, tracepack.EventSocketClose)
	if err != nil {
		return nil, err
	}

	return writeSpec(&packSpec{seed: seed, meta: segmentMeta(seed), codec: c, blocks: [][]tracepack.Record{
		{connect, newData(1, 1, tracepack.DirHostToEquipment, dataFrame(1, 1, true, 1, nil))},
		{newData(2, 1, tracepack.DirEquipmentToHost, dataFrame(1, 2, false, 1, []byte{0x41, 0x00})), closed},
	}})
}

// truncationExpect is the expectation of a truncationBasePack and of its cuts (the tracepack format specification §13):
// a cut inside the file header is too short, one inside the pack metadata ends before it;
// every other cut opens unfinalized with the blocks wholly before it validated,
// and a cut that ends inside a block, or inside the footer and trailer, stops the walk there.
func truncationExpect() *Expectation {
	read := func(from Pos, validated int, records uint64, end Pos, stopped bool) CutWant {
		c := CutWant{From: from, Outcome: tracepack.OutcomeUnfinalized, Validated: validated, Records: records, PrefixEnd: end}
		if stopped {
			c.WalkStop = end
		}

		return c
	}

	return &Expectation{
		Outcome: tracepack.OutcomeFinalizedConsistent, FooterValid: true, Blocks: 2, Seqs: seqRange(0, 3), PrefixEnd: AtEnd(),
		Cuts: []CutWant{
			{From: AtOffset(0), Rejection: RejectShortObject},
			{From: AtOffset(format.FileHeaderLen), Rejection: RejectMetadataPastObject},
			read(AtBlock(0), 0, 0, AtBlock(0), false),
			read(AtBlock(0).Plus(1), 0, 0, AtBlock(0), true),
			read(AtBlock(1), 1, 2, AtBlock(1), false),
			read(AtBlock(1).Plus(1), 1, 2, AtBlock(1), true),
			read(AtEnd(), 2, 4, AtEnd(), false),
			read(AtEnd().Plus(1), 2, 4, AtEnd(), true),
		},
	}
}
