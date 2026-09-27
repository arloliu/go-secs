package tracepack_test

import (
	"bytes"
	"errors"
	"fmt"
	"testing"

	"github.com/stretchr/testify/require"

	"github.com/arloliu/go-secs/tracepack"
	"github.com/arloliu/go-secs/tracepack/internal/codec"
	"github.com/arloliu/go-secs/tracepack/internal/format"
)

// The forward-walk reader below is test-only:
// it reads an unfinalized pack the way the tracepack format specification I-1 allows a reader to without a footer,
// file header, then pack metadata, then block after block (envelope, body, header section, payload section),
// and checks I-2 on every block, so every writer test asserts through bytes a reader independent of the writer accepts.

// errWalk reports a forward-walk failure that is not already an error of an internal decoder.
var errWalk = errors.New("forward walk")

// walkedBlock is one block of a walked pack.
type walkedBlock struct {
	// Offset is the file offset of the block envelope.
	Offset int
	// Env is the decoded block envelope.
	Env format.BlockEnvelope
	// Body is the on-disk block body.
	Body []byte
	// Records are the block's records, their payloads copied out of the decoded body.
	Records []tracepack.Record
}

// walkedPack is a pack read by walkPack.
type walkedPack struct {
	Header format.FileHeader
	// MetaBytes are the raw pack metadata bytes.
	MetaBytes []byte
	Meta      *tracepack.PackMeta
	Blocks    []walkedBlock
}

// records returns every record of p in file order.
func (p *walkedPack) records() []tracepack.Record {
	var out []tracepack.Record
	for _, b := range p.Blocks {
		out = append(out, b.Records...)
	}

	return out
}

// mustWalkPack walks b and fails the test on any error.
func mustWalkPack(t *testing.T, b []byte) *walkedPack {
	t.Helper()

	p, err := walkPack(b)
	require.NoError(t, err)

	return p
}

// walkPack reads the file header, the pack metadata and every block of b, in file order, until b ends.
func walkPack(b []byte) (*walkedPack, error) {
	h, err := format.UnmarshalFileHeader(b)
	if err != nil {
		return nil, err
	}

	metaEnd := format.FileHeaderLen + int(h.PackMetadataLen)
	if metaEnd > len(b) {
		return nil, fmt.Errorf("%w: pack metadata ends at %d, file is %d bytes", errWalk, metaEnd, len(b))
	}

	metaBytes := b[format.FileHeaderLen:metaEnd]
	if crc := format.CRC(metaBytes); crc != h.PackMetadataCRC {
		return nil, fmt.Errorf("%w: pack metadata CRC 0x%08X, header says 0x%08X", errWalk, crc, h.PackMetadataCRC)
	}

	meta, err := tracepack.UnmarshalPackMeta(metaBytes)
	if err != nil {
		return nil, err
	}

	p := &walkedPack{Header: h, MetaBytes: metaBytes, Meta: meta}
	for off := metaEnd; off < len(b); {
		blk, next, err := walkBlock(b, off)
		if err != nil {
			return nil, fmt.Errorf("block at %d: %w", off, err)
		}

		p.Blocks = append(p.Blocks, blk)
		off = next
	}

	return p, nil
}

// walkBlock reads the block whose envelope starts at b[off:] and returns it with the offset after its body.
func walkBlock(b []byte, off int) (walkedBlock, int, error) {
	env, err := format.UnmarshalBlockEnvelope(b[off:])
	if err != nil {
		return walkedBlock{}, 0, err
	}

	bodyStart := off + format.EnvelopeLen
	bodyEnd := bodyStart + int(env.BodyLen)
	if bodyEnd > len(b) {
		return walkedBlock{}, 0, fmt.Errorf("%w: body ends at %d, file is %d bytes", errWalk, bodyEnd, len(b))
	}

	body := b[bodyStart:bodyEnd]
	if crc := format.CRC(body); crc != env.BodyCRC {
		return walkedBlock{}, 0, fmt.Errorf("%w: body CRC 0x%08X, envelope says 0x%08X", errWalk, crc, env.BodyCRC)
	}

	decoded, err := codec.Decode(env.Codec, nil, body, int(env.UncompressedLen))
	if err != nil {
		return walkedBlock{}, 0, err
	}

	records, err := walkRecords(&env, decoded)
	if err != nil {
		return walkedBlock{}, 0, err
	}

	return walkedBlock{Offset: off, Env: env, Body: body, Records: records}, bodyEnd, nil
}

