package tracepack

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"slices"
	"sync"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/arloliu/go-secs/tracepack/internal/format"
	"github.com/arloliu/go-secs/tracepack/internal/tlv"
)

// controlTestFrame is an HSMS Linktest.req frame: SessionID 0xFFFF, SType 5, System Bytes 00 00 00 01.
var controlTestFrame = []byte{0, 0, 0, 0x0A, 0xFF, 0xFF, 0x00, 0x00, 0x00, 0x05, 0x00, 0x00, 0x00, 0x01}

// errStopIterate is the error a test callback returns to stop an iteration.
var errStopIterate = errors.New("stop iterating")

// iterItem is an Item copied out of an Iterate callback.
type iterItem struct {
	rec   Record
	extra []byte
	block int
}

// iterRun is what one Iterate call yielded and returned.
type iterRun struct {
	items []iterItem
	res   Result
}

// copyItem returns a copy of it that stays valid after the callback returns.
func copyItem(it *Item) iterItem {
	rec := it.Record
	rec.Payload = bytes.Clone(rec.Payload)

	return iterItem{rec: rec, extra: bytes.Clone(it.HeaderExtra), block: it.Block}
}

// runQuery runs q over r and returns what it yielded.
func runQuery(ctx context.Context, r *Reader, q Query) (iterRun, error) {
	var run iterRun
	res, err := r.Iterate(ctx, q, func(it *Item) error {
		run.items = append(run.items, copyItem(it))
		return nil
	})
	run.res = res

	return run, err
}

// iterate runs q over r and returns what it yielded; it fails the test on an error.
func iterate(t testing.TB, r *Reader, q Query) iterRun {
	t.Helper()

	run, err := runQuery(t.Context(), r, q)
	require.NoError(t, err)

	return run
}

// seqs returns the seqs of items, in order.
func (r *iterRun) seqs() []uint64 {
	out := make([]uint64, 0, len(r.items))
	for _, it := range r.items {
		out = append(out, it.rec.Seq)
	}

	return out
}

// storedAs returns rec as a query yields it: without its payload unless the query asks for payloads.
func storedAs(rec Record, payloads bool) Record {
	if !payloads {
		rec.Payload = nil
	}

	return rec
}

// shortCapture returns a data record over the first n bytes of blockTestFrame, its copies set from those bytes.
func shortCapture(seq uint64, ts int64, n int) Record {
	r := testDataRecord(seq, ts, 1)
	r.Payload = blockTestFrame[:n]
	r.DecodeStatus = DecodeStatusShortFrame
	r.SetHeaderCopies()

	return r
}

// controlRecord returns a control record over controlTestFrame, its copies set from the frame.
func controlRecord(seq uint64, ts int64, epoch uint32, dir Dir) Record {
	r := Record{
		Seq: seq, TSUTCNs: ts, MonoPresent: true, Epoch: epoch,
		Kind: KindControl, Dir: dir, DecodeStatus: DecodeStatusNotApplicable, Payload: controlTestFrame,
	}
	r.SetHeaderCopies()

	return r
}

// openGated opens file through a gatedReader without gated rounds, which logs every ReadAt call.
func openGated(t testing.TB, file []byte) (*Reader, *gatedReader) {
	t.Helper()

	g := newGatedReader(file)
	r, err := Open(t.Context(), g, int64(len(file)), ReaderOptions{})
	require.NoError(t, err)

	return r, g
}

// blocksRead runs q over r, opened through g, and returns the blocks it read, in order, with what it yielded.
func blocksRead(t *testing.T, r *Reader, g *gatedReader, q Query) ([]int, iterRun) {
	t.Helper()

	before := len(g.allCalls())
	run := iterate(t, r, q)

	calls := g.allCalls()[before:]
	read := make([]int, 0, len(calls))
	blocks := r.Blocks()
	for _, c := range calls {
		i := slices.IndexFunc(blocks, func(b BlockInfo) bool { return int64(b.Offset) == c.off })
		require.GreaterOrEqual(t, i, 0, "a read at %d is not a block read", c.off)
		read = append(read, i)
	}

	return read, run
}

func TestIterateInvalidQuery(t *testing.T) {
	t.Parallel()

	p := writeReaderPack(t, readerPackConfig{}, hourRecords(2, 3))
	r, g := openGated(t, p.file)
	opened := len(g.allCalls())

	tests := []struct {
		name string
		q    Query
	}{
		{name: "TimeFrom after TimeTo", q: Query{Filter: Filter{TimeFrom: new(blockTestHour + 1), TimeTo: new(blockTestHour)}}},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()

			res, err := r.Iterate(t.Context(), tt.q, func(*Item) error {
				t.Fatal("an invalid query yields nothing")
				return nil
			})
			require.ErrorIs(t, err, ErrInvalidQuery)
			assert.Equal(t, Result{}, res)
			assert.Len(t, g.allCalls(), opened, "an invalid query reads nothing")
		})
	}
}

func TestIterateEmptyTimeRange(t *testing.T) {
	t.Parallel()

	p := writeReaderPack(t, readerPackConfig{}, hourRecords(2, 3))
	r, g := openGated(t, p.file)

	at := blockTestHour + 1
	read, run := blocksRead(t, r, g, Query{Filter: Filter{TimeFrom: &at, TimeTo: &at}})
	assert.Empty(t, read, "an empty range prunes every indexed block")
	assert.Empty(t, run.items)
	assert.True(t, run.res.Complete())
}

