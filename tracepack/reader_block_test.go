package tracepack

import (
	"bytes"
	"encoding/binary"
	"fmt"
	"io"
	"math"
	"math/rand/v2"
	"runtime"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/arloliu/go-secs/tracepack/internal/codec"
	"github.com/arloliu/go-secs/tracepack/internal/format"
	"github.com/arloliu/go-secs/tracepack/internal/tlv"
)

// wideExtraLen is the number of bytes a widened block appends behind offset 56 of every record header.
const wideExtraLen = 8

// storedBlockRecord is one record as the Writer stored it: its record header and its payload.
type storedBlockRecord struct {
	h       format.RecordHeader
	payload []byte
}

// testBlock is one block of a pack that rebuildPack re-encodes: its decoded records and the adjustments to apply.
type testBlock struct {
	// env is the block's original envelope; the re-encoded block keeps its record_count and first_seq.
	env      format.BlockEnvelope
	codec    uint8
	headers  []format.RecordHeader
	payloads [][]byte
	// tail is appended to the decoded body after the payload section.
	tail []byte
	// onDisk, when set, adjusts the encoded body before its body_crc is computed.
	onDisk func([]byte) []byte
	// patchEnv, when set, adjusts the envelope written to disk, after the F-2 entry was taken from it.
	patchEnv func(*format.BlockEnvelope)
	// patchEntry, when set, adjusts the block's F-2 entry.
	patchEntry func(*format.F2Entry)
}

// storedBlocks returns the records the Writer stored for recs, split into the blocks it wrote.
func storedBlocks(blocks []blockSummary, recs []Record) [][]storedBlockRecord {
	out := make([][]storedBlockRecord, len(blocks))
	k := 0
	for i := range blocks {
		out[i] = make([]storedBlockRecord, 0, blocks[i].recordCount)
		for range blocks[i].recordCount {
			r := &recs[k]
			k++
			out[i] = append(out[i], storedBlockRecord{h: canonicalHeader(r, r.Seq, transportEventOf(r)), payload: r.Payload})
		}
	}

	return out
}

// stepRecords returns the records of steps.
func stepRecords(steps []footerTestStep) []Record {
	out := make([]Record, len(steps))
	for i := range steps {
		out[i] = steps[i].rec
	}

	return out
}

// wideExtra returns the bytes a widened block appends to the header of record j of seq seq.
func wideExtra(j int, seq uint64) []byte {
	return []byte{0xE0, byte(j), 0xA5, 3, 4, 5, byte(seq >> 8), byte(seq)}
}

// widen gives every record header of b the wideExtraLen bytes of wideExtra behind offset 56.
func widen(b *testBlock) {
	for j := range b.headers {
		b.headers[j].Extra = wideExtra(j, b.headers[j].Seq)
	}
}

// textDataRecord returns a data record over an HSMS S1F3 frame whose message text is n bytes of numbered lines,
// which zstd compresses into several blocks of the compressed type.
func textDataRecord(seq uint64, ts int64, n int) Record {
	r := bigDataRecord(seq, ts, n)

	var text []byte
	for i := 0; len(text) < n; i++ {
		text = fmt.Appendf(text, "line %d of a message text that fills several zstd blocks\n", i)
	}
	copy(r.Payload[len(blockTestFrame):], text)

	return r
}

// randomDataRecord returns a data record over an HSMS S1F3 frame whose message text is n pseudo-random bytes,
// which zstd cannot compress.
func randomDataRecord(seq uint64, ts int64, n int) Record {
	r := bigDataRecord(seq, ts, n)

	rng := rand.New(rand.NewPCG(uint64(n), seq))
	for i := len(blockTestFrame); i < len(r.Payload); i++ {
		r.Payload[i] = byte(rng.Uint32())
	}

	return r
}

