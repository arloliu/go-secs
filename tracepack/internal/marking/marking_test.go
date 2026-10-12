package marking

import (
	"go/build"
	"path/filepath"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/arloliu/go-secs/tracepack/internal/tlv"
)

// modulePath is the import path of the module's root package.
const modulePath = "github.com/arloliu/go-secs/tracepack"

// frame returns a data record payload of n bytes: a 14-byte frame prefix and a message text of n - 14 bytes.
func frame(n int) []byte {
	return make([]byte, n)
}

// annotation returns an annotation payload holding annotation_kind, then a text entry of text,
// and the offset of the text value in it.
func annotation(text string) ([]byte, uint64) {
	b := tlv.AppendEntry(nil, tlv.U8Entry(0x0001, 1))
	off := uint64(len(b) + tlv.HeaderLen)

	return tlv.AppendEntry(b, tlv.UTF8Entry(tagText, text)), off
}

func ranges(pairs ...uint64) []Range {
	out := make([]Range, 0, len(pairs)/2)
	for i := 0; i+1 < len(pairs); i += 2 {
		out = append(out, Range{Offset: pairs[i], Length: pairs[i+1]})
	}

	return out
}

// Every check of the package is reached through plain values alone.
func TestMarkingPure(t *testing.T) {
	t.Parallel()

	t.Run("entry on its own", func(t *testing.T) {
		t.Parallel()

		tests := []struct {
			name  string
			entry Entry
			want  []error
		}{
			{"valid", Entry{Ranges: ranges(14, 2, 20, 4)}, nil},
			{"valid with a 16-byte siv", Entry{Ranges: ranges(14, 2), HasSIV: true, SIVLen: SIVLen}, nil},
			{"malformed value", Entry{Malformed: true}, []error{ErrRangesMalformed}},
			{"empty array", Entry{}, []error{ErrRangesEmpty}},
			{"element above 2^63 - 1", Entry{Ranges: ranges(1<<63, 1)}, []error{ErrRangesMalformed}},
			{"zero length", Entry{Ranges: ranges(14, 0)}, []error{ErrRangeLength}},
			{"unsorted", Entry{Ranges: ranges(20, 2, 14, 2)}, []error{ErrRangeOrder}},
			{"overlapping", Entry{Ranges: ranges(14, 4, 16, 4)}, []error{ErrRangeOrder}},
			{"adjacent", Entry{Ranges: ranges(14, 2, 16, 2)}, []error{ErrRangeOrder}},
			{"siv of 0 bytes", Entry{Ranges: ranges(14, 2), HasSIV: true}, []error{ErrSIVLength}},
			{"siv of 15 bytes", Entry{Ranges: ranges(14, 2), HasSIV: true, SIVLen: 15}, []error{ErrSIVLength}},
			{"siv of 17 bytes", Entry{Ranges: ranges(14, 2), HasSIV: true, SIVLen: 17}, []error{ErrSIVLength}},
			{"empty array and an ill-sized siv", Entry{HasSIV: true, SIVLen: 1}, []error{ErrRangesEmpty, ErrSIVLength}},
		}
		for _, tt := range tests {
			t.Run(tt.name, func(t *testing.T) {
				t.Parallel()

				got := CheckEntry(&tt.entry)
				require.Len(t, got, len(tt.want))
				for i, want := range tt.want {
					assert.ErrorIs(t, got[i], want)
				}
			})
		}
	})

	t.Run("entry order", func(t *testing.T) {
		t.Parallel()

		a := Entry{Seq: 1, Ranges: ranges(20, 2)}
		assert.NoError(t, CheckOrder(&a, &Entry{Seq: 2, Ranges: ranges(14, 2)}))
		assert.NoError(t, CheckOrder(&a, &Entry{Seq: 1, Ranges: ranges(20, 4)}), "an equal first offset keeps the order")
		assert.NoError(t, CheckOrder(&a, &Entry{Seq: 1, Ranges: ranges(30, 4)}))
		require.ErrorIs(t, CheckOrder(&a, &Entry{Seq: 0, Ranges: ranges(30, 4)}), ErrEntryOrder)
		require.ErrorIs(t, CheckOrder(&a, &Entry{Seq: 1, Ranges: ranges(14, 4)}), ErrEntryOrder)
		assert.NoError(t, CheckOrder(&a, &Entry{Seq: 1, Malformed: true}), "without a first offset only the seqs are compared")
		require.ErrorIs(t, CheckOrder(&a, &Entry{Seq: 0}), ErrEntryOrder)
	})

	t.Run("redaction-present", func(t *testing.T) {
		t.Parallel()

		assert.NoError(t, CheckPresence(false, 0))
		assert.NoError(t, CheckPresence(true, 2))
		require.ErrorIs(t, CheckPresence(true, 0), ErrPresence)
		require.ErrorIs(t, CheckPresence(false, 1), ErrPresence)
	})

	t.Run("record", func(t *testing.T) {
		t.Parallel()

		ann, textOff := annotation("secret")
		siv := func(e Entry) Entry { e.HasSIV, e.SIVLen = true, SIVLen; return e }
		data := func(n int) Record { return Record{Seq: 7, Kind: KindData, Redacted: true, Payload: frame(n)} }

		tests := []struct {
			name      string
			rec       Record
			entries   []Entry
			encrypted bool
			want      []Defect
		}{
			{"valid data mask", data(30), []Entry{{Seq: 7, Ranges: ranges(14, 16)}}, false, nil},
			{"valid encrypted mask", data(30), []Entry{siv(Entry{Seq: 7, Ranges: ranges(14, 16)})}, true, nil},
			{"valid annotation mask", Record{Seq: 7, Kind: KindAnnotation, Redacted: true, Payload: ann},
				[]Entry{{Seq: 7, Ranges: ranges(textOff, 6)}}, true, nil},
			{"redacted without entry", data(30), nil, false, []Defect{{Entry: -1, Err: ErrRedactedBit}}},
			{"masked record of a kind never masked", Record{Seq: 7, Kind: KindOther, Redacted: true, Payload: frame(30)},
				[]Entry{{Seq: 7, Ranges: ranges(14, 16)}}, false, []Defect{{Entry: -1, Err: ErrKindNotMaskable}}},
			{"masked record of a kind never masked, under encryption", Record{Seq: 7, Kind: KindOther, Redacted: true, Payload: frame(30)},
				[]Entry{{Seq: 7, Ranges: ranges(0, 40)}}, true, []Defect{{Entry: -1, Err: ErrKindNotMaskable}}},
			{"redacted record of a kind never masked, without entry", Record{Seq: 7, Kind: KindOther, Redacted: true, Payload: frame(14)},
				nil, false, []Defect{{Entry: -1, Err: ErrRedactedBit}, {Entry: -1, Err: ErrKindNotMaskable}}},
			{"named record of a kind never masked, without redacted", Record{Seq: 7, Kind: KindOther, Payload: frame(30)},
				[]Entry{{Seq: 7, Ranges: ranges(14, 2)}}, false, []Defect{{Entry: -1, Err: ErrRedactedBit}, {Entry: -1, Err: ErrKindNotMaskable}}},
			{"entry naming a record without redacted", Record{Seq: 7, Kind: KindData, Payload: frame(30)},
				[]Entry{{Seq: 7, Ranges: ranges(14, 2)}}, false, []Defect{{Entry: -1, Err: ErrRedactedBit}}},
			{"entry naming another seq", data(30), []Entry{{Seq: 8, Ranges: ranges(14, 2)}}, false,
				[]Defect{{Entry: 0, Err: ErrSeq}}},
			{"range past payload_len", data(30), []Entry{{Seq: 7, Ranges: ranges(20, 11)}}, false,
				[]Defect{{Entry: 0, Err: ErrRangeOutside}}},
			{"offset past payload_len", data(30), []Entry{{Seq: 7, Ranges: ranges(31, 1)}}, false,
				[]Defect{{Entry: 0, Err: ErrRangeOutside}}},
			{"range inside the frame prefix", data(30), []Entry{{Seq: 7, Ranges: ranges(13, 2)}}, false,
				[]Defect{{Entry: 0, Err: ErrRangePrefix}}},
			{"annotation range outside its value", Record{Seq: 7, Kind: KindAnnotation, Redacted: true, Payload: ann},
				[]Entry{{Seq: 7, Ranges: ranges(textOff-1, 2)}}, false, []Defect{{Entry: 0, Err: ErrRangeValue}}},
			{"annotation whose entries do not decode", Record{Seq: 7, Kind: KindAnnotation, Redacted: true, Payload: ann[:len(ann)-1]},
				[]Entry{{Seq: 7, Ranges: ranges(textOff, 2)}}, false, []Defect{{Entry: 0, Err: ErrRangeValue}}},
			{"data entry without siv under encryption", data(30), []Entry{{Seq: 7, Ranges: ranges(14, 2)}}, true,
				[]Defect{{Entry: 0, Err: ErrSIVMissing}}},
			{"data entry without siv, no encryption", data(30), []Entry{{Seq: 7, Ranges: ranges(14, 2)}}, false, nil},
			{"annotation entry with siv under encryption", Record{Seq: 7, Kind: KindAnnotation, Redacted: true, Payload: ann},
				[]Entry{siv(Entry{Seq: 7, Ranges: ranges(textOff, 2)})}, true, []Defect{{Entry: 0, Err: ErrSIVAnnotation}}},
			{"overlapping encrypted entries", data(40),
				[]Entry{siv(Entry{Seq: 7, Ranges: ranges(14, 10)}), siv(Entry{Seq: 7, Ranges: ranges(20, 4)})}, true,
				[]Defect{{Entry: 1, Err: ErrEncryptedOverlap}}},
			{"duplicate encrypted entries", data(40),
				[]Entry{siv(Entry{Seq: 7, Ranges: ranges(14, 4)}), siv(Entry{Seq: 7, Ranges: ranges(14, 4)})}, true,
				[]Defect{{Entry: 1, Err: ErrEncryptedOverlap}}},
			{"adjacent encrypted entries", data(40),
				[]Entry{siv(Entry{Seq: 7, Ranges: ranges(14, 4)}), siv(Entry{Seq: 7, Ranges: ranges(18, 4)})}, true, nil},
			{"interleaved encrypted entries", data(60),
				[]Entry{siv(Entry{Seq: 7, Ranges: ranges(14, 2, 30, 2)}), siv(Entry{Seq: 7, Ranges: ranges(20, 2, 31, 4)})}, true,
				[]Defect{{Entry: 1, Err: ErrEncryptedOverlap}}},
			{"overlapping zero masks", data(40),
				[]Entry{{Seq: 7, Ranges: ranges(14, 10)}, {Seq: 7, Ranges: ranges(20, 4)}}, false, nil},
		}
		for _, tt := range tests {
			t.Run(tt.name, func(t *testing.T) {
				t.Parallel()

				got := CheckRecord(&tt.rec, tt.entries, tt.encrypted)
				require.Len(t, got, len(tt.want), "%v", got)
				for i, want := range tt.want {
					assert.Equal(t, want.Entry, got[i].Entry)
					assert.ErrorIs(t, got[i].Err, want.Err)
				}
			})
		}
	})

	t.Run("no root import", func(t *testing.T) {
		t.Parallel()

		for _, path := range moduleDeps(t, ".") {
			assert.NotEqual(t, modulePath, path, "internal/marking must not import the root package")
		}
	})
}

// moduleDeps returns every package of the module that the package in dir imports, directly or not,
// read from the source files with go/build.
func moduleDeps(t *testing.T, dir string) []string {
	t.Helper()

	root, err := filepath.Abs("../..")
	require.NoError(t, err)

	var deps []string
	seen := map[string]bool{}
	pending := []string{dir}
	for len(pending) > 0 {
		pkg, err := build.ImportDir(pending[0], 0)
		require.NoError(t, err)
		pending = pending[1:]
		for _, path := range pkg.Imports {
			if seen[path] || (path != modulePath && !strings.HasPrefix(path, modulePath+"/")) {
				continue
			}
			seen[path] = true
			deps = append(deps, path)
			pending = append(pending, filepath.Join(root, strings.TrimPrefix(strings.TrimPrefix(path, modulePath), "/")))
		}
	}

	return deps
}