func TestIterateAll(t *testing.T) {
	t.Parallel()

	for _, c := range []Codec{CodecNone, CodecZstd} {
		t.Run(c.String(), func(t *testing.T) {
			t.Parallel()

			recs := hourRecords(3, 4)
			p := writeReaderPack(t, readerPackConfig{codec: c}, recs)
			r := mustOpen(t, p.file, ReaderOptions{})

			for _, payloads := range []bool{false, true} {
				run := iterate(t, r, Query{Payloads: payloads})
				require.Len(t, run.items, len(recs), "payloads %v", payloads)

				for k, it := range run.items {
					assert.Equal(t, storedAs(recs[k], payloads), it.rec)
					assert.Nil(t, it.extra)
					assert.Equal(t, k/4, it.block)
				}
				assert.Equal(t, Result{}, run.res, "a complete result with nothing to report")
			}
		})
	}
}

func TestIterateHeaderExtra(t *testing.T) {
	t.Parallel()

	recs := hourRecords(2, 3)
	p := writeReaderPack(t, readerPackConfig{codec: CodecZstd}, recs)
	file := rebuildPack(t, p.file, func(i int, b *testBlock) {
		if i == 1 {
			widen(b)
		}
	})
	r := mustOpen(t, file, ReaderOptions{})
	require.NoError(t, r.Header().FooterErr)

	for _, payloads := range []bool{false, true} {
		run := iterate(t, r, Query{Payloads: payloads})
		require.Len(t, run.items, len(recs))
		for k, it := range run.items {
			if it.block == 1 {
				assert.Equal(t, wideExtra(k-3, recs[k].Seq), it.extra, "record %d", k)
			} else {
				assert.Nil(t, it.extra, "record %d", k)
			}
		}
	}
}

// copyVector is a record whose stored copy of one field disagrees with its payload,
// with a filter on the stored value and one on the payload's value.
type copyVector struct {
	name string
	// edit changes the stored copy of a record over blockTestFrame.
	edit func(r *Record)
	// header selects the stored value; payload selects the payload's value.
	header, payload Filter
}

// copyVectors returns stored copies that disagree with their payload for S/F, SessionID and System Bytes.
func copyVectors() []copyVector {
	return []copyVector{
		{
			name:   "S6F11 over S1F3",
			edit:   func(r *Record) { r.Stream, r.Function = 6, 11 },
			header: Filter{SF: []SF{{Stream: 6, Function: 11}}}, payload: Filter{SF: []SF{{Stream: 1, Function: 3}}},
		},
		{
			name:   "SessionID",
			edit:   func(r *Record) { r.SessionID = 0x0BAD },
			header: Filter{SessionIDs: []uint16{0x0BAD}}, payload: Filter{SessionIDs: []uint16{0x1234}},
		},
		{
			name:   "SystemBytes",
			edit:   func(r *Record) { r.SystemBytes = [4]byte{1, 2, 3, 4} },
			header: Filter{SystemBytes: &[4]byte{1, 2, 3, 4}}, payload: Filter{SystemBytes: &[4]byte{0xDE, 0xAD, 0xBE, 0xEF}},
		},
	}
}

// disagreeingRecord returns the record of v: a data record over blockTestFrame whose stored copy v changes.
func (v *copyVector) disagreeingRecord() Record {
	r := testDataRecord(0, blockTestHour, 1)
	v.edit(&r)

	return r
}

// TestIteratePredicatesReadPayload checks that the HSMS header predicates of a Filter are evaluated on the payload,
// whatever the record header stores (the tracepack semantics specification §7.4), with and without payloads.
func TestIteratePredicatesReadPayload(t *testing.T) {
	t.Parallel()

	for _, v := range copyVectors() {
		t.Run(v.name, func(t *testing.T) {
			t.Parallel()

			rec := v.disagreeingRecord()
			p := writeReaderPack(t, readerPackConfig{codec: CodecZstd}, []Record{rec})
			r := mustOpen(t, p.file, ReaderOptions{})

			for _, payloads := range []bool{false, true} {
				none := iterate(t, r, Query{Filter: v.header, Payloads: payloads})
				assert.Empty(t, none.items, "payloads %v: the stored value is never matched", payloads)
				assert.Equal(t, Result{}, none.res)

				run := iterate(t, r, Query{Filter: v.payload, Payloads: payloads})
				require.Len(t, run.items, 1, "payloads %v: the payload's value is matched", payloads)
				assert.Equal(t, storedAs(rec, payloads), run.items[0].rec, "the record is yielded as stored")
				assert.Equal(t, Result{}, run.res)
			}
		})
	}
}