// decodeTestBlock decodes the block whose envelope starts b.
func decodeTestBlock(t testing.TB, b []byte) *testBlock {
	t.Helper()

	env, err := format.UnmarshalBlockEnvelope(b)
	require.NoError(t, err)
	body := b[format.EnvelopeLen : format.EnvelopeLen+int(env.BodyLen)]
	decoded, err := codec.Decode(env.Codec, nil, body, int(env.UncompressedLen))
	require.NoError(t, err)

	n, rhl := int(env.RecordCount), int(env.RecordHeaderLen)
	tb := &testBlock{
		env: env, codec: env.Codec,
		headers: make([]format.RecordHeader, 0, n), payloads: make([][]byte, 0, n),
	}
	off := n * rhl
	for j := range n {
		h, err := format.UnmarshalRecordHeader(decoded[j*rhl:], rhl)
		require.NoError(t, err)
		tb.headers = append(tb.headers, h)
		tb.payloads = append(tb.payloads, decoded[off:off+int(h.PayloadLen)])
		off += int(h.PayloadLen)
	}

	return tb
}

// rebuildPack returns file with every block decoded, adjusted by edit, and re-encoded.
//
// The envelope of a re-encoded block takes its lengths, body_crc, codec and record_header_len from the new body,
// then patchEnv applies to the envelope written to disk.
// For a finalized pack the footer and trailer are rebuilt:
// each F-2 entry takes its location and envelope fields from the re-encoded block before patchEnv,
// then patchEntry applies;
// F-3 and F-5 content_bytes follow the F-2 uncompressed_len,
// so the footer stays valid (the tracepack format specification §10).
// Every other footer value is kept, so a changed record seq, time or epoch disagrees with its F-2 entry.
// A pack that is not finalized has its blocks walked to the end of the file.
func rebuildPack(t testing.TB, file []byte, edit func(i int, b *testBlock)) []byte {
	t.Helper()

	hdr, err := format.UnmarshalFileHeader(file)
	require.NoError(t, err)
	start := uint64(format.FileHeaderLen) + uint64(hdr.PackMetadataLen)

	end := uint64(len(file))
	tr, trErr := format.UnmarshalTrailer(file[len(file)-format.TrailerLen:])
	var parts *footerParts
	if trErr == nil {
		_, decoded, _ := splitPack(t, file)
		parts = splitFooter(t, decoded)
		end = tr.FooterOffset
	}

	out := bytes.Clone(file[:start])
	var content uint64
	for i, off := 0, start; off < end; i++ {
		b := decodeTestBlock(t, file[off:])
		off += format.EnvelopeLen + uint64(b.env.BodyLen)
		edit(i, b)

		env, enc := b.encode(t)
		at := uint64(len(out))
		disk := env
		if b.patchEnv != nil {
			b.patchEnv(&disk)
		}
		out = append(format.AppendBlockEnvelope(out, &disk), enc...)

		if parts == nil {
			continue
		}
		e := &parts.entries[i]
		e.Offset, e.OnDiskLen = at, uint32(format.EnvelopeLen+len(enc))
		e.UncompressedLen, e.RecordCount, e.BodyCRC = env.UncompressedLen, env.RecordCount, env.BodyCRC
		e.FirstSeq, e.RecordHeaderLen = env.FirstSeq, env.RecordHeaderLen
		if b.patchEntry != nil {
			b.patchEntry(e)
		}
		content += uint64(e.UncompressedLen)
	}
	if parts == nil {
		return out
	}

	parts.f5 = replaceEntry(t, parts.f5, f5TagContentBytes, 0, tlv.U64Entry(f5TagContentBytes, content))
	footer := parts.encode()
	enc, err := codec.Encode(tr.FooterCodec, nil, footer)
	require.NoError(t, err)
	tr.FooterOffset = uint64(len(out))
	tr.FooterLen, tr.FooterUncompressedLen, tr.FooterCRC = uint64(len(enc)), uint64(len(footer)), format.CRC(enc)

	return format.AppendTrailer(append(out, enc...), &tr)
}

