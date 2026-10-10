package tracepack

import (
	"bytes"
	"slices"
	"testing"

	"github.com/stretchr/testify/require"

	"github.com/arloliu/go-secs/tracepack/internal/format"
	"github.com/arloliu/go-secs/tracepack/internal/tlv"
)

// viewCapture is the capture of the merge test scopes, whose UTC hour is the one of blockTestHour.
var viewCapture = UUID{0xCA, 0x97}

// Pack and replacement-set ids of the test packs:
// segments seg*, the archives mem* of the replacement sets set*, and patP1, a pack of role repair.
var (
	seg0, seg1, seg2, seg3 = UUID{0x50}, UUID{0x51}, UUID{0x52}, UUID{0x53}
	setA, setB, setC, setD = UUID{0x5E, 0xA}, UUID{0x5E, 0xB}, UUID{0x5E, 0xC}, UUID{0x5E, 0xD}
	memA, memB, memC, memD = UUID{0xA0, 0xA}, UUID{0xA0, 0xB}, UUID{0xA0, 0xC}, UUID{0xA0, 0xD}
	patP1                  = UUID{0xB0, 1}
)

// generationMeta makes m the pack metadata of an archive, the member of replacement set set at (epoch, gen), whose compacted_from is from.
func generationMeta(m *PackMeta, set UUID, epoch, gen uint64, from []UUID) {
	m.PackRole = PackRoleArchive
	m.ScopeGeneration, m.PublisherEpoch = new(gen), new(epoch)
	m.ReplacementSetID, m.ReplacementSetSize, m.ReplacementSetIndex = new(set), new(uint64(1)), new(uint64(0))
	m.CompactionLevel, m.CompactedFrom = 1, from
}

// overwriteMetaValue overwrites, in place, the value of the first entry of tag in raw, pack metadata bytes.
func overwriteMetaValue(t testing.TB, raw []byte, tag uint16, value []byte) {
	t.Helper()

	list, err := tlv.Decode(raw)
	require.NoError(t, err)
	e := nthEntry(t, list, tag, 0)
	require.Len(t, value, len(e.Value))
	// e.Value aliases raw.
	copy(e.Value, value)
}

// permutations returns every order of ids.
func permutations(ids []UUID) [][]UUID {
	if len(ids) <= 1 {
		return [][]UUID{slices.Clone(ids)}
	}

	out := make([][]UUID, 0, len(ids)*len(ids))
	for i := range ids {
		for _, rest := range permutations(slices.Concat(ids[:i], ids[i+1:])) {
			out = append(out, append([]UUID{ids[i]}, rest...))
		}
	}

	return out
}

// repairSteps returns the records of the repair test pack, with caller seqs from 10, all in blockTestHour,
// written by writeRepairPack as five blocks:
//
//	block 0: 10 start boundary e0, 11 data e1, 13 data e1 (seq gap)
//	block 1: 14 data e1, 15 socket-close e1, 20 gap boundary e0 with gap_start and gap_end (seq gap)
//	block 2: 21 data e2, 22 data e2
//	block 3: 23 data e2, 30 data e3 (seq gap)
//	block 4: 31 stop boundary e3
func repairSteps(t testing.TB) []footerTestStep {
	t.Helper()

	h := blockTestHour
	boundary := func(seq uint64, ts int64, epoch uint32, ev *TransportEvent) Record {
		return testEventRecord(t, seq, ts, epoch, ev)
	}

	return []footerTestStep{
		{rec: boundary(10, h+50, 0, &TransportEvent{Event: EventCaptureBoundary, BoundaryKind: new(BoundaryKindStart)})},
		{rec: testDataRecord(11, h+10, 1)},
		{rec: testDataRecord(13, h+20, 1), flush: true},
		{rec: testDataRecord(14, h+30, 1)},
		{rec: boundary(15, h+35, 1, &TransportEvent{Event: EventSocketClose})},
		{rec: boundary(20, h+300, 0, &TransportEvent{
			Event: EventCaptureBoundary, BoundaryKind: new(BoundaryKindGap), GapStart: new(h + 100), GapEnd: new(h + 200),
		}), flush: true},
		{rec: testDataRecord(21, h+400, 2)},
		{rec: testDataRecord(22, h+410, 2), flush: true},
		{rec: testDataRecord(23, h+420, 2)},
		{rec: testDataRecord(30, h+500, 3), flush: true},
		{rec: boundary(31, h+600, 3, &TransportEvent{Event: EventCaptureBoundary, BoundaryKind: new(BoundaryKindStop)})},
	}
}

// repairTestMeta returns the pack metadata of the repair test pack: a segment whose period is blockTestHour.
func repairTestMeta() *PackMeta {
	return &PackMeta{
		ToolID: "tool", Recorder: "rec", Writer: "wr", Classifiers: []string{"c"},
		PackRole: PackRoleSegment, ScopeGeneration: new(uint64(0)),
		PeriodStart: blockTestHour, PeriodEnd: blockTestHour + hourNs,
	}
}

// repairPack is a pack written for the tests, with the Writer's own summaries of its blocks.
type repairPack struct {
	file   []byte
	blocks []blockSummary
}