func TestIterateShortCaptureCopies(t *testing.T) {
	t.Parallel()

	// Seven captured bytes hold SessionID and stream, but not function or System Bytes,
	// whose stored copies are zero: a query for the zero value must still not match them (§7.2).
	short := shortCapture(0, blockTestHour, 7)
	require.Equal(t, FieldValiditySessionID|FieldValidityStreamAndW, short.FieldValidity)
	event := testEventRecord(t, 1, blockTestHour+1, 1, &TransportEvent{Event: EventSocketClose})

	tests := []struct {
		name string
		f    Filter
		want []uint64
	}{
		{name: "SF needs function", f: Filter{SF: []SF{{Stream: 1, Function: 0}}}},
		{name: "SF with unavailable", f: Filter{SF: []SF{{Stream: 1, Function: 0}}, IncludeUnavailable: true}, want: []uint64{0, 1}},
		{name: "zero System Bytes", f: Filter{SystemBytes: &[4]byte{}}},
		{name: "System Bytes with unavailable", f: Filter{SystemBytes: &[4]byte{9, 9, 9, 9}, IncludeUnavailable: true}, want: []uint64{0, 1}},
		{name: "available SessionID", f: Filter{SessionIDs: []uint16{0x1234}}, want: []uint64{0}},
		{name: "available SessionID disagrees", f: Filter{SessionIDs: []uint16{0x9999}, IncludeUnavailable: true}, want: []uint64{1}},
		{
			name: "every predicate",
			f:    Filter{SF: []SF{{Stream: 7, Function: 7}}, SessionIDs: []uint16{0x1234}, SystemBytes: &[4]byte{}, IncludeUnavailable: true},
			want: []uint64{0, 1},
		},
		{name: "zero SessionID of an event", f: Filter{SessionIDs: []uint16{0}}},
	}
	for _, validate := range []bool{false, true} {
		p := writeReaderPack(t, readerPackConfig{codec: CodecZstd, validate: validate}, []Record{short, event})
		r := mustOpen(t, p.file, ReaderOptions{})

		for _, tt := range tests {
			for _, payloads := range []bool{false, true} {
				run := iterate(t, r, Query{Filter: tt.f, Payloads: payloads})
				assert.Equal(t, tt.want, nilIfEmpty(run.seqs()), "%s, validated %v, payloads %v", tt.name, validate, payloads)
			}
		}
	}
}

// nilIfEmpty returns v, or nil when it is empty.
func nilIfEmpty[T any](v []T) []T {
	if len(v) == 0 {
		return nil
	}

	return v
}

// clearedSystemBytes returns a data record over the whole blockTestFrame
// whose System Bytes bit is clear and whose copy is zero,
// as a log conversion stores an identity its source did not carry (the tracepack storage specification §7).
func clearedSystemBytes(seq uint64) Record {
	r := testDataRecord(seq, blockTestHour+int64(seq), 1)
	r.FieldValidity &^= FieldValiditySystemBytes
	r.SystemBytes = [4]byte{}

	return r
}

func TestIterateClearBitOverPayloadBytes(t *testing.T) {
	t.Parallel()

	// The payload holds System Bytes DE AD BE EF, but the stored bit is clear,
	// so the field is unavailable, whether the Writer validated its blocks or not (the tracepack format specification §7.2).
	rec := clearedSystemBytes(0)
	payloadSB := [4]byte(blockTestFrame[copySystemBytesOff:copySystemBytesEnd])

	for _, validate := range []bool{false, true} {
		p := writeReaderPack(t, readerPackConfig{codec: CodecZstd, validate: validate}, []Record{rec})
		r := mustOpen(t, p.file, ReaderOptions{})

		for _, payloads := range []bool{false, true} {
			name := fmt.Sprintf("validated %v, payloads %v", validate, payloads)
			for _, sb := range [][4]byte{payloadSB, {}} {
				run := iterate(t, r, Query{Filter: Filter{SystemBytes: &sb}, Payloads: payloads})
				assert.Empty(t, run.items, "%s: System Bytes % X", name, sb)
			}

			run := iterate(t, r, Query{Filter: Filter{SystemBytes: &payloadSB, IncludeUnavailable: true}, Payloads: payloads})
			assert.Len(t, run.items, 1, name)

			sf := iterate(t, r, Query{Filter: Filter{SF: []SF{{Stream: 1, Function: 3}}}, Payloads: payloads})
			assert.Len(t, sf.items, 1, "%s: the fields whose bits are set stay available", name)
		}
	}
}

func TestIterateSetBitBeyondPayload(t *testing.T) {
	t.Parallel()

	// Every bit is set, but the payload ends before System Bytes: a writer defect (the tracepack format specification §7.2),
	// whose field a reader treats as unavailable.
	rec := testDataRecord(0, blockTestHour, 1)
	rec.Payload = blockTestFrame[:copySTypeEnd]
	rec.DecodeStatus = DecodeStatusShortFrame

	p := writeReaderPack(t, readerPackConfig{codec: CodecZstd}, []Record{rec})
	r := mustOpen(t, p.file, ReaderOptions{})

	tests := []struct {
		name    string
		f       Filter
		yielded bool
	}{
		{name: "stored System Bytes", f: Filter{SystemBytes: &rec.SystemBytes}},
		{name: "zero System Bytes", f: Filter{SystemBytes: &[4]byte{}}},
		{name: "unavailable requested", f: Filter{SystemBytes: &[4]byte{}, IncludeUnavailable: true}, yielded: true},
		{name: "an available field", f: Filter{SF: []SF{{Stream: 1, Function: 3}}}, yielded: true},
	}
	for _, tt := range tests {
		for _, payloads := range []bool{false, true} {
			run := iterate(t, r, Query{Filter: tt.f, Payloads: payloads})
			if tt.yielded {
				assert.Len(t, run.items, 1, "%s, payloads %v", tt.name, payloads)
			} else {
				assert.Empty(t, run.items, "%s, payloads %v", tt.name, payloads)
			}
		}
	}
}

