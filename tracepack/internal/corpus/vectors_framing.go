package corpus

import (
	"bytes"
	"encoding/binary"
	"fmt"
	"math"

	"github.com/arloliu/go-secs/tracepack"
	"github.com/arloliu/go-secs/tracepack/internal/format"
	"github.com/arloliu/go-secs/tracepack/internal/tlv"
)

// Values of the framing group.
const (
	// overThreshold is the block size threshold of framing-record-over-threshold, in bytes.
	overThreshold = 256
	// overflowCount and overflowHeaderLen are the record_count and record_header_len of framing-header-section-overflow:
	// their product is 2^32.
	overflowCount     = 1 << 18
	overflowHeaderLen = 1 << 14
	// framingsCount is the number of records of framing-two-framings' block of 44-byte headers.
	framingsCount = 4
	// framingsSeq is the seq of framing-two-framings' long-header record:
	// header bytes 0 to 7 are byte 0 of the four short records' seqs 0 to 3, then byte 1 of them, all 0.
	framingsSeq = 0x03020100
)

// framingVectors returns the recipes of the framing group (the tracepack corpus specification §9.4).
func framingVectors() []Recipe {
	return []Recipe{
		{
			ID: "framing-header-extension", Title: "twelve blocks of one record, of record_header_len 44 and 45 to 55, each with its own extension bytes",
			Cites: []string{"FMT §7.1", "FMT §6", "FMT §16"}, Class: ClassRead,
			Build: func(seed string) (*Built, error) {
				blocks := make([][]tracepack.Record, 0, 12)
				for s := range uint64(12) {
					blocks = append(blocks, []tracepack.Record{footerData(s, 1)})
				}
				pack, err := writeSpec(&packSpec{seed: seed, meta: segmentMeta(seed), blocks: blocks})
				if err != nil {
					return nil, err
				}
				// Block i's record gains i extension bytes, byte j of them i<<4 | j.
				for i := 1; i < len(blocks); i++ {
					if pack, err = PatchBody(pack, i, func(b *Body) error {
						for j := range i {
							b.Rows = append(b.Rows, byte(i<<4|j))
						}
						b.RecordHeaderLen += i

						return nil
					}); err != nil {
						return nil, err
					}
				}

				return &Built{Pack: pack}, nil
			},
			Expect: &Expectation{
				Outcome: tracepack.OutcomeFinalizedConsistent, FooterValid: true, Blocks: 12, Seqs: seqRange(0, 11), PrefixEnd: AtEnd(),
				ExportHas: []string{
					`"record_flags":[],"record_header_len":44,"payload"`,
					`"record_header_len":45,"header_extra":"EA=="`,
					`"record_header_len":55,"header_extra":"sLGys7S1tre4ubo="`,
				},
			},
		},
		{
			ID: "framing-two-framings", Title: "two blocks of equal decoded bodies: four records of 44-byte headers, one record whose header is the whole body",
			Cites: []string{"FMT §6", "FMT §7.1", "FMT I-2", "FMT §16"}, Class: ClassRead,
			Build: twoFramingsPack,
			// The long header holds the short records' transposed header section and payloads, 4 × 44 + 4 × 14 = 232 bytes;
			// its seq, time, epoch and payload_len are bytes of the short records' seqs, so 0x03020100, 0, 0 and 0.
			Expect: &Expectation{
				Outcome: tracepack.OutcomeFinalizedConsistent, FooterValid: true, Blocks: 2,
				Seqs: []uint64{0, 1, 2, 3, framingsSeq}, PrefixEnd: AtEnd(),
				ExportHas: []string{
					`"seq":"50462976","ts_utc_ns":"0","mono_ns":"0","epoch":0,"payload_len":0,"trailing_bytes":0,` +
						`"quality":["correlation-incomplete"],"kind":"data","dir":"host-to-equipment","fidelity":"wire-exact",` +
						`"decode_status":"not-attempted","field_validity":[],"record_flags":[],"record_header_len":232,"header_extra":`,
				},
			},
		},
		{
			ID: "framing-gathered-headers", Title: "a block of three records of 47-byte headers, their seqs, lengths and extension bytes gathered from the columns",
			Cites: []string{"FMT §6", "FMT §7.1", "FMT I-2"}, Class: ClassRead,
			Build: func(seed string) (*Built, error) {
				recs := []tracepack.Record{
					newData(0, 1, tracepack.DirHostToEquipment, dataFrame(1, 1, true, 1, nil)),
					newData(2, 1, tracepack.DirEquipmentToHost, dataFrame(1, 2, false, 1, []byte{0x21, 0x01, 0x00})),
					newData(5, 1, tracepack.DirHostToEquipment, dataFrame(6, 11, true, 2, []byte{0x01, 0x02, 0x21, 0x01, 0x00, 0x01, 0x00})),
				}
				pack, err := writeSpec(&packSpec{seed: seed, meta: segmentMeta(seed), blocks: [][]tracepack.Record{recs}})
				if err != nil {
					return nil, err
				}
				// Row j gains the extension bytes 0xE0 | j, j, 0xA5.
				pack, err = PatchBody(pack, 0, func(b *Body) error {
					var rows []byte
					for j := range len(b.Rows) / b.RecordHeaderLen {
						rows = append(rows, b.Rows[j*b.RecordHeaderLen:(j+1)*b.RecordHeaderLen]...)
						rows = append(rows, 0xE0|byte(j), byte(j), 0xA5)
					}
					b.Rows, b.RecordHeaderLen = rows, b.RecordHeaderLen+3

					return nil
				})

				return &Built{Pack: pack}, err
			},
			Expect: &Expectation{
				Outcome: tracepack.OutcomeFinalizedConsistent, FooterValid: true, Blocks: 1, Seqs: []uint64{0, 2, 5}, PrefixEnd: AtEnd(),
				ExportHas: []string{
					`"seq":"0","ts_utc_ns":"1791000000000000000","mono_ns":"0","epoch":1,"payload_len":14,`,
					`"seq":"2","ts_utc_ns":"1791000000002000000","mono_ns":"0","epoch":1,"payload_len":17,`,
					`"seq":"5","ts_utc_ns":"1791000000005000000","mono_ns":"0","epoch":1,"payload_len":21,`,
					`"record_header_len":47,"header_extra":"4ACl"`, `"record_header_len":47,"header_extra":"4QGl"`,
					`"record_header_len":47,"header_extra":"4gKl"`,
				},
			},
		},
		{
			ID: "framing-header-len-below-44", Title: "a middle block whose envelope states record_header_len 43, the footer valid",
			Cites: []string{"FMT §6", "FMT §7.1", "FMT §13"}, Class: ClassRead, Labels: []string{LabelNonconformingWriter},
			Build: framingEdit(func(pack []byte) ([]byte, error) {
				// Each row loses its last byte, record_flags, and the envelope states 43.
				pack, err := PatchBody(pack, 1, func(b *Body) error {
					var rows []byte
					for j := range len(b.Rows) / b.RecordHeaderLen {
						rows = append(rows, b.Rows[j*b.RecordHeaderLen:(j+1)*b.RecordHeaderLen-1]...)
					}
					b.Rows, b.RecordHeaderLen = rows, b.RecordHeaderLen-1

					return nil
				})
				if err != nil {
					return nil, err
				}

				// The F-2 entry keeps 44, the value the footer validation allows, so that the footer stays valid.
				return PatchFooter(pack, func(d []byte) ([]byte, error) {
					e, err := F2Entry(d, 1)
					if err != nil {
						return nil, err
					}
					binary.LittleEndian.PutUint16(d[e.Off+f2RecordHeaderLenOff:], format.RecordHeaderLen)

					return d, nil
				})
			}),
			// The envelope's 43 is below 44, a corrupt block (the tracepack format specification §6),
			// and it also disagrees with the F-2 entry's 44:
			// the vector checks that such a block fails, not which check fails it.
			// A validated block follows, and the footer gives the failed block's seqs.
			Expect: &Expectation{
				Outcome: tracepack.OutcomeCorruptMiddle, FooterValid: true, Blocks: 3, Seqs: []uint64{0, 1, 4, 5},
				Failed: []FailedWant{{Block: 1, Cause: tracepack.ReasonCorruptBlock}}, PrefixEnd: AtBlock(1),
				Lost: []LostWant{{First: 2, Last: 3, TimeStart: msAt(2), TimeEnd: msAt(3)}},
			},
		},
		{
			ID: "framing-header-section-overflow", Title: "a last block of record_count 2^18 and record_header_len 16384, its CRCs valid and its F-2 entry agreeing",
			Cites: []string{"FMT §6", "FMT §2", "FMT I-2", "FMT §13"}, Class: ClassRead, Labels: []string{LabelNonconformingWriter},
			Build: headerSectionOverflowPack,
			// The header section of 2^32 bytes exceeds every valid uncompressed_len: the block fails,
			// and the valid footer gives its seqs, 2 to 2 + 2^18 − 1, at the time of its one stored record.
			Expect: &Expectation{
				Outcome: tracepack.OutcomeFinalizedTruncated, FooterValid: true, Blocks: 2, Seqs: []uint64{0, 1},
				Failed: []FailedWant{{Block: 1, Cause: tracepack.ReasonCorruptBlock}}, PrefixEnd: AtBlock(1),
				Lost: []LostWant{{First: 2, Last: 2 + overflowCount - 1, TimeStart: msAt(2), TimeEnd: msAt(2)}},
			},
		},
		{
			ID: "framing-header-len-index-mismatch", Title: "a middle block whose F-2 entry states record_header_len 45, its envelope 44",
			Cites: []string{"FMT §10", "FMT §6", "FMT §13"}, Class: ClassRead, Labels: []string{LabelNonconformingWriter},
			Build: framingEdit(footerEdit(func(d []byte) ([]byte, error) {
				e, err := F2Entry(d, 1)
				if err != nil {
					return nil, err
				}
				if got := binary.LittleEndian.Uint16(d[e.Off+f2RecordHeaderLenOff:]); got != format.RecordHeaderLen {
					return nil, fmt.Errorf("%w: record_header_len %d, not %d", ErrPackEdit, got, format.RecordHeaderLen)
				}
				binary.LittleEndian.PutUint16(d[e.Off+f2RecordHeaderLenOff:], format.RecordHeaderLen+1)

				return d, nil
			})),
			// The footer stays valid; the envelope disagrees with its F-2 entry, so the block fails and the footer gives its seqs.
			Expect: &Expectation{
				Outcome: tracepack.OutcomeCorruptMiddle, FooterValid: true, Blocks: 3, Seqs: []uint64{0, 1, 4, 5},
				Failed: []FailedWant{{Block: 1, Cause: tracepack.ReasonCorruptBlock}}, PrefixEnd: AtBlock(1),
				Lost: []LostWant{{First: 2, Last: 3, TimeStart: msAt(2), TimeEnd: msAt(3)}},
			},
		},
		zstdDamageVector("framing-zstd-malformed", "a middle block holding a hand-built zstd frame whose last block is malformed", ZstdMalformed, false),
		zstdDamageVector("framing-zstd-short", "a middle block holding a hand-built zstd frame that ends one byte short", ZstdShort, false),
		zstdDamageVector("framing-zstd-short-checksum", "a middle block holding a hand-built zstd frame that ends one byte short, with a content checksum", ZstdShort, true),
		{
			ID: "framing-record-over-threshold", Title: "a record larger than the 256-byte block threshold, alone in its block",
			Cites: []string{"FMT §2", "FMT §6", "CORPUS §8"}, Class: ClassRead,
			Build: func(seed string) (*Built, error) {
				big := newData(1, 1, tracepack.DirEquipmentToHost, dataFrame(6, 12, false, 1, bytes.Repeat([]byte{0xA5}, 300)))
				pack, err := writeSpec(&packSpec{seed: seed, meta: segmentMeta(seed), threshold: overThreshold, blocks: [][]tracepack.Record{
					{footerData(0, 1), big, footerData(2, 1)},
				}})

				return &Built{Pack: pack}, err
			},
			// The Writer closes the block before the large record and after it, so the three records take three blocks.
			Expect: &Expectation{
				Outcome: tracepack.OutcomeFinalizedConsistent, FooterValid: true, Blocks: 3, Seqs: seqRange(0, 2), PrefixEnd: AtEnd(),
				ExportHas: []string{`"seq":"1","ts_utc_ns":"1791000000001000000","mono_ns":"0","epoch":1,"payload_len":314,`},
			},
		},
	}
}

