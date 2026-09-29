package tracepack

import (
	"bytes"
	"context"
	"encoding/binary"
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
	rec      Record
	extra    []byte
	block    int
	level    ReadLevel
	mismatch bool
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

	return iterItem{rec: rec, extra: bytes.Clone(it.HeaderExtra), block: it.Block, level: it.Level, mismatch: it.CopyMismatch}
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

// storedAs returns rec as a read at level returns it: without its payload unless the read is full.
func storedAs(rec Record, level ReadLevel) Record {
	if level != ReadFull {
		rec.Payload = nil
	}

	return rec
}

// payloadCopies returns rec with its copy fields and field_validity set from its payload, as SetHeaderCopies does.
func payloadCopies(rec Record) Record {
	rec.SetHeaderCopies()

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

// notesMeta sets a one-byte notes entry, which falselyAttest overwrites in place.
func notesMeta(m *PackMeta) {
	m.Notes = new("x")
}

// falselyAttest returns a copy of file, written with notesMeta, whose one-byte notes entry is overwritten in place
// by blocks_validated = true, with pack_metadata_crc and header_crc recomputed:
// the pack of a nonconforming writer that asserts an attestation its blocks do not satisfy.
// Both entries are nine bytes long, so nothing else moves.
func falselyAttest(t testing.TB, file []byte) []byte {
	t.Helper()

	patched := false
	out := patchMeta(t, file, func(meta []byte) {
		for off := 0; off+8 <= len(meta); {
			tag, n := binary.LittleEndian.Uint16(meta[off:]), int(binary.LittleEndian.Uint32(meta[off+4:]))
			if tag == tagNotes && n == 1 {
				binary.LittleEndian.PutUint16(meta[off:], tagBlocksValidated)
				meta[off+2], meta[off+8] = uint8(tlv.TypeBool), 1
				patched = true

				return
			}
			off += 8 + n
		}
	})
	require.True(t, patched, "the pack has a one-byte notes entry")

	return out
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

func TestQueryModeString(t *testing.T) {
	t.Parallel()

	assert.Equal(t, "provisional", QueryProvisional.String())
	assert.Equal(t, "authoritative", QueryAuthoritative.String())
	assert.Equal(t, "unknown(2)", QueryMode(2).String())
}

func TestReadLevelString(t *testing.T) {
	t.Parallel()

	assert.Equal(t, "unknown(0)", ReadLevel(0).String())
	assert.Equal(t, "header-only", ReadHeaderOnly.String())
	assert.Equal(t, "attested", ReadAttested.String())
	assert.Equal(t, "full", ReadFull.String())
	assert.Equal(t, "unknown(4)", ReadLevel(4).String())
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
		{name: "unknown mode", q: Query{Mode: QueryMode(2)}},
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

			for _, mode := range []QueryMode{QueryProvisional, QueryAuthoritative} {
				for _, payloads := range []bool{false, true} {
					run := iterate(t, r, Query{Mode: mode, Payloads: payloads})
					require.Len(t, run.items, len(recs), "%v, payloads %v", mode, payloads)

					level := ReadHeaderOnly
					if payloads {
						level = ReadFull
					}
					for k, it := range run.items {
						assert.Equal(t, storedAs(recs[k], level), it.rec)
						assert.Nil(t, it.extra)
						assert.Equal(t, k/4, it.block)
						assert.Equal(t, level, it.level)
						assert.False(t, it.mismatch)
					}
					assert.Equal(t, Result{}, run.res, "a complete result with nothing to report")
				}
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

// copyVector is a record whose stored copy of one field disagrees with its payload (I-10),
// with a filter on the stored value and one on the payload's value.
type copyVector struct {
	name string
	// edit changes the stored copy of a record over blockTestFrame.
	edit func(r *Record)
	// header selects the stored value; payload selects the payload's value.
	header, payload Filter
}

// copyVectors returns the query-mode vectors of the tracepack format specification §16:
// S6F11 copies over an S1F3 payload, and the same for SessionID and System Bytes.
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

func TestIterateQueryModes(t *testing.T) {
	t.Parallel()

	for _, v := range copyVectors() {
		t.Run(v.name, func(t *testing.T) {
			t.Parallel()

			rec := v.disagreeingRecord()
			p := writeReaderPack(t, readerPackConfig{codec: CodecZstd}, []Record{rec})
			r := mustOpen(t, p.file, ReaderOptions{})
			require.False(t, r.Header().Attested)

			tests := []struct {
				name     string
				q        Query
				yielded  bool
				level    ReadLevel
				mismatch bool
				hv       []int
			}{
				{name: "provisional header value", q: Query{Filter: v.header}, yielded: true, level: ReadHeaderOnly, hv: []int{0}},
				{name: "provisional payload value", q: Query{Filter: v.payload}, hv: []int{0}},
				{
					name: "authoritative payload value", q: Query{Filter: v.payload, Mode: QueryAuthoritative},
					yielded: true, level: ReadFull, mismatch: true,
				},
				{name: "authoritative header value", q: Query{Filter: v.header, Mode: QueryAuthoritative}},
				{
					name: "provisional header value with payloads", q: Query{Filter: v.header, Payloads: true},
					yielded: true, level: ReadFull, mismatch: true, hv: []int{0},
				},
				{name: "provisional payload value with payloads", q: Query{Filter: v.payload, Payloads: true}, hv: []int{0}},
				{
					name: "authoritative without copy predicate", q: Query{Mode: QueryAuthoritative, Filter: Filter{Kinds: []Kind{KindData}}},
					yielded: true, level: ReadHeaderOnly,
				},
			}
			for _, tt := range tests {
				run := iterate(t, r, tt.q)
				assert.Equal(t, tt.hv, run.res.HeaderValidated, tt.name)
				assert.True(t, run.res.Complete(), tt.name)
				if !tt.yielded {
					assert.Empty(t, run.items, tt.name)
					continue
				}

				require.Len(t, run.items, 1, tt.name)
				it := run.items[0]
				assert.Equal(t, storedAs(rec, tt.level), it.rec, "%s: the record as stored", tt.name)
				assert.Equal(t, tt.level, it.level, tt.name)
				assert.Equal(t, tt.mismatch, it.mismatch, tt.name)
				if tt.level == ReadFull {
					assert.Equal(t, testDataRecord(0, blockTestHour, 1), payloadCopies(it.rec), "%s: SetHeaderCopies gives the payload's values", tt.name)
				}
			}
		})
	}
}

func TestIterateQueryModesAttestedControl(t *testing.T) {
	t.Parallel()

	rec := testDataRecord(0, blockTestHour, 1)
	p := writeReaderPack(t, readerPackConfig{codec: CodecZstd, validate: true}, []Record{rec})
	r := mustOpen(t, p.file, ReaderOptions{})
	require.True(t, r.Header().Attested)

	for _, v := range copyVectors() {
		for _, mode := range []QueryMode{QueryProvisional, QueryAuthoritative} {
			// The record agrees with its payload, so the header value of the vector matches nothing.
			none := iterate(t, r, Query{Filter: v.header, Mode: mode})
			assert.Empty(t, none.items, "%s, %v", v.name, mode)
			assert.Equal(t, Result{}, none.res, "%s, %v", v.name, mode)

			run := iterate(t, r, Query{Filter: v.payload, Mode: mode})
			require.Len(t, run.items, 1, "%s, %v", v.name, mode)
			it := run.items[0]
			assert.Equal(t, storedAs(rec, ReadAttested), it.rec)
			assert.Equal(t, ReadAttested, it.level, "%s, %v: an attested block is read header-only", v.name, mode)
			assert.False(t, it.mismatch)
			assert.Equal(t, Result{}, run.res, "%s, %v: no header-validated for an attested block", v.name, mode)
		}
	}
}

func TestIterateQueryModes_NonconformingFalseAttestation(t *testing.T) {
	t.Parallel()

	v := copyVectors()[0]
	rec := v.disagreeingRecord()
	p := writeReaderPack(t, readerPackConfig{codec: CodecZstd, meta: notesMeta}, []Record{rec})
	r := mustOpen(t, falselyAttest(t, p.file), ReaderOptions{})
	require.True(t, r.Header().Attested, "the nonconforming writer asserts blocks_validated")

	// A header-only read of an attested block trusts its copies, so the false assertion goes undetected (§16).
	run := iterate(t, r, Query{Filter: v.header, Mode: QueryAuthoritative})
	require.Len(t, run.items, 1)
	assert.Equal(t, storedAs(rec, ReadAttested), run.items[0].rec)
	assert.Equal(t, ReadAttested, run.items[0].level)
	assert.False(t, run.items[0].mismatch)
	assert.Equal(t, Result{}, run.res)

	none := iterate(t, r, Query{Filter: v.payload, Mode: QueryAuthoritative})
	assert.Empty(t, none.items, "the payload's S1F3 is never consulted")
	assert.Equal(t, Result{}, none.res)

	// A full read decodes the payload and reports the disagreement.
	full := iterate(t, r, Query{Filter: v.header, Mode: QueryAuthoritative, Payloads: true})
	require.Len(t, full.items, 1)
	assert.Equal(t, ReadFull, full.items[0].level)
	assert.True(t, full.items[0].mismatch)
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
		require.Equal(t, validate, r.Header().Attested)

		for _, tt := range tests {
			for _, mode := range []QueryMode{QueryProvisional, QueryAuthoritative} {
				run := iterate(t, r, Query{Filter: tt.f, Mode: mode})
				assert.Equal(t, tt.want, nilIfEmpty(run.seqs()), "%s, %v, attested %v", tt.name, mode, validate)
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
	// so the field is unavailable in every mode, attested or not (the tracepack format specification §7.2).
	rec := clearedSystemBytes(0)
	payloadSB := [4]byte(blockTestFrame[copySystemBytesOff:copySystemBytesEnd])

	for _, validate := range []bool{false, true} {
		p := writeReaderPack(t, readerPackConfig{codec: CodecZstd, validate: validate}, []Record{rec})
		r := mustOpen(t, p.file, ReaderOptions{})
		require.Equal(t, validate, r.Header().Attested, "the validating Writer accepts a clear bit over present bytes")

		for _, mode := range []QueryMode{QueryProvisional, QueryAuthoritative} {
			for _, payloads := range []bool{false, true} {
				name := fmt.Sprintf("attested %v, %v, payloads %v", validate, mode, payloads)
				for _, sb := range [][4]byte{payloadSB, {}} {
					run := iterate(t, r, Query{Filter: Filter{SystemBytes: &sb}, Mode: mode, Payloads: payloads})
					assert.Empty(t, run.items, "%s: System Bytes % X", name, sb)
				}

				run := iterate(t, r, Query{Filter: Filter{SystemBytes: &payloadSB, IncludeUnavailable: true}, Mode: mode, Payloads: payloads})
				require.Len(t, run.items, 1, name)
				assert.False(t, run.items[0].mismatch, "%s: a clear bit over present bytes is no disagreement", name)

				sf := iterate(t, r, Query{Filter: Filter{SF: []SF{{Stream: 1, Function: 3}}}, Mode: mode, Payloads: payloads})
				assert.Len(t, sf.items, 1, "%s: the fields whose bits are set stay available", name)
			}
		}
	}
}

func TestIterateSetBitBeyondPayload(t *testing.T) {
	t.Parallel()

	// Every bit is set, but the payload ends before System Bytes: a writer defect (the tracepack format specification §7.2).
	rec := testDataRecord(0, blockTestHour, 1)
	rec.Payload = blockTestFrame[:copySTypeEnd]
	rec.DecodeStatus = DecodeStatusShortFrame

	_, err := buildReaderPack(readerPackConfig{codec: CodecZstd, validate: true}, []Record{rec})
	require.ErrorIs(t, err, ErrValidation, "a validating Writer rejects the record")

	p := writeReaderPack(t, readerPackConfig{codec: CodecZstd}, []Record{rec})
	r := mustOpen(t, p.file, ReaderOptions{})
	require.False(t, r.Header().Attested)

	f := Filter{SystemBytes: &rec.SystemBytes}
	tests := []struct {
		name     string
		q        Query
		yielded  bool
		level    ReadLevel
		mismatch bool
		hv       []int
	}{
		// A provisional query selects on the stored header copy, relying on it.
		{name: "provisional", q: Query{Filter: f}, yielded: true, level: ReadHeaderOnly, hv: []int{0}},
		{name: "provisional with payloads", q: Query{Filter: f, Payloads: true}, yielded: true, level: ReadFull, mismatch: true, hv: []int{0}},
		// An authoritative query finds the field unavailable, since the payload lacks its bytes.
		{name: "authoritative", q: Query{Filter: f, Mode: QueryAuthoritative}},
		{name: "authoritative with payloads", q: Query{Filter: f, Mode: QueryAuthoritative, Payloads: true}},
		{
			name: "authoritative with unavailable", q: Query{Filter: Filter{SystemBytes: &[4]byte{}, IncludeUnavailable: true}, Mode: QueryAuthoritative},
			yielded: true, level: ReadFull, mismatch: true,
		},
		{
			name: "authoritative on an available field", q: Query{Filter: Filter{SF: []SF{{Stream: 1, Function: 3}}}, Mode: QueryAuthoritative},
			yielded: true, level: ReadFull, mismatch: true,
		},
	}
	for _, tt := range tests {
		run := iterate(t, r, tt.q)
		assert.Equal(t, tt.hv, run.res.HeaderValidated, tt.name)
		if !tt.yielded {
			assert.Empty(t, run.items, tt.name)
			continue
		}

		require.Len(t, run.items, 1, tt.name)
		assert.Equal(t, tt.level, run.items[0].level, tt.name)
		assert.Equal(t, tt.mismatch, run.items[0].mismatch, tt.name)
	}
}

func TestIterateCopyMismatch(t *testing.T) {
	t.Parallel()

	base := func(seq uint64) Record { return testDataRecord(seq, blockTestHour+int64(seq), 1) }
	edits := []struct {
		name string
		edit func(r *Record)
	}{
		{name: "agreeing"},
		{name: "SessionID", edit: func(r *Record) { r.SessionID++ }},
		{name: "stream", edit: func(r *Record) { r.Stream++ }},
		{name: "W", edit: func(r *Record) { r.W = !r.W }},
		{name: "function", edit: func(r *Record) { r.Function++ }},
		{name: "PType", edit: func(r *Record) { r.PType++ }},
		{name: "SType", edit: func(r *Record) { r.SType++ }},
		{name: "SystemBytes", edit: func(r *Record) { r.SystemBytes[3]++ }},
		// The copies and field_validity of the whole frame, stored over a payload that lacks SType and System Bytes.
		{name: "short capture validity", edit: func(r *Record) { r.Payload = r.Payload[:9] }},
	}
	recs := make([]Record, 0, len(edits)+1)
	for i, e := range edits {
		r := base(uint64(i))
		if e.edit != nil {
			e.edit(&r)
		}
		recs = append(recs, r)
	}
	recs = append(recs, testEventRecord(t, uint64(len(edits)), blockTestHour+100, 1, &TransportEvent{Event: EventSocketClose}))

	p := writeReaderPack(t, readerPackConfig{codec: CodecZstd}, recs)
	r := mustOpen(t, p.file, ReaderOptions{})

	for _, mode := range []QueryMode{QueryProvisional, QueryAuthoritative} {
		run := iterate(t, r, Query{Mode: mode, Payloads: true})
		require.Len(t, run.items, len(recs))
		for k, it := range run.items {
			want := k > 0 && k < len(edits)
			assert.Equal(t, ReadFull, it.level)
			assert.Equal(t, want, it.mismatch, "record %d, %v", k, mode)
			if k < len(edits) {
				assert.Equal(t, recs[k], it.rec, "record %d is yielded as stored", k)
			}
		}

		hdr := iterate(t, r, Query{Mode: mode})
		require.Len(t, hdr.items, len(recs))
		for _, it := range hdr.items {
			assert.Equal(t, ReadHeaderOnly, it.level)
			assert.False(t, it.mismatch, "a header-only read never reports a mismatch")
		}
	}

	attested := writeReaderPack(t, readerPackConfig{codec: CodecZstd, validate: true}, []Record{base(0)})
	run := iterate(t, mustOpen(t, attested.file, ReaderOptions{}), Query{})
	require.Len(t, run.items, 1)
	assert.Equal(t, ReadAttested, run.items[0].level)
	assert.False(t, run.items[0].mismatch)
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
	assert.Equal(t, []int{0, 1}, res.HeaderValidated, "the result so far")
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
	assert.Equal(t, []int{0}, res.HeaderValidated)
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
		hv   []int
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
		{
			name: "pruning adds no header-validated", f: Filter{TimeTo: hour(1), SF: []SF{{Stream: 1, Function: 3}}},
			read: []int{0}, want: []uint64{0, 1, 2}, hv: []int{0},
		},
	}
	for _, tt := range tests {
		read, run := blocksRead(t, r, g, Query{Filter: tt.f})
		assert.Equal(t, tt.read, nilIfEmpty(read), "%s: blocks read", tt.name)
		assert.Equal(t, tt.want, nilIfEmpty(run.seqs()), tt.name)
		assert.Equal(t, tt.hv, run.res.HeaderValidated, tt.name)
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
	assert.Equal(t, all, run.res.HeaderValidated, "every block read was selected by its copies")
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

					level := ReadHeaderOnly
					if payloads {
						level = ReadFull
					}
					for _, it := range run.items {
						require.Less(t, it.rec.Seq, uint64(len(recs)))
						require.Equal(t, storedAs(recs[it.rec.Seq], level), it.rec, "cut at %d", n)
					}
					require.True(t, slices.IsSorted(run.seqs()))
				}
			}
		})
	}
}

// concurrencyQueries returns queries whose results differ over the attested coverage pack.
func concurrencyQueries() []Query {
	return []Query{
		{},
		{Payloads: true},
		{Mode: QueryAuthoritative, Filter: Filter{SF: []SF{{Stream: 1, Function: 3}}}},
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
			assert.Equal(t, serial, got[g], "goroutine %d, attested %v", g, validate)
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
	require.True(t, h.Attested)
	*h.Meta.BlocksValidated = false
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
		assert.Equal(t, before[i].res.HeaderValidated, after.res.HeaderValidated, "query %d", i)
	}
}

// fuzzIterateQueries returns the queries FuzzIterate runs over every input:
// with and without payloads, a copy-field predicate, and a time range that prunes, each in both modes.
func fuzzIterateQueries() []Query {
	base := []Query{
		{Payloads: true},
		{},
		{Filter: Filter{SF: []SF{{Stream: 1, Function: 3}}}},
		{Filter: Filter{
			TimeFrom: new(blockTestHour + hourNs), TimeTo: new(blockTestHour + 2*hourNs),
			SessionIDs: []uint16{0x1234}, IncludeUnavailable: true,
		}},
	}

	out := make([]Query, 0, 2*len(base))
	for _, mode := range []QueryMode{QueryProvisional, QueryAuthoritative} {
		for _, q := range base {
			q.Mode = mode
			out = append(out, q)
		}
	}

	return out
}

// fuzzRecordKey returns a key identifying the record of it byte for byte: its fields, HeaderExtra and, at ReadFull, its payload.
func fuzzRecordKey(it *Item) string {
	rec := it.Record
	rec.Payload = nil
	key := fmt.Sprintf("%+v|%x", rec, it.HeaderExtra)
	if it.Level == ReadFull {
		key += fmt.Sprintf("|%x", it.Record.Payload)
	}

	return key
}

// fuzzIterateSeeds returns the seed packs of FuzzIterate:
// indexed, attested, walked, copy disagreements with a short capture and an event, coverage, widened headers,
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

// FuzzIterate opens mutated packs and iterates them with fixed queries in both modes.
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

		attested := r.Header().Attested
		nblocks := len(r.Blocks())
		for _, q := range queries {
			var unknown []string
			res, err := r.Iterate(t.Context(), q, func(it *Item) error {
				require.GreaterOrEqual(t, it.Block, 0)
				require.Less(t, it.Block, nblocks)
				require.Equal(t, fuzzLevel(&q, attested), it.Level)
				require.True(t, !it.CopyMismatch || it.Level == ReadFull, "a mismatch needs a full read")
				require.Equal(t, it.Level == ReadFull, it.Record.Payload != nil, "a payload iff a full read")
				if key := fuzzRecordKey(it); !known[key] {
					unknown = append(unknown, key)
				}

				return nil
			})
			require.NoError(t, err)

			require.True(t, slices.IsSorted(res.HeaderValidated))
			require.Equal(t, len(res.HeaderValidated), len(slices.Compact(slices.Clone(res.HeaderValidated))))
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

// fuzzLevel returns the level Iterate reads q at over a pack that is attested or not.
func fuzzLevel(q *Query, attested bool) ReadLevel {
	switch {
	case q.Payloads || (q.Mode == QueryAuthoritative && q.Filter.hasCopyPredicate() && !attested):
		return ReadFull
	case attested:
		return ReadAttested
	default:
		return ReadHeaderOnly
	}
}