func TestIterateCallbackError(t *testing.T) {
	t.Parallel()

	recs := hourRecords(3, 4)
	p := writeReaderPack(t, readerPackConfig{codec: CodecZstd}, recs)
	r := mustOpen(t, p.file, ReaderOptions{})

	var seen []uint64
	res, err := r.Iterate(t.Context(), Query{Filter: Filter{SF: []SF{{Stream: 1, Function: 3}}}}, func(it *Item) error {
		seen = append(seen, it.Record.Seq)
		if it.Record.Seq == 5 {
			return errStopIterate
		}

		return nil
	})
	require.Equal(t, errStopIterate, err, "the callback's error is returned as is") //nolint:testifylint // identity, not errors.Is
	assert.Equal(t, []uint64{0, 1, 2, 3, 4, 5}, seen)
	assert.True(t, res.Complete(), "the result so far")
}

func TestIterateContextCancelled(t *testing.T) {
	t.Parallel()

	recs := hourRecords(3, 4)
	p := writeReaderPack(t, readerPackConfig{codec: CodecZstd}, recs)
	r := mustOpen(t, p.file, ReaderOptions{})

	ctx, cancel := context.WithCancel(t.Context())
	defer cancel()

	var blocks []int
	_, err := r.Iterate(ctx, Query{}, func(it *Item) error {
		blocks = append(blocks, it.Block)
		cancel()

		return nil
	})
	require.ErrorIs(t, err, context.Canceled)
	assert.Equal(t, []int{0, 0, 0, 0}, blocks, "the block being yielded completes; the next is not read")
}

func TestIterateReadError(t *testing.T) {
	t.Parallel()

	recs := hourRecords(3, 4)
	p := writeReaderPack(t, readerPackConfig{codec: CodecZstd}, recs)
	r, g := openGated(t, p.file)
	failAt := int64(r.Blocks()[1].Offset)
	g.mu.Lock()
	g.fail = func(off int64, _ int) bool { return off == failAt }
	g.mu.Unlock()

	var seen []uint64
	res, err := r.Iterate(t.Context(), Query{Filter: Filter{SessionIDs: []uint16{0x1234}}}, func(it *Item) error {
		seen = append(seen, it.Record.Seq)
		return nil
	})
	require.ErrorIs(t, err, errInjected)
	assert.Equal(t, []uint64{0, 1, 2, 3}, seen)
	assert.True(t, res.Complete(), "the result so far")
}

// pruneTestRecords returns three records in each of four UTC hours, one block per hour:
//
//	block 0: data, host-to-equipment, epoch 1
//	block 1: data, equipment-to-host, epoch 2
//	block 2: control, host-to-equipment, epoch 3
//	block 3: data, host-to-equipment, epochs 5 and 7
func pruneTestRecords() []Record {
	recs := make([]Record, 0, 12)
	for i := range 12 {
		seq, ts := uint64(i), blockTestHour+int64(i/3)*hourNs+int64(i%3)
		switch i / 3 {
		case 0:
			recs = append(recs, testDataRecord(seq, ts, 1))
		case 1:
			r := testDataRecord(seq, ts, 2)
			r.Dir = DirEquipmentToHost
			recs = append(recs, r)
		case 2:
			recs = append(recs, controlRecord(seq, ts, 3, DirHostToEquipment))
		default:
			recs = append(recs, testDataRecord(seq, ts, uint32(5+2*(i%2))))
		}
	}

	return recs
}

func TestIteratePrunes(t *testing.T) {
	t.Parallel()

	recs := pruneTestRecords()
	p := writeReaderPack(t, readerPackConfig{codec: CodecZstd}, recs)
	require.Len(t, p.blocks, 4)
	r, g := openGated(t, p.file)
	require.NoError(t, r.Header().FooterErr)

	hour := func(h int64) *int64 { return new(blockTestHour + h*hourNs) }
	tests := []struct {
		name string
		f    Filter
		read []int
		want []uint64
	}{
		{name: "time", f: Filter{TimeFrom: hour(1), TimeTo: hour(2)}, read: []int{1}, want: []uint64{3, 4, 5}},
		{name: "time from", f: Filter{TimeFrom: new(blockTestHour + 2*hourNs + 2)}, read: []int{2, 3}, want: []uint64{8, 9, 10, 11}},
		{name: "time to", f: Filter{TimeTo: new(blockTestHour + 1)}, read: []int{0}, want: []uint64{0}},
		{name: "time touching ts_max", f: Filter{TimeFrom: new(blockTestHour + 2), TimeTo: hour(1)}, read: []int{0}, want: []uint64{2}},
		{name: "time touching ts_min", f: Filter{TimeFrom: new(blockTestHour + 3), TimeTo: new(blockTestHour + hourNs + 1)}, read: []int{1}, want: []uint64{3}},
		{name: "epoch", f: Filter{Epochs: []uint32{2}}, read: []int{1}, want: []uint64{3, 4, 5}},
		{name: "epoch inside the range of no record", f: Filter{Epochs: []uint32{6}}, read: []int{3}},
		{name: "epoch of no block", f: Filter{Epochs: []uint32{4, 9}}},
		{name: "kind", f: Filter{Kinds: []Kind{KindControl}}, read: []int{2}, want: []uint64{6, 7, 8}},
		{name: "kind outside the counts", f: Filter{Kinds: []Kind{Kind(200)}}},
		{name: "dir", f: Filter{Dirs: []Dir{DirEquipmentToHost}}, read: []int{1}, want: []uint64{3, 4, 5}},
		{name: "dir outside the counts", f: Filter{Dirs: []Dir{Dir(9), DirLocal}}},
		{name: "kind and dir", f: Filter{Kinds: []Kind{KindData}, Dirs: []Dir{DirHostToEquipment}}, read: []int{0, 3}, want: []uint64{0, 1, 2, 9, 10, 11}},
		{name: "time and S/F", f: Filter{TimeTo: hour(1), SF: []SF{{Stream: 1, Function: 3}}}, read: []int{0}, want: []uint64{0, 1, 2}},
	}
	for _, tt := range tests {
		read, run := blocksRead(t, r, g, Query{Filter: tt.f})
		assert.Equal(t, tt.read, nilIfEmpty(read), "%s: blocks read", tt.name)
		assert.Equal(t, tt.want, nilIfEmpty(run.seqs()), tt.name)
		assert.True(t, run.res.Complete(), tt.name)
	}
}

