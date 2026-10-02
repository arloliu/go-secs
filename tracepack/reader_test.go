package tracepack

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"runtime"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/arloliu/go-secs/tracepack/internal/codec"
	"github.com/arloliu/go-secs/tracepack/internal/format"
	"github.com/arloliu/go-secs/tracepack/internal/tlv"
)

// gateTimeout bounds how long a gated call waits for the other calls of its round.
const gateTimeout = 2 * time.Second

var (
	// errGateTimeout reports a gated call whose round never filled: its calls were not issued concurrently.
	errGateTimeout = errors.New("the other calls of the round never started")
	// errInjected is the failure a test reader injects into a ReadAt call.
	errInjected = errors.New("injected read failure")
)

// readCall is one ReadAt call: its offset and length.
type readCall struct {
	off int64
	n   int
}

// gatedReader is an io.ReaderAt over data that logs every call.
// The calls of each gated round are held until the round's expected number of calls have started,
// so a round whose calls are issued one after another times out and fails;
// calls after the last gated round pass through at once.
type gatedReader struct {
	data   []byte
	rounds []int
	// fail, when set, makes the calls it selects fail with errInjected.
	fail func(off int64, n int) bool

	mu sync.Mutex
	// calls[i] holds the calls of gated round i; calls[len(rounds)] holds the calls after the last gated round.
	calls [][]readCall
	gates []chan struct{}
	round int
}

var _ io.ReaderAt = (*gatedReader)(nil)

// readerPackConfig configures buildReaderPack.
type readerPackConfig struct {
	codec     Codec
	threshold int
	validate  bool
	// meta, when set, adjusts the pack metadata before the Writer is created.
	meta func(*PackMeta)
	// open leaves the pack unfinalized: the Writer flushes its last block and is never closed.
	open bool
}

// readerPack is a pack written for the reader tests, with the Writer's own summaries of its blocks.
type readerPack struct {
	file   []byte
	blocks []blockSummary
}

// packLayout locates the parts of a finalized pack.
type packLayout struct {
	size        int64
	blocksStart uint64
	tr          format.Trailer
}

// eofAtEnd is an io.ReaderAt that returns io.EOF with every read that reaches the end of its data,
// as io.ReaderAt allows.
type eofAtEnd struct {
	r *bytes.Reader
}

// newGatedReader returns a gatedReader over data whose gated round i holds rounds[i] calls.
func newGatedReader(data []byte, rounds ...int) *gatedReader {
	g := &gatedReader{
		data:   data,
		rounds: rounds,
		calls:  make([][]readCall, len(rounds)+1),
		gates:  make([]chan struct{}, len(rounds)),
	}
	for i := range g.gates {
		g.gates[i] = make(chan struct{})
	}

	return g
}

// readerTestMeta returns the pack metadata of the reader test packs.
func readerTestMeta() *PackMeta {
	return &PackMeta{
		ToolID: "tool", Recorder: "rec", Writer: "wr", Classifiers: []string{"c"},
		PackRole: PackRoleSegment, ScopeGeneration: new(uint64(0)),
	}
}

// buildReaderPack writes recs into a new pack configured by c.
func buildReaderPack(c readerPackConfig, recs []Record) (*readerPack, error) {
	meta := readerTestMeta()
	if c.meta != nil {
		c.meta(meta)
	}
	if len(recs) > 0 {
		meta.SeqStart = recs[0].Seq
	}

	var buf bytes.Buffer
	w, err := NewWriter(&buf, WriterOptions{
		Meta: meta, Facts: PackFacts{AnyClassified: true},
		Codec: c.codec, BlockThreshold: c.threshold, Validate: c.validate,
	})
	if err != nil {
		return nil, err
	}
	for i := range recs {
		if err := w.Append(&recs[i]); err != nil {
			return nil, err
		}
	}
	if c.open {
		err = w.Flush()
	} else {
		_, err = w.Close()
	}
	if err != nil {
		return nil, err
	}

	return &readerPack{file: buf.Bytes(), blocks: w.blocks}, nil
}

// writeReaderPack writes recs into a new pack configured by c.
func writeReaderPack(t testing.TB, c readerPackConfig, recs []Record) *readerPack {
	t.Helper()

	p, err := buildReaderPack(c, recs)
	require.NoError(t, err)

	return p
}

// hourRecords returns perHour data records in each of hours consecutive UTC hours, with seqs from 0,
// so a Writer with the default block threshold writes one block per hour.
func hourRecords(hours, perHour int) []Record {
	recs := make([]Record, 0, hours*perHour)
	for h := range hours {
		for i := range perHour {
			recs = append(recs, testDataRecord(uint64(len(recs)), blockTestHour+int64(h)*hourNs+int64(i), 1))
		}
	}

	return recs
}

// bigFooterPack is a validated pack of single-record blocks whose footer, written with codec none, exceeds 1 MiB.
var bigFooterPack = sync.OnceValues(func() (*readerPack, error) {
	recs := make([]Record, 4400)
	for i := range recs {
		recs[i] = testDataRecord(uint64(i), blockTestHour+int64(i), 1)
	}

	return buildReaderPack(readerPackConfig{threshold: 1, validate: true}, recs)
})

// layoutOf locates the parts of the finalized pack file.
func layoutOf(t testing.TB, file []byte) packLayout {
	t.Helper()

	hdr, err := format.UnmarshalFileHeader(file)
	require.NoError(t, err)
	tr, err := format.UnmarshalTrailer(file[len(file)-format.TrailerLen:])
	require.NoError(t, err)

	return packLayout{size: int64(len(file)), blocksStart: format.FileHeaderLen + uint64(hdr.PackMetadataLen), tr: tr}
}

