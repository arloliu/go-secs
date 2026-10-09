package corpus

import (
	"encoding/binary"
	"fmt"
	"math"

	"github.com/arloliu/go-secs/tracepack"
	"github.com/arloliu/go-secs/tracepack/internal/format"
)

// Values of the basic group.
const (
	// codecSeed is the identity seed of the vectors that hold the codec records under each codec.
	codecSeed = "basic-codec"
	// uuidVector is the vector whose pack_id is the literal UUID of primitives.json.
	uuidVector = "basic-uuid-byte-order"
	// unknownCodec is the codec of basic-unknown-codec's damaged block: a value outside the registry.
	unknownCodec = 7
	// maxU64 is the largest u64 value the format allows (the tracepack format specification §2).
	maxU64 = math.MaxInt64
)

// uuidByteOrder is the pack_id of basic-uuid-byte-order: version 7, variant 10, every byte distinct,
// so a reader that orders the bytes otherwise than the canonical string reads another value.
var uuidByteOrder = tracepack.UUID{0x0f, 0x1e, 0x2d, 0x3c, 0x4b, 0x5a, 0x79, 0x68, 0x87, 0x96, 0xa5, 0xb4, 0xc3, 0xd2, 0xe1, 0xf0}

// basicVectors returns the recipes of the basic group (the tracepack corpus specification §9.4).
func basicVectors() []Recipe {
	return []Recipe{
		{
			ID: "basic-empty-pack", Title: "a file header and pack metadata, nothing else",
			Cites: []string{"CORPUS §8", "FMT §13", "FMT I-5"}, Class: ClassRead,
			Build: func(seed string) (*Built, error) {
				pack, err := writeSpec(&packSpec{seed: seed, meta: segmentMeta(seed), open: true})

				return &Built{Pack: pack, Queries: []QuerySpec{allQuery()}}, err
			},
			Expect: &Expectation{
				Outcome: tracepack.OutcomeUnfinalized, Unfinalized: true, PrefixEnd: AtEnd(),
				Queries: []QueryWant{{ID: "all", Incomplete: []IncompleteWant{Truncated(AtEnd())}}},
			},
		},
		{
			ID: "basic-zero-records", Title: "a finalized pack without blocks",
			Cites: []string{"CORPUS §8", "FMT §10"}, Class: ClassRead,
			Build: func(seed string) (*Built, error) {
				pack, err := writeSpec(&packSpec{seed: seed, meta: segmentMeta(seed)})

				return &Built{Pack: pack}, err
			},
			Expect: &Expectation{
				Outcome: tracepack.OutcomeFinalizedConsistent, FooterValid: true, PrefixEnd: AtEnd(),
				Stats: &StatsWant{Available: true},
			},
		},
		{
			ID: "basic-codec-none", Title: "data, control, transport-event and annotation records in three blocks, codec none",
			Cites: []string{"FMT §16", "FMT §2", "FMT §13", "SEM §7.4"}, Class: ClassRead, IdentitySeed: codecSeed,
			Build: func(seed string) (*Built, error) {
				pack, err := codecPack(seed, tracepack.CodecNone)

				return &Built{Pack: pack, Queries: codecQueries()}, err
			},
			Expect: codecExpect(codecQueryWants()),
		},
		{
			ID: "basic-codec-zstd", Title: "the records of basic-codec-none, blocks and footer of codec zstd",
			Cites: []string{"FMT §16", "FMT §2", "FMT §13"}, Class: ClassRead, IdentitySeed: codecSeed,
			Codec: CodecZstd, EncoderMade: true,
			Build: func(seed string) (*Built, error) {
				pack, err := codecPack(seed, tracepack.CodecZstd)

				return &Built{Pack: pack}, err
			},
			Expect: codecExpect(nil),
		},
		{
			ID: "basic-codec-mixed", Title: "the records of basic-codec-none, the middle block of codec zstd",
			Cites: []string{"FMT §16", "FMT §2", "FMT §13"}, Class: ClassRead, IdentitySeed: codecSeed,
			Codec: CodecMixed, EncoderMade: true,
			Build: func(seed string) (*Built, error) {
				return editedCodecPack(seed, func(pack []byte) ([]byte, error) {
					return PatchBody(pack, 1, func(b *Body) error { b.Codec = uint8(tracepack.CodecZstd); return nil })
				})
			},
			Expect: codecExpect(nil),
		},
		{
			ID: "basic-truncated-tail", Title: "a pack cut inside its last block",
			Cites: []string{"FMT §13", "FMT I-1", "FMT I-5"}, Class: ClassRead, Labels: []string{LabelDamaged},
			Build: func(seed string) (*Built, error) {
				return editedCodecPack(seed, func(pack []byte) ([]byte, error) {
					l, err := Locate(pack)
					if err != nil {
						return nil, err
					}

					return pack[:l.Blocks[2].Offset+format.EnvelopeLen+10], nil
				})
			},
			Expect: &Expectation{
				Outcome: tracepack.OutcomeUnfinalized, Unfinalized: true, Blocks: 2, Seqs: seqRange(0, 7),
				WalkStop: AtEnd(), PrefixEnd: AtEnd(),
			},
		},
		{
			ID: "basic-corrupt-middle", Title: "a middle block failing its body CRC, the footer valid",
			Cites: []string{"FMT §13", "FMT I-2", "CORPUS §5.4"}, Class: ClassRead, Labels: []string{LabelDamaged},
			Build: func(seed string) (*Built, error) {
				b, err := editedCodecPack(seed, func(pack []byte) ([]byte, error) {
					l, err := Locate(pack)
					if err != nil {
						return nil, err
					}

					return FlipByte(pack, int(l.Blocks[1].Offset)+format.EnvelopeLen+3)
				})
				if err == nil {
					b.Queries = []QuerySpec{allQuery(), {ID: "kinds-data", Cites: []string{"SEM §7.4"}, Filter: Filter{Kinds: []string{"data"}}}}
				}

				return b, err
			},
			Expect: middleFailedExpect(tracepack.ReasonCorruptBlock, []QueryWant{
				{ID: "all", Seqs: append(seqRange(0, 3), seqRange(8, 11)...), Incomplete: []IncompleteWant{BlockDefect(tracepack.ReasonCorruptBlock, 1)}},
				{ID: "kinds-data", Seqs: []uint64{3, 10, 11}, Incomplete: []IncompleteWant{BlockDefect(tracepack.ReasonCorruptBlock, 1)}},
			}),
		},
		{
			ID: "basic-bad-envelope-crc-indexed", Title: "a middle block failing its envelope CRC, passed by its F-2 entry",
			Cites: []string{"FMT §13", "FMT §6", "FMT §10"}, Class: ClassRead, Labels: []string{LabelDamaged},
			Build: func(seed string) (*Built, error) {
				return editedCodecPack(seed, func(pack []byte) ([]byte, error) {
					return flipEnvelopeCRC(pack, 1)
				})
			},
			Expect: middleFailedExpect(tracepack.ReasonCorruptBlock, nil),
		},
		{
			ID: "basic-bad-envelope-crc-walked", Title: "a middle block failing its envelope CRC in a finalized pack whose footer is rejected",
			Cites: []string{"FMT §13", "FMT §6", "CORPUS §5.4"}, Class: ClassRead, Labels: []string{LabelDamaged},
			Build: func(seed string) (*Built, error) {
				b, err := editedCodecPack(seed, func(pack []byte) ([]byte, error) {
					// F-5's record_count one above the records: a footer that passes its CRC and fails validation.
					rejected, err := PatchFooter(pack, func(d []byte) ([]byte, error) {
						f5, err := F5List(d)
						if err != nil {
							return nil, err
						}

						return d, SetU64(d, f5, f5RecordCountTag, 13)
					})
					if err != nil {
						return nil, err
					}

					return flipEnvelopeCRC(rejected, 1)
				})
				if err == nil {
					b.Queries = []QuerySpec{allQuery()}
				}

				return b, err
			},
			// The walk stops at the block, so the trailer's totals are not compared (the tracepack format specification §13),
			// and the query reports the walk stop, the first case of the tracepack corpus specification §5.4.
			Expect: &Expectation{
				Outcome: tracepack.OutcomeFinalizedTruncated, Blocks: 1, Seqs: seqRange(0, 3),
				WalkStop: AtBlock(1), PrefixEnd: AtBlock(1),
				Queries: []QueryWant{{ID: "all", Seqs: seqRange(0, 3), Incomplete: []IncompleteWant{Truncated(AtBlock(1))}}},
			},
		},
		{
			ID: "basic-unknown-codec", Title: "a middle block of a codec outside the registry, its CRCs and F-2 entry valid",
			Cites: []string{"FMT §2", "FMT §13"}, Class: ClassRead, Codec: CodecMixed, Labels: []string{LabelNonconformingWriter},
			Build: func(seed string) (*Built, error) {
				b, err := editedCodecPack(seed, func(pack []byte) ([]byte, error) {
					return PatchEnvelope(pack, 1, func(env []byte) { env[envelopeCodecOff] = unknownCodec })
				})
				if err == nil {
					b.Queries = []QuerySpec{allQuery()}
				}

				return b, err
			},
			Expect: middleFailedExpect(tracepack.ReasonUnknownCodec, []QueryWant{
				{ID: "all", Seqs: append(seqRange(0, 3), seqRange(8, 11)...), Incomplete: []IncompleteWant{BlockDefect(tracepack.ReasonUnknownCodec, 1)}},
			}),
		},
		{
			ID: "basic-unknown-tags-and-enums", Title: "unknown and private tags and unknown enum values in the metadata, record headers and event bodies",
			Cites: []string{"FMT §5", "FMT §9", "JSONL §3", "JSONL §4"}, Class: ClassRead,
			Build: unknownTagsPack,
			Expect: &Expectation{
				Outcome: tracepack.OutcomeFinalizedConsistent, FooterValid: true, Blocks: 1, Seqs: seqRange(0, 3), PrefixEnd: AtEnd(),
				ExportHas: []string{
					`"vantage":"unknown(200)"`, `"lifecycle_coverage":"unknown(77)"`,
					`{"tag":256,"value_type":32,"value":"AQID"}`, `{"tag":256,"value_type":7,"value":"BA=="}`,
					`{"tag":32769,"value_type":6,"value":"cHJpdmF0ZQ=="}`,
					`"kind":"unknown(9)"`, `"dir":"unknown(9)"`, `"fidelity":"unknown(9)"`, `"decode_status":"unknown(99)"`,
					`"event":"unknown(99)"`, `"cur_state":"unknown(77)"`, `"cause":"unknown(200)"`,
					`{"tag":3840,"value_type":7,"value":"CQ=="}`, `{"tag":32770,"value_type":1,"value":"BQ=="}`,
					`"annotation_kind":"unknown(99)"`, `{"tag":3841,"value_type":48,"value":"Bw=="}`,
				},
			},
		},
		{
			ID: "basic-unordered-timestamps", Title: "ts_utc_ns not ascending within and across blocks",
			Cites: []string{"FMT §16", "FMT §10", "SEM §4"}, Class: ClassRead,
			Build: func(seed string) (*Built, error) {
				rec := func(seq uint64, ms int64) tracepack.Record {
					r := newData(seq, 1, tracepack.DirHostToEquipment, dataFrame(1, 1, true, uint32(seq), nil))
					r.TSUTCNs = msAt(ms)

					return r
				}
				pack, err := writeSpec(&packSpec{seed: seed, meta: segmentMeta(seed), blocks: [][]tracepack.Record{
					{rec(0, 5), rec(1, 2)}, {rec(2, 1), rec(3, 9)},
				}})

				return &Built{Pack: pack}, err
			},
			Expect: &Expectation{
				Outcome: tracepack.OutcomeFinalizedConsistent, FooterValid: true, Blocks: 2, Seqs: seqRange(0, 3), PrefixEnd: AtEnd(),
			},
		},
		maxIntVector(),
		{
			ID: "basic-max-int-extract", Title: "an extract whose records lie at both i64 extremes of ts_utc_ns",
			Cites: []string{"FMT §2", "CORPUS §8", "STO §2"}, Class: ClassRead,
			Build: func(seed string) (*Built, error) {
				meta := segmentMeta(seed)
				meta.PackRole, meta.ScopeGeneration, meta.ExtractFilter = tracepack.PackRoleExtract, nil, new("all records")
				meta.PeriodStart, meta.PeriodEnd = math.MinInt64, math.MaxInt64
				rec := func(seq uint64, ts int64) tracepack.Record {
					r := newData(seq, 1, tracepack.DirHostToEquipment, dataFrame(1, 1, true, uint32(seq), nil))
					r.TSUTCNs = ts

					return r
				}
				// The Writer closes a block at each change of UTC hour (I-13), so the records take one block each.
				pack, err := writeSpec(&packSpec{seed: seed, meta: meta, blocks: [][]tracepack.Record{
					{rec(0, math.MinInt64), rec(1, TimeBase), rec(2, math.MaxInt64)},
				}})

				return &Built{Pack: pack}, err
			},
			Expect: &Expectation{
				Outcome: tracepack.OutcomeFinalizedConsistent, FooterValid: true, Blocks: 3, Seqs: seqRange(0, 2), PrefixEnd: AtEnd(),
				ExportHas: []string{`"ts_utc_ns":"-9223372036854775808"`, `"ts_utc_ns":"9223372036854775807"`},
			},
		},
		{
			ID: "basic-seq-over-limit", Title: "a last block whose first seq is 2^63, its CRCs valid",
			Cites: []string{"FMT §2", "FMT §13", "CORPUS §8"}, Class: ClassRead, Labels: []string{LabelNonconformingWriter},
			Build: func(seed string) (*Built, error) {
				pack, err := writeSpec(&packSpec{seed: seed, meta: segmentMeta(seed), blocks: [][]tracepack.Record{
					{newData(0, 1, tracepack.DirHostToEquipment, dataFrame(1, 1, true, 0, nil)),
						newData(1, 1, tracepack.DirEquipmentToHost, dataFrame(1, 2, false, 0, nil))},
					{newData(2, 1, tracepack.DirHostToEquipment, dataFrame(1, 13, true, 1, nil))},
				}})
				if err != nil {
					return nil, err
				}
				// The record's seq and the envelope's first_seq become 2^63; the footer keeps the seqs it states.
				over := uint64(maxU64) + 1
				pack, err = PatchBody(pack, 1, func(b *Body) error {
					binary.LittleEndian.PutUint64(b.Rows, over)
					return nil
				})
				if err != nil {
					return nil, err
				}
				pack, err = PatchEnvelope(pack, 1, func(env []byte) { binary.LittleEndian.PutUint64(env[envelopeFirstSeqOff:], over) })

				return &Built{Pack: pack}, err
			},
			// The envelope's first_seq is outside the format's limits, a decode error (the tracepack format specification §2),
			// and the envelope also disagrees with its F-2 entry, which keeps first_seq 2:
			// the vector checks that such a block fails, not which check fails it.
			// Its F-2 entry and F-3 summary give the lost range.
			Expect: &Expectation{
				Outcome: tracepack.OutcomeFinalizedTruncated, FooterValid: true, Blocks: 2, Seqs: seqRange(0, 1),
				Failed: []FailedWant{{Block: 1, Cause: tracepack.ReasonCorruptBlock}}, PrefixEnd: AtBlock(1),
				Lost: []LostWant{{First: 2, Last: 2, TimeStart: msAt(2), TimeEnd: msAt(2)}},
			},
		},
		{
			ID: uuidVector, Title: "a pack whose pack_id is the UUID of primitives.json",
			Cites: []string{"FMT §2", "CORPUS §5.1"}, Class: ClassRead,
			Build: func(seed string) (*Built, error) {
				pack, err := writeSpec(&packSpec{seed: seed, meta: segmentMeta(seed), packID: uuidByteOrder, blocks: [][]tracepack.Record{
					{newData(0, 1, tracepack.DirHostToEquipment, dataFrame(1, 1, true, 1, nil))},
				}})

				return &Built{Pack: pack}, err
			},
			Expect: &Expectation{
				Outcome: tracepack.OutcomeFinalizedConsistent, FooterValid: true, Blocks: 1, Seqs: []uint64{0}, PrefixEnd: AtEnd(),
				ExportHas: []string{`"pack_id":"0f1e2d3c-4b5a-7968-8796-a5b4c3d2e1f0"`},
			},
		},
	}
}

