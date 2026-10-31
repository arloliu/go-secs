package tracepack

import (
	"context"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// scribbledItem is an Item copied out of a callback, with its Marking.
type scribbledItem struct {
	item      iterItem
	redaction *RecordRedaction
}

// scribbleItem overwrites every field of it, as a callback that reuses the Item it receives may.
func scribbleItem(it *Item) {
	*it = Item{
		Record:      Record{Seq: 1 << 62, TSUTCNs: -1, MonoPresent: true, Kind: 0xff, Payload: []byte{0xee}},
		HeaderExtra: []byte{0xdd},
		Input:       99,
		Block:       -7,
		Conflict:    true,
		Redaction:   &RecordRedaction{WhollyMasked: true},
	}
}

// collectScribbled runs read with a callback that copies each Item, then, when overwrite is set, scribbles over it.
func collectScribbled(t *testing.T, overwrite bool, read func(fn func(*Item) error) (Result, error)) ([]scribbledItem, Result) {
	t.Helper()

	var got []scribbledItem
	res, err := read(func(it *Item) error {
		got = append(got, scribbledItem{item: copyItem(it), redaction: it.Redaction})
		if overwrite {
			scribbleItem(it)
		}

		return nil
	})
	require.NoError(t, err)

	return got, res
}

// A callback that overwrites every field of the Item it receives changes nothing a read yields after it:
// each record's Item is set in full, its pack, block, conflict, header extension and Marking included.
func TestReadsUnaffectedByItemScribble(t *testing.T) {
	t.Parallel()

	widePack := writeReaderPack(t, readerPackConfig{codec: CodecZstd}, hourRecords(2, 3))
	wide := rebuildPack(t, widePack.file, func(i int, b *testBlock) {
		if i == 1 {
			widen(b)
		}
	})
	masked := rewriteMeta(t, redactionMasked(t), func(m *PackMeta) { m.Redaction[1].MaskedRanges = []MaskedRange{{16, 5}} })
	conflicting := [][]byte{
		planTestPack(t, seg0, captureLow, false, blockAt(100, 5, 6, 7)),
		planTestPack(t, seg1, captureLow, false, changed(blockAt(0, 6), 1, 6)),
		planTestPack(t, seg2, captureLow, false, changed(blockAt(50, 6), 2, 6)),
	}
	queries := []Query{
		{Payloads: true},
		{},
		{Filter: Filter{SF: []SF{{Stream: 7, Function: 3}}}, Payloads: true},
	}

	type readCase struct {
		name string
		read func(q Query) func(fn func(*Item) error) (Result, error)
	}
	iterateOf := func(file []byte) func(q Query) func(fn func(*Item) error) (Result, error) {
		r := mustOpen(t, file, ReaderOptions{})
		return func(q Query) func(fn func(*Item) error) (Result, error) {
			return func(fn func(*Item) error) (Result, error) { return r.Iterate(context.Background(), q, fn) }
		}
	}
	mergeOf := func(order Order, files ...[]byte) func(q Query) func(fn func(*Item) error) (Result, error) {
		readers := make([]*Reader, len(files))
		for i, f := range files {
			readers[i] = mustOpen(t, f, ReaderOptions{})
		}
		return func(q Query) func(fn func(*Item) error) (Result, error) {
			return func(fn func(*Item) error) (Result, error) {
				return MergeIterate(context.Background(), readers, q, MergeIterateOptions{Order: order}, fn)
			}
		}
	}
	cases := []readCase{
		{"iterate wide headers", iterateOf(wide)},
		{"iterate masked", iterateOf(masked)},
		{"merge masked capture order", mergeOf(OrderCapture, masked)},
		{"merge wide time order", mergeOf(OrderTime, wide)},
		{"merge conflicts capture order", mergeOf(OrderCapture, conflicting...)},
		{"merge conflicts time order", mergeOf(OrderTime, conflicting...)},
	}
	for _, tc := range cases {
		for qi, q := range queries {
			want, wantRes := collectScribbled(t, false, tc.read(q))
			got, gotRes := collectScribbled(t, true, tc.read(q))
			assert.Equal(t, want, got, "%s, query %d", tc.name, qi)
			assert.Equal(t, wantRes, gotRes, "%s, query %d", tc.name, qi)
		}
	}

	// The fixtures exercise every field the scribble overwrites.
	var extra, redacted, conflict, otherPack bool
	for _, tc := range cases {
		items, _ := collectScribbled(t, false, tc.read(Query{Payloads: true}))
		for _, it := range items {
			extra = extra || it.item.extra != nil
			redacted = redacted || it.redaction != nil
			conflict = conflict || it.item.conflict
			otherPack = otherPack || it.item.pack != 0
		}
	}
	assert.True(t, extra && redacted && conflict && otherPack, "extra %v, redacted %v, conflict %v, other pack %v", extra, redacted, conflict, otherPack)
}
