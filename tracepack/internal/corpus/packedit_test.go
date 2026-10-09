package corpus

import (
	"bytes"
	"encoding/binary"
	"runtime"
	"slices"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/arloliu/go-secs/tracepack"
	"github.com/arloliu/go-secs/tracepack/internal/format"
	"github.com/arloliu/go-secs/tracepack/internal/tlv"
)

// Tags and record header positions the pack surgery tests edit (the tracepack format specification §7.1 and §10).
const (
	f3QualityUnionTag uint16 = 0x0005
	f5QualityUnionTag uint16 = 0x0008
	retiredF3Tag      uint16 = 0x0004

	f2TSMaxOff = 48

	rowTSOff            = 8
	rowQualityHighOff   = 37
	rowFieldValidityOff = 42
	rowRecordFlagsOff   = 43
)

// editBlocks returns the records of the pack the surgery tests edit: three blocks of two, two and one data records of epoch 1.
func editBlocks() [][]tracepack.Record {
	return [][]tracepack.Record{
		{dataRecord(0, 1, []byte("alpha")), dataRecord(1, 1, []byte("bravo"))},
		{dataRecord(2, 1, []byte("charlie")), dataRecord(3, 1, []byte("delta"))},
		{dataRecord(4, 1, []byte("echo"))},
	}
}

// editPack writes the finalized pack of editBlocks with codec c.
func editPack(t *testing.T, c tracepack.Codec) []byte {
	t.Helper()

	return writePackCodec(t, testMeta(), editBlocks(), true, c)
}

// must returns a function that requires err to be nil and returns v:
// must(f())(t) checks a call returning a value and an error inline.
func must[T any](v T, err error) func(t *testing.T) T {
	return func(t *testing.T) T {
		t.Helper()
		require.NoError(t, err)

		return v
	}
}

// storedRows returns the record header rows of block i of pack, untransposed, and their record_header_len.
func storedRows(t *testing.T, pack []byte, i int) ([]byte, int) {
	t.Helper()

	l := must(Locate(pack))(t)
	s := l.Blocks[i]
	blk := pack[s.Offset : s.Offset+s.Len]
	decoded, hs, err := decodeBlock(blk)
	require.NoError(t, err)
	n, rhl := int(binary.LittleEndian.Uint32(blk[envelopeRecordCountOff:])), int(binary.LittleEndian.Uint16(blk[envelopeRecordHeaderLenOff:]))

	return format.UntransposeHeaders(nil, decoded[:hs], n, rhl), rhl
}

// widenRows gives every row of b an extension area of 6 bytes, numbered by row, behind offset 44.
func widenRows(b *Body) {
	var rows []byte
	for j := range len(b.Rows) / b.RecordHeaderLen {
		rows = append(rows, b.Rows[j*b.RecordHeaderLen:(j+1)*b.RecordHeaderLen]...)
		rows = append(rows, 0xE0, byte(j), 0xA5, 1, 2, 3)
	}
	b.Rows, b.RecordHeaderLen = rows, b.RecordHeaderLen+6
}

// footerU64 returns the u64 value of the first entry of tag in the entry list s of the decoded footer d.
func footerU64(t *testing.T, d []byte, s Section, tag uint16) uint64 {
	t.Helper()

	at := must(EntryOffset(d, s, tag, 0))(t)

	return binary.LittleEndian.Uint64(d[at+tlv.HeaderLen:])
}

// orQualityUnion sets bits in block i's F-3 quality_union and in F-5's.
func orQualityUnion(t *testing.T, pack []byte, i int, bits uint64) []byte {
	t.Helper()

	return must(PatchFooter(pack, func(d []byte) ([]byte, error) {
		f3 := must(F3List(d, i))(t)
		f5 := must(F5List(d))(t)
		require.NoError(t, SetU64(d, f3, f3QualityUnionTag, footerU64(t, d, f3, f3QualityUnionTag)|bits))
		require.NoError(t, SetU64(d, f5, f5QualityUnionTag, footerU64(t, d, f5, f5QualityUnionTag)|bits))

		return d, nil
	}))(t)
}