// corpusPrimitives returns primitives.json:
// the CRC rows of the file header and the first block envelope of basic-uuid-byte-order's pack,
// and the UUID rows, that pack's pack_id among them.
func corpusPrimitives(files map[string][]byte) (Primitives, error) {
	pack, ok := files[uuidVector+"/"+FilePack]
	if !ok {
		return Primitives{}, fmt.Errorf("corpus: primitives.json needs the pack of %s", uuidVector)
	}
	l, err := Locate(pack)
	if err != nil || len(l.Blocks) == 0 {
		return Primitives{}, fmt.Errorf("corpus: %s holds no block envelope: %w", uuidVector, err)
	}
	env := pack[l.Blocks[0].Offset : l.Blocks[0].Offset+format.EnvelopeLen]

	return NewPrimitives(pack[:format.FileHeaderLen], env, []PrimitiveUUID{
		{UUID: uuidByteOrder, Pack: uuidVector},
		{UUID: tracepack.UUID{1, 2, 3, 4, 5, 6, 7, 8, 9, 10, 11, 12, 13, 14, 15, 16}},
	})
}

// seqRange returns the seqs first to last.
func seqRange(first, last uint64) []uint64 {
	var out []uint64
	for s := first; s <= last; s++ {
		out = append(out, s)
	}

	return out
}

