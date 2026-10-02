package tracepack

import (
	"context"
	"fmt"
	"io"
)

// blockIdentity is what a block's F-2 entry states about its bytes and records:
// first and last seq, record count, lengths, record_header_len and body_crc.
// Once each block agrees with its F-2 entry, blocks of different identities differ in their bytes,
// while blocks of one identity are only candidates for equality (the tracepack storage specification §4 Merge).
type blockIdentity struct {
	firstSeq        uint64
	lastSeq         uint64
	recordCount     uint32
	onDiskLen       uint32
	uncompressedLen uint32
	recordHeaderLen uint16
	bodyCRC         uint32
}

// mergeUnit is one block the archive is written from, unless coalescing encodes it with others:
// an input block, written as it is with its F-3 list,
// or a block the record-by-record resolution encoded, written with the F-3 list its summary gives.
//
// A unit's bytes and records alias the buffers it was read or encoded in,
// which the next read, or the resolution's next block, reuses,
// so a unit is valid only until emit returns;
// a unit that waits in a coalescing group is copied into the group's buffers (coalesceGroup).
// An input block's F-3 list is never copied by the merge:
// it stays in the input's retained F-3 section, which lives as long as the merge,
// and the Writer copies it only once its footer budget admits the block.
type mergeUnit struct {
	// raw holds the block's envelope and on-disk body.
	raw []byte
	// sum is the summary of the block's records (summaryOf), sharing no memory with the buffers the unit aliases.
	sum blockSummary
	// f3 is an input block's F-3 entry list as its input's footer stores it,
	// aliasing the input's retained F-3 section, which is never modified; nil for an encoded unit.
	f3 []byte
	// headerLen is the block's record_header_len, and size its decoded size, Σ record_header_len + payload_len.
	headerLen int
	size      uint64
	// d holds an input block's records, as the block was read and decoded,
	// aliasing the buffers it was read in, as raw does; nil for an encoded unit.
	d *decodedBlock
	// rows holds each record's header row of an encoded unit, record_header_len bytes as stored,
	// and payloads each record's payload, in seq order; nil for an input block, whose records d holds.
	rows     [][]byte
	payloads [][]byte
	// encoded reports a block the record-by-record resolution encoded, not an input block.
	encoded bool
}

// mergeRun is the state of one archive write (mergePlan.write):
// the Writer, the report so far, the block buffers reads reuse, the unit the resolution is filling,
// and the pending coalescing group.
type mergeRun struct {
	p *mergePlan
	w *Writer
	// codec encodes the blocks the resolution and coalescing encode.
	codec      Codec
	onConflict func(Conflict)
	// coalesce reports that units are coalesced, MergeOptions.NoCoalesce not being set.
	coalesce bool
	rep      MergeReport
	// free holds the block buffers that no block holds, for the next reads.
	free []*blockBuf
	// unit holds the records of the next block the resolution encodes, and enc the buffers it is encoded in.
	unit unitBuilder
	enc  encodeBuf
	// group is the pending coalescing group.
	group coalesceGroup
	// resolved counts the records every resolution of the merge resolved, one per seq,
	// for the check of ctx every resolveCtxRecords records.
	resolved int
}

// newMergeUnit returns the unit of an input block read as d, whose envelope and on-disk body raw holds:
// s is the summary of its records and f3 its F-3 list as its input's footer stores it.
// The unit aliases raw and the buffers d was decoded in, so it is valid only until the next read into them.
func newMergeUnit(raw []byte, d *decodedBlock, s blockSummary, f3 []byte) mergeUnit {
	return mergeUnit{
		raw: raw, sum: s, f3: f3, d: d,
		headerLen: int(d.env.RecordHeaderLen), size: uint64(d.env.UncompressedLen),
	}
}