func TestLocate(t *testing.T) {
	t.Parallel()

	blocks := editBlocks()
	pack := editPack(t, tracepack.CodecNone)
	start := metaEnd(pack)
	want := make([]Span, 0, len(blocks))
	end := start
	for _, b := range blocks {
		want = append(want, Span{Offset: end, Len: blockLen(b)})
		end += blockLen(b)
	}

	assert.Equal(t, Layout{MetaEnd: start, Blocks: want, End: end, Finalized: true}, must(Locate(pack))(t))

	open := writePack(t, testMeta(), blocks, false)
	assert.Equal(t, Layout{MetaEnd: start, Blocks: want, End: end}, must(Locate(open))(t))
	assert.Equal(t, Layout{MetaEnd: start, Blocks: want[:2], End: want[2].Offset}, must(Locate(open[:end-1]))(t), "a cut last block is not found")
}

func TestPackEditIdentity(t *testing.T) {
	t.Parallel()

	keep := func(*Body) error { return nil }
	ops := []struct {
		name      string
		finalized bool // the edit needs a finalized pack
		edit      func(pack []byte) ([]byte, error)
	}{
		{"header", false, func(p []byte) ([]byte, error) { return PatchHeader(p, func([]byte) {}) }},
		{"metadata", false, func(p []byte) ([]byte, error) { return PatchMeta(p, func(m []byte) []byte { return m }) }},
		{"envelope", false, func(p []byte) ([]byte, error) { return PatchEnvelope(p, 1, func([]byte) {}) }},
		{"body 0", false, func(p []byte) ([]byte, error) { return PatchBody(p, 0, keep) }},
		{"body 1", false, func(p []byte) ([]byte, error) { return PatchBody(p, 1, keep) }},
		{"reencode", false, func(p []byte) ([]byte, error) {
			return ReencodeBlocks(p, func(int, *Block) error { return nil })
		}},
		{"footer", true, func(p []byte) ([]byte, error) {
			return PatchFooter(p, func(d []byte) ([]byte, error) { return d, nil })
		}},
		{"footer parts", true, func(p []byte) ([]byte, error) {
			return PatchFooter(p, func(d []byte) ([]byte, error) {
				parts, err := SplitFooter(d)
				if err != nil {
					return nil, err
				}

				return parts.Join()
			})
		}},
		{"trailer", true, func(p []byte) ([]byte, error) { return PatchTrailer(p, func([]byte) {}) }},
	}

	for _, c := range []tracepack.Codec{tracepack.CodecNone, tracepack.CodecZstd} {
		finalized := editPack(t, c)
		open := writePackCodec(t, testMeta(), editBlocks(), false, c)
		packs := map[string][]byte{"finalized": finalized, "unfinalized": open, "cut": open[:len(open)-3]}
		for name, pack := range packs {
			for _, op := range ops {
				if op.finalized && name != "finalized" {
					continue
				}
				t.Run(c.String()+" "+name+" "+op.name, func(t *testing.T) {
					t.Parallel()

					assert.Equal(t, pack, must(op.edit(pack))(t))
				})
			}
		}
	}
}

