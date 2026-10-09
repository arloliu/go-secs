package corpus

import (
	"context"
	"encoding/binary"
	"fmt"
	"slices"

	"github.com/arloliu/go-secs/tracepack"
	"github.com/arloliu/go-secs/tracepack/internal/format"
)

// InjectionID is the id of the vector holding the output of a validating Writer
// whose encoded block has an injected I-2 defect.
const InjectionID = "validation-i2-injection"

// WriterRun is what a Writer of the corpus is given: its options, without its output, and the records of each block,
// appended in order and flushed after each block.
type WriterRun struct {
	Options tracepack.WriterOptions
	Blocks  [][]tracepack.Record
}

// InjectionRun returns the run of the validating Writer of validation-i2-injection.
// Its last block is the one whose encoded body the run damages:
// a caller passes each encoded body unchanged but the last, given to InjectI2.
// The Writer then fails that block's validation, writes neither the block nor a trailer,
// and its output equals the vector's pack.tpk:
// the file header, the pack metadata and every earlier block (the tracepack format specification §12 and §16).
//
// Returns:
//   - *WriterRun: the run.
//   - error: a record that cannot be built.
func InjectionRun() (*WriterRun, error) {
	seed := InjectionID
	connect, err := newSocketEvent(0, 1, tracepack.EventSocketConnect)
	if err != nil {
		return nil, err
	}
	s := &packSpec{seed: seed, meta: segmentMeta(seed)}

	return &WriterRun{Options: s.options(), Blocks: [][]tracepack.Record{
		{connect, footerData(1, 1)},
		{footerData(2, 1), footerData(3, 1)},
		{footerData(4, 1), footerData(5, 1)},
	}}, nil
}

// InjectI2 returns a copy of enc, an encoded block body of codec none, whose first byte is inverted:
// the low byte of the first record's seq,
// so that the block breaks I-2 (its first seq is not first_seq) while its encoding is whole.
func InjectI2(enc []byte) []byte {
	out := slices.Clone(enc)
	if len(out) > 0 {
		out[0] ^= 0xFF
	}

	return out
}

// validationVectors returns the recipes of the validation group (the tracepack corpus specification §9.4).
func validationVectors() []Recipe {
	return []Recipe{
		{
			ID: InjectionID, Title: "a validating Writer's output stopped before a block with an injected I-2 defect",
			Cites: []string{"FMT §12", "FMT §16", "FMT I-2", "CORPUS §1", "CORPUS §5.4"}, Class: ClassRead,
			Build: func(seed string) (*Built, error) {
				run, err := InjectionRun()
				if err != nil {
					return nil, err
				}
				// The Writer has written every block before the damaged one when it fails.
				s := &packSpec{seed: seed, meta: run.Options.Meta, blocks: run.Blocks[:len(run.Blocks)-1], open: true}
				pack, err := writeSpec(s)
				if err != nil {
					return nil, err
				}
				if err := checkInjection(run, InjectI2); err != nil {
					return nil, err
				}

				return &Built{Pack: pack, Queries: []QuerySpec{allQuery()}}, nil
			},
			// Every block written is walked and validated; the pack is not finalized,
			// so a read reports it truncated at the end of its last block (the tracepack corpus specification §5.4).
			Expect: &Expectation{
				Outcome: tracepack.OutcomeUnfinalized, Unfinalized: true, Blocks: 2, Seqs: seqRange(0, 3), PrefixEnd: AtEnd(),
				Queries: []QueryWant{{ID: "all", Seqs: seqRange(0, 3), Incomplete: []IncompleteWant{Truncated(AtEnd())}}},
			},
		},
	}
}

// checkInjection checks that inject, InjectI2 outside tests, breaks I-2, and only I-2, in the last block of run:
// the run's blocks written whole and left unfinalized, so that no F-2 entry is compared,
// with inject applied to the last block's stored body and its envelope's body_crc recomputed,
// verify that block, and that block alone, as failed.
func checkInjection(run *WriterRun, inject func(enc []byte) []byte) error {
	last := len(run.Blocks) - 1
	pack, err := writeSpec(&packSpec{seed: InjectionID, meta: run.Options.Meta, blocks: run.Blocks, open: true})
	if err != nil {
		return err
	}
	l, err := Locate(pack)
	if err != nil {
		return err
	}
	s, err := l.block(last)
	if err != nil {
		return err
	}
	body := inject(pack[s.Offset+format.EnvelopeLen : s.Offset+s.Len])
	copy(pack[s.Offset+format.EnvelopeLen:], body)
	if pack, err = PatchEnvelope(pack, last, func(env []byte) {
		binary.LittleEndian.PutUint32(env[envelopeBodyCRCOff:], format.CRC(body))
	}); err != nil {
		return err
	}

	v, err := verifyBytes(context.Background(), pack)
	if err != nil {
		return err
	}
	if len(v.FailedBlocks) != 1 || v.FailedBlocks[0].Block != last || v.WalkStop != nil || v.BlocksValidated != last {
		return fmt.Errorf("corpus: the injected defect does not fail block %d alone: %+v", last, v)
	}

	return nil
}