// indexedInfos returns the BlockInfo a valid footer gives each of blocks.
func indexedInfos(blocks []blockSummary) []BlockInfo {
	out := make([]BlockInfo, len(blocks))
	for i := range blocks {
		s := &blocks[i]
		out[i] = BlockInfo{
			Offset: s.offset, OnDiskLen: s.onDiskLen, UncompressedLen: s.uncompressedLen, RecordCount: s.recordCount,
			BodyCRC: s.bodyCRC, RecordHeaderLen: s.recordHeaderLen, FirstSeq: s.firstSeq,
			Indexed: true, LastSeq: s.lastSeq, TSMin: s.tsMin, TSMax: s.tsMax, EpochMin: s.epochMin, EpochMax: s.epochMax,
		}
	}

	return out
}

// walkedInfos returns the BlockInfo the forward walk gives each of blocks.
func walkedInfos(blocks []blockSummary) []BlockInfo {
	out := make([]BlockInfo, len(blocks))
	for i := range blocks {
		s := &blocks[i]
		out[i] = BlockInfo{
			Offset: s.offset, OnDiskLen: s.onDiskLen, UncompressedLen: s.uncompressedLen, RecordCount: s.recordCount,
			BodyCRC: s.bodyCRC, RecordHeaderLen: s.recordHeaderLen, FirstSeq: s.firstSeq,
		}
	}

	return out
}

// openBytes opens file through a bytes.Reader.
func openBytes(t testing.TB, file []byte, opts ReaderOptions) (*Reader, error) {
	t.Helper()

	return Open(t.Context(), bytes.NewReader(file), int64(len(file)), opts)
}

// mustOpen opens file through a bytes.Reader and fails the test on error.
func mustOpen(t testing.TB, file []byte, opts ReaderOptions) *Reader {
	t.Helper()

	r, err := openBytes(t, file, opts)
	require.NoError(t, err)

	return r
}

// patchHeader returns a copy of file whose file header is re-encoded, with a fresh header_crc, after fn changes it.
func patchHeader(t testing.TB, file []byte, fn func(*format.FileHeader)) []byte {
	t.Helper()

	hdr, err := format.UnmarshalFileHeader(file)
	require.NoError(t, err)
	fn(&hdr)

	out := bytes.Clone(file)
	copy(out, format.AppendFileHeader(nil, &hdr))

	return out
}

// patchMeta returns a copy of file whose pack metadata bytes fn changes in place,
// with pack_metadata_crc and header_crc recomputed.
func patchMeta(t testing.TB, file []byte, fn func(meta []byte)) []byte {
	t.Helper()

	out := bytes.Clone(file)
	hdr, err := format.UnmarshalFileHeader(out)
	require.NoError(t, err)

	meta := out[format.FileHeaderLen : format.FileHeaderLen+int(hdr.PackMetadataLen)]
	fn(meta)
	hdr.PackMetadataCRC = format.CRC(meta)
	copy(out, format.AppendFileHeader(nil, &hdr))

	return out
}

// patchTrailer returns a copy of file whose trailer is re-encoded, with a fresh trailer_crc, after fn changes it.
func patchTrailer(t testing.TB, file []byte, fn func(*format.Trailer)) []byte {
	t.Helper()

	tr := layoutOf(t, file).tr
	fn(&tr)

	return format.AppendTrailer(bytes.Clone(file[:len(file)-format.TrailerLen]), &tr)
}

// refooter returns file with its footer replaced by decoded, encoded with the trailer's footer_codec,
// and its trailer rebuilt for the new footer;
// footer_offset is unchanged, so the file size may change.
func refooter(t testing.TB, file, decoded []byte) []byte {
	t.Helper()

	tr := layoutOf(t, file).tr
	enc, err := codec.Encode(tr.FooterCodec, nil, decoded)
	require.NoError(t, err)

	tr.FooterLen = uint64(len(enc))
	tr.FooterUncompressedLen = uint64(len(decoded))
	tr.FooterCRC = format.CRC(enc)
	out := append(bytes.Clone(file[:tr.FooterOffset]), enc...)

	return format.AppendTrailer(out, &tr)
}

// invalidFooterFile returns file with a CRC-valid footer that fails the footer validation of §10:
// its F-5 statistics lack record_count.
func invalidFooterFile(t testing.TB, file []byte) []byte {
	t.Helper()

	_, decoded, _ := splitPack(t, file)
	parts := splitFooter(t, decoded)
	parts.f5 = removeEntries(parts.f5, f5TagRecordCount)

	return refooter(t, file, parts.encode())
}

// flipByte returns a copy of file with the byte at off inverted.
func flipByte(file []byte, off uint64) []byte {
	out := bytes.Clone(file)
	out[off] ^= 0xFF

	return out
}

// tailWindows returns options that read the pack of layout l in the head and tail path,
// with windows just larger than its pack metadata and its footer.
func tailWindows(t testing.TB, l packLayout) ReaderOptions {
	t.Helper()

	opts := ReaderOptions{
		WholeReadThreshold: 1,
		HeadWindow:         int64(l.blocksStart) + 16,
		TailWindow:         l.size - int64(l.tr.FooterOffset) + 16,
	}
	require.Less(t, opts.HeadWindow+opts.TailWindow, l.size, "the windows must not cover the pack")

	return opts
}

// packOr returns p, or def when p is nil.
func packOr(p, def *readerPack) *readerPack {
	if p == nil {
		return def
	}

	return p
}

