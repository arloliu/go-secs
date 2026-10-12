package redact_test

import (
	"bytes"
	"errors"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/arloliu/go-secs/tracepack"
	"github.com/arloliu/go-secs/tracepack/internal/secsitem"
	"github.com/arloliu/go-secs/tracepack/redact"
)

// Flags of a fuzzed record.
const (
	// fuzzBadPType gives the frame PType 1, failing a classification predicate before the item predicate.
	fuzzBadPType = 1 << iota
	// fuzzBadLength makes the frame's length field disagree with its captured length.
	fuzzBadLength
	// fuzzControl makes the record a control record.
	fuzzControl
	// fuzzAnnotation makes the record an unparsed-entry annotation whose raw value is the text.
	fuzzAnnotation
	// fuzzAnnotationBytes makes the record an annotation whose payload is the text itself.
	fuzzAnnotationBytes
	// fuzzUnobserved makes the record unobserved by design.
	fuzzUnobserved
	// fuzzShort cuts the frame to fewer than 14 bytes.
	fuzzShort
)

// fuzzSF are the streams and functions of the fuzzed data records, selected by an index.
var fuzzSF = [][2]byte{{7, 3}, {13, 6}, {16, 11}, {1, 3}, {1, 1}, {1, 5}, {6, 11}}

// encryptedFuzzRules are item rules of a policy with encryption: no two of one S/F overlap.
var encryptedFuzzRules = []redact.ItemRule{
	{Stream: 7, Function: 3, Path: "2", Domain: "ppbody"},
	{Stream: 13, Function: 6, Path: "4/*", Domain: "dataset"},
	{Stream: 16, Function: 11, Path: "5/3/*/2", Domain: "rcpparval"},
	{Stream: 1, Function: 3, Path: "1/*/2", Domain: "a"},
	{Stream: 1, Function: 3, Path: "2", Domain: "b"},
	{Stream: 1, Function: 1, Path: "", Domain: "root"},
	{Stream: 1, Function: 5, Path: "*/1", Domain: "first"},
}

// zeroFuzzRules are item rules of a policy without encryption, several of one S/F overlapping.
var zeroFuzzRules = []redact.ItemRule{
	{Stream: 7, Function: 3, Path: "2", Domain: "ppbody"},
	{Stream: 7, Function: 3, Path: "", Domain: "root"},
	{Stream: 7, Function: 3, Path: "*", Domain: "any"},
	{Stream: 13, Function: 6, Path: "4/*", Domain: "dataset"},
	{Stream: 1, Function: 3, Path: "1", Domain: "a"},
	{Stream: 1, Function: 3, Path: "1/*", Domain: "b"},
	{Stream: 1, Function: 3, Path: "*/2", Domain: "c"},
	{Stream: 1, Function: 5, Path: "*/1", Domain: "first"},
}

// fuzzRecord builds the record a fuzz input describes.
func fuzzRecord(t *testing.T, flags, sfSel uint8, seq uint64, text []byte) *tracepack.Record {
	t.Helper()

	seq &= 1<<63 - 1
	switch {
	case flags&fuzzAnnotation != 0:
		return annotationRecord(t, seq, &tracepack.Annotation{AnnotationKind: tracepack.AnnotationKindUnparsedEntry, Raw: append([]byte{}, text...)})
	case flags&fuzzAnnotationBytes != 0:
		return &tracepack.Record{Seq: seq, Kind: tracepack.KindAnnotation, Payload: bytes.Clone(text)}
	}

	s := fuzzSF[int(sfSel)%len(fuzzSF)]
	f := frame(s[0], s[1], text)
	if flags&fuzzBadPType != 0 {
		f[8] = 1
	}
	if flags&fuzzBadLength != 0 {
		f[3]++
	}
	if flags&fuzzShort != 0 {
		f = f[:int(sfSel)%14]
	}
	r := dataRecord(seq, f)
	if flags&fuzzControl != 0 {
		r.Kind = tracepack.KindControl
	}
	if flags&fuzzUnobserved != 0 {
		r.Quality |= tracepack.QualityUnobserved
	}

	return r
}