// writeRepairPack writes steps into a new pack whose metadata is repairTestMeta changed by meta, when set;
// with open it flushes the last block and never closes the Writer, so the pack is not finalized.
func writeRepairPack(t testing.TB, c Codec, meta func(*PackMeta), open bool, steps []footerTestStep) *repairPack {
	t.Helper()

	m := repairTestMeta()
	if meta != nil {
		meta(m)
	}
	if len(steps) > 0 {
		m.SeqStart = steps[0].rec.Seq
	}

	var buf bytes.Buffer
	// Merge and lookup tests write packs whose period or records lie outside one UTC hour,
	// so the Writer's scope checks are off.
	w, err := NewWriter(&buf, WriterOptions{Meta: m, Codec: c, BlockThreshold: 1 << 16, allowScopeBreach: true})
	require.NoError(t, err)
	for i := range steps {
		require.NoError(t, w.Append(&steps[i].rec), "step %d", i)
		if steps[i].flush {
			require.NoError(t, w.Flush())
		}
	}
	if open {
		require.NoError(t, w.Flush())
	} else {
		_, err = w.Close()
		require.NoError(t, err)
	}

	return &repairPack{file: buf.Bytes(), blocks: slices.Clone(w.blocks)}
}

// defaultRepairPack writes the repair test pack of repairSteps, finalized, with CodecZstd.
func defaultRepairPack(t testing.TB) *repairPack {
	t.Helper()

	p := writeRepairPack(t, CodecZstd, nil, false, repairSteps(t))
	require.Len(t, p.blocks, 5)

	return p
}

// bodyByteOf returns the offset of the second body byte of block i of p.
func (p *repairPack) bodyByteOf(i int) uint64 {
	return p.blocks[i].offset + format.EnvelopeLen + 1
}

// setMetaRole returns a copy of file with the pack metadata's pack_role value byte set to role.
func setMetaRole(t testing.TB, file []byte, role uint8) []byte {
	t.Helper()

	return patchMeta(t, file, func(meta []byte) {
		entries, err := tlv.Decode(meta)
		require.NoError(t, err)
		for _, e := range entries {
			if e.Tag == tagPackRole {
				meta[e.Offset+tlv.HeaderLen] = role

				return
			}
		}
		t.Fatal("no pack_role entry")
	})
}

// archiveMeta makes the repair test pack a generation-2 archive of a capture-clock recorder,
// with compaction lineage, a durability interval, inherited coverage holding an unknown nested tag,
// and four private unknown entries.
func archiveMeta(m *PackMeta) {
	m.PackRole = PackRoleArchive
	m.ScopeGeneration, m.PublisherEpoch = new(uint64(2)), new(uint64(3))
	m.ReplacementSetID, m.ReplacementSetSize, m.ReplacementSetIndex = new(UUID{0xAA}), new(uint64(1)), new(uint64(0))
	m.Supersedes = []UUID{{0xAB}}
	m.CompactionLevel, m.CompactedFrom = 1, []UUID{{0xC1}, {0xC2}}
	m.TimeSource = TimeSourceCaptureClock
	m.CaptureOriginUTCNs, m.CaptureOriginMonoNs, m.ClockStepToleranceNs = new(blockTestHour), new(int64(5)), new(uint64(1000))
	m.FlushIntervalNs = new(uint64(60_000_000_000))
	m.Coverage = []Coverage{{SeqFirst: new(uint64(1)), SeqLast: new(uint64(2)), Unknown: []RawEntry{{Tag: 0x0050, Type: 7, Value: []byte{9}}}}}
	m.Unknown = []RawEntry{
		{Tag: 0x8001, Type: 7, Value: []byte{1}},
		{Tag: 0x8002, Type: 7, Value: []byte{2}},
		{Tag: 0x8003, Type: 7, Value: []byte{3}},
		{Tag: 0x8004, Type: 7, Value: []byte("kept")},
	}
}

// blockBytes returns the on-disk bytes of every located block of file that keep selects, in file order.
func blockBytes(t testing.TB, file []byte, keep func(i int) bool) [][]byte {
	t.Helper()

	r := mustOpen(t, file, ReaderOptions{})
	var out [][]byte
	for i, b := range r.Blocks() {
		if keep(i) {
			out = append(out, file[b.Offset:b.Offset+uint64(b.OnDiskLen)])
		}
	}

	return out
}

// blockReadAt serves file, and on each read of exactly the n bytes at off calls hook with the read's number, from 1,
// and the bytes served, which hook may change; an error of hook fails the read.
type blockReadAt struct {
	file  []byte
	off   int64
	n     int
	reads int
	hook  func(read int, p []byte) error
}

func (b *blockReadAt) ReadAt(p []byte, off int64) (int, error) {
	n, err := bytes.NewReader(b.file).ReadAt(p, off)
	if err == nil && off == b.off && len(p) == b.n {
		b.reads++
		if err := b.hook(b.reads, p); err != nil {
			return 0, err
		}
	}

	return n, err
}

// shortAfterWriter writes into buf until limit bytes are written, then takes one byte fewer than each write without an error.
type shortAfterWriter struct {
	buf   bytes.Buffer
	limit int
}

func (s *shortAfterWriter) Write(p []byte) (int, error) {
	if s.buf.Len()+len(p) <= s.limit || len(p) == 0 {
		return s.buf.Write(p)
	}

	return s.buf.Write(p[:len(p)-1])
}