// write writes the archive p plans to dst, as Merge states it:
// the header, the units of every cluster in ascending first seq, coalesced unless opts.NoCoalesce is set,
// then the footer and the trailer.
// An error after the header returns without closing the Writer, so no footer or trailer is written,
// and so does a conflict, once every cluster was read and compared.
// On error the report holds only the conflicts found.
func (p *mergePlan) write(ctx context.Context, dst io.Writer, opts *MergeOptions) (MergeReport, error) {
	m, err := p.startRun(dst, opts)
	if err != nil {
		return MergeReport{}, err
	}
	for k := range p.clusters {
		if err := m.writeCluster(ctx, &p.clusters[k]); err != nil {
			return MergeReport{Conflicts: m.rep.Conflicts}, err
		}
	}
	if n := m.rep.Conflicts; n > 0 {
		return MergeReport{Conflicts: n, ConflictsComplete: true}, fmt.Errorf("tracepack: merge: %d conflicts: %w", n, ErrMergeConflict)
	}

	// No unit follows the last group.
	if err := m.flushGroup(); err != nil {
		return MergeReport{}, err
	}
	if err := ctx.Err(); err != nil {
		return MergeReport{}, fmt.Errorf("tracepack: merge: %w", err)
	}
	if _, err := m.w.Close(); err != nil {
		return MergeReport{}, err
	}
	m.rep.Size = m.w.offset
	m.rep.ConflictsComplete = true

	return m.rep, nil
}

// startRun writes the archive's file header and pack metadata to dst,
// and returns the run that writes the rest of the archive.
func (p *mergePlan) startRun(dst io.Writer, opts *MergeOptions) (*mergeRun, error) {
	w, err := startWriter(dst, WriterOptions{
		Meta: p.meta, Facts: p.facts, Codec: opts.Codec, Sync: opts.Sync,
		PackID: p.packID, CaptureID: p.captureID,
	})
	if err != nil {
		return nil, err
	}
	// newMergePlan applied the default, so the budget is never 0, which would leave the Writer without one.
	w.footerBudget = uint64(p.maxFooterLen)

	m := &mergeRun{
		p: p, w: w, codec: opts.Codec, onConflict: opts.OnConflict, coalesce: !opts.NoCoalesce,
		rep: MergeReport{PackID: p.packID, ReplacementSetID: *p.meta.ReplacementSetID, Inputs: len(p.inputs)},
	}
	m.enc.hook = p.encodedHook
	m.group.enc.hook = p.coalescedHook

	return m, nil
}

// readUnit reads block b in full into buf, checks it as readInputBlock does, and returns its unit, which aliases buf.
func (p *mergePlan) readUnit(ctx context.Context, b *mergeBlock, buf *blockBuf) (mergeUnit, error) {
	d, s, err := p.readInputBlock(ctx, b, buf)
	if err != nil {
		return mergeUnit{}, err
	}

	return newMergeUnit(buf.raw, d, s, b.f3), nil
}

// readInputBlock checks ctx, then reads block b of its input in full into buf
// and checks it as the tracepack storage specification §4 Merge requires before any of its bytes is written:
// the checks of a full read (the tracepack format specification §6), its agreement with its F-2 entry,
// and the summary of its records against its F-3 summary (checkSummary).
// A read that fails, or ends early, is the input's read error, as verification returns it:
// the preflight opened the whole object,
// so a block read cut short says that the object changed or its ReaderAt is inconsistent, not that the pack is defective.
//
// Returns:
//   - *decodedBlock: the block, aliasing buf until buf's next use; nil on error.
//   - blockSummary: the summary of its records (summaryOf), sharing no memory with buf.
//   - error: ctx's error, wrapped;
//     an error wrapping ErrMergeInput, naming the pack and the block, as ErrMergeInput lists;
//     an error wrapping the input's ReaderAt error, or io.ErrUnexpectedEOF for a read cut short,
//     or the ErrReadLimit of a block over MaxBlockLen, naming the input and the block.
func (p *mergePlan) readInputBlock(ctx context.Context, b *mergeBlock, buf *blockBuf) (*decodedBlock, blockSummary, error) {
	if err := ctx.Err(); err != nil {
		return nil, blockSummary{}, fmt.Errorf("tracepack: merge: %w", err)
	}

	r, id := p.inputs[b.input], p.inputID(b.input)
	d, def, err := r.readBlock(b.block, buf)
	switch {
	case err != nil:
		return nil, blockSummary{}, fmt.Errorf("tracepack: merge: input %d, pack %s, block %d: %w", b.input, id, b.block, err)
	case def != nil && def.Reason == ReasonLimit:
		return nil, blockSummary{}, fmt.Errorf("tracepack: merge: input %d, pack %s: %w", b.input, id, def.Err)
	case def != nil:
		// A failed block, or a block whose records disagree with its F-2 entry, which readBlock returns beside the block.
		return nil, blockSummary{}, inputError(b.input, id, def.Err)
	}

	s := summaryOf(d)
	if err := checkSummary(&r.footer.blocks[b.block], &s); err != nil {
		return nil, blockSummary{}, inputError(b.input, id, blockDefect(b.block, &r.blocks[b.block], ReasonIndexMismatch, err).Err)
	}

	return d, s, nil
}