// allQuery is the query that selects every record.
func allQuery() QuerySpec {
	return QuerySpec{ID: "all", Cites: []string{"SEM §7.4", "CORPUS §5.4"}}
}

// codecBlocks returns the records of the codec vectors, three blocks of epochs 1 and 2:
// a connection's events, HSMS control and data messages and an annotation,
// the connection closed and another opened.
func codecBlocks() ([][]tracepack.Record, error) {
	h2e, e2h := tracepack.DirHostToEquipment, tracepack.DirEquipmentToHost
	emptyList := []byte{0x01, 0x00}
	ev := func(seq uint64, epoch uint32, e tracepack.Event) func() (tracepack.Record, error) {
		return func() (tracepack.Record, error) { return newSocketEvent(seq, epoch, e) }
	}
	fixed := func(r tracepack.Record) func() (tracepack.Record, error) {
		return func() (tracepack.Record, error) { return r, nil }
	}
	spec := [][]func() (tracepack.Record, error){
		{
			ev(0, 1, tracepack.EventSocketConnect),
			fixed(newControl(1, 1, h2e, controlFrame(stypeSelectReq, 0, 0, 1))),
			fixed(newControl(2, 1, e2h, controlFrame(stypeSelectRsp, 0, 0, 1))),
			fixed(newData(3, 1, h2e, dataFrame(1, 1, true, 2, nil))),
		},
		{
			fixed(newData(4, 1, e2h, dataFrame(1, 2, false, 2, emptyList))),
			func() (tracepack.Record, error) { return newNote(5, 1, "operator note") },
			fixed(newData(6, 1, e2h, dataFrame(6, 11, true, 3, emptyList))),
			fixed(newData(7, 1, h2e, dataFrame(6, 12, false, 3, []byte{0x21, 0x01, 0x00}))),
		},
		{
			ev(8, 1, tracepack.EventSocketClose),
			ev(9, 2, tracepack.EventSocketConnect),
			fixed(newData(10, 2, h2e, dataFrame(1, 13, true, 4, emptyList))),
			fixed(newData(11, 2, e2h, dataFrame(1, 14, false, 4, []byte{0x01, 0x02, 0x21, 0x01, 0x00, 0x01, 0x00}))),
		},
	}

	out := make([][]tracepack.Record, 0, len(spec))
	for _, b := range spec {
		var recs []tracepack.Record
		for _, mk := range b {
			r, err := mk()
			if err != nil {
				return nil, err
			}
			recs = append(recs, r)
		}
		out = append(out, recs)
	}

	return out, nil
}