// framingPack writes three blocks of two data records of epoch 1, seqs 0 to 5.
func framingPack(seed string) ([]byte, error) {
	return writeSpec(&packSpec{seed: seed, meta: segmentMeta(seed), blocks: [][]tracepack.Record{
		{footerData(0, 1), footerData(1, 1)}, {footerData(2, 1), footerData(3, 1)}, {footerData(4, 1), footerData(5, 1)},
	}})
}

// framingEdit returns the Build of a vector that is framingPack after edit.
func framingEdit(edit func(pack []byte) ([]byte, error)) func(seed string) (*Built, error) {
	return func(seed string) (*Built, error) {
		pack, err := framingPack(seed)
		if err != nil {
			return nil, err
		}
		if pack, err = edit(pack); err != nil {
			return nil, err
		}

		return &Built{Pack: pack}, nil
	}
}

// zstdDamageVector returns a vector of the codec pack whose middle block holds the hand-built zstd frame of damage
// (the tracepack corpus specification §6.2), every other block and the footer of codec none.
// The frame's header section is intact; its stream is not, so the block fails I-2 alone,
// and the valid footer gives its seqs.
func zstdDamageVector(id, title string, damage ZstdDamage, checksum bool) Recipe {
	return Recipe{
		ID: id, Title: title, Cites: []string{"FMT §16", "FMT I-2", "FMT §2", "CORPUS §6.2"}, Class: ClassRead,
		Codec: CodecMixed, Labels: []string{LabelDamaged},
		Build: func(seed string) (*Built, error) {
			return editedCodecPack(seed, func(pack []byte) ([]byte, error) {
				return DamageZstd(pack, 1, damage, checksum)
			})
		},
		Expect: middleFailedExpect(tracepack.ReasonCorruptBlock, nil),
	}
}

