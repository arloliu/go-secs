package secsitem_test

import (
	"bytes"
	"encoding/binary"
	"os"
	"path/filepath"
	"slices"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/arloliu/go-secs/tracepack"
	"github.com/arloliu/go-secs/tracepack/classify"
	"github.com/arloliu/go-secs/tracepack/internal/secsitem"
)

// corpusDir is the root of the conformance corpus, from this package's directory.
const corpusDir = "../../testdata/corpus"

// crossCheckVectors are the classifier vectors whose data frames the walker is checked against.
var crossCheckVectors = []string{"sem-item-validity", "sem-decode-status-classified"}

// crossCheckSize is the number of data frames of crossCheckVectors with a non-empty message text
// whose classification reaches the item predicate.
const crossCheckSize = 53

// item returns a SECS-II item of the format code code with one length byte and the body body.
func item(code byte, body ...byte) []byte {
	return append([]byte{code<<2 | 1, byte(len(body))}, body...)
}

// list returns the header of a SECS-II list of n elements with one length byte.
func list(n byte) []byte {
	return []byte{1, n}
}

// frame returns an HSMS data frame of SessionID 1, the given stream, function 3, and the given PType and SType,
// whose message text is text, with a length field that agrees with the frame.
func frame(stream, ptype, stype byte, text []byte) []byte {
	f := make([]byte, 14, 14+len(text))
	binary.BigEndian.PutUint32(f, uint32(10+len(text)))
	f[4], f[5] = 0, 1
	f[6], f[7], f[8], f[9] = stream, 3, ptype, stype
	f = append(f, text...)

	return f
}

// nested returns depth lists of one element each, the innermost holding a U1 item.
func nested(depth int) []byte {
	var b []byte
	for range depth {
		b = append(b, list(1)...)
	}

	return append(b, item(0o51, 7)...)
}

// reachesItemPredicate reports whether a classification is one of the statuses of the item predicate and after it.
func reachesItemPredicate(s tracepack.DecodeStatus) bool {
	return s == tracepack.DecodeStatusOK || s == tracepack.DecodeStatusOKWithTrailing || s == tracepack.DecodeStatusItemDecodeError
}

// classifiedFrames returns the payloads of the classified data records of the vector id's committed pack,
// with the pack's max_frame_len; a record storing not-attempted is a case of the export only, not of the classifier.
func classifiedFrames(t *testing.T, id string) ([][]byte, uint64) {
	t.Helper()

	pack, err := os.ReadFile(filepath.Join(corpusDir, id, "pack.tpk"))
	require.NoError(t, err)
	r, err := tracepack.Open(t.Context(), bytes.NewReader(pack), int64(len(pack)), tracepack.ReaderOptions{})
	require.NoError(t, err, id)
	require.Len(t, r.Header().Meta.MaxFrameLens, 1, id)

	var out [][]byte
	_, err = r.Iterate(t.Context(), tracepack.Query{Payloads: true}, func(it *tracepack.Item) error {
		if it.Record.Kind == tracepack.KindData && it.Record.DecodeStatus != tracepack.DecodeStatusNotAttempted {
			out = append(out, bytes.Clone(it.Record.Payload))
		}

		return nil
	})
	require.NoError(t, err, id)

	return out, r.Header().Meta.MaxFrameLens[0]
}