// resealBlocks recomputes, in place, body_crc and envelope_crc of every block envelope found by following body_len
// from the end of the pack metadata, stopping at a bad magic or a body past the end of data.
// It lets a fuzzer's mutation of a block reach the checks behind the CRCs.
func resealBlocks(data []byte) {
	hdr, err := format.UnmarshalFileHeader(data)
	if err != nil {
		return
	}

	off := uint64(format.FileHeaderLen) + uint64(hdr.PackMetadataLen)
	for off+format.EnvelopeLen <= uint64(len(data)) {
		env := data[off : off+format.EnvelopeLen]
		end := off + format.EnvelopeLen + uint64(binary.LittleEndian.Uint32(env[8:12]))
		if string(env[:4]) != format.EnvelopeMagic || end > uint64(len(data)) {
			return
		}

		binary.LittleEndian.PutUint32(env[20:24], format.CRC(data[off+format.EnvelopeLen:end]))
		binary.LittleEndian.PutUint32(env[36:40], format.CRC(env[:36]))
		off = end
	}
}

// requireBlocksRead reads every block of r through one blockBuf and checks each against want.
func requireBlocksRead(t *testing.T, r *Reader, want [][]storedBlockRecord) {
	t.Helper()

	require.Len(t, r.blocks, len(want))

	var buf blockBuf
	for i := range want {
		d, def, err := r.readBlock(i, &buf)
		require.NoError(t, err)
		require.Nil(t, def, "block %d", i)
		require.NotNil(t, d)
		requireBlockConsistent(t, &r.blocks[i], d)

		require.Equal(t, len(want[i]), d.count())
		for j, w := range want[i] {
			assert.Equal(t, w.h, d.header(j), "block %d record %d", i, j)
			assert.Equal(t, w.payload, d.payload(j), "block %d record %d", i, j)
		}
	}
}

// requireBlockConsistent checks what readBlock guarantees for a block it returns:
// the envelope agrees with info, the seqs start at first_seq and strictly increase,
// every header carries the bytes behind the known record header, the length equation of I-2 holds,
// and every payload is present.
func requireBlockConsistent(t *testing.T, info *BlockInfo, d *decodedBlock) {
	t.Helper()

	require.Equal(t, info.RecordHeaderLen, d.env.RecordHeaderLen)
	require.Equal(t, info.UncompressedLen, d.env.UncompressedLen)
	require.Equal(t, int(info.RecordCount), d.count())

	total := uint64(d.count()) * uint64(info.RecordHeaderLen)
	var prev uint64
	for j := range d.count() {
		h := d.header(j)
		require.Len(t, h.Extra, int(info.RecordHeaderLen)-format.RecordHeaderLen)
		if j == 0 {
			require.Equal(t, info.FirstSeq, h.Seq)
		} else {
			require.Greater(t, h.Seq, prev)
		}
		prev = h.Seq
		total += uint64(h.PayloadLen)

		rec := blockRecord(d, j)
		require.Len(t, d.payload(j), int(h.PayloadLen))
		require.Len(t, rec.Payload, int(h.PayloadLen))
	}
	require.Equal(t, uint64(info.UncompressedLen), total, "record_count × record_header_len + Σ payload_len")
}

// requireBlockDefect reads block i of r
// and requires a defect of reason at the block's offset whose error contains msg, with no block.
func requireBlockDefect(t *testing.T, r *Reader, i int, reason IncompleteReason, msg string) {
	t.Helper()

	var buf blockBuf
	d, def, err := r.readBlock(i, &buf)
	require.NoError(t, err)
	require.Nil(t, d, "a defective block yields nothing")
	require.NotNil(t, def)
	assert.Equal(t, reason, def.Reason, "%v", def.Err)
	assert.Equal(t, i, def.Block)
	assert.Equal(t, int64(r.blocks[i].Offset), def.Offset)
	assert.Nil(t, def.Coverage)
	require.ErrorContains(t, def.Err, msg)
}

// encode returns the block's envelope, computed from its re-encoded body, and the encoded body.
func (b *testBlock) encode(t testing.TB) (format.BlockEnvelope, []byte) {
	t.Helper()

	var body []byte
	for j := range b.headers {
		body = format.AppendRecordHeader(body, &b.headers[j])
	}
	for _, p := range b.payloads {
		body = append(body, p...)
	}
	body = append(body, b.tail...)

	enc, err := codec.Encode(b.codec, nil, body)
	require.NoError(t, err)
	if b.onDisk != nil {
		enc = b.onDisk(enc)
	}

	env := b.env
	env.Codec = b.codec
	env.RecordHeaderLen = uint16(format.RecordHeaderLen + len(b.headers[0].Extra))
	env.BodyLen = uint32(len(enc))
	env.UncompressedLen = uint32(len(body))
	env.BodyCRC = format.CRC(enc)

	return env, enc
}