// addFuzzSeeds adds the seeds both fuzz targets share.
func addFuzzSeeds(f *testing.F) {
	f.Add(uint8(0), uint8(0), uint64(1), ppbodyText("PP1", []byte("SECRET-RECIPE-01")))
	f.Add(uint8(0), uint8(1), uint64(2), datasetText([]byte{1, 2, 3}, []byte{4, 5, 6, 7}))
	f.Add(uint8(0), uint8(2), uint64(3), recipeText([]byte{0, 1, 'v', 'a', 'l'}, []byte{0, 1}))
	f.Add(uint8(0), uint8(3), uint64(4), list(list(ascii("A"), ascii("B")), ascii("C")))
	f.Add(uint8(0), uint8(4), uint64(5), list(ascii("ROOT"), list()))
	f.Add(uint8(0), uint8(5), uint64(6), list(list(ascii("x")), list(ascii("y"), ascii("z"))))
	f.Add(uint8(fuzzAnnotation), uint8(0), uint64(7), []byte("unparsed source text"))
	f.Add(uint8(fuzzAnnotationBytes), uint8(0), uint64(8), []byte{0, 1, 2})
	f.Add(uint8(0), uint8(0), uint64(9), []byte(nil))
	f.Add(uint8(fuzzControl), uint8(0), uint64(10), []byte(nil))
	f.Add(uint8(fuzzBadPType), uint8(0), uint64(11), ppbodyText("PP1", []byte("BODY")))
	f.Add(uint8(fuzzBadLength), uint8(0), uint64(12), ppbodyText("PP1", []byte("BODY")))
	f.Add(uint8(0), uint8(0), uint64(13), append(ppbodyText("PP1", []byte("BODY")), 0, 0))
	f.Add(uint8(fuzzUnobserved), uint8(0), uint64(14), make([]byte, 24))
	f.Add(uint8(fuzzShort), uint8(9), uint64(15), []byte(nil))
}

// checkLayout requires that masked keeps r's payload length and every byte outside the entries' ranges,
// and, as checkProtected finds them without the ranges, the frame prefix and the item headers of a resolved mask.
// It returns, for each payload byte, whether an entry's range covers it.
func checkLayout(t *testing.T, r *tracepack.Record, masked tracepack.Record, entries []tracepack.RedactionEntry) []bool {
	t.Helper()

	require.Len(t, masked.Payload, len(r.Payload))
	covered := make([]bool, len(r.Payload))
	resolved := false
	for _, e := range entries {
		resolved = resolved || e.ItemPath != nil
		for _, mr := range e.MaskedRanges {
			for k := mr.Offset; k < mr.Offset+mr.Length; k++ {
				covered[k] = true
			}
		}
	}
	for k := range r.Payload {
		if !covered[k] {
			require.Equal(t, r.Payload[k], masked.Payload[k], "byte %d outside the ranges", k)
		}
	}
	checkProtected(t, r, masked, resolved)

	return covered
}

// checkProtected requires, whatever ranges Mask returned, that masked keeps the bytes of r no mask may cover:
// a data record's 14-byte frame prefix, the length field and the HSMS header,
// and, when resolved reports an entry with an item path, every item header and localized-string header of the original text,
// found by headerSpans, a decoder of the test's own.
func checkProtected(t *testing.T, r *tracepack.Record, masked tracepack.Record, resolved bool) {
	t.Helper()

	if r.Kind != tracepack.KindData {
		return
	}
	n := min(len(r.Payload), 14)
	require.Equal(t, r.Payload[:n], masked.Payload[:n], "the frame prefix")
	if !resolved {
		return
	}

	spans, ok := headerSpans(r.Payload[14:])
	require.True(t, ok, "an entry with an item path in a text that is not one valid item")
	for _, s := range spans {
		require.Equal(t, r.Payload[14+s[0]:14+s[1]], masked.Payload[14+s[0]:14+s[1]], "the header at text offset %d", s[0])
	}
}

// elementWidths holds the element size of every format code of SEMI E5 §9.2 but the list.
var elementWidths = map[byte]int{
	0o10: 1, 0o11: 1, 0o20: 1, 0o21: 1, 0o22: 1,
	0o30: 8, 0o31: 1, 0o32: 2, 0o34: 4, 0o40: 8, 0o44: 4,
	0o50: 8, 0o51: 1, 0o52: 2, 0o54: 4,
}

// headerSpans decodes text as one SECS-II item (SEMI E5 §9) and returns the [start, end) spans, in text,
// of every item header (the format byte and the length bytes) and of every non-empty localized string's 2-byte header.
//
// Returns:
//   - [][2]int: the spans, in text order.
//   - bool: false when text is not exactly one valid item.
func headerSpans(text []byte) ([][2]int, bool) {
	var spans [][2]int
	end, ok := walkHeaders(text, 0, 0, &spans)

	return spans, ok && end == len(text)
}

// walkHeaders decodes the item at off of b, inside depth enclosing lists, appends its header spans to spans,
// and returns its end offset; false when the item is not valid or nests deeper than 64 lists.
func walkHeaders(b []byte, off, depth int, spans *[][2]int) (int, bool) {
	if off >= len(b) {
		return 0, false
	}
	code, n := b[off]>>2, int(b[off]&0x03)
	if n == 0 || off+1+n > len(b) {
		return 0, false
	}
	length := 0
	for _, c := range b[off+1 : off+1+n] {
		length = length<<8 | int(c)
	}
	body := off + 1 + n
	*spans = append(*spans, [2]int{off, body})

	if code == 0 {
		if depth >= 64 {
			return 0, false
		}
		for range length {
			end, ok := walkHeaders(b, body, depth+1, spans)
			if !ok {
				return 0, false
			}
			body = end
		}

		return body, true
	}

	width, known := elementWidths[code]
	if !known || length%width != 0 || length > len(b)-body || code == 0o22 && length == 1 {
		return 0, false
	}
	if code == 0o22 && length >= 2 {
		*spans = append(*spans, [2]int{body, body + 2})
	}

	return body + length, true
}