func TestIterateNeverPrunesWalkedBlocks(t *testing.T) {
	t.Parallel()

	recs := pruneTestRecords()
	p := writeReaderPack(t, readerPackConfig{codec: CodecZstd, open: true}, recs)
	r, g := openGated(t, p.file)
	require.Len(t, r.Blocks(), 4)

	all := []int{0, 1, 2, 3}
	filters := []Filter{
		{TimeFrom: new(blockTestHour + hourNs), TimeTo: new(blockTestHour + 2*hourNs)},
		{Epochs: []uint32{2}},
		{Dirs: []Dir{DirEquipmentToHost}},
		{Kinds: []Kind{KindData}, Dirs: []Dir{DirEquipmentToHost}},
	}
	for i, f := range filters {
		read, run := blocksRead(t, r, g, Query{Filter: f})
		assert.Equal(t, all, read, "filter %d: a walked block is always read", i)
		assert.Equal(t, []uint64{3, 4, 5}, run.seqs(), "filter %d", i)
		require.Len(t, run.res.Incomplete, 1)
		assert.Equal(t, ReasonTruncated, run.res.Incomplete[0].Reason)
	}

	read, run := blocksRead(t, r, g, Query{Filter: Filter{Epochs: []uint32{2}, SF: []SF{{Stream: 1, Function: 3}}}})
	assert.Equal(t, all, read)
	assert.Equal(t, []uint64{3, 4, 5}, run.seqs())
}

// TestIteratePruningTrustsValidatedFooter documents the trust model:
// pruning trusts the F-2 ranges of a footer that passed the validation of the tracepack format specification §10,
// so a writer defect that shifts a block's time range within its hour excludes the block's records without a defect.
// Only a read of the block, or verify, compares its records with the F-2 entry.
func TestIteratePruningTrustsValidatedFooter(t *testing.T) {
	t.Parallel()

	h := blockTestHour
	steps := make([]footerTestStep, 0, 9)
	for b := range 3 {
		for j := range 3 {
			steps = append(steps, footerTestStep{rec: testDataRecord(uint64(3*b+j), h+int64(1000*b+j), 1), flush: j == 2})
		}
	}
	p := writeFooterTestPack(t, CodecZstd, steps)
	require.Len(t, p.blocks, 3)

	// Block 1 holds times h+1000 to h+1002; its F-2 entry and F-3 epoch entry are moved to h+1500 to h+1502,
	// inside the pack's range, so F-5 is unchanged and the footer still validates.
	parts := splitFooter(t, p.decoded)
	parts.entries[1].TSMin, parts.entries[1].TSMax = h+1500, h+1502
	parts.f3[1] = withNestedAt(t, parts.f3[1], f3TagEpoch, 0, epochTagTSMin, tlv.I64Entry(epochTagTSMin, h+1500))
	parts.f3[1] = withNestedAt(t, parts.f3[1], f3TagEpoch, 0, epochTagTSMax, tlv.I64Entry(epochTagTSMax, h+1502))
	r, g := openGated(t, refooter(t, p.file, parts.encode()))
	require.NoError(t, r.Header().FooterErr, "the shifted footer validates")

	read, run := blocksRead(t, r, g, Query{Filter: Filter{TimeFrom: new(h + 1000), TimeTo: new(h + 1003)}})
	assert.Empty(t, read, "the block is pruned by its F-2 range")
	assert.Empty(t, run.items, "its records are not found")
	assert.True(t, run.res.Complete(), "and nothing reports them missing")

	read, run = blocksRead(t, r, g, Query{Filter: Filter{TimeFrom: new(h + 1500), TimeTo: new(h + 1503)}})
	assert.Equal(t, []int{1}, read)
	assert.Empty(t, run.items)
	require.Len(t, run.res.Incomplete, 1, "a block that is read is compared with its F-2 entry")
	assert.Equal(t, ReasonCorruptBlock, run.res.Incomplete[0].Reason)
	assert.Equal(t, 1, run.res.Incomplete[0].Block)
}