func TestReadBlockRoundTrip(t *testing.T) {
	t.Parallel()

	for _, c := range []Codec{CodecNone, CodecZstd} {
		t.Run(c.String()+" indexed", func(t *testing.T) {
			t.Parallel()

			steps := richFooterSteps(t)
			p := writeFooterTestPack(t, c, steps)
			r := mustOpen(t, p.file, ReaderOptions{})
			require.NoError(t, r.Header().FooterErr)

			requireBlocksRead(t, r, storedBlocks(p.blocks, stepRecords(steps)))
		})
		t.Run(c.String()+" walked", func(t *testing.T) {
			t.Parallel()

			recs := hourRecords(3, 7)
			p := writeReaderPack(t, readerPackConfig{codec: c, open: true}, recs)
			r := mustOpen(t, p.file, ReaderOptions{})
			require.Error(t, r.Header().FooterErr)

			requireBlocksRead(t, r, storedBlocks(p.blocks, recs))
		})
	}
}

func TestReadBlockRecords(t *testing.T) {
	t.Parallel()

	recs := hourRecords(2, 5)
	p := writeReaderPack(t, readerPackConfig{codec: CodecZstd}, recs)
	r := mustOpen(t, p.file, ReaderOptions{})

	var buf blockBuf
	k := 0
	for i := range p.blocks {
		d, def, err := r.readBlock(i, &buf)
		require.NoError(t, err)
		require.Nil(t, def)
		for j := range d.count() {
			assert.Equal(t, recs[k], blockRecord(d, j), "record %d", k)
			k++
		}
	}
	assert.Equal(t, len(recs), k)
}

func TestStoredRecord(t *testing.T) {
	t.Parallel()

	h := format.RecordHeader{
		Seq: 7, TSUTCNs: 8, MonoNs: 9, Epoch: 10, PayloadLen: 3, TrailingBytes: 11,
		SystemBytes: [4]byte{1, 2, 3, 4}, SessionID: 12, Quality: 13,
		Stream: 14, Function: 15, PType: 16, SType: 17,
		Kind: 18, Dir: 19, Fidelity: 20, DecodeStatus: 21, FieldValidity: 22,
		RecordFlags: uint8(RecordFlagsW | RecordFlagsMonoPresent),
		Extra:       []byte{1},
	}
	payload := []byte{0xA, 0xB, 0xC}
	want := Record{
		Seq: 7, TSUTCNs: 8, MonoNs: 9, MonoPresent: true, Epoch: 10,
		Kind: 18, Dir: 19, Fidelity: 20, DecodeStatus: 21, TrailingBytes: 11,
		SystemBytes: [4]byte{1, 2, 3, 4}, SessionID: 12,
		Stream: 14, W: true, Function: 15, PType: 16, SType: 17,
		Quality: 13, FieldValidity: 22, Payload: payload,
	}
	assert.Equal(t, want, storedRecord(&h, payload))

	h.RecordFlags = 0
	want.W, want.MonoPresent = false, false
	assert.Equal(t, want, storedRecord(&h, payload), "mono_ns is kept as stored even when mono_present is clear")
}