// codecPack writes the finalized pack of codecBlocks with codec c.
func codecPack(seed string, c tracepack.Codec) ([]byte, error) {
	blocks, err := codecBlocks()
	if err != nil {
		return nil, err
	}

	return writeSpec(&packSpec{seed: seed, meta: segmentMeta(seed), codec: c, blocks: blocks})
}

// editedCodecPack returns the codec none pack of codecBlocks after edit.
func editedCodecPack(seed string, edit func(pack []byte) ([]byte, error)) (*Built, error) {
	pack, err := codecPack(seed, tracepack.CodecNone)
	if err != nil {
		return nil, err
	}
	if pack, err = edit(pack); err != nil {
		return nil, err
	}

	return &Built{Pack: pack}, nil
}

// codecQueries are the queries of basic-codec-none: by time, kind, direction and epoch.
func codecQueries() []QuerySpec {
	cites := []string{"SEM §7.4"}

	return []QuerySpec{
		allQuery(),
		{ID: "time-window", Cites: cites, Filter: Filter{TimeFrom: new(I64(msAt(3))), TimeTo: new(I64(msAt(7)))}},
		{ID: "kinds-control-and-events", Cites: cites, Filter: Filter{Kinds: []string{"control", "transport-event"}}},
		{ID: "dir-equipment-to-host", Cites: cites, Filter: Filter{Dirs: []string{"equipment-to-host"}}},
		{ID: "epoch-2", Cites: cites, Filter: Filter{Epochs: []uint32{2}}},
	}
}