// coverageTestEntries returns the coverage entries of the coverage test pack, whose capture is own.
func coverageTestEntries(own, other UUID) []Coverage {
	h := blockTestHour
	at := func(v int64) *int64 { return new(h + v) }

	return []Coverage{
		{TimeStart: at(100)},                   // 0: no end
		{TimeEnd: at(50)},                      // 1: no start
		{TimeStart: at(500), TimeEnd: at(100)}, // 2: inverted time
		{SeqFirst: new(uint64(10)), SeqLast: new(uint64(5)), TimeStart: at(900), TimeEnd: at(950)}, // 3: inverted seqs
		{TimeStart: at(200), TimeEnd: at(300)},                                                     // 4
		{CaptureID: &other, TimeStart: at(0), TimeEnd: at(1000)},                                   // 5: another capture
		{CaptureID: &own, TimeStart: at(600), TimeEnd: at(700), SeqFirst: new(uint64(1))},          // 6: this capture, named
	}
}

// coveragePack returns a pack of hourRecords(1, 3) whose pack metadata carries coverageTestEntries,
// its file header patched to the capture those entries call its own, and the entries.
func coveragePack(t testing.TB, validate bool) ([]byte, []Coverage) {
	t.Helper()

	own, other := UUID{0xC0, 1}, UUID{0xC0, 2}
	entries := coverageTestEntries(own, other)
	p := writeReaderPack(t, readerPackConfig{codec: CodecZstd, validate: validate, meta: func(m *PackMeta) { m.Coverage = entries }}, hourRecords(1, 3))
	file := patchHeader(t, p.file, func(h *format.FileHeader) { h.CaptureID = format.UUID(own) })

	return file, entries
}

// coverageHits returns the indexes into entries of the coverage defects of res, in order,
// requiring each to be a coverage defect of no block and no offset.
func coverageHits(t testing.TB, res *Result, entries []Coverage) []int {
	t.Helper()

	out := make([]int, 0, len(res.Incomplete))
	for _, d := range res.Incomplete {
		require.Equal(t, ReasonCoverage, d.Reason)
		assert.Equal(t, -1, d.Block)
		assert.Equal(t, int64(-1), d.Offset)
		require.Error(t, d.Err)
		require.NotNil(t, d.Coverage)
		i := slices.IndexFunc(entries, func(c Coverage) bool { return assert.ObjectsAreEqual(c, *d.Coverage) })
		require.GreaterOrEqual(t, i, 0, "the defect carries its entry")
		out = append(out, i)
	}

	return out
}

func TestIterateCoverage(t *testing.T) {
	t.Parallel()

	file, entries := coveragePack(t, false)
	r := mustOpen(t, file, ReaderOptions{})

	h := blockTestHour
	at := func(v int64) *int64 { return new(h + v) }
	tests := []struct {
		name     string
		from, to *int64
		want     []int
	}{
		{name: "no time filter", want: []int{0, 1, 2, 3, 4, 6}},
		{name: "touching an inclusive end", from: at(300), to: at(400), want: []int{0, 2, 3, 4}},
		{name: "past an end", from: at(301), to: at(400), want: []int{0, 2, 3}},
		{name: "touching a start excluded by the half-open end", from: at(150), to: at(200), want: []int{0, 2, 3}},
		{name: "touching a start", from: at(150), to: at(201), want: []int{0, 2, 3, 4}},
		{name: "touching an end without start", from: at(50), to: at(60), want: []int{1, 2, 3}},
		{name: "past an end without start", from: at(51), to: at(60), want: []int{2, 3}},
		{name: "from only", from: at(650), want: []int{0, 2, 3, 6}},
		{name: "to only, before a start without end", to: at(100), want: []int{1, 2, 3}},
		{name: "to only, at a start without end", to: at(101), want: []int{0, 1, 2, 3}},
		{name: "disjoint from all but the inverted", from: at(-100), to: at(-50), want: []int{1, 2, 3}},
		{name: "empty range", from: at(250), to: at(250), want: []int{2, 3}},
	}
	for _, tt := range tests {
		run := iterate(t, r, Query{Filter: Filter{TimeFrom: tt.from, TimeTo: tt.to}})
		assert.Equal(t, tt.want, coverageHits(t, &run.res, entries), tt.name)
		assert.Equal(t, len(tt.want) == 0, run.res.Complete(), tt.name)
	}

	all := iterate(t, r, Query{Payloads: true})
	assert.Len(t, all.items, 3, "coverage defects never stop the records")
}

func TestIterateResultOrder(t *testing.T) {
	t.Parallel()

	// An unfinalized pack with coverage, whose second block fails its body CRC:
	// the walk's defect first, then coverage, then the block.
	recs := hourRecords(3, 2)
	own := UUID{0xC0, 1}
	p := writeReaderPack(t, readerPackConfig{
		codec: CodecZstd, open: true,
		meta: func(m *PackMeta) { m.Coverage = []Coverage{{CaptureID: &own}} },
	}, recs)
	file := patchHeader(t, p.file, func(h *format.FileHeader) { h.CaptureID = format.UUID(own) })
	file = flipByte(file, p.blocks[1].offset+format.EnvelopeLen)

	r := mustOpen(t, file, ReaderOptions{})
	run := iterate(t, r, Query{Payloads: true})
	require.Len(t, run.res.Incomplete, 3)
	assert.Equal(t, ReasonTruncated, run.res.Incomplete[0].Reason)
	assert.Equal(t, ReasonCoverage, run.res.Incomplete[1].Reason)
	assert.Equal(t, ReasonCorruptBlock, run.res.Incomplete[2].Reason)
	assert.Equal(t, 1, run.res.Incomplete[2].Block)
	require.Error(t, run.res.FooterErr)
	assert.Equal(t, []uint64{0, 1, 4, 5}, run.seqs())
}