func TestReadBlockWideHeaders(t *testing.T) {
	t.Parallel()

	all := func(int) bool { return true }
	odd := func(i int) bool { return i%2 == 1 }
	tests := []struct {
		name  string
		codec Codec
		open  bool
		wide  func(i int) bool
	}{
		{name: "every block none", codec: CodecNone, wide: all},
		{name: "every block zstd", codec: CodecZstd, wide: all},
		{name: "mixed zstd", codec: CodecZstd, wide: odd},
		{name: "mixed none walked", codec: CodecNone, open: true, wide: odd},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()

			recs := hourRecords(4, 6)
			p := writeReaderPack(t, readerPackConfig{codec: tt.codec, open: tt.open}, recs)
			require.Len(t, p.blocks, 4)
			want := storedBlocks(p.blocks, recs)
			file := rebuildPack(t, p.file, func(i int, b *testBlock) {
				if tt.wide(i) {
					widen(b)
				}
			})
			for i := range want {
				for j := range want[i] {
					if tt.wide(i) {
						want[i][j].h.Extra = wideExtra(j, want[i][j].h.Seq)
					}
				}
			}

			r := mustOpen(t, file, ReaderOptions{})
			assert.Equal(t, !tt.open, r.Header().FooterErr == nil)
			for i, b := range r.Blocks() {
				rhl := uint16(format.RecordHeaderLen)
				if tt.wide(i) {
					rhl += wideExtraLen
				}
				assert.Equal(t, rhl, b.RecordHeaderLen, "block %d", i)
			}

			requireBlocksRead(t, r, want)
		})
	}
}