func TestPackEditOutcomes(t *testing.T) {
	t.Parallel()

	type failure struct {
		block  int
		reason tracepack.IncompleteReason
	}
	tests := []struct {
		name        string
		edit        func(t *testing.T, pack []byte) []byte
		outcome     tracepack.Outcome
		failed      []failure
		disagreeing []int
		footerValid bool
	}{
		{
			name: "body byte flipped",
			edit: func(t *testing.T, pack []byte) []byte {
				return must(FlipByte(pack, int(must(Locate(pack))(t).Blocks[1].Offset)+testEnvelopeLen+5))(t)
			},
			outcome: tracepack.OutcomeCorruptMiddle, failed: []failure{{1, tracepack.ReasonCorruptBlock}}, footerValid: true,
		},
		{
			name: "unknown codec",
			edit: func(t *testing.T, pack []byte) []byte {
				return must(PatchEnvelope(pack, 1, func(env []byte) { env[envelopeCodecOff] = 9 }))(t)
			},
			outcome: tracepack.OutcomeCorruptMiddle, failed: []failure{{1, tracepack.ReasonUnknownCodec}}, footerValid: true,
		},
		{
			name: "envelope record_header_len against its F-2 entry",
			edit: func(t *testing.T, pack []byte) []byte {
				return must(PatchEnvelope(pack, 1, func(env []byte) { binary.LittleEndian.PutUint16(env[envelopeRecordHeaderLenOff:], 45) }))(t)
			},
			outcome: tracepack.OutcomeCorruptMiddle, failed: []failure{{1, tracepack.ReasonCorruptBlock}}, footerValid: true,
		},
		{
			name: "header extension, the footer following",
			edit: func(t *testing.T, pack []byte) []byte {
				return must(PatchBody(pack, 1, func(b *Body) error { widenRows(b); return nil }))(t)
			},
			outcome: tracepack.OutcomeFinalizedConsistent, footerValid: true,
		},
		{
			name: "block recoded to zstd",
			edit: func(t *testing.T, pack []byte) []byte {
				return must(PatchBody(pack, 1, func(b *Body) error { b.Codec = uint8(tracepack.CodecZstd); return nil }))(t)
			},
			outcome: tracepack.OutcomeFinalizedConsistent, footerValid: true,
		},
		{
			name: "raw record time past F-2 ts_max",
			edit: func(t *testing.T, pack []byte) []byte {
				return must(PatchBody(pack, 1, func(b *Body) error {
					binary.LittleEndian.PutUint64(b.Rows[b.RecordHeaderLen+rowTSOff:], uint64(testHour+10*int64(time.Microsecond)))
					return nil
				}))(t)
			},
			outcome: tracepack.OutcomeFinalizedInconsistent, disagreeing: []int{1}, footerValid: true,
		},
		{
			name: "re-encoded record time past F-2 ts_max",
			edit: func(t *testing.T, pack []byte) []byte {
				return must(ReencodeBlocks(pack, func(i int, b *Block) error {
					if i == 1 {
						b.Headers[1].TSUTCNs = testHour + 10*int64(time.Microsecond)
					}
					return nil
				}))(t)
			},
			outcome: tracepack.OutcomeFinalizedInconsistent, disagreeing: []int{1}, footerValid: true,
		},
		{
			name: "F-5 record_count dropped",
			edit: func(t *testing.T, pack []byte) []byte {
				return must(PatchFooter(pack, func(d []byte) ([]byte, error) {
					parts := must(SplitFooter(d))(t)
					parts.F5 = must(DropEntries(parts.F5, f5RecordCountTag))(t)
					return parts.Join()
				}))(t)
			},
			outcome: tracepack.OutcomeFinalizedInconsistent,
		},
		{
			name: "trailer record_count off by one",
			edit: func(t *testing.T, pack []byte) []byte {
				return must(PatchTrailer(pack, func(tr []byte) { addU64(tr, trailerRecordCountOff, 1) }))(t)
			},
			outcome: tracepack.OutcomeFinalizedInconsistent,
		},
		{
			name: "file header reserved bytes",
			edit: func(t *testing.T, pack []byte) []byte {
				return must(PatchHeader(pack, func(h []byte) { copy(h[64:76], bytes.Repeat([]byte{0xC3}, 12)) }))(t)
			},
			outcome: tracepack.OutcomeFinalizedConsistent, footerValid: true,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()

			rep := verifyPack(t, tt.edit(t, editPack(t, tracepack.CodecNone)))
			assert.Equal(t, tt.outcome, rep.Outcome)
			assert.Equal(t, tt.footerValid, rep.FooterErr == nil, "footer valid: %v", rep.FooterErr)
			var failed []failure
			for _, d := range rep.Failed {
				failed = append(failed, failure{d.Block, d.Reason})
			}
			assert.Equal(t, tt.failed, failed)
			var disagreeing []int
			for _, d := range rep.Disagreements {
				if d.Block >= 0 && !slices.Contains(disagreeing, d.Block) {
					disagreeing = append(disagreeing, d.Block)
				}
			}
			assert.Equal(t, tt.disagreeing, disagreeing)
		})
	}
}