// TestWalkAgainstClassify checks the walker's "one valid item" against the classifier of the module
// over the data frames of the classifier vectors with a non-empty message text whose classification reaches the item predicate:
// the text is one valid item exactly when the classifier says ok.
// Outside that set it pins the walker's own rule:
// an empty text holds no item, a frame failing an earlier predicate is screened by its captured text,
// and a list nested 64 deep is walked while one nested 65 deep cannot be screened.
func TestWalkAgainstClassify(t *testing.T) {
	t.Parallel()

	t.Run("corpus", func(t *testing.T) {
		t.Parallel()

		checked := 0
		for _, id := range crossCheckVectors {
			frames, maxFrameLen := classifiedFrames(t, id)
			for _, f := range frames {
				if len(f) <= 14 {
					continue
				}
				status, _ := classify.Frame(f, int(maxFrameLen))
				if !reachesItemPredicate(status) {
					continue
				}
				checked++
				text, err := secsitem.Screen(f[14:])
				require.NoError(t, err, "%s: % x", id, f)
				assert.Equal(t, status == tracepack.DecodeStatusOK, text.OneItem(), "%s: %s, text % x", id, status, f[14:])
			}
		}
		assert.Equal(t, crossCheckSize, checked, "the frames of the cross-check")
	})

	t.Run("empty text", func(t *testing.T) {
		t.Parallel()

		text, err := secsitem.Screen(nil)
		require.NoError(t, err)
		assert.False(t, text.OneItem())
		assert.False(t, text.HasContent())
	})

	// Data-kind frames failing a predicate before the item predicate, each with a non-empty message text:
	// the walker screens the captured text as it screens the same text in a well-formed frame.
	earlier := []struct {
		name  string
		frame func(text []byte) []byte
		want  tracepack.DecodeStatus
	}{
		{"length-mismatch", func(text []byte) []byte {
			f := frame(7, 0, 0, text)
			f[3]++

			return f
		}, tracepack.DecodeStatusLengthMismatch},
		{"bad-ptype", func(text []byte) []byte { return frame(7, 1, 0, text) }, tracepack.DecodeStatusBadPType},
		{"bad-stype", func(text []byte) []byte { return frame(7, 0, 8, text) }, tracepack.DecodeStatusBadSType},
		{"control-with-body", func(text []byte) []byte { return frame(7, 0, 5, text) }, tracepack.DecodeStatusControlWithBody},
		{"oversized", func(text []byte) []byte { return frame(7, 0, 0, text) }, tracepack.DecodeStatusOversized},
	}
	texts := [][]byte{
		slices.Concat(list(2), item(0o20, 'P', '1'), item(0o10, 1, 2, 3)),
		slices.Concat(item(0o51, 5), []byte{0xFF}),
		item(0o52, 1, 2, 3),
	}
	for _, tt := range earlier {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()

			for _, text := range texts {
				f := tt.frame(text)
				status, _ := classify.Frame(f, 16)
				require.Equal(t, tt.want.String(), status.String(), "% x", f)

				wellFormed, _ := classify.Frame(frame(7, 0, 0, text), 0)
				got, err := secsitem.Screen(f[14:])
				require.NoError(t, err)
				assert.Equal(t, wellFormed == tracepack.DecodeStatusOK, got.OneItem(), "text % x", text)
			}
		})
	}

	t.Run("depth", func(t *testing.T) {
		t.Parallel()

		text, err := secsitem.Screen(nested(secsitem.MaxDepth))
		require.NoError(t, err)
		assert.True(t, text.OneItem())
		status, _ := classify.Frame(frame(1, 0, 0, nested(secsitem.MaxDepth)), 0)
		assert.Equal(t, tracepack.DecodeStatusOK.String(), status.String())

		_, err = secsitem.Screen(nested(secsitem.MaxDepth + 1))
		require.ErrorIs(t, err, secsitem.ErrDepth)
		status, _ = classify.Frame(frame(1, 0, 0, nested(secsitem.MaxDepth+1)), 0)
		assert.Equal(t, tracepack.DecodeStatusItemDecodeError.String(), status.String())

		// A list nested too deep after a malformed item: the text is known not to be one valid item.
		_, err = secsitem.Screen(slices.Concat(list(2), item(0o52, 1), nested(secsitem.MaxDepth+1)))
		require.NoError(t, err)
	})
}

// TestScreen pins the walker's validity and content rules item by item.
func TestScreen(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name    string
		text    []byte
		one     bool
		content bool
	}{
		{"empty list", list(0), true, false},
		{"zero-length binary", item(0o10), true, false},
		{"list of empty leaves", slices.Concat(list(2), item(0o20), item(0o51)), true, false},
		{"format-22 of 2 bytes", item(0o22, 0, 1), true, false},
		{"format-22 of 3 bytes", item(0o22, 0, 1, 'A'), true, true},
		{"format-22 of 1 byte", item(0o22, 0), false, false},
		{"ascii", item(0o20, 'A'), true, true},
		{"trailing", slices.Concat(item(0o20, 'A'), []byte{0}), false, true},
		{"three length bytes", []byte{0o20<<2 | 3, 0, 0, 1, 'A'}, true, true},
		{"zero length bytes", []byte{0o20 << 2}, false, false},
		{"undefined format", item(0o77), false, false},
		{"u4 width", item(0o54, 1, 2, 3), false, false},
		{"missing element", slices.Concat(list(2), item(0o51, 1)), false, true},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()

			text, err := secsitem.Screen(tt.text)
			require.NoError(t, err)
			assert.Equal(t, tt.one, text.OneItem())
			if tt.one {
				assert.Equal(t, tt.content, text.HasContent())
			}
		})
	}
}