// FuzzMaskRestore masks fuzzed records under a policy with encryption:
// masking keeps the payload length and every byte outside the masked ranges, item headers included,
// a record nothing matched is returned unchanged,
// and every masked data record restores its original, while an annotation keeps no original.
func FuzzMaskRestore(f *testing.F) {
	addFuzzSeeds(f)
	p := baselinePolicy(true)
	p.ItemRules = encryptedFuzzRules
	c := mustCompile(f, p)
	pol := c.PackPolicy()
	key := encryptionKey(captureA, encKeyID)

	f.Fuzz(func(t *testing.T, flags, sfSel uint8, seq uint64, text []byte) {
		r := fuzzRecord(t, flags, sfSel, seq, text)
		orig := bytes.Clone(r.Payload)
		masked, entries, err := c.Mask(captureA, r, r.Quality.Has(tracepack.QualityUnobserved), &testKeys{})
		if err != nil {
			require.ErrorIs(t, err, redact.ErrCannotScreen)
			return
		}
		require.Equal(t, orig, r.Payload)
		if len(entries) == 0 {
			require.Equal(t, *r, masked)
			return
		}
		require.NotEqual(t, tracepack.KindControl, r.Kind, "a control record is never masked")
		require.False(t, r.Quality.Has(tracepack.QualityUnobserved), "an unobserved record is never masked")
		checkLayout(t, r, masked, entries)

		rr := &tracepack.RecordRedaction{Entries: entries}
		got, err := redact.Restore(captureA, &masked, rr, &pol, key)
		if r.Kind == tracepack.KindAnnotation {
			require.ErrorIs(t, err, redact.ErrNotRestorable)
			return
		}
		require.NoError(t, err)
		require.Equal(t, orig, got)
	})
}

// FuzzMaskZero masks fuzzed records under a policy without encryption, whose rules overlap,
// and annotations under a policy with encryption too:
// every target byte is zero afterwards and every other byte unchanged,
// and each entry's digest is that of the original bytes the tracepack semantics specification §8 Digest names,
// computed before any mask of the record changed them.
func FuzzMaskZero(f *testing.F) {
	addFuzzSeeds(f)
	p := baselinePolicy(false)
	p.ItemRules = zeroFuzzRules
	p.AnnotationRules = append(p.AnnotationRules, redact.AnnotationRule{Kind: tracepack.AnnotationKindUnparsedEntry, Domain: "again"})
	zero := mustCompile(f, p)
	encrypted := mustCompile(f, baselinePolicy(true))

	f.Fuzz(func(t *testing.T, flags, sfSel uint8, seq uint64, text []byte) {
		r := fuzzRecord(t, flags, sfSel, seq, text)
		for _, c := range []*redact.Compiled{zero, encrypted} {
			if c == encrypted && r.Kind != tracepack.KindAnnotation {
				continue
			}
			masked, entries, err := c.Mask(captureA, r, r.Quality.Has(tracepack.QualityUnobserved), &testKeys{})
			if err != nil {
				require.True(t, errors.Is(err, redact.ErrCannotScreen), "%v", err)
				continue
			}
			covered := checkLayout(t, r, masked, entries)
			for k, cov := range covered {
				if cov {
					require.Zero(t, masked.Payload[k], "target byte %d", k)
				}
			}
			for i, e := range entries {
				assert.False(t, e.HasSIV, "entry %d", i)
				assert.Equal(t, digestOf(e.Domain, originalOf(t, r, &e)), e.Digest, "entry %d", i)
			}
		}
	})
}

// originalOf returns the original bytes the digest of e covers in r:
// the encoded item at its path for a resolved path, the whole message text for a whole-text mask,
// and the masked value for an annotation.
func originalOf(t *testing.T, r *tracepack.Record, e *tracepack.RedactionEntry) []byte {
	t.Helper()

	if r.Kind == tracepack.KindAnnotation {
		require.Len(t, e.MaskedRanges, 1)
		mr := e.MaskedRanges[0]

		return r.Payload[mr.Offset : mr.Offset+mr.Length]
	}
	if e.ItemPath == nil {
		require.Equal(t, redact.DomainMessageText, e.Domain)
		return r.Payload[14:]
	}

	text, err := secsitem.Screen(r.Payload[14:])
	require.NoError(t, err)
	p, err := secsitem.ParsePattern(*e.ItemPath)
	require.NoError(t, err)
	sels, ok := text.Select(p)
	require.True(t, ok)
	require.Len(t, sels, 1)

	return r.Payload[14+sels[0].Item.Start : 14+sels[0].Item.End]
}
