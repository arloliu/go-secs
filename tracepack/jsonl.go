package tracepack

import (
	"context"
	"errors"
	"fmt"
	"io"
	"slices"

	"github.com/arloliu/go-secs/tracepack/internal/format"
)

// ExportJSONL writes the canonical JSONL export of the pack r reads to w
// (the tracepack format specification §15, the tracepack JSONL specification).
//
// The export is a header line, from the file header and the pack metadata as stored,
// then one record line per record r.Iterate(ctx, Query{Payloads: true}, fn) yields, in the same order:
// the records of every block the read validates, in file order, a block whose records disagree with its F-2 entry included.
// A failed block is left out, and nothing marks its place.
// Every record header field is written as stored, reserved bits and unknown enum values included,
// with the block's record_header_len, the header's extension area, and the payload;
// a transport-event or annotation record whose payload is a valid TLV body also carries the body, rendered by name.
// A record whose payload is not a valid body is written without it and adds nothing to the Result.
// The output depends on the pack's bytes alone: two exports of one pack are byte-identical.
//
// The Result is the one that Iterate call returns:
// the defects Open found, the coverage entries of the pack metadata, which every read intersects,
// and the defect of every block that failed or disagrees with its F-2 entry;
// FooterErr and FooterErrs as Iterate sets them, a footer error alone not making the Result incomplete.
// Where Iterate reports a reader budget as a ReasonLimit defect, ExportJSONL fails instead,
// so that an export that succeeds never depends on ReaderOptions:
// before writing anything when the footer was not used because it is over MaxFooterLen
// or the forward walk stopped at MaxWalkedBlocks,
// and when it reaches a block over MaxBlockLen, after the lines of the blocks before it.
// No ReasonLimit defect is ever listed in the Result.
//
// ExportJSONL writes each line in pieces of at most 64 KiB, so a line may take several Write calls.
// Beyond the block it reads, as Iterate holds it, and the Result's diagnostics,
// its memory does not grow with the length of a payload, a value or an entry list.
//
// Parameters:
//   - ctx: cancels the export before the header line and between blocks.
//   - r: the pack; its file header and pack metadata passed Open, so every pack it reads has an export.
//   - w: receives the export.
//
// Returns:
//   - Result: the status of the read; on an error it holds what was found before the error.
//   - error: an error wrapping ErrReadLimit for a reader budget, as above;
//     ctx's error, wrapped; a ReadAt error, as Iterate returns it, wrapping io.ErrUnexpectedEOF for a short read without another error;
//     or a write error of w, wrapped, io.ErrShortWrite for a Write that accepts fewer bytes without an error.
//     On an error the bytes already written stay written, and the caller discards them: they are not an export.
func ExportJSONL(ctx context.Context, r *Reader, w io.Writer) (Result, error) {
	// The zero filter selects every record and prunes no block, as Query{Payloads: true} does.
	var f Filter
	res := r.startResult(&f)
	if err := r.exportLimitErr(); err != nil {
		res.Incomplete = slices.DeleteFunc(res.Incomplete, func(d Defect) bool { return d.Reason == ReasonLimit })

		return res, err
	}
	if err := ctx.Err(); err != nil {
		return res, fmt.Errorf("tracepack: export JSONL: %w", err)
	}

	jw := newJSONLWriter(w)
	if err := writeJSONLHeader(jw, r); err != nil {
		return res, err
	}

	return r.exportBlocks(ctx, jw, &f, res)
}

// jsonlBodyRegistry returns the registry of the body of a record of kind,
// or nil for a kind whose records carry no body (the tracepack JSONL specification §6).
func jsonlBodyRegistry(kind Kind) *jsonlRegistry {
	if kind == KindTransportEvent {
		return jsonlTransportEventRegistry
	}
	if kind == KindAnnotation {
		return jsonlAnnotationRegistry
	}

	return nil
}