// defectOffsets returns the offsets of defects, each of which must be a ReasonTruncated defect of no block.
func defectOffsets(t testing.TB, defects []Defect) []int64 {
	t.Helper()

	out := make([]int64, 0, len(defects))
	for _, d := range defects {
		assert.Equal(t, ReasonTruncated, d.Reason)
		assert.Equal(t, -1, d.Block)
		require.Error(t, d.Err)
		out = append(out, d.Offset)
	}

	return out
}

// ReadAt implements io.ReaderAt.
func (g *gatedReader) ReadAt(p []byte, off int64) (int, error) {
	gate, fail := g.enter(off, len(p))
	if gate != nil {
		select {
		case <-gate:
		case <-time.After(gateTimeout):
			return 0, errGateTimeout
		}
	}
	if fail {
		return 0, errInjected
	}

	return bytes.NewReader(g.data).ReadAt(p, off)
}

// enter logs a call and returns the gate of its round, nil when it is not gated, and whether it fails.
func (g *gatedReader) enter(off int64, n int) (chan struct{}, bool) {
	g.mu.Lock()
	defer g.mu.Unlock()

	fail := g.fail != nil && g.fail(off, n)
	r := g.round
	g.calls[r] = append(g.calls[r], readCall{off: off, n: n})
	if r == len(g.rounds) {
		return nil, fail
	}
	if len(g.calls[r]) == g.rounds[r] {
		close(g.gates[r])
		g.round++
	}

	return g.gates[r], fail
}

// roundCalls returns the calls of gated round i, or the calls after the last gated round for i = len(rounds).
func (g *gatedReader) roundCalls(i int) []readCall {
	g.mu.Lock()
	defer g.mu.Unlock()

	return slices.Clone(g.calls[i])
}

// allCalls returns every call, in the order they were logged within each round.
func (g *gatedReader) allCalls() []readCall {
	g.mu.Lock()
	defer g.mu.Unlock()

	return slices.Concat(g.calls...)
}

// ReadAt implements io.ReaderAt.
func (e eofAtEnd) ReadAt(p []byte, off int64) (int, error) {
	n, err := e.r.ReadAt(p, off)
	if err == nil && off+int64(n) == e.r.Size() {
		err = io.EOF
	}

	return n, err
}

func TestIncompleteReasonString(t *testing.T) {
	t.Parallel()

	tests := []struct {
		r    IncompleteReason
		want string
	}{
		{ReasonCorruptBlock, "corrupt-block"},
		{ReasonUnknownCodec, "unknown-codec"},
		{ReasonTruncated, "truncated"},
		{ReasonCoverage, "coverage"},
		{ReasonLimit, "limit"},
		{ReasonIndexMismatch, "index-mismatch"},
		{0, "unknown(0)"},
		{IncompleteReason(200), "unknown(200)"},
	}
	for _, tt := range tests {
		assert.Equal(t, tt.want, tt.r.String())
	}
}

func TestResultComplete(t *testing.T) {
	t.Parallel()

	assert.True(t, Result{}.Complete())
	assert.True(t, Result{FooterErr: ErrInvalidFooter}.Complete(),
		"a footer failure is reported beside the defects, never as one")
	assert.True(t, Result{FooterErrs: []PackError{{Pack: 1, Err: ErrInvalidFooter}}}.Complete(),
		"a pack's footer failure is reported beside the defects, never as one")
	assert.True(t, Result{Conflicts: []Conflict{{Seq: 1}}}.Complete(), "a conflict is not a defect")
	assert.False(t, Result{Incomplete: []Defect{{Reason: ReasonTruncated, Block: -1, Offset: 80}}}.Complete())
}

func TestOpenValidPack(t *testing.T) {
	t.Parallel()

	for _, c := range []Codec{CodecNone, CodecZstd} {
		for _, validate := range []bool{false, true} {
			t.Run(fmt.Sprintf("%s validate %t", c, validate), func(t *testing.T) {
				t.Parallel()

				p := writeReaderPack(t, readerPackConfig{codec: c, validate: validate}, hourRecords(3, 40))
				l := layoutOf(t, p.file)
				hdr, err := format.UnmarshalFileHeader(p.file)
				require.NoError(t, err)

				r := mustOpen(t, p.file, ReaderOptions{})
				h := r.Header()
				assert.True(t, h.Finalized)
				require.NoError(t, h.FooterErr)
				assert.Equal(t, &TrailerInfo{BlockCount: 3, RecordCount: 120, LastSeq: 119, FooterOffset: l.tr.FooterOffset}, h.Trailer)
				assert.Equal(t, format.FormatMajor, h.FormatMajor)
				assert.Equal(t, hdr.FormatMinor, h.FormatMinor)
				assert.Equal(t, hdr.Flags, h.Flags)
				assert.Equal(t, hdr.WriterStartUTCNs, h.WriterStartUTCNs)
				assert.Equal(t, UUID(hdr.PackID), h.PackID)
				assert.Equal(t, UUID(hdr.CaptureID), h.CaptureID)
				require.NotNil(t, h.Meta)
				assert.Equal(t, "tool", h.Meta.ToolID)

				assert.Equal(t, indexedInfos(p.blocks), r.Blocks())
				assert.NotNil(t, r.footer)
				assert.Empty(t, r.openDefects)
			})
		}
	}
}

func TestOpenEmptyPack(t *testing.T) {
	t.Parallel()

	p := writeReaderPack(t, readerPackConfig{codec: CodecZstd}, nil)
	r := mustOpen(t, p.file, ReaderOptions{})

	h := r.Header()
	assert.True(t, h.Finalized)
	require.NoError(t, h.FooterErr)
	assert.Equal(t, uint32(0), h.Trailer.BlockCount)
	assert.Empty(t, r.Blocks())
	assert.Empty(t, r.openDefects)
}