// headerSectionOverflowPack builds framing-header-section-overflow:
// two blocks of data records of epoch 1, seqs 0 and 1, then 2;
// the last block's envelope then states record_count 2^18 and record_header_len 16384,
// and the footer and trailer are written for a block of 2^18 data records, seqs 2 to 2^18 + 1, all at its one record's time,
// so that the footer is valid and its F-2 entry agrees with the envelope.
func headerSectionOverflowPack(seed string) (*Built, error) {
	pack, err := writeSpec(&packSpec{seed: seed, meta: segmentMeta(seed), blocks: [][]tracepack.Record{
		{footerData(0, 1), footerData(1, 1)}, {footerData(2, 1)},
	}})
	if err != nil {
		return nil, err
	}
	if pack, err = PatchEnvelope(pack, 1, func(env []byte) {
		binary.LittleEndian.PutUint32(env[envelopeRecordCountOff:], overflowCount)
		binary.LittleEndian.PutUint16(env[envelopeRecordHeaderLenOff:], overflowHeaderLen)
	}); err != nil {
		return nil, err
	}

	const (
		last  = 2 + overflowCount - 1
		total = 2 + overflowCount
	)
	pack, err = PatchFooter(pack, func(d []byte) ([]byte, error) {
		e, err := F2Entry(d, 1)
		if err != nil {
			return nil, err
		}
		if err := setLE32(d, e.Off+f2RecordCountOff, 1, overflowCount); err != nil {
			return nil, err
		}
		if err := setLE64(d, e.Off+f2LastSeqOff, 2, last); err != nil {
			return nil, err
		}
		binary.LittleEndian.PutUint16(d[e.Off+f2RecordHeaderLenOff:], overflowHeaderLen)

		f3, err := F3List(d, 1)
		if err != nil {
			return nil, err
		}
		f5, err := F5List(d)
		if err != nil {
			return nil, err
		}
		counts := []struct {
			list       Section
			tag        uint16
			width, idx int
			old, new   uint64
		}{
			{f3, f3KindCountsTag, 4, int(tracepack.KindData), 1, overflowCount},
			{f3, f3DirCountsTag, 4, int(tracepack.DirHostToEquipment), 1, overflowCount},
			{f3, f3DecodeStatusCountsTag, 4, int(tracepack.DecodeStatusNotAttempted), 1, overflowCount},
			{f5, f5KindCountsTag, 8, int(tracepack.KindData), 3, total},
			{f5, f5DirCountsTag, 8, int(tracepack.DirHostToEquipment), 3, total},
			{f5, f5DecodeStatusCountsTag, 8, int(tracepack.DecodeStatusNotAttempted), 3, total},
		}
		for _, c := range counts {
			if err := setCount(d, c.list, c.tag, c.width, c.idx, c.old, c.new); err != nil {
				return nil, err
			}
		}
		nested := []struct {
			list          Section
			tag, inner    uint16
			old, newValue uint64
		}{
			{f3, f3EpochTag, epochRecordCountTag, 1, overflowCount},
			{f3, f3EpochTag, epochSeqLastTag, 2, last},
			{f5, f5EpochTag, epochRecordCountTag, 3, total},
			{f5, f5EpochTag, epochSeqLastTag, 2, last},
		}
		for _, n := range nested {
			if err := setNested(d, n.list, n.tag, 0, n.inner, n.old, n.newValue); err != nil {
				return nil, err
			}
		}
		at, err := EntryOffset(d, f5, f5RecordCountTag, 0)
		if err != nil {
			return nil, err
		}
		if err := setLE64(d, at+tlv.HeaderLen, 3, total); err != nil {
			return nil, err
		}

		return d, setSeqRange(d, f5, 0, [2]uint64{0, 2}, [2]uint64{0, last})
	})
	if err != nil {
		return nil, err
	}

	var terr error
	pack, err = PatchTrailer(pack, func(tr []byte) {
		if terr = setLE64(tr, trailerRecordCountOff, 3, total); terr == nil {
			terr = setLE64(tr, trailerLastSeqOff, 2, last)
		}
	})
	if err == nil {
		err = terr
	}

	return &Built{Pack: pack}, err
}