// duplicateCandidate reports whether every block of cluster c has the F-2 identity of its first block.
func (p *mergePlan) duplicateCandidate(c *mergeCluster) bool {
	first := p.identity(&c.blocks[0])
	for k := 1; k < len(c.blocks); k++ {
		if p.identity(&c.blocks[k]) != first {
			return false
		}
	}

	return true
}

// identity returns the F-2 identity of block b.
func (p *mergePlan) identity(b *mergeBlock) blockIdentity {
	info := &p.inputs[b.input].blocks[b.block]

	return blockIdentity{
		firstSeq: info.FirstSeq, lastSeq: info.LastSeq, recordCount: info.RecordCount,
		onDiskLen: info.OnDiskLen, uncompressedLen: info.UncompressedLen,
		recordHeaderLen: info.RecordHeaderLen, bodyCRC: info.BodyCRC,
	}
}

// inputID returns the pack_id of input i, which openInputs checked against the view's Packs.
func (p *mergePlan) inputID(i int) UUID {
	return UUID(p.inputs[i].hdr.PackID)
}

// writeCluster reads every block of cluster c once, in full, checking it as readInputBlock does,
// and emits the units the archive holds for c, in seq order:
// a block alone, as its unit;
// a duplicate candidate, a cluster of one F-2 identity, whose blocks are byte-for-byte equal, as the unit of its first block,
// the others dropped as duplicates;
// any other cluster, and a duplicate candidate whose blocks differ, as the units its resolution encodes.
// The choice between a duplicate candidate and a resolution is made from F-2, before any record of c is resolved.
func (m *mergeRun) writeCluster(ctx context.Context, c *mergeCluster) error {
	if len(c.blocks) == 1 {
		buf := m.buffer()
		u, err := m.p.readUnit(ctx, &c.blocks[0], buf)
		if err != nil {
			return err
		}
		err = m.emit(&u)
		m.release(buf)

		return err
	}

	r := &resolution{m: m, c: c}
	if m.p.duplicateCandidate(c) {
		// Its blocks share their first seq, so each is loaded, or dropped as a duplicate, before any record is resolved,
		// and a block that differs is resolved with the blocks already read.
		if err := r.loadFrontier(ctx); err != nil {
			return err
		}
		if len(r.open) == 1 {
			o := r.open[0]
			u := newMergeUnit(o.buf.raw, o.d, r.sums[o.pos], c.blocks[o.pos].f3)
			err := m.emit(&u)
			m.release(o.buf)

			return err
		}
	}

	return r.run(ctx)
}

// buffer returns a block buffer that no block holds.
func (m *mergeRun) buffer() *blockBuf {
	n := len(m.free)
	if n == 0 {
		return new(blockBuf)
	}
	buf := m.free[n-1]
	m.free = m.free[:n-1]

	return buf
}

// release returns buf, whose block is no longer held, for a later read.
func (m *mergeRun) release(buf *blockBuf) {
	m.free = append(m.free, buf)
}