func TestOpenRejectsSizeBelowFileHeader(t *testing.T) {
	t.Parallel()

	p := writeReaderPack(t, readerPackConfig{}, hourRecords(1, 1))
	g := newGatedReader(p.file)

	r, err := Open(t.Context(), g, format.FileHeaderLen-1, ReaderOptions{})
	require.ErrorIs(t, err, ErrNotTracepack)
	require.ErrorIs(t, err, io.ErrUnexpectedEOF)
	assert.Nil(t, r)
	assert.Empty(t, g.allCalls(), "nothing is read from an object shorter than the file header")
}

func TestOpenRejectsBeforeAnyBlockRead(t *testing.T) {
	t.Parallel()

	p := writeReaderPack(t, readerPackConfig{}, hourRecords(3, 40))
	l := layoutOf(t, p.file)
	opts := tailWindows(t, l)
	roundOne := []readCall{{off: 0, n: int(opts.HeadWindow)}, {off: l.size - opts.TailWindow, n: int(opts.TailWindow)}}
	pml := uint32(l.blocksStart - format.FileHeaderLen)

	tests := []struct {
		name    string
		file    []byte
		maxMeta int64
		want    error
	}{
		{name: "bad file magic", file: flipByte(p.file, 1), want: ErrNotTracepack},
		{name: "bad header_crc", file: flipByte(p.file, 30), want: ErrChecksum},
		{
			name: "format_major 2",
			file: patchHeader(t, p.file, func(h *format.FileHeader) { h.FormatMajor = 2 }),
			want: ErrUnsupportedFormat,
		},
		{
			name: "pack metadata entry with tag 0",
			file: patchMeta(t, p.file, func(m []byte) { m[0], m[1] = 0, 0 }),
			want: tlv.ErrZeroTag,
		},
		{
			name: "pack metadata past the object",
			file: patchHeader(t, p.file, func(h *format.FileHeader) { h.PackMetadataLen = uint32(l.size) }),
			want: io.ErrUnexpectedEOF,
		},
		{name: "pack metadata over MaxPackMetadataLen", file: p.file, maxMeta: int64(pml) - 1, want: ErrReadLimit},
		{name: "bad pack_metadata_crc", file: flipByte(p.file, format.FileHeaderLen+20), want: ErrChecksum},
		{
			name: "invalid pack metadata",
			file: patchMeta(t, p.file, func(m []byte) { m[0], m[1] = 0, 0 }),
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()

			o := opts
			o.MaxPackMetadataLen = tt.maxMeta
			g := newGatedReader(tt.file, 2)

			r, err := Open(t.Context(), g, l.size, o)
			require.Error(t, err)
			assert.Nil(t, r)
			if tt.want != nil {
				require.ErrorIs(t, err, tt.want)
			}
			assert.ElementsMatch(t, roundOne, g.allCalls(), "the pack is rejected after the first round, before any block read")
		})
	}
}