// codecQueryWants are the results of codecQueries: each a complete read.
// The time window is half-open: msAt(3) is in, msAt(7) out.
func codecQueryWants() []QueryWant {
	return []QueryWant{
		{ID: "all", Seqs: seqRange(0, 11)},
		{ID: "time-window", Seqs: seqRange(3, 6)},
		{ID: "kinds-control-and-events", Seqs: []uint64{0, 1, 2, 8, 9}},
		{ID: "dir-equipment-to-host", Seqs: []uint64{2, 4, 6, 11}},
		{ID: "epoch-2", Seqs: seqRange(9, 11)},
	}
}

// codecExpect is the expectation of a codec vector, every block validated, with queries.
// Epoch 1 is ended by the socket-close at seq 8, and no record ends epoch 2 (the tracepack format specification §10).
func codecExpect(queries []QueryWant) *Expectation {
	return &Expectation{
		Outcome: tracepack.OutcomeFinalizedConsistent, FooterValid: true, Blocks: 3, Seqs: seqRange(0, 11), PrefixEnd: AtEnd(),
		Stats:   &StatsWant{Available: true, Epochs: []EpochWant{{Epoch: 1, CloseSeq: new(uint64(8))}, {Epoch: 2}}},
		Queries: queries,
	}
}

// middleFailedExpect is the expectation of the codec pack whose middle block fails with cause, the footer valid:
// its records are lost, and the footer's F-3 summary gives their range.
func middleFailedExpect(cause tracepack.IncompleteReason, queries []QueryWant) *Expectation {
	return &Expectation{
		Outcome: tracepack.OutcomeCorruptMiddle, FooterValid: true, Blocks: 3, Seqs: append(seqRange(0, 3), seqRange(8, 11)...),
		Failed: []FailedWant{{Block: 1, Cause: cause}}, PrefixEnd: AtBlock(1),
		Lost:    []LostWant{{First: 4, Last: 7, TimeStart: msAt(4), TimeEnd: msAt(7)}},
		Queries: queries,
	}
}