// TestPackEditSuccessiveRawEdits checks that raw edits keep what an earlier raw edit stored:
// reserved bits and an extension area in the record headers, reserved envelope and file header bytes,
// and a footer value, while later edits rewrite the same block and move the blocks after another.
func TestPackEditSuccessiveRawEdits(t *testing.T) {
	t.Parallel()

	pack := editPack(t, tracepack.CodecNone)
	pack = must(PatchBody(pack, 0, func(b *Body) error {
		widenRows(b)
		for j := 0; j < len(b.Rows); j += b.RecordHeaderLen {
			b.Rows[j+rowQualityHighOff] |= 0x80
			b.Rows[j+rowFieldValidityOff] |= 0x80
			b.Rows[j+rowRecordFlagsOff] |= 0x80
		}

		return nil
	}))(t)
	pack = must(PatchEnvelope(pack, 0, func(env []byte) {
		env[5] = 0x5A
		copy(env[32:36], []byte{1, 2, 3, 4})
	}))(t)
	pack = must(PatchHeader(pack, func(h []byte) { copy(h[64:76], bytes.Repeat([]byte{0xC3}, 12)) }))(t)
	pack = orQualityUnion(t, pack, 0, 0x8000)
	first := verifyPack(t, pack)
	require.Equal(t, tracepack.OutcomeFinalizedConsistent, first.Outcome, "%+v", first)

	wantRows, rhl := storedRows(t, pack, 0)
	require.Equal(t, 50, rhl)

	pack = must(PatchBody(pack, 0, func(b *Body) error { b.Payloads[len(b.Payloads)-1] ^= 0x01; return nil }))(t)
	pack = must(PatchBody(pack, 1, func(b *Body) error { b.Codec = uint8(tracepack.CodecZstd); return nil }))(t)
	pack = must(PatchEnvelope(pack, 2, func(env []byte) { env[33] = 0x77 }))(t)

	gotRows, rhl := storedRows(t, pack, 0)
	assert.Equal(t, 50, rhl)
	assert.Equal(t, wantRows, gotRows, "rows: reserved bits and extension areas")
	l := must(Locate(pack))(t)
	env := pack[l.Blocks[0].Offset:]
	assert.Equal(t, byte(0x5A), env[5])
	assert.Equal(t, []byte{1, 2, 3, 4}, env[32:36])
	assert.Equal(t, bytes.Repeat([]byte{0xC3}, 12), pack[64:76])
	d := must(DecodedFooter(pack))(t)
	assert.Equal(t, uint64(0x8000), footerU64(t, d, must(F3List(d, 0))(t), f3QualityUnionTag)&0x8000)
	assert.Equal(t, tracepack.OutcomeFinalizedConsistent, verifyPack(t, pack).Outcome)

	// Re-encoding, by contrast, writes every structure from its typed fields:
	// reserved bits and bytes fall to zero, while the extension area is a defined part of the header and stays.
	re := must(ReencodeBlocks(pack, func(int, *Block) error { return nil }))(t)
	reRows, rhl := storedRows(t, re, 0)
	require.Equal(t, 50, rhl)
	assert.Zero(t, reRows[rowQualityHighOff]&0x80)
	assert.Zero(t, reRows[rowFieldValidityOff]&0x80)
	assert.Zero(t, reRows[rowRecordFlagsOff]&0x80)
	assert.Equal(t, wantRows[44:50], reRows[44:50])
	assert.Zero(t, re[must(Locate(re))(t).Blocks[0].Offset+5])
}

// TestPackEditPreservesFooterBytes checks that raw surgery keeps F-4, the F-2 bytes past offset 80, retired F-3 tags
// and the reserved byte of a TLV entry header through successive edits of the blocks and the footer.
func TestPackEditPreservesFooterBytes(t *testing.T) {
	t.Parallel()

	f4 := []byte{0xF4, 1, 2, 3, 4, 5, 6, 7}
	retired := tlv.AppendEntry(nil, tlv.U64Entry(retiredF3Tag, 99))
	pack := must(PatchFooter(editPack(t, tracepack.CodecNone), func(d []byte) ([]byte, error) {
		p := must(SplitFooter(d))(t)
		for i := range p.Entries {
			p.Entries[i] = append(p.Entries[i], 0xF2, byte(i), 0xEE, 0xEE)
		}
		p.SetF4(f4)
		p.F3[3] = 0x77 // the reserved byte of block 0's first F-3 entry, which starts F-3
		so := binary.LittleEndian.Uint64(p.Entries[1][f2SummaryOffsetOff:])
		sl := binary.LittleEndian.Uint32(p.Entries[1][f2SummaryLenOff:])
		require.NoError(t, p.SetF3List(1, slices.Concat(p.F3[so:so+uint64(sl)], retired)))

		return p.Join()
	}))(t)
	require.Equal(t, tracepack.OutcomeFinalizedConsistent, verifyPack(t, pack).Outcome)

	pack = must(PatchBody(pack, 0, func(b *Body) error { widenRows(b); return nil }))(t)
	pack = must(PatchBody(pack, 2, func(b *Body) error { b.Codec = uint8(tracepack.CodecZstd); return nil }))(t)
	pack = orQualityUnion(t, pack, 1, 0)
	pack = must(PatchTrailer(pack, func([]byte) {}))(t)

	rep := verifyPack(t, pack)
	require.NoError(t, rep.FooterErr)
	assert.Equal(t, tracepack.OutcomeFinalizedConsistent, rep.Outcome)
	p := must(SplitFooter(must(DecodedFooter(pack))(t)))(t)
	assert.Equal(t, f4, p.F4)
	assert.Equal(t, uint16(0x0003), binary.LittleEndian.Uint16(p.Prologue[prologueFlagsOff:]), "F-3 and F-4 present")
	for i, e := range p.Entries {
		assert.Equal(t, []byte{0xF2, byte(i), 0xEE, 0xEE}, e[format.F2EntryLen:], "entry %d", i)
	}
	assert.Equal(t, byte(0x77), p.F3[3])
	d := must(DecodedFooter(pack))(t)
	f3 := must(F3List(d, 1))(t)
	assert.True(t, bytes.HasSuffix(d[f3.Off:f3.Off+f3.Len], retired), "block 1's F-3 list ends with the retired tag")
}