func TestOpenRounds(t *testing.T) {
	t.Parallel()

	p := writeReaderPack(t, readerPackConfig{}, hourRecords(3, 40))
	l := layoutOf(t, p.file)
	s, fo, bs := l.size, int64(l.tr.FooterOffset), int64(l.blocksStart)
	tail := tailWindows(t, l)
	h, w := tail.HeadWindow, tail.TailWindow
	require.Greater(t, bs, int64(100), "the pack metadata must exceed a 100-byte head window")
	require.Greater(t, s-fo, int64(100), "the footer must exceed a 100-byte tail window")

	notes := writeReaderPack(t, readerPackConfig{meta: func(m *PackMeta) { m.Notes = new(strings.Repeat("n", 6000)) }}, hourRecords(3, 40))
	nl := layoutOf(t, notes.file)
	nt := tailWindows(t, nl)
	nt.HeadWindow = 4096

	with := func(o ReaderOptions, fn func(*ReaderOptions)) ReaderOptions {
		fn(&o)
		return o
	}

	tests := []struct {
		name   string
		pack   *readerPack
		opts   ReaderOptions
		rounds [][]readCall
	}{
		{name: "whole read under the threshold", opts: ReaderOptions{}, rounds: [][]readCall{{{0, int(s)}}}},
		{
			name:   "whole read when the windows cover the object",
			opts:   ReaderOptions{WholeReadThreshold: 1, HeadWindow: s / 2, TailWindow: s - s/2},
			rounds: [][]readCall{{{0, int(s)}}},
		},
		{name: "head and tail", opts: tail, rounds: [][]readCall{{{0, int(h)}, {s - w, int(w)}}}},
		{
			name:   "catalog hint",
			opts:   with(tail, func(o *ReaderOptions) { o.FooterOffset = uint64(fo) }),
			rounds: [][]readCall{{{0, int(h)}, {fo, int(s - fo)}}},
		},
		{
			name:   "hint inside the footer costs a second round",
			opts:   with(tail, func(o *ReaderOptions) { o.FooterOffset = uint64(fo + 10) }),
			rounds: [][]readCall{{{0, int(h)}, {fo + 10, int(s - fo - 10)}}, {{fo, 10}}},
		},
		{
			name:   "hint before the footer",
			opts:   with(tail, func(o *ReaderOptions) { o.FooterOffset = uint64(bs) }),
			rounds: [][]readCall{{{0, int(h)}, {bs, int(s - bs)}}},
		},
		{
			name:   "hint inside the file header is ignored",
			opts:   with(tail, func(o *ReaderOptions) { o.FooterOffset = format.FileHeaderLen - 1 }),
			rounds: [][]readCall{{{0, int(h)}, {s - w, int(w)}}},
		},
		{
			name:   "hint inside the trailer is ignored",
			opts:   with(tail, func(o *ReaderOptions) { o.FooterOffset = uint64(s - format.TrailerLen) }),
			rounds: [][]readCall{{{0, int(h)}, {s - w, int(w)}}},
		},
		{
			name: "hint beyond MaxFooterLen is ignored",
			opts: with(tail, func(o *ReaderOptions) {
				o.FooterOffset = uint64(bs)
				o.MaxFooterLen = s - bs - format.TrailerLen - 1
			}),
			rounds: [][]readCall{{{0, int(h)}, {s - w, int(w)}}},
		},
		{
			name:   "pack metadata beyond the head window",
			opts:   with(tail, func(o *ReaderOptions) { o.HeadWindow = 100 }),
			rounds: [][]readCall{{{0, 100}, {s - w, int(w)}}, {{100, int(bs - 100)}}},
		},
		{
			name:   "footer beyond the tail window",
			opts:   with(tail, func(o *ReaderOptions) { o.TailWindow = 100 }),
			rounds: [][]readCall{{{0, int(h)}, {s - 100, 100}}, {{fo, int(s - 100 - fo)}}},
		},
		{
			name: "pack metadata and footer beyond their windows share the second round",
			opts: with(tail, func(o *ReaderOptions) {
				o.HeadWindow = 100
				o.TailWindow = 100
			}),
			rounds: [][]readCall{{{0, 100}, {s - 100, 100}}, {{100, int(bs - 100)}, {fo, int(s - 100 - fo)}}},
		},
		{
			name: "long notes beyond the head window",
			pack: notes,
			opts: nt,
			rounds: [][]readCall{
				{{0, 4096}, {nl.size - nt.TailWindow, int(nt.TailWindow)}},
				{{4096, int(nl.blocksStart) - 4096}},
			},
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()

			pack := packOr(tt.pack, p)
			counts := make([]int, len(tt.rounds))
			for i := range tt.rounds {
				counts[i] = len(tt.rounds[i])
			}
			g := newGatedReader(pack.file, counts...)

			r, err := Open(t.Context(), g, int64(len(pack.file)), tt.opts)
			require.NoError(t, err)
			for i := range tt.rounds {
				assert.ElementsMatch(t, tt.rounds[i], g.roundCalls(i), "round %d", i+1)
			}
			assert.Empty(t, g.roundCalls(len(tt.rounds)), "no read after the bootstrap rounds")

			hdr := r.Header()
			assert.True(t, hdr.Finalized)
			require.NoError(t, hdr.FooterErr)
			assert.Equal(t, indexedInfos(pack.blocks), r.Blocks())
		})
	}
}

func TestOpenReadErrors(t *testing.T) {
	t.Parallel()

	p := writeReaderPack(t, readerPackConfig{}, hourRecords(3, 40))

	t.Run("failing ReadAt", func(t *testing.T) {
		t.Parallel()

		g := newGatedReader(p.file)
		g.fail = func(int64, int) bool { return true }
		_, err := Open(t.Context(), g, int64(len(p.file)), ReaderOptions{})
		require.ErrorIs(t, err, errInjected)
	})

	t.Run("object shorter than its size", func(t *testing.T) {
		t.Parallel()

		short := p.file[:len(p.file)-10]
		_, err := Open(t.Context(), bytes.NewReader(short), int64(len(p.file)), ReaderOptions{})
		require.ErrorIs(t, err, io.ErrUnexpectedEOF)
	})

	t.Run("io.EOF with a full read", func(t *testing.T) {
		t.Parallel()

		r, err := Open(t.Context(), eofAtEnd{bytes.NewReader(p.file)}, int64(len(p.file)), ReaderOptions{})
		require.NoError(t, err)
		assert.True(t, r.Header().Finalized)
	})
}

func TestOpenHonorsContext(t *testing.T) {
	t.Parallel()

	p := writeReaderPack(t, readerPackConfig{open: true}, hourRecords(3, 40))

	t.Run("canceled before the first round", func(t *testing.T) {
		t.Parallel()

		ctx, cancel := context.WithCancel(t.Context())
		cancel()
		g := newGatedReader(p.file)
		_, err := Open(ctx, g, int64(len(p.file)), ReaderOptions{})
		require.ErrorIs(t, err, context.Canceled)
		assert.Empty(t, g.allCalls())
	})

	t.Run("canceled during the walk", func(t *testing.T) {
		t.Parallel()

		ctx, cancel := context.WithCancel(t.Context())
		defer cancel()
		g := newGatedReader(p.file)
		g.fail = func(_ int64, n int) bool {
			if n == format.EnvelopeLen {
				cancel()
			}

			return false
		}
		_, err := Open(ctx, g, int64(len(p.file)), ReaderOptions{})
		require.ErrorIs(t, err, context.Canceled)
		assert.Len(t, g.allCalls(), 2, "the whole read, then one envelope before the cancellation is seen")
	})
}