// writeJSONLRecord writes the record line of record j of the block d (the tracepack JSONL specification §6)
// and returns the first write error of jw.
// The fields come from the gathered record header as stored, so reserved and retired bits survive.
func writeJSONLRecord(jw *jsonlWriter, d *decodedBlock, j int) error {
	h := d.header(j)
	payload := d.payload(j)

	jw.beginObject()
	jw.key("seq")
	jw.u64(h.Seq)
	jw.key("ts_utc_ns")
	jw.i64(h.TSUTCNs)
	jw.key("mono_ns")
	jw.i64(h.MonoNs)
	jw.key("epoch")
	jw.num(h.Epoch)
	jw.key("payload_len")
	jw.num(h.PayloadLen)
	jw.key("trailing_bytes")
	jw.num(h.TrailingBytes)
	jw.key("quality")
	jw.bitSet(jsonlQualityNames, uint32(h.Quality))
	jw.key("kind")
	jw.enum(jsonlKindNames, h.Kind)
	jw.key("dir")
	jw.enum(jsonlDirNames, h.Dir)
	jw.key("fidelity")
	jw.enum(jsonlFidelityNames, h.Fidelity)
	jw.key("decode_status")
	jw.enum(jsonlDecodeStatusNames, h.DecodeStatus)
	jw.key("field_validity")
	jw.bitSet(jsonlFieldValidityNames, uint32(h.FieldValidity))
	jw.key("record_flags")
	jw.bitSet(jsonlRecordFlagNames, uint32(h.RecordFlags))
	jw.key("record_header_len")
	jw.num(uint32(d.env.RecordHeaderLen))
	if d.env.RecordHeaderLen > format.RecordHeaderLen {
		jw.key("header_extra")
		jw.base64(h.Extra)
	}
	jw.key("payload")
	jw.base64(payload)
	// Only the kind and the payload decide the body; jsonlValidBody checks any other kind as an annotation.
	if reg := jsonlBodyRegistry(Kind(h.Kind)); reg != nil && jsonlValidBody(Kind(h.Kind), payload) {
		jw.key("body")
		reg.render(jw, payload)
	}
	jw.endObject()

	return jw.endLine()
}

// exportBlocks writes the record lines of every block of r that the filter f, which selects every record, does not prune,
// in file order, reading each as Iterate does, and adds each block's defect to res.
// It checks ctx before each block, and fails at a block over MaxBlockLen instead of listing it.
func (r *Reader) exportBlocks(ctx context.Context, jw *jsonlWriter, f *Filter, res Result) (Result, error) {
	var buf blockBuf
	for i := range r.blocks {
		if err := ctx.Err(); err != nil {
			return res, fmt.Errorf("tracepack: export JSONL: %w", err)
		}
		if r.prunes(i, f) {
			continue
		}

		d, def, err := r.readBlock(i, &buf)
		if err != nil {
			return res, err
		}
		if def != nil && def.Reason == ReasonLimit {
			return res, fmt.Errorf("tracepack: export JSONL: %w", def.Err)
		}
		if def != nil {
			res.Incomplete = append(res.Incomplete, *def)
		}
		if d == nil {
			continue
		}
		for j := range d.count() {
			if err := writeJSONLRecord(jw, d, j); err != nil {
				return res, err
			}
		}
	}

	return res, nil
}

// exportLimitErr returns the reader budget the bootstrap reached,
// as the error ExportJSONL fails with before writing anything:
// the footer, or its blocks' F-3 lists, over MaxFooterLen, or a forward walk that stopped at MaxWalkedBlocks.
func (r *Reader) exportLimitErr() error {
	if errors.Is(r.footerErr, ErrReadLimit) {
		return fmt.Errorf("tracepack: export JSONL: %w", r.footerErr)
	}
	if r.walkStop != nil && r.walkStop.Reason == ReasonLimit {
		return fmt.Errorf("tracepack: export JSONL: %w", r.walkStop.Err)
	}

	return nil
}