// TestSelect resolves patterns over one valid item, with the concrete paths, item bounds and leaf content of each selection.
func TestSelect(t *testing.T) {
	t.Parallel()

	// <L[3] <A "ID"> <L[2] <U1 5> <A "">> <L[2] <L[2] <A "N"> <B 1 2>> <L[2] <A "M"> <W22 0 1 'x'>>>>
	text := slices.Concat(
		list(3),
		item(0o20, 'I', 'D'),
		list(2), item(0o51, 5), item(0o20),
		list(2),
		list(2), item(0o20, 'N'), item(0o10, 1, 2),
		list(2), item(0o20, 'M'), item(0o22, 0, 1, 'x'),
	)
	scr, err := secsitem.Screen(text)
	require.NoError(t, err)
	require.True(t, scr.OneItem())

	type sel struct {
		path    string
		item    secsitem.Range
		content []secsitem.Range
	}
	tests := []struct {
		pattern  string
		resolves bool
		want     []sel
	}{
		{"", true, []sel{{"", secsitem.Range{Start: 0, End: len(text)}, []secsitem.Range{
			{Start: 4, End: 6}, {Start: 10, End: 11}, {Start: 19, End: 20}, {Start: 22, End: 24}, {Start: 28, End: 29}, {Start: 33, End: 34},
		}}}},
		{"1", true, []sel{{"1", secsitem.Range{Start: 2, End: 6}, []secsitem.Range{{Start: 4, End: 6}}}}},
		{"2/2", true, []sel{{"2/2", secsitem.Range{Start: 11, End: 13}, nil}}},
		{"3/*/2", true, []sel{
			{"3/1/2", secsitem.Range{Start: 20, End: 24}, []secsitem.Range{{Start: 22, End: 24}}},
			{"3/2/2", secsitem.Range{Start: 29, End: 34}, []secsitem.Range{{Start: 33, End: 34}}},
		}},
		{"2/*", true, []sel{
			{"2/1", secsitem.Range{Start: 8, End: 11}, []secsitem.Range{{Start: 10, End: 11}}},
			{"2/2", secsitem.Range{Start: 11, End: 13}, nil},
		}},
		{"4", false, nil},
		{"1/1", false, nil},
		{"*/1", false, nil},
		{"3/*/3", false, nil},
	}
	for _, tt := range tests {
		t.Run(tt.pattern, func(t *testing.T) {
			t.Parallel()

			p, err := secsitem.ParsePattern(tt.pattern)
			require.NoError(t, err)
			got, ok := scr.Select(p)
			require.Equal(t, tt.resolves, ok)
			if !ok {
				return
			}
			require.Len(t, got, len(tt.want))
			for i, w := range tt.want {
				assert.Equal(t, w.path, got[i].Path)
				assert.Equal(t, w.item, got[i].Item)
				assert.Equal(t, w.content, scr.Content(got[i].Item, nil), w.path)
			}
		})
	}

	empty, err := secsitem.Screen(slices.Concat(list(2), item(0o20, 'A'), list(0)))
	require.NoError(t, err)
	p, err := secsitem.ParsePattern("2/*")
	require.NoError(t, err)
	got, ok := empty.Select(p)
	assert.True(t, ok, "a * over an empty list resolves")
	assert.Empty(t, got)
}

// TestParsePattern accepts the empty root pattern, numbered and * steps, and refuses every other step.
func TestParsePattern(t *testing.T) {
	t.Parallel()

	for _, s := range []string{"", "1", "*", "4/*/2", "2/*/4/3/*/2", "16777215"} {
		p, err := secsitem.ParsePattern(s)
		require.NoError(t, err, s)
		assert.Equal(t, s, p.String())
	}
	for _, s := range []string{"/", "0", "01", "1/", "/1", "1//2", "a", "-1", "+1", "**", "16777216", " 1"} {
		_, err := secsitem.ParsePattern(s)
		require.ErrorIs(t, err, secsitem.ErrPattern, "%q", s)
	}
}

// TestOverlaps decides overlap by the path patterns alone:
// identical, prefix and * against a numbered step overlap; a differing numbered step does not.
func TestOverlaps(t *testing.T) {
	t.Parallel()

	tests := []struct {
		a, b string
		want bool
	}{
		{"2", "2", true},
		{"", "4/*", true},
		{"4", "4/*/2", true},
		{"4/*", "4/3", true},
		{"*/2", "3/*", true},
		{"4/*/2", "4/*/1", false},
		{"2", "3", false},
		{"2/1", "3/*", false},
	}
	for _, tt := range tests {
		a, err := secsitem.ParsePattern(tt.a)
		require.NoError(t, err)
		b, err := secsitem.ParsePattern(tt.b)
		require.NoError(t, err)
		assert.Equal(t, tt.want, a.Overlaps(b), "%q %q", tt.a, tt.b)
		assert.Equal(t, tt.want, b.Overlaps(a), "%q %q", tt.b, tt.a)
	}
}