// walkRecords splits a decoded body into its records and checks the I-2 equations on them.
func walkRecords(env *format.BlockEnvelope, decoded []byte) ([]tracepack.Record, error) {
	hsLen, err := format.HeaderSectionLen(env.RecordCount, env.RecordHeaderLen)
	if err != nil {
		return nil, err
	}
	if hsLen > len(decoded) {
		return nil, fmt.Errorf("%w: header section %d bytes, body %d", errWalk, hsLen, len(decoded))
	}

	hlen := int(env.RecordHeaderLen)
	records := make([]tracepack.Record, 0, env.RecordCount)
	payloadOff := hsLen

	for i := range int(env.RecordCount) {
		h, err := format.UnmarshalRecordHeader(decoded[i*hlen:], hlen)
		if err != nil {
			return nil, err
		}

		end := payloadOff + int(h.PayloadLen)
		if end > len(decoded) {
			return nil, fmt.Errorf("%w: record %d payload ends at %d, body %d", errWalk, i, end, len(decoded))
		}

		if err := checkSeqOrder(env, records, i, h.Seq); err != nil {
			return nil, err
		}

		records = append(records, recordOf(&h, bytes.Clone(decoded[payloadOff:end])))
		payloadOff = end
	}

	if payloadOff != len(decoded) {
		return nil, fmt.Errorf("%w: headers and payloads cover %d bytes, uncompressed_len %d", errWalk, payloadOff, len(decoded))
	}

	return records, nil
}

// checkSeqOrder checks the seq rules of I-2 for record i: the first equals first_seq, later ones strictly increase.
func checkSeqOrder(env *format.BlockEnvelope, prev []tracepack.Record, i int, seq uint64) error {
	if i == 0 && seq != env.FirstSeq {
		return fmt.Errorf("%w: first seq %d, envelope first_seq %d", errWalk, seq, env.FirstSeq)
	}
	if i > 0 && seq <= prev[i-1].Seq {
		return fmt.Errorf("%w: record %d seq %d after %d", errWalk, i, seq, prev[i-1].Seq)
	}

	return nil
}

// recordOf converts a decoded record header and its payload into a Record.
func recordOf(h *format.RecordHeader, payload []byte) tracepack.Record {
	flags := tracepack.RecordFlags(h.RecordFlags)

	return tracepack.Record{
		Seq:           h.Seq,
		TSUTCNs:       h.TSUTCNs,
		MonoNs:        h.MonoNs,
		MonoPresent:   flags.Has(tracepack.RecordFlagsMonoPresent),
		Epoch:         h.Epoch,
		Kind:          tracepack.Kind(h.Kind),
		Dir:           tracepack.Dir(h.Dir),
		Fidelity:      tracepack.Fidelity(h.Fidelity),
		DecodeStatus:  tracepack.DecodeStatus(h.DecodeStatus),
		TrailingBytes: h.TrailingBytes,
		SystemBytes:   h.SystemBytes,
		SessionID:     h.SessionID,
		Stream:        h.Stream,
		W:             flags.Has(tracepack.RecordFlagsW),
		Function:      h.Function,
		PType:         h.PType,
		SType:         h.SType,
		Quality:       tracepack.Quality(h.Quality),
		FieldValidity: tracepack.FieldValidity(h.FieldValidity),
		Payload:       payload,
	}
}