// TestPackEditRefusesOutOfRange checks that every operation reports, instead of panicking on,
// a pack cut at any length, a pack with any one byte inverted, and an index or section outside its structure.
func TestPackEditRefusesOutOfRange(t *testing.T) {
	t.Parallel()

	keep := func(*Body) error { return nil }
	sameFooter := func(d []byte) ([]byte, error) { return d, nil }
	ops := map[string]func(p []byte) error{
		"locate":   func(p []byte) error { _, err := Locate(p); return err },
		"header":   func(p []byte) error { _, err := PatchHeader(p, func([]byte) {}); return err },
		"metadata": func(p []byte) error { _, err := PatchMeta(p, func(m []byte) []byte { return m }); return err },
		"envelope": func(p []byte) error { _, err := PatchEnvelope(p, 0, func([]byte) {}); return err },
		"body":     func(p []byte) error { _, err := PatchBody(p, 2, keep); return err },
		"reencode": func(p []byte) error { _, err := ReencodeBlocks(p, func(int, *Block) error { return nil }); return err },
		"zstd":     func(p []byte) error { _, err := DamageZstd(p, 0, ZstdShort, true); return err },
		"footer":   func(p []byte) error { _, err := PatchFooter(p, sameFooter); return err },
		"trailer":  func(p []byte) error { _, err := PatchTrailer(p, func([]byte) {}); return err },
		"decoded":  func(p []byte) error { _, err := DecodedFooter(p); return err },
		"flip":     func(p []byte) error { _, err := FlipByte(p, len(p)); return err },
		"split": func(p []byte) error {
			d, err := DecodedFooter(p)
			if err != nil {
				return err
			}
			parts, err := SplitFooter(d)
			if err != nil {
				return err
			}
			_, err = parts.Join()

			return err
		},
	}

	pack := editPack(t, tracepack.CodecNone)
	for name, op := range ops {
		t.Run(name, func(t *testing.T) {
			t.Parallel()

			for n := range len(pack) {
				err := op(pack[:n])
				if n < testFileHeaderLen {
					require.ErrorIs(t, err, ErrPackEdit, "cut at %d", n)
				}
			}
			for off := range pack {
				_ = op(must(FlipByte(pack, off))(t))
			}
		})
	}

	d := must(DecodedFooter(pack))(t)
	f5 := must(F5List(d))(t)
	refusals := map[string]error{
		"envelope -1":           second(PatchEnvelope(pack, -1, func([]byte) {})),
		"envelope 3":            second(PatchEnvelope(pack, 3, func([]byte) {})),
		"body 3":                second(PatchBody(pack, 3, keep)),
		"zstd 3":                second(DamageZstd(pack, 3, ZstdMalformed, false)),
		"zstd damage 0":         second(DamageZstd(pack, 0, 0, false)),
		"flip -1":               second(FlipByte(pack, -1)),
		"metadata length":       second(PatchMeta(pack, func(m []byte) []byte { return append(m, 0) })),
		"rows not whole":        second(PatchBody(pack, 0, func(b *Body) error { b.Rows = b.Rows[1:]; return nil })),
		"record_header_len 0":   second(PatchBody(pack, 0, func(b *Body) error { b.RecordHeaderLen = 0; return nil })),
		"unknown codec encode":  second(PatchBody(pack, 0, func(b *Body) error { b.Codec = 9; return nil })),
		"reencode no records":   second(ReencodeBlocks(pack, func(_ int, b *Block) error { b.Headers = nil; return nil })),
		"F-2 entry -1":          second(F2Entry(d, -1)),
		"F-2 entry 3":           second(F2Entry(d, 3)),
		"F-2 short footer":      second(F2Entry(d[:format.FooterPrologueLen-1], 0)),
		"F-2 cut entry":         second(F2Entry(d[:format.FooterPrologueLen+format.F2EntryLen-1], 0)),
		"F-3 cut list":          second(F3List(d[:f5.Off-1], 2)),
		"F-5 cut":               second(F5List(d[:len(d)-1])),
		"section outside":       second(EntryOffset(d, Section{Off: len(d), Len: 1}, f5RecordCountTag, 0)),
		"section negative":      second(EntryOffset(d, Section{Off: -1, Len: 1}, f5RecordCountTag, 0)),
		"entry missing":         second(EntryOffset(d, f5, f5RecordCountTag, 1)),
		"nested missing":        second(NestedValueOffset(d, f5, f5EpochTag, 0, 0x7FFF)),
		"u64 of another length": SetU64(d, must(F3List(d, 0))(t), f3EpochTag, 1),
		"drop missing":          second(DropEntries(d[f5.Off:f5.Off+f5.Len], 0x7FFF)),
		"drop unframed":         second(DropEntries([]byte{1, 0, 4}, 1)),
		"split short":           second(SplitFooter(d[:format.FooterPrologueLen-1])),
		"split cut":             second(SplitFooter(d[:len(d)-1])),
		"F-3 list 3":            (&FooterParts{}).SetF3List(3, nil),
		"join entry lengths": second((&FooterParts{
			Prologue: d[:format.FooterPrologueLen], Entries: [][]byte{make([]byte, 80), make([]byte, 81)},
		}).Join()),
		"join short prologue": second((&FooterParts{Prologue: d[:10]}).Join()),
		"pack metadata past the pack": second(PatchMeta(must(PatchHeader(pack[:200], func(h []byte) {
			binary.LittleEndian.PutUint32(h[headerPackMetadataLenOff:], 1000)
		}))(t), func(m []byte) []byte { return m })),
	}
	for name, err := range refusals {
		assert.ErrorIs(t, err, ErrPackEdit, name)
	}
}