func TestReadBlockCorruption(t *testing.T) {
	t.Parallel()

	recs := hourRecords(3, 40)
	closed := writeReaderPack(t, readerPackConfig{codec: CodecZstd}, recs)
	open := writeReaderPack(t, readerPackConfig{codec: CodecZstd, open: true}, recs)
	require.Len(t, closed.blocks, 3)
	at := closed.blocks[1].offset
	last := int(closed.blocks[1].recordCount) - 1

	// rebuilt returns the closed pack with block 1 adjusted by edit.
	rebuilt := func(edit func(b *testBlock)) func(t testing.TB) []byte {
		return func(t testing.TB) []byte {
			return rebuildPack(t, closed.file, func(i int, b *testBlock) {
				if i == 1 {
					edit(b)
				}
			})
		}
	}
	// patched returns file with the envelope of block 1 re-encoded after fn changes it.
	patched := func(file []byte, fn func(*format.BlockEnvelope)) func(t testing.TB) []byte {
		return func(t testing.TB) []byte {
			return patchEnvelope(t, file, at, fn)
		}
	}

	tests := []struct {
		name   string
		file   func(t testing.TB) []byte
		walked bool
		reason IncompleteReason
		msg    string
	}{
		{name: "envelope record_header_len 64, F-2 56", file: rebuilt(func(b *testBlock) {
			widen(b)
			b.patchEntry = func(e *format.F2Entry) { e.RecordHeaderLen = format.RecordHeaderLen }
		}), msg: "record_header_len"},
		{name: "envelope record_header_len 56, F-2 64", file: rebuilt(func(b *testBlock) {
			b.patchEntry = func(e *format.F2Entry) { e.RecordHeaderLen = format.RecordHeaderLen + wideExtraLen }
		}), msg: "record_header_len"},
		{name: "envelope uncompressed_len vs F-2", file: rebuilt(func(b *testBlock) {
			b.patchEntry = func(e *format.F2Entry) { e.UncompressedLen++ }
		}), msg: "uncompressed_len"},
		{name: "envelope body_crc vs F-2", file: rebuilt(func(b *testBlock) {
			b.patchEntry = func(e *format.F2Entry) { e.BodyCRC ^= 1 }
		}), msg: "body_crc"},
		{name: "envelope body_len vs F-2 on_disk_len", file: rebuilt(func(b *testBlock) {
			b.patchEnv = func(e *format.BlockEnvelope) { e.BodyLen-- }
		}), msg: "on_disk_len"},
		{name: "record_header_len 55", file: patched(closed.file, func(e *format.BlockEnvelope) {
			e.RecordHeaderLen = format.RecordHeaderLen - 1
		}), msg: "record_header_len"},
		{name: "body_crc mismatch", file: func(testing.TB) []byte {
			return flipByte(closed.file, at+format.EnvelopeLen+uint64(closed.blocks[1].onDiskLen-format.EnvelopeLen)/2)
		}, msg: "body_crc"},
		{name: "unknown codec", file: patched(closed.file, func(e *format.BlockEnvelope) { e.Codec = 7 }),
			reason: ReasonUnknownCodec, msg: "codec 7"},
		{name: "first seq differs from first_seq", file: rebuilt(func(b *testBlock) { b.headers[0].Seq-- }), msg: "first_seq"},
		{name: "seq repeats", file: rebuilt(func(b *testBlock) { b.headers[5].Seq = b.headers[4].Seq }), msg: "does not follow"},
		{name: "seq decreases", file: rebuilt(func(b *testBlock) { b.headers[5].Seq = b.headers[3].Seq }), msg: "does not follow"},
		{name: "decoded body one byte longer", file: rebuilt(func(b *testBlock) { b.tail = []byte{0} }), msg: "cover"},
		{name: "payload_len one too large", file: rebuilt(func(b *testBlock) { b.headers[last].PayloadLen++ }), msg: "past uncompressed_len"},
		{name: "payload_len one too small", file: rebuilt(func(b *testBlock) { b.headers[0].PayloadLen-- }), msg: "cover"},
		{name: "last seq vs F-2 last_seq", file: rebuilt(func(b *testBlock) { b.headers[last].Seq++ }), msg: "last_seq"},
		{name: "min ts vs F-2 ts_min", file: rebuilt(func(b *testBlock) { b.headers[0].TSUTCNs++ }), msg: "ts_min"},
		{name: "max ts vs F-2 ts_max", file: rebuilt(func(b *testBlock) { b.headers[last].TSUTCNs++ }), msg: "ts_max"},
		{name: "min epoch vs F-2 epoch_min", file: rebuilt(func(b *testBlock) { b.headers[3].Epoch = 0 }), msg: "epoch_min"},
		{name: "max epoch vs F-2 epoch_max", file: rebuilt(func(b *testBlock) { b.headers[3].Epoch = 2 }), msg: "epoch_max"},
		{name: "record_count × record_header_len overflows", walked: true, file: patched(open.file, func(e *format.BlockEnvelope) {
			e.RecordCount, e.RecordHeaderLen = math.MaxUint32, math.MaxUint16
		}), msg: "header section"},
		{name: "header section longer than uncompressed_len", walked: true, file: patched(open.file, func(e *format.BlockEnvelope) {
			e.RecordCount = e.UncompressedLen/format.RecordHeaderLen + 1
		}), msg: "header section"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()

			r := mustOpen(t, tt.file(t), ReaderOptions{})
			require.Equal(t, tt.walked, r.Header().FooterErr != nil, "the footer must stay valid: %v", r.Header().FooterErr)
			require.Len(t, r.blocks, 3)
			require.Equal(t, !tt.walked, r.blocks[1].Indexed)

			reason := tt.reason
			if reason == 0 {
				reason = ReasonCorruptBlock
			}
			requireBlockDefect(t, r, 1, reason, tt.msg)

			var buf blockBuf
			for _, i := range []int{0, 2} {
				d, def, err := r.readBlock(i, &buf)
				require.NoError(t, err)
				require.Nil(t, def, "block %d still reads", i)
				requireBlockConsistent(t, &r.blocks[i], d)
			}
		})
	}
}

func TestReadBlockDamagedStreamAfterHeaders(t *testing.T) {
	t.Parallel()

	recs := []Record{
		testDataRecord(0, blockTestHour, 1),
		testDataRecord(1, blockTestHour+1, 1),
		textDataRecord(2, blockTestHour+2, 300<<10),
	}
	p := writeReaderPack(t, readerPackConfig{codec: CodecZstd}, recs)
	require.Len(t, p.blocks, 1)
	// The byte before the 4-byte content checksum is the last byte of the frame's last block,
	// far past the header section, which lies in the frame's first block.
	file := rebuildPack(t, p.file, func(_ int, b *testBlock) {
		b.onDisk = func(enc []byte) []byte {
			enc[len(enc)-5] ^= 0xFF
			return enc
		}
	})
	r := mustOpen(t, file, ReaderOptions{})
	require.NoError(t, r.Header().FooterErr)

	info := r.blocks[0]
	body := file[info.Offset+format.EnvelopeLen : info.Offset+uint64(info.OnDiskLen)]
	_, err := codec.Decode(codec.Zstd, nil, body, int(info.UncompressedLen))
	require.Error(t, err, "the damage must break a full decode")

	// Every read is full, so a stream damaged only after an intact header section still makes the block corrupt
	// (the tracepack format specification §6).
	requireBlockDefect(t, r, 0, ReasonCorruptBlock, "decode body")
}