func TestOpenBrokenTrailer(t *testing.T) {
	t.Parallel()

	p := writeReaderPack(t, readerPackConfig{codec: CodecZstd}, hourRecords(3, 40))
	l := layoutOf(t, p.file)
	trailerAt := uint64(l.size - format.TrailerLen)

	tests := []struct {
		name string
		file []byte
	}{
		{name: "bad magic", file: flipByte(p.file, trailerAt+60)},
		{name: "bad trailer_crc", file: flipByte(p.file, trailerAt+30)},
		{name: "trailer_version 2", file: patchTrailer(t, p.file, func(tr *format.Trailer) { tr.TrailerVersion = 2 })},
		{
			name: "footer does not end at the trailer",
			file: patchTrailer(t, p.file, func(tr *format.Trailer) { tr.FooterLen-- }),
		},
		{
			name: "footer inside the pack metadata",
			file: patchTrailer(t, p.file, func(tr *format.Trailer) {
				tr.FooterOffset = l.blocksStart - 1
				tr.FooterLen = trailerAt - tr.FooterOffset
			}),
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()

			r := mustOpen(t, tt.file, ReaderOptions{})
			h := r.Header()
			assert.False(t, h.Finalized)
			assert.Nil(t, h.Trailer)
			require.Error(t, h.FooterErr, "the footer was not used")
			assert.Equal(t, walkedInfos(p.blocks), r.Blocks())
			assert.Equal(t, []int64{int64(l.tr.FooterOffset)}, defectOffsets(t, r.openDefects),
				"the walk to the end of the object stops at the footer")
		})
	}
}

func TestOpenFooterCRCMismatch(t *testing.T) {
	t.Parallel()

	p := writeReaderPack(t, readerPackConfig{validate: true}, hourRecords(3, 40))
	l := layoutOf(t, p.file)

	r := mustOpen(t, flipByte(p.file, l.tr.FooterOffset+5), ReaderOptions{})
	h := r.Header()
	assert.False(t, h.Finalized)
	require.NotNil(t, h.Trailer, "the trailer itself is valid")
	require.ErrorIs(t, h.FooterErr, ErrChecksum)
	assert.Equal(t, walkedInfos(p.blocks), r.Blocks())
	assert.Equal(t, []int64{int64(l.tr.FooterOffset)}, defectOffsets(t, r.openDefects), "a pack that is not finalized is truncated")
}

func TestOpenInvalidFooterFallsBackToWalk(t *testing.T) {
	t.Parallel()

	p := writeReaderPack(t, readerPackConfig{codec: CodecZstd, validate: true}, hourRecords(3, 40))

	tests := []struct {
		name string
		file []byte
	}{
		{name: "footer fails validation", file: invalidFooterFile(t, p.file)},
		{name: "unknown footer_codec", file: patchTrailer(t, p.file, func(tr *format.Trailer) { tr.FooterCodec = 9 })},
		{
			name: "footer decodes short of footer_uncompressed_len",
			file: patchTrailer(t, p.file, func(tr *format.Trailer) { tr.FooterUncompressedLen++ }),
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()

			r := mustOpen(t, tt.file, ReaderOptions{})
			h := r.Header()
			assert.True(t, h.Finalized, "finalization depends on the footer CRC, not on the footer's content")
			require.ErrorIs(t, h.FooterErr, ErrInvalidFooter)
			assert.Nil(t, r.footer)
			assert.Equal(t, walkedInfos(p.blocks), r.Blocks())
			assert.Empty(t, r.openDefects, "the walk accounts for the trailer's blocks and records")
		})
	}
}

func TestOpenWalkDisagreesWithTrailer(t *testing.T) {
	t.Parallel()

	p := writeReaderPack(t, readerPackConfig{}, hourRecords(3, 40))
	invalid := invalidFooterFile(t, p.file)

	tests := []struct {
		name  string
		patch func(*format.Trailer)
	}{
		{name: "block_count", patch: func(tr *format.Trailer) { tr.BlockCount++ }},
		{name: "record_count", patch: func(tr *format.Trailer) { tr.RecordCount-- }},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()

			r := mustOpen(t, patchTrailer(t, invalid, tt.patch), ReaderOptions{})
			h := r.Header()
			assert.True(t, h.Finalized)
			require.ErrorIs(t, h.FooterErr, ErrInvalidFooter)
			assert.Equal(t, walkedInfos(p.blocks), r.Blocks())
			assert.Equal(t, []int64{-1}, defectOffsets(t, r.openDefects))
		})
	}
}

func TestOpenDecodedFooterOverBudget(t *testing.T) {
	t.Parallel()

	p := writeReaderPack(t, readerPackConfig{codec: CodecZstd}, hourRecords(3, 40))
	l := layoutOf(t, p.file)
	require.Greater(t, l.tr.FooterUncompressedLen, l.tr.FooterLen)

	r := mustOpen(t, p.file, ReaderOptions{MaxFooterLen: int64(l.tr.FooterLen)})
	h := r.Header()
	assert.True(t, h.Finalized)
	require.ErrorIs(t, h.FooterErr, ErrReadLimit)
	assert.Equal(t, walkedInfos(p.blocks), r.Blocks())
	assert.Empty(t, r.openDefects)
}