// second returns the error of a call returning a value and an error.
func second[T any](_ T, err error) error {
	return err
}

// TestPackEditBoundsUncompressedLen checks that an edit that decodes a block refuses an uncompressed_len over the reader's block budget
// before a decoder sizes its output by it.
// It is not parallel: it measures the process-wide TotalAlloc.
func TestPackEditBoundsUncompressedLen(t *testing.T) {
	pack := must(PatchEnvelope(editPack(t, tracepack.CodecZstd), 1, func(env []byte) {
		binary.LittleEndian.PutUint32(env[envelopeUncompressedLenOff:], tracepack.DefaultMaxBlockLen+1)
	}))(t)

	var before, after runtime.MemStats
	runtime.ReadMemStats(&before)
	assert.ErrorIs(t, second(PatchBody(pack, 1, func(*Body) error { return nil })), ErrPackEdit, "PatchBody")
	assert.ErrorIs(t, second(DamageZstd(pack, 1, ZstdShort, false)), ErrPackEdit, "DamageZstd")
	assert.ErrorIs(t, second(ReencodeBlocks(pack, func(int, *Block) error { return nil })), ErrPackEdit, "ReencodeBlocks")
	runtime.ReadMemStats(&after)

	delta := after.TotalAlloc - before.TotalAlloc
	require.Less(t, delta, uint64(1<<20), "the length is checked before a decoder allocates for it")
}