// TestReadBlockBudgetAllocation is not parallel: it measures the process-wide TotalAlloc.
// The block is zstd, whose full decode would size its output by uncompressed_len before decoding.
func TestReadBlockBudgetAllocation(t *testing.T) {
	const claimed = 1 << 30

	p := writeReaderPack(t, readerPackConfig{codec: CodecZstd}, hourRecords(3, 5))
	file := rebuildPack(t, p.file, func(i int, b *testBlock) {
		if i != 1 {
			return
		}
		b.patchEnv = func(e *format.BlockEnvelope) { e.UncompressedLen = claimed }
		b.patchEntry = func(e *format.F2Entry) { e.UncompressedLen = claimed }
	})
	r := mustOpen(t, file, ReaderOptions{})
	require.NoError(t, r.Header().FooterErr, "a block may claim 1 GiB within the format limits")
	require.Equal(t, uint32(claimed), r.Blocks()[1].UncompressedLen)

	var buf blockBuf
	var before, after runtime.MemStats
	runtime.ReadMemStats(&before)
	for range 2 {
		d, def, err := r.readBlock(1, &buf)
		require.NoError(t, err)
		require.Nil(t, d)
		require.NotNil(t, def)
		assert.Equal(t, ReasonLimit, def.Reason)
		require.ErrorIs(t, def.Err, ErrReadLimit)
	}
	runtime.ReadMemStats(&after)

	delta := after.TotalAlloc - before.TotalAlloc
	t.Logf("two reads of a block claiming %d bytes allocated %d bytes", claimed, delta)
	require.Less(t, delta, uint64(1<<20), "the budget is checked before any allocation")

	requireBlockDefect(t, r, 1, ReasonLimit, "MaxBlockLen")
	for _, i := range []int{0, 2} {
		d, def, err := r.readBlock(i, &buf)
		require.NoError(t, err)
		require.Nil(t, def)
		requireBlockConsistent(t, &r.blocks[i], d)
	}
}

func TestReadBlockOverBudget(t *testing.T) {
	t.Parallel()

	// Each case's big block exceeds MaxBlockLen by the named length alone.
	tests := []struct {
		name string
		big  Record
		// limit returns the MaxBlockLen for the pack's big block.
		limit func(s *blockSummary) int64
		msg   string
	}{
		{
			// zstd stores the zeros of the message text in far fewer bytes.
			name: "uncompressed_len", big: bigDataRecord(1, blockTestHour+hourNs, 20_000),
			limit: func(*blockSummary) int64 { return 4096 }, msg: "uncompressed_len",
		},
		{
			// zstd stores incompressible bytes in a frame a few bytes longer than them.
			name: "body_len", big: randomDataRecord(1, blockTestHour+hourNs, 20_000),
			limit: func(s *blockSummary) int64 { return int64(s.uncompressedLen) }, msg: "body_len",
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()

			recs := []Record{testDataRecord(0, blockTestHour, 1), tt.big, testDataRecord(2, blockTestHour+2*hourNs, 1)}
			p := writeReaderPack(t, readerPackConfig{codec: CodecZstd, open: true}, recs)
			require.Len(t, p.blocks, 3)
			s := &p.blocks[1]
			limit := tt.limit(s)
			bodyLen, uncompressedLen := int64(s.onDiskLen-format.EnvelopeLen), int64(s.uncompressedLen)
			require.Equal(t, tt.msg == "body_len", bodyLen > limit)
			require.Equal(t, tt.msg == "uncompressed_len", uncompressedLen > limit)

			r := mustOpen(t, p.file, ReaderOptions{MaxBlockLen: limit})
			require.False(t, r.blocks[1].Indexed, "the pack is walked")
			requireBlockDefect(t, r, 1, ReasonLimit, tt.msg)

			var buf blockBuf
			for _, i := range []int{0, 2} {
				d, def, err := r.readBlock(i, &buf)
				require.NoError(t, err)
				require.Nil(t, def, "block %d after the big block still reads", i)
				require.Equal(t, uint64(i), d.header(0).Seq)
			}
		})
	}
}