// twoFramingsPack builds framing-two-framings, an extract so that its records may lie in different hours.
// Block 0 holds four data records of 44-byte headers, seqs 0 to 3;
// their times are chosen so that a 44-byte header gathered from the start of their transposed header section reads them
// as trailing_bytes 0, quality correlation-incomplete, kind data, dir host-to-equipment, fidelity wire-exact,
// decode_status not-attempted, and no field_validity or record_flags bits.
// Block 1 is one record whose header is block 0's whole decoded body and whose payload is empty:
// the Writer writes that record with the 44 bytes it knows, and the surgery widens its header to the whole body.
// The construction checks that the Writer's header is the body's first 44 bytes and that the two bodies are equal.
func twoFramingsPack(seed string) (*Built, error) {
	meta := segmentMeta(seed)
	meta.PackRole, meta.ScopeGeneration, meta.ExtractFilter = tracepack.PackRoleExtract, nil, new("two framings")
	meta.PeriodStart, meta.PeriodEnd = math.MinInt64, math.MaxInt64

	// Bytes 0, 1 and 2 of each short record's ts_utc_ns: header columns 8, 9 and 10 of the section.
	low := [framingsCount][3]byte{
		{0, byte(tracepack.QualityCorrelationIncomplete), byte(tracepack.FidelityWireExact)},
		{0, 0, byte(tracepack.DecodeStatusNotAttempted)},
		{0, byte(tracepack.KindData), 0},
		{0, byte(tracepack.DirHostToEquipment), 0},
	}
	short := make([]tracepack.Record, 0, framingsCount)
	for i, b := range low {
		target := int64(b[0]) | int64(b[1])<<8 | int64(b[2])<<16
		// The first instant of TimeBase's hour, at or after i × 2^24 ns, whose low 24 bits are target.
		ts := TimeBase + int64(i)<<24
		ts += (target - ts) & (1<<24 - 1)
		r := newData(uint64(i), 1, tracepack.DirHostToEquipment, dataFrame(1, 1, true, uint32(i), nil))
		r.TSUTCNs = ts
		short = append(short, r)
	}
	long := tracepack.Record{
		Seq: framingsSeq, TSUTCNs: 0, Epoch: 0, Kind: tracepack.KindData, Dir: tracepack.DirHostToEquipment,
		Fidelity: tracepack.FidelityWireExact,
	}
	pack, err := writeSpec(&packSpec{seed: seed, meta: meta, blocks: [][]tracepack.Record{short, {long}}})
	if err != nil {
		return nil, err
	}

	l, err := Locate(pack)
	if err != nil {
		return nil, err
	}
	body, _, err := decodeBlock(pack[l.Blocks[0].Offset : l.Blocks[0].Offset+l.Blocks[0].Len])
	if err != nil {
		return nil, err
	}
	pack, err = PatchBody(pack, 1, func(b *Body) error {
		if !bytes.Equal(b.Rows, body[:format.RecordHeaderLen]) {
			return fmt.Errorf("%w: the long record's header % x is not the short block's first 44 bytes % x", ErrPackEdit, b.Rows, body[:format.RecordHeaderLen])
		}
		b.Rows, b.RecordHeaderLen, b.Payloads = bytes.Clone(body), len(body), nil

		return nil
	})
	if err != nil {
		return nil, err
	}

	if l, err = Locate(pack); err != nil {
		return nil, err
	}
	long1, _, err := decodeBlock(pack[l.Blocks[1].Offset : l.Blocks[1].Offset+l.Blocks[1].Len])
	if err != nil {
		return nil, err
	}
	if !bytes.Equal(long1, body) {
		return nil, fmt.Errorf("%w: the two blocks' decoded bodies differ", ErrPackEdit)
	}

	return &Built{Pack: pack}, nil
}