// flipEnvelopeCRC returns pack with block i's stored envelope_crc inverted in its first byte.
func flipEnvelopeCRC(pack []byte, i int) ([]byte, error) {
	l, err := Locate(pack)
	if err != nil {
		return nil, err
	}
	s, err := l.block(i)
	if err != nil {
		return nil, err
	}

	return FlipByte(pack, int(s.Offset)+blockEnvelopeCRCOff)
}

// unknownTagsPack builds basic-unknown-tags-and-enums:
// metadata with unknown enum values, unknown tags of a known and an unknown value type, one repeated, and a private tag;
// a record of each unknown header enum;
// a transport event and an annotation with unknown enum values, unknown tags and a private tag.
func unknownTagsPack(seed string) (*Built, error) {
	meta := segmentMeta(seed)
	meta.Vantage, meta.LifecycleCoverage = 200, 77
	meta.Classifiers = []string{corpusClassifier}
	meta.Unknown = []tracepack.RawEntry{
		{Tag: 0x0100, Type: 0x20, Value: []byte{1, 2, 3}},
		{Tag: 0x0100, Type: 7, Value: []byte{4}},
		{Tag: 0x8001, Type: 6, Value: []byte("private")},
	}

	opaque := newData(0, 1, tracepack.DirHostToEquipment, []byte("opaque"))
	opaque.Kind, opaque.FieldValidity = 9, 0
	odd := newData(1, 1, 9, dataFrame(1, 1, true, 1, nil))
	odd.Fidelity, odd.DecodeStatus = 9, 99
	ev, err := newEvent(2, 1, &tracepack.TransportEvent{
		Event: 99, CurState: new(tracepack.State(77)), Cause: new(tracepack.Cause(200)),
		Unknown: []tracepack.RawEntry{{Tag: 0x0F00, Type: 7, Value: []byte{9}}, {Tag: 0x8002, Type: 1, Value: []byte{5}}},
	})
	if err != nil {
		return nil, err
	}
	note, err := newAnnotation(3, 1, &tracepack.Annotation{
		AnnotationKind: 99, Text: new("an annotation of an unknown kind"),
		Unknown: []tracepack.RawEntry{{Tag: 0x0F01, Type: 0x30, Value: []byte{7}}},
	})
	if err != nil {
		return nil, err
	}

	pack, err := writeSpec(&packSpec{seed: seed, meta: meta, blocks: [][]tracepack.Record{{opaque, odd, ev, note}}})

	return &Built{Pack: pack}, err
}