func TestReadBlockReadErrors(t *testing.T) {
	t.Parallel()

	p := writeReaderPack(t, readerPackConfig{codec: CodecZstd}, hourRecords(2, 3))
	at := int64(p.blocks[1].offset)

	t.Run("ReadAt error", func(t *testing.T) {
		t.Parallel()

		g := newGatedReader(p.file)
		r, err := Open(t.Context(), g, int64(len(p.file)), ReaderOptions{})
		require.NoError(t, err)
		g.mu.Lock()
		g.fail = func(off int64, _ int) bool { return off == at }
		g.mu.Unlock()

		var buf blockBuf
		d, def, err := r.readBlock(1, &buf)
		require.ErrorIs(t, err, errInjected)
		require.Nil(t, d)
		require.Nil(t, def, "a read failure is an error, not a defect")
	})

	t.Run("walked block changed after Open", func(t *testing.T) {
		t.Parallel()

		open := writeReaderPack(t, readerPackConfig{codec: CodecZstd, open: true}, hourRecords(2, 3))
		r := mustOpen(t, open.file, ReaderOptions{})
		require.False(t, r.blocks[0].Indexed)
		r.ra = bytes.NewReader(patchEnvelope(t, open.file, open.blocks[0].offset, func(e *format.BlockEnvelope) { e.FirstSeq++ }))

		requireBlockDefect(t, r, 0, ReasonCorruptBlock, "disagrees with the forward walk")
	})

	t.Run("short read", func(t *testing.T) {
		t.Parallel()

		r := mustOpen(t, p.file, ReaderOptions{})
		r.ra = bytes.NewReader(p.file[:at+format.EnvelopeLen+1])

		var buf blockBuf
		d, def, err := r.readBlock(1, &buf)
		require.ErrorIs(t, err, io.ErrUnexpectedEOF)
		require.Nil(t, d)
		require.Nil(t, def)
	})
}

func FuzzReadBlock(f *testing.F) {
	recs := hourRecords(2, 3)
	for _, c := range []readerPackConfig{{}, {codec: CodecZstd}, {codec: CodecZstd, open: true}} {
		p := writeReaderPack(f, c, recs)
		wide := rebuildPack(f, p.file, func(i int, b *testBlock) {
			if i == 1 {
				widen(b)
			}
		})
		for _, reseal := range []bool{false, true} {
			f.Add(p.file, reseal)
			f.Add(wide, reseal)
		}
	}

	f.Fuzz(func(t *testing.T, data []byte, reseal bool) {
		if reseal {
			resealBlocks(data)
		}

		opts := ReaderOptions{MaxPackMetadataLen: 1 << 20, MaxFooterLen: 1 << 20, MaxBlockLen: 1 << 20}
		r, err := Open(t.Context(), bytes.NewReader(data), int64(len(data)), opts)
		if err != nil {
			return
		}

		var buf blockBuf
		for i := range r.blocks {
			d, def, err := r.readBlock(i, &buf)
			require.NoError(t, err, "every block lies inside the object")
			if def != nil {
				require.Nil(t, d)
				require.Equal(t, i, def.Block)
				require.Error(t, def.Err)
				require.Contains(t, []IncompleteReason{ReasonCorruptBlock, ReasonUnknownCodec, ReasonLimit}, def.Reason)
				continue
			}
			requireBlockConsistent(t, &r.blocks[i], d)
		}
	})
}

// blockRecord returns record i of d as stored, with its payload.
func blockRecord(d *decodedBlock, i int) Record {
	h := d.header(i)

	return storedRecord(&h, d.payload(i))
}