func TestIterateOverBudgetBlock(t *testing.T) {
	t.Parallel()

	recs := []Record{
		testDataRecord(0, blockTestHour, 1),
		bigDataRecord(1, blockTestHour+hourNs, 20_000),
		testDataRecord(2, blockTestHour+2*hourNs, 1),
	}
	for _, open := range []bool{false, true} {
		p := writeReaderPack(t, readerPackConfig{open: open}, recs)
		require.Len(t, p.blocks, 3)
		r := mustOpen(t, p.file, ReaderOptions{MaxBlockLen: 4096})

		for _, payloads := range []bool{false, true} {
			run := iterate(t, r, Query{Payloads: payloads})
			assert.Equal(t, []uint64{0, 2}, run.seqs(), "open %v: the blocks around the big one are yielded", open)

			var limits []Defect
			for _, d := range run.res.Incomplete {
				if d.Reason == ReasonLimit {
					limits = append(limits, d)
				} else {
					assert.Equal(t, ReasonTruncated, d.Reason, "only the unfinalized pack is truncated")
				}
			}
			require.Len(t, limits, 1)
			assert.Equal(t, 1, limits[0].Block)
			assert.Equal(t, int64(p.blocks[1].offset), limits[0].Offset)
			require.ErrorIs(t, limits[0].Err, ErrReadLimit)
			assert.Equal(t, !open, len(run.res.Incomplete) == 1)
		}
	}
}

func TestIterateTruncated(t *testing.T) {
	t.Parallel()

	recs := hourRecords(3, 3)
	for _, open := range []bool{false, true} {
		t.Run(fmt.Sprintf("open %v", open), func(t *testing.T) {
			t.Parallel()

			p := writeReaderPack(t, readerPackConfig{codec: CodecZstd, open: open}, recs)
			start := p.blocks[0].offset
			for n := start; n < uint64(len(p.file)); n++ {
				r, err := openBytes(t, p.file[:n], ReaderOptions{})
				require.NoError(t, err, "cut at %d", n)

				for _, payloads := range []bool{false, true} {
					run := iterate(t, r, Query{Payloads: payloads})
					require.False(t, run.res.Complete(), "cut at %d", n)
					assert.True(t, slices.ContainsFunc(run.res.Incomplete, func(d Defect) bool { return d.Reason == ReasonTruncated }), "cut at %d", n)

					for _, it := range run.items {
						require.Less(t, it.rec.Seq, uint64(len(recs)))
						require.Equal(t, storedAs(recs[it.rec.Seq], payloads), it.rec, "cut at %d", n)
					}
					require.True(t, slices.IsSorted(run.seqs()))
				}
			}
		})
	}
}

// concurrencyQueries returns queries whose results differ over the coverage pack.
func concurrencyQueries() []Query {
	return []Query{
		{},
		{Payloads: true},
		{Filter: Filter{SF: []SF{{Stream: 1, Function: 3}}}},
		{Filter: Filter{TimeFrom: new(blockTestHour + 1), TimeTo: new(blockTestHour + 250)}},
		{Filter: Filter{TimeFrom: new(blockTestHour + 650), SessionIDs: []uint16{0x1234}}},
		{Filter: Filter{Kinds: []Kind{KindControl}}},
	}
}

func TestIterateConcurrent(t *testing.T) {
	t.Parallel()

	for _, validate := range []bool{false, true} {
		file, _ := coveragePack(t, validate)
		r := mustOpen(t, file, ReaderOptions{})

		queries := concurrencyQueries()
		serial := make([]iterRun, len(queries))
		for i, q := range queries {
			serial[i] = iterate(t, r, q)
		}

		const rounds = 8
		got := make([][]iterRun, rounds)
		errs := make([]error, rounds)
		var wg sync.WaitGroup
		for g := range rounds {
			got[g] = make([]iterRun, len(queries))
			wg.Go(func() {
				for k := range queries {
					// Each goroutine starts at a different query, so different queries overlap.
					i := (g + k) % len(queries)
					got[g][i], errs[g] = runQuery(t.Context(), r, queries[i])
					if errs[g] != nil {
						return
					}
				}
			})
		}
		wg.Wait()

		for g := range rounds {
			require.NoError(t, errs[g])
			assert.Equal(t, serial, got[g], "goroutine %d, validated %v", g, validate)
		}
	}
}