// TestOpenSharedF3ListsOverBudget opens a pack whose 32 blocks share one kind_counts array of 64 KiB (sharedCountsF3Pack)
// under a MaxFooterLen that holds the footer but not the blocks' lists, each counted once per block:
// the footer is not used, as for a footer over the budget, and the blocks are walked.
// Iterate reads every record and reports the budget, and Verify and Repair fail with it before reading a block,
// as for any footer over the budget; none reports the footer invalid.
// Under the default budget the same footer is valid and used.
func TestOpenSharedF3ListsOverBudget(t *testing.T) {
	t.Parallel()

	const blocks = 32
	file, lists := sharedCountsF3Pack(t, mergePack(t, seg0, nil, hourSteps(1, blocks, 1)), 64<<10)
	require.NoError(t, mustOpen(t, file, ReaderOptions{}).Header().FooterErr)
	requireConsistent(t, file)

	opts := ReaderOptions{MaxFooterLen: int64(lists / 2)}
	require.Less(t, layoutOf(t, file).tr.FooterUncompressedLen, uint64(opts.MaxFooterLen), "the budget holds the footer")
	r := mustOpen(t, file, opts)
	h := r.Header()
	assert.True(t, h.Finalized)
	require.ErrorIs(t, h.FooterErr, ErrReadLimit)
	assert.NotErrorIs(t, h.FooterErr, ErrInvalidFooter, "a budget, not a defect")
	require.ErrorContains(t, h.FooterErr, "the F-3 lists of blocks 0 to ")
	require.Len(t, r.Blocks(), blocks)
	for i, b := range r.Blocks() {
		assert.False(t, b.Indexed, "block %d", i)
	}
	assert.Empty(t, r.openDefects)

	run := iterate(t, r, Query{})
	assert.Len(t, run.items, blocks)
	assert.Equal(t, h.FooterErr, run.res.FooterErr)
	assert.Empty(t, run.res.Incomplete)

	_, err := verifyBytes(t, file, VerifyOptions{Reader: opts})
	require.ErrorIs(t, err, ErrReadLimit)
	require.ErrorContains(t, err, "the F-3 lists of blocks 0 to ")
	ro := repairOpts()
	ro.Reader = opts
	patch, rep, err := repairBytes(t, file, ro)
	require.ErrorIs(t, err, ErrReadLimit)
	assert.Empty(t, patch)
	assert.Equal(t, RepairReport{}, rep)
}

// TestOpenSharedF3ListsAtBudget opens a pack whose 4 blocks share a kind_counts array (sharedCountsF3Pack)
// under a MaxFooterLen equal to the total of its blocks' lists, each counted once per block, and one byte below:
// the footer is used at the budget, and below it the blocks are walked, the last block's list being the one over it.
// A pack the Writer wrote, whose lists lie end to end and total less than its footer,
// is indexed under the smallest budget that holds its footer.
func TestOpenSharedF3ListsAtBudget(t *testing.T) {
	t.Parallel()

	plain := mergePack(t, seg0, nil, hourSteps(1, 4, 1))
	pl := layoutOf(t, plain).tr
	r := mustOpen(t, plain, ReaderOptions{MaxFooterLen: int64(max(pl.FooterLen, pl.FooterUncompressedLen))})
	require.NoError(t, r.Header().FooterErr, "the Writer's pack")

	file, lists := sharedCountsF3Pack(t, plain, 4<<10)
	l := layoutOf(t, file).tr
	require.Less(t, max(l.FooterLen, l.FooterUncompressedLen), lists-1, "the footer fits under both budgets")

	r = mustOpen(t, file, ReaderOptions{MaxFooterLen: int64(lists)})
	require.NoError(t, r.Header().FooterErr)
	assert.True(t, r.Blocks()[0].Indexed)

	r = mustOpen(t, file, ReaderOptions{MaxFooterLen: int64(lists) - 1})
	require.ErrorIs(t, r.Header().FooterErr, ErrReadLimit)
	require.ErrorContains(t, r.Header().FooterErr, fmt.Sprintf("the F-3 lists of blocks 0 to 3 total %d bytes", lists))
	assert.False(t, r.Blocks()[0].Indexed)
}

// TestOpenSharedF3ListsAllocation opens a pack whose 128 blocks share one kind_counts array of 256 KiB
// under a MaxFooterLen of 1 MiB, which holds the footer:
// Open allocates about the budget, not the 32 MiB that parsing every block's list would take.
// It is not parallel: it measures the process-wide TotalAlloc.
func TestOpenSharedF3ListsAllocation(t *testing.T) {
	const (
		blocks = 128
		pad    = 256 << 10
		budget = 1 << 20
	)

	file, lists := sharedCountsF3Pack(t, mergePack(t, seg0, nil, hourSteps(1, blocks, 1)), pad)
	require.Greater(t, lists, uint64(blocks*pad))
	require.Less(t, layoutOf(t, file).tr.FooterUncompressedLen, uint64(budget/2), "the budget holds the footer")

	var before, after runtime.MemStats
	runtime.ReadMemStats(&before)
	r, err := Open(t.Context(), bytes.NewReader(file), int64(len(file)), ReaderOptions{MaxFooterLen: budget})
	runtime.ReadMemStats(&after)

	require.NoError(t, err)
	require.ErrorIs(t, r.Header().FooterErr, ErrReadLimit)
	delta := after.TotalAlloc - before.TotalAlloc
	t.Logf("opening %d lists that total %d bytes allocated %d bytes", blocks, lists, delta)
	assert.Less(t, delta, uint64(4*budget), "no list is parsed past the budget")
}