// maxIntVector returns basic-max-int: the at-limit values of the tracepack corpus specification §8 but ts_utc_ns.
// Its seqs end at 2^63-1, its records are of epoch 2^32-1 with mono_ns at both i64 extremes and trailing_bytes 2^32-1,
// clock-step events carry clock_step_ns and gap boundaries gap_start at both i64 extremes,
// and the metadata's device_id, clock_step_tolerance_ns and max_frame_len are 2^63-1.
func maxIntVector() Recipe {
	const first = maxU64 - 5
	h2e := tracepack.DirHostToEquipment

	return Recipe{
		ID: "basic-max-int", Title: "seqs, epochs, mono_ns, trailing_bytes, event and metadata values at the format's limits",
		Cites: []string{"FMT §2", "CORPUS §8"}, Class: ClassRead,
		Build: func(seed string) (*Built, error) {
			meta := segmentMeta(seed)
			meta.TimeSource = tracepack.TimeSourceCaptureClock
			meta.CaptureOriginUTCNs, meta.CaptureOriginMonoNs = new(TimeBase), new(int64(0))
			meta.ClockStepToleranceNs, meta.DeviceID = new(uint64(maxU64)), new(uint64(maxU64))
			meta.MaxFrameLens = []uint64{maxU64}
			meta.Classifiers = []string{corpusClassifier}
			meta.SeqStart = first

			low := newData(first, math.MaxUint32, h2e, dataFrame(1, 1, true, 1, nil))
			low.MonoPresent, low.MonoNs = true, math.MinInt64
			low.DecodeStatus, low.TrailingBytes = tracepack.DecodeStatusOKWithTrailing, math.MaxUint32
			high := newData(first+1, math.MaxUint32, h2e, dataFrame(1, 1, true, 2, nil))
			high.MonoPresent, high.MonoNs = true, math.MaxInt64
			events := []*tracepack.TransportEvent{
				{Event: tracepack.EventClockStep, ClockStepNs: new(int64(math.MinInt64))},
				{Event: tracepack.EventClockStep, ClockStepNs: new(int64(math.MaxInt64))},
				{Event: tracepack.EventCaptureBoundary, BoundaryKind: new(tracepack.BoundaryKindGap), GapStart: new(int64(math.MinInt64)), GapEnd: new(TimeBase)},
				{Event: tracepack.EventCaptureBoundary, BoundaryKind: new(tracepack.BoundaryKindGap), GapStart: new(int64(math.MaxInt64)), GapEnd: new(int64(math.MaxInt64))},
			}
			recs := []tracepack.Record{low, high}
			for k, e := range events {
				r, err := newEvent(first+2+uint64(k), math.MaxUint32, e)
				if err != nil {
					return nil, err
				}
				recs = append(recs, r)
			}
			for k := range recs {
				// Timestamps follow the record's place, not its seq, which is near 2^63.
				recs[k].TSUTCNs = msAt(int64(k))
			}
			pack, err := writeSpec(&packSpec{seed: seed, meta: meta, blocks: [][]tracepack.Record{recs[:3], recs[3:]}})

			return &Built{Pack: pack}, err
		},
		Expect: &Expectation{
			Outcome: tracepack.OutcomeFinalizedConsistent, FooterValid: true, Blocks: 2, Seqs: seqRange(first, maxU64), PrefixEnd: AtEnd(),
			Stats: &StatsWant{
				Available: true, Epochs: []EpochWant{{Epoch: math.MaxUint32}},
				Boundaries: []BoundaryWant{{Seq: first + 4, Kind: tracepack.BoundaryKindGap}, {Seq: maxU64, Kind: tracepack.BoundaryKindGap}},
			},
			ExportHas: []string{
				`"seq":"9223372036854775807"`, `"epoch":4294967295`, `"trailing_bytes":4294967295`,
				`"mono_ns":"-9223372036854775808"`, `"mono_ns":"9223372036854775807"`,
				`"clock_step_ns":"-9223372036854775808"`, `"clock_step_ns":"9223372036854775807"`,
				`"gap_start":"-9223372036854775808"`, `"gap_start":"9223372036854775807"`,
				`"device_id":"9223372036854775807"`, `"clock_step_tolerance_ns":"9223372036854775807"`,
				`"max_frame_len":["9223372036854775807"]`,
			},
		},
	}
}