func TestIterateUnaffectedByCallerMutation(t *testing.T) {
	t.Parallel()

	file, entries := coveragePack(t, true)
	r := mustOpen(t, file, ReaderOptions{})
	queries := concurrencyQueries()
	before := make([]iterRun, len(queries))
	hits := make([][]int, len(queries))
	for i, q := range queries {
		before[i] = iterate(t, r, q)
		hits[i] = coverageHits(t, &before[i].res, entries)
	}

	h := r.Header()
	h.Meta.PackRole = PackRoleExtract
	*h.Meta.Coverage[0].TimeStart = blockTestHour + 5000
	h.Meta.Coverage = h.Meta.Coverage[:1]

	// The coverage entries of the defects are the caller's too.
	for _, run := range before {
		for _, d := range run.res.Incomplete {
			for _, v := range []*int64{d.Coverage.TimeStart, d.Coverage.TimeEnd} {
				if v != nil {
					*v = 1
				}
			}
			if d.Coverage.CaptureID != nil {
				d.Coverage.CaptureID[0]++
			}
			d.Coverage.SeqLast = nil
		}
	}

	for i, q := range queries {
		after := iterate(t, r, q)
		assert.Equal(t, before[i].items, after.items, "query %d", i)
		assert.Equal(t, hits[i], coverageHits(t, &after.res, entries), "query %d", i)
	}
}

// fuzzIterateQueries returns the queries FuzzIterate runs over every input:
// with and without payloads, an HSMS header predicate, and a time range that prunes.
func fuzzIterateQueries() []Query {
	return []Query{
		{Payloads: true},
		{},
		{Filter: Filter{SF: []SF{{Stream: 1, Function: 3}}}, Payloads: true},
		{Filter: Filter{
			TimeFrom: new(blockTestHour + hourNs), TimeTo: new(blockTestHour + 2*hourNs),
			SessionIDs: []uint16{0x1234}, IncludeUnavailable: true,
		}},
	}
}

// fuzzRecordKey returns a key identifying the record of it byte for byte: its fields, HeaderExtra and, when present, its payload.
func fuzzRecordKey(it *Item) string {
	rec := it.Record
	rec.Payload = nil
	key := fmt.Sprintf("%+v|%x", rec, it.HeaderExtra)
	if it.Record.Payload != nil {
		key += fmt.Sprintf("|%x", it.Record.Payload)
	}

	return key
}

// fuzzIterateSeeds returns the seed packs of FuzzIterate:
// indexed, validated, walked, copy disagreements with a short capture and an event, coverage, widened headers,
// and blocks that prune by time, epoch, kind and direction.
func fuzzIterateSeeds(f *testing.F) [][]byte {
	v := copyVectors()[0]
	mixed := []Record{
		v.disagreeingRecord(),
		shortCapture(1, blockTestHour+1, 7),
		testEventRecord(f, 2, blockTestHour+2, 1, &TransportEvent{Event: EventSocketClose}),
	}
	coverage, _ := coveragePack(f, false)
	wide := writeReaderPack(f, readerPackConfig{codec: CodecZstd}, hourRecords(2, 2))

	return [][]byte{
		writeReaderPack(f, readerPackConfig{}, hourRecords(2, 3)).file,
		writeReaderPack(f, readerPackConfig{codec: CodecZstd, validate: true}, hourRecords(2, 3)).file,
		writeReaderPack(f, readerPackConfig{codec: CodecZstd, open: true}, hourRecords(2, 3)).file,
		writeReaderPack(f, readerPackConfig{codec: CodecZstd}, mixed).file,
		coverage,
		rebuildPack(f, wide.file, func(i int, b *testBlock) {
			if i == 1 {
				widen(b)
			}
		}),
		writeReaderPack(f, readerPackConfig{codec: CodecZstd}, pruneTestRecords()).file,
	}
}

// FuzzIterate opens mutated packs and iterates them with fixed queries.
// Iterate must never panic, and must keep the invariants of the tracepack semantics specification §7.4 on every input.
// A complete result over a mutated input yields only records of the seed packs, byte for byte,
// because every byte a record is read from is covered by a CRC that a mutation breaks.
func FuzzIterate(f *testing.F) {
	opts := ReaderOptions{MaxPackMetadataLen: 1 << 20, MaxFooterLen: 1 << 20, MaxBlockLen: 1 << 20}
	queries := fuzzIterateQueries()

	known := make(map[string]bool)
	for _, file := range fuzzIterateSeeds(f) {
		f.Add(file)

		r := mustOpen(f, file, opts)
		for _, q := range []Query{{Payloads: true}, {}} {
			_, err := r.Iterate(f.Context(), q, func(it *Item) error {
				known[fuzzRecordKey(it)] = true
				return nil
			})
			require.NoError(f, err)
		}
	}

	f.Fuzz(func(t *testing.T, data []byte) {
		r, err := Open(t.Context(), bytes.NewReader(data), int64(len(data)), opts)
		if err != nil {
			return
		}

		nblocks := len(r.Blocks())
		for _, q := range queries {
			var unknown []string
			res, err := r.Iterate(t.Context(), q, func(it *Item) error {
				require.GreaterOrEqual(t, it.Block, 0)
				require.Less(t, it.Block, nblocks)
				require.Equal(t, q.Payloads, it.Record.Payload != nil, "a payload iff the query asks for payloads")
				if key := fuzzRecordKey(it); !known[key] {
					unknown = append(unknown, key)
				}

				return nil
			})
			require.NoError(t, err)

			for _, d := range res.Incomplete {
				require.True(t, d.Block == -1 || (d.Block >= 0 && d.Block < nblocks))
				require.Equal(t, d.Reason == ReasonCoverage, d.Coverage != nil)
			}
			if res.Complete() {
				require.Empty(t, unknown, "a complete result yields only records of the seed packs; %v", q)
			}
		}
	})
}