func TestOpenFooterOverBudget(t *testing.T) {
	t.Parallel()

	p, err := bigFooterPack()
	require.NoError(t, err)
	l := layoutOf(t, p.file)
	fo := int64(l.tr.FooterOffset)
	const chunk = 1 << 20
	require.Greater(t, l.tr.FooterLen, uint64(chunk), "the footer must span more than one CRC chunk")
	require.LessOrEqual(t, l.tr.FooterLen, uint64(2*chunk))
	opts := ReaderOptions{MaxFooterLen: 64 << 10}
	chunks := []readCall{{fo, chunk}, {fo + chunk, int(l.tr.FooterLen) - chunk}}

	t.Run("CRC valid", func(t *testing.T) {
		t.Parallel()

		g := newGatedReader(p.file, 2)
		r, err := Open(t.Context(), g, l.size, opts)
		require.NoError(t, err)

		h := r.Header()
		assert.True(t, h.Finalized, "the streamed footer CRC settles finalization")
		require.ErrorIs(t, h.FooterErr, ErrReadLimit)
		assert.Equal(t, walkedInfos(p.blocks), r.Blocks())
		assert.Empty(t, r.openDefects)

		after := g.roundCalls(1)
		require.Len(t, after, len(chunks)+len(p.blocks))
		assert.Equal(t, chunks, after[:len(chunks)], "the footer CRC is read in sequential chunks of at most 1 MiB")
		for _, c := range after[len(chunks):] {
			assert.Equal(t, format.EnvelopeLen, c.n)
		}
	})

	t.Run("failing chunk read", func(t *testing.T) {
		t.Parallel()

		g := newGatedReader(p.file, 2)
		g.fail = func(off int64, _ int) bool { return off == fo+chunk }
		_, err := Open(t.Context(), g, l.size, opts)
		require.ErrorIs(t, err, errInjected)
	})

	t.Run("CRC broken", func(t *testing.T) {
		t.Parallel()

		r := mustOpen(t, flipByte(p.file, l.tr.FooterOffset+chunk+7), opts)
		h := r.Header()
		assert.False(t, h.Finalized)
		require.ErrorIs(t, h.FooterErr, ErrChecksum)
		assert.NotErrorIs(t, h.FooterErr, ErrReadLimit, "a corrupt footer is reported as corrupt, not as over the budget")
		assert.Equal(t, walkedInfos(p.blocks), r.Blocks())
		assert.Equal(t, []int64{fo}, defectOffsets(t, r.openDefects))
	})

	t.Run("pack metadata rejected before the footer is read", func(t *testing.T) {
		t.Parallel()

		g := newGatedReader(patchMeta(t, p.file, func(m []byte) { m[0], m[1] = 0, 0 }), 2)
		_, err := Open(t.Context(), g, l.size, opts)
		require.ErrorIs(t, err, tlv.ErrZeroTag)
		assert.Empty(t, g.roundCalls(1), "no footer chunk is read for a pack whose metadata is rejected")
	})
}

func TestHeaderReturnsACopy(t *testing.T) {
	t.Parallel()

	start, end := blockTestHour, blockTestHour+hourNs
	p := writeReaderPack(t, readerPackConfig{validate: true, meta: func(m *PackMeta) {
		m.Coverage = []Coverage{{TimeStart: &start, TimeEnd: &end}}
	}}, hourRecords(2, 10))
	r := mustOpen(t, p.file, ReaderOptions{})
	want := r.Header()

	h := r.Header()
	assert.NotSame(t, h.Meta, r.meta, "the Reader never hands out its own metadata")
	*h.Meta.Coverage[0].TimeStart = 0
	h.Meta.Coverage = append(h.Meta.Coverage, Coverage{})
	h.Meta.ToolID = "changed"
	h.Trailer.BlockCount = 99
	h.Finalized = false

	assert.Equal(t, want, r.Header())
	assert.True(t, r.finalized)
	assert.Equal(t, "tool", r.meta.ToolID)
	assert.Equal(t, []Coverage{{TimeStart: &start, TimeEnd: &end}}, r.meta.Coverage)
	assert.Equal(t, uint32(2), r.trailer.BlockCount)
}

func TestBlocksReturnsACopy(t *testing.T) {
	t.Parallel()

	p := writeReaderPack(t, readerPackConfig{}, hourRecords(2, 10))
	r := mustOpen(t, p.file, ReaderOptions{})

	b := r.Blocks()
	b[0].Offset = 1
	assert.Equal(t, indexedInfos(p.blocks), r.Blocks())
}

func FuzzOpen(f *testing.F) {
	seeds := []readerPackConfig{
		{},
		{codec: CodecZstd, validate: true},
		{codec: CodecZstd, open: true},
	}
	for _, c := range seeds {
		p := writeReaderPack(f, c, hourRecords(2, 3))
		f.Add(p.file, uint16(0), uint16(0), uint64(0))
		f.Add(p.file, uint16(96), uint16(128), uint64(len(p.file)/2))
	}
	empty := writeReaderPack(f, readerPackConfig{codec: CodecZstd}, nil)
	f.Add(empty.file, uint16(0), uint16(0), uint64(0))

	f.Fuzz(func(t *testing.T, data []byte, head, tail uint16, hint uint64) {
		opts := ReaderOptions{
			WholeReadThreshold: 1,
			HeadWindow:         int64(head),
			TailWindow:         int64(tail),
			FooterOffset:       hint,
			MaxPackMetadataLen: 1 << 20,
			MaxFooterLen:       1 << 20,
			MaxBlockLen:        1 << 20,
		}
		r, err := Open(t.Context(), bytes.NewReader(data), int64(len(data)), opts)
		if err != nil {
			require.Nil(t, r)
			return
		}

		h := r.Header()
		require.NotNil(t, h.Meta)
		require.Equal(t, h.FooterErr == nil, r.footer != nil, "the footer is used iff FooterErr is nil")
		require.Equal(t, h.Trailer != nil, r.trailer != nil)
		if !h.Finalized {
			require.NotEmpty(t, r.openDefects, "a pack that is not finalized is truncated")
		}
		for _, b := range r.Blocks() {
			require.Equal(t, r.footer != nil, b.Indexed)
			require.LessOrEqual(t, b.Offset+uint64(b.OnDiskLen), uint64(len(data)))
		}
	})
}