// TestLocateFinalizedWalk checks the rules Locate applies to a finalized pack:
// the trailer's footer_offset lies between the pack metadata and the trailer, and the block walk ends exactly there.
func TestLocateFinalizedWalk(t *testing.T) {
	t.Parallel()

	pack := editPack(t, tracepack.CodecNone)
	l := must(Locate(pack))(t)
	trailerAt := uint64(len(pack) - testTrailerLen)
	for name, footerOffset := range map[string]uint64{
		"one before the footer": l.End - 1,
		"one after the footer":  l.End + 1,
		"past the trailer":      trailerAt + 1,
		"before the metadata":   l.MetaEnd - 1,
	} {
		moved := must(PatchTrailer(pack, func(tr []byte) { binary.LittleEndian.PutUint64(tr[trailerFooterOffsetOff:], footerOffset) }))(t)
		assert.ErrorIs(t, second(Locate(moved)), ErrPackEdit, name)
	}

	// Without the range check this walk would run past the pack's end: footer_offset far beyond it, and a block long enough to reach it.
	far := bytes.Clone(pack)
	binary.LittleEndian.PutUint32(far[l.Blocks[2].Offset+envelopeBodyLenOff:], 1<<20)
	far = must(PatchTrailer(far, func(tr []byte) { binary.LittleEndian.PutUint64(tr[trailerFooterOffsetOff:], 1<<40) }))(t)
	assert.ErrorIs(t, second(Locate(far)), ErrPackEdit, "footer_offset far past the pack")

	longer := must(PatchEnvelope(pack, 2, func(env []byte) {
		binary.LittleEndian.PutUint32(env[envelopeBodyLenOff:], binary.LittleEndian.Uint32(env[envelopeBodyLenOff:])+1)
	}))(t)
	assert.ErrorIs(t, second(Locate(longer)), ErrPackEdit, "a body_len that walks past footer_offset")

	// Once the trailer is broken the pack is not finalized:
	// a block edit moves the old footer and trailer as bytes after the last block, unchanged.
	broken := must(FlipByte(pack, len(pack)-1))(t)
	widened := must(PatchBody(broken, 1, func(b *Body) error { widenRows(b); return nil }))(t)
	assert.Equal(t, broken[l.End:], widened[len(widened)-(len(broken)-int(l.End)):])
	assert.Len(t, widened, len(broken)+2*6)
}

// TestPackEditRecomputes checks the values the surgery recomputes for an edit, each read back:
// the pack metadata's length and CRC, an F-2 entry's first_seq on re-encoding, and its record_count after PatchBody.
func TestPackEditRecomputes(t *testing.T) {
	t.Parallel()

	const seqStartTag uint16 = 0x0027
	pack := editPack(t, tracepack.CodecNone)
	same := must(PatchMeta(pack, func(m []byte) []byte {
		require.NoError(t, SetU64(m, Section{Len: len(m)}, seqStartTag, 9))
		return m
	}))(t)
	assert.Equal(t, uint64(9), openPack(t, same).Header().Meta.SeqStart, "same-length metadata: CRC recomputed")

	bare := writePack(t, testMeta(), nil, false)
	unknown := []byte{0x00, 0x7F, 7, 0, 1, 0, 0, 0, 0xAB}
	longer := must(PatchMeta(bare, func(m []byte) []byte { return append(m, unknown...) }))(t)
	assert.Equal(t, []tracepack.RawEntry{{Tag: 0x7F00, Type: 7, Value: []byte{0xAB}}}, openPack(t, longer).Header().Meta.Unknown,
		"longer metadata: length and CRC recomputed")

	reseq := must(ReencodeBlocks(pack, func(i int, b *Block) error {
		if i == 1 {
			b.Envelope.FirstSeq = 20
			b.Headers[0].Seq, b.Headers[1].Seq = 20, 21
		}
		return nil
	}))(t)
	d := must(DecodedFooter(reseq))(t)
	e := must(F2Entry(d, 1))(t)
	assert.Equal(t, uint64(20), binary.LittleEndian.Uint64(d[e.Off+f2FirstSeqOff:]), "F-2 first_seq follows the re-encoded envelope")

	dropped := must(PatchBody(pack, 1, func(b *Body) error {
		b.Rows = b.Rows[:b.RecordHeaderLen]
		b.Payloads = b.Payloads[:14+len("charlie")]
		return nil
	}))(t)
	d = must(DecodedFooter(dropped))(t)
	e = must(F2Entry(d, 1))(t)
	assert.Equal(t, uint32(1), binary.LittleEndian.Uint32(d[e.Off+f2RecordCountOff:]), "F-2 record_count follows the envelope")
}
