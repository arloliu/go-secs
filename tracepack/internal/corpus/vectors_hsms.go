package corpus

import (
	"encoding/binary"
	"fmt"
	"slices"

	"github.com/arloliu/go-secs/tracepack"
)

// Names of the field_validity bits fields.json lists (the tracepack JSONL specification §3.2).
const (
	fieldSessionID   = "session_id"
	fieldStreamAndW  = "stream_and_w"
	fieldFunction    = "function"
	fieldPType       = "ptype"
	fieldSType       = "stype"
	fieldSystemBytes = "system_bytes"
)

// Field lists of fields.json rows: every field but System Bytes, and every field.
var (
	headFields = []string{fieldSessionID, fieldStreamAndW, fieldFunction, fieldPType, fieldSType}
	allFields  = []string{fieldSessionID, fieldStreamAndW, fieldFunction, fieldPType, fieldSType, fieldSystemBytes}
)

// hsmsCites are the clauses every vector of the hsms group exercises.
var hsmsCites = []string{"FMT §7.2", "FMT §16", "SEM §7.4", "CORPUS §5.4", "CORPUS §5.5"}

// headerValues are the values of the HSMS header fields of a fields.json row.
type headerValues struct {
	session                uint16
	stream                 uint8
	w                      bool
	function, ptype, stype uint8
	systemBytes            uint32
}

// headerFields returns the fields.json row of seq listing the fields names, in this order,
// and the values v of exactly those fields.
func headerFields(seq uint64, v headerValues, names ...string) Fields {
	out := Fields{Seq: U64(seq), Available: append([]string{}, names...)}
	for _, n := range names {
		switch n {
		case fieldSessionID:
			out.SessionID = new(v.session)
		case fieldStreamAndW:
			out.Stream, out.W = new(v.stream), new(v.w)
		case fieldFunction:
			out.Function = new(v.function)
		case fieldPType:
			out.PType = new(v.ptype)
		case fieldSType:
			out.SType = new(v.stype)
		case fieldSystemBytes:
			out.SystemBytes = systemBytes(v.systemBytes)
		default:
			// A name of no field is listed as it is, and the comparison with the read fails.
		}
	}

	return out
}

// systemBytes returns the 4 System Bytes of v, as a filter states them.
func systemBytes(v uint32) []byte {
	return binary.BigEndian.AppendUint32(nil, v)
}

// hsmsQuery is a query of the hsms group.
func hsmsQuery(id string, f Filter) QuerySpec {
	return QuerySpec{ID: id, Cites: []string{"FMT §7.2", "SEM §7.4", "CORPUS §5.4"}, Filter: f}
}

// hsmsBuild returns the Build of a pack of metadata meta and one block holding records, with fields.json and the queries qs.
func hsmsBuild(meta func(seed string) *tracepack.PackMeta, records func() ([]tracepack.Record, error), qs ...QuerySpec) func(seed string) (*Built, error) {
	return func(seed string) (*Built, error) {
		recs, err := records()
		if err != nil {
			return nil, err
		}
		pack, err := writeSpec(&packSpec{seed: seed, meta: meta(seed), blocks: [][]tracepack.Record{recs}})
		if err != nil {
			return nil, err
		}

		return &Built{Pack: pack, Queries: qs, Fields: true}, nil
	}
}

// hsmsExpect returns the expectation of a finalized-consistent pack of one block exporting seqs,
// with its fields.json rows and query results.
func hsmsExpect(seqs []uint64, fields []Fields, queries ...QueryWant) *Expectation {
	return &Expectation{
		Outcome: tracepack.OutcomeFinalizedConsistent, FooterValid: true, Blocks: 1, Seqs: seqs, PrefixEnd: AtEnd(),
		Fields: fields, Queries: queries,
	}
}

// shortFrameValues are the header values of the frame every short capture of hsms-short-captures is cut from:
// S1F1 with W, SessionID 1.
var shortFrameValues = headerValues{session: 1, stream: 1, w: true, function: 1, systemBytes: 0x0A0B0C0D}

// hsmsVectors returns the recipes of the hsms group (the tracepack corpus specification §9.4).
func hsmsVectors() []Recipe {
	return []Recipe{
		shortCapturesVector(),
		systemBytesUnavailableVector(),
		systemBytesSetShortVector(),
		controlFramesVector(),
		predicatesVector(),
	}
}

// shortCapturesVector is hsms-short-captures: data records whose payloads are the first 5 to 14 bytes of one frame,
// at seqs 0 to 9, each with the field_validity of its captured bytes.
func shortCapturesVector() Recipe {
	cases := make([]Case, 0, 10)
	for n := 5; n <= 14; n++ {
		cases = append(cases, Case{ID: fmt.Sprintf("payload-%d-bytes", n), Seq: U64(n - 5), Cites: []string{"FMT §7.2"}})
	}
	v := shortFrameValues

	return Recipe{
		ID: "hsms-short-captures", Title: "data records of 5 to 14 captured bytes, each field available exactly when its bit is set",
		Cites: hsmsCites, Class: ClassRead, Cases: cases,
		Build: hsmsBuild(segmentMeta, func() ([]tracepack.Record, error) {
			frame := dataFrame(v.stream, v.function, v.w, v.systemBytes, nil)
			recs := make([]tracepack.Record, 0, 10)
			for n := 5; n <= 14; n++ {
				recs = append(recs, newData(uint64(n-5), 1, tracepack.DirHostToEquipment, frame[:n]))
			}

			return recs, nil
		},
			hsmsQuery("include-unavailable", Filter{
				SF: []SF{{Stream: 1, Function: 1}}, SessionIDs: []uint16{1}, SystemBytes: systemBytes(v.systemBytes), IncludeUnavailable: true,
			}),
			hsmsQuery("session-ids", Filter{SessionIDs: []uint16{1}}),
			hsmsQuery("sf", Filter{SF: []SF{{Stream: 1, Function: 1}}}),
			hsmsQuery("system-bytes", Filter{SystemBytes: systemBytes(v.systemBytes)}),
		),
		// A field is available iff the payload ends at or after its last byte (the tracepack format specification §7.2):
		// SessionID from 6 bytes, stream and W from 7, function from 8, PType from 9, SType from 10, System Bytes at 14.
		// Every available field holds the frame's value, so each predicate selects exactly the records where its fields are available,
		// and with the request for unavailable fields every record.
		Expect: hsmsExpect(seqRange(0, 9), []Fields{
			headerFields(0, v),
			headerFields(1, v, fieldSessionID),
			headerFields(2, v, fieldSessionID, fieldStreamAndW),
			headerFields(3, v, fieldSessionID, fieldStreamAndW, fieldFunction),
			headerFields(4, v, fieldSessionID, fieldStreamAndW, fieldFunction, fieldPType),
			headerFields(5, v, headFields...),
			headerFields(6, v, headFields...),
			headerFields(7, v, headFields...),
			headerFields(8, v, headFields...),
			headerFields(9, v, allFields...),
		},
			QueryWant{ID: "include-unavailable", Seqs: seqRange(0, 9)},
			QueryWant{ID: "session-ids", Seqs: seqRange(1, 9)},
			QueryWant{ID: "sf", Seqs: seqRange(3, 9)},
			QueryWant{ID: "system-bytes", Seqs: []uint64{9}},
		),
	}
}

// systemBytesUnavailableVector is hsms-system-bytes-unavailable, a log conversion
// (the tracepack semantics specification §2 and the tracepack storage specification §7):
// at seq 0 a primary whose entry carried no System Bytes, its payload holding the placeholder bytes 0,
// its System Bytes bit clear;
// at seq 1 another entry's message, whose System Bytes, 0, the source carried.
// The source has no connect or accept line, so both records are of epoch 0,
// and both are correlation-incomplete (the tracepack storage specification §7).
func systemBytesUnavailableVector() Recipe {
	primary := headerValues{session: 1, stream: 1, w: true, function: 1}
	carried := headerValues{session: 1, stream: 1, function: 2}

	return Recipe{
		ID: "hsms-system-bytes-unavailable", Title: "a data record whose System Bytes bit is clear while its payload holds them",
		Cites: slices.Concat(hsmsCites, []string{"SEM §2", "STO §7"}), Class: ClassRead,
		Build: hsmsBuild(logMeta, func() ([]tracepack.Record, error) {
			return []tracepack.Record{
				newLogData(0, 0, tracepack.DirHostToEquipment, dataFrame(1, 1, true, 0, nil), tracepack.FieldValiditySystemBytes),
				newLogData(1, 0, tracepack.DirEquipmentToHost, dataFrame(1, 2, false, 0, nil), 0),
			}, nil
		},
			hsmsQuery("system-bytes", Filter{SystemBytes: systemBytes(0)}),
			hsmsQuery("system-bytes-include-unavailable", Filter{SystemBytes: systemBytes(0), IncludeUnavailable: true}),
		),
		// A clear bit leaves the field unavailable although the payload holds its bytes (the tracepack format specification §7.2),
		// so the primary's placeholder bytes are matched only on request; the carried System Bytes are.
		Expect: func() *Expectation {
			e := hsmsExpect(seqRange(0, 1), []Fields{
				headerFields(0, primary, headFields...),
				headerFields(1, carried, allFields...),
			},
				QueryWant{ID: "system-bytes", Seqs: []uint64{1}},
				QueryWant{ID: "system-bytes-include-unavailable", Seqs: seqRange(0, 1)},
			)
			e.ExportHas = []string{
				`"capture_method":"log","vantage":"host"`,
				`"epoch":0,"payload_len":14,"trailing_bytes":0,` +
					`"quality":["correlation-incomplete"],"kind":"data","dir":"host-to-equipment","fidelity":"reconstructed",` +
					`"decode_status":"reconstructed-ok","field_validity":["session_id","stream_and_w","function","ptype","stype"]`,
				`"epoch":0,"payload_len":14,"trailing_bytes":0,` +
					`"quality":["correlation-incomplete"],"kind":"data","dir":"equipment-to-host","fidelity":"reconstructed",` +
					`"decode_status":"reconstructed-ok","field_validity":["session_id","stream_and_w","function","ptype","stype","system_bytes"]`,
			}

			return e
		}(),
	}
}

// systemBytesSetShortVector is hsms-system-bytes-set-short:
// at seq 0 the first 10 bytes of a primary whose System Bytes are 0, ending before them, its System Bytes bit set by raw surgery;
// at seq 1 its whole reply.
// The query asks for System Bytes 0, so a reader that read the missing bytes as 0 would match the primary.
func systemBytesSetShortVector() Recipe {
	primary := headerValues{session: 1, stream: 1, w: true, function: 1}
	reply := headerValues{session: 1, stream: 1, function: 2}

	return Recipe{
		ID: "hsms-system-bytes-set-short", Title: "a data record whose System Bytes bit is set while its payload ends before them",
		Cites: slices.Concat(hsmsCites, []string{"FMT §13"}), Class: ClassRead, Labels: []string{LabelNonconformingWriter},
		Build: func(seed string) (*Built, error) {
			b, err := hsmsBuild(segmentMeta, func() ([]tracepack.Record, error) {
				return []tracepack.Record{
					newData(0, 1, tracepack.DirHostToEquipment, dataFrame(1, 1, true, 0, nil)[:10]),
					newData(1, 1, tracepack.DirEquipmentToHost, dataFrame(1, 2, false, 0, nil)),
				}, nil
			},
				hsmsQuery("system-bytes", Filter{SystemBytes: systemBytes(0)}),
				hsmsQuery("system-bytes-include-unavailable", Filter{SystemBytes: systemBytes(0), IncludeUnavailable: true}),
			)(seed)
			if err != nil {
				return nil, err
			}
			// The Writer refuses the bit at append; no footer value states field_validity.
			b.Pack, err = PatchBody(b.Pack, 0, func(body *Body) error {
				body.Rows[rowFieldValidityOff] |= byte(tracepack.FieldValiditySystemBytes)
				return nil
			})

			return b, err
		},
		// The field is unavailable whatever its bit says, since the payload lacks its bytes,
		// and verify reports the set bit as a writer defect (the tracepack format specification §7.2 and §13).
		Expect: func() *Expectation {
			e := hsmsExpect(seqRange(0, 1), []Fields{
				headerFields(0, primary, headFields...),
				headerFields(1, reply, allFields...),
			},
				QueryWant{ID: "system-bytes", Seqs: []uint64{1}},
				QueryWant{ID: "system-bytes-include-unavailable", Seqs: seqRange(0, 1)},
			)
			e.WriterDefects = []DefectWant{{Kind: tracepack.WriterDefectFieldValidity, Seq: 0}}

			return e
		}(),
	}
}

// controlFramesVector is hsms-control-frames: a Select.rsp, a Deselect.rsp, a Reject.req and a Linktest.req (SEMI E37 §8.3),
// whose header bytes 2 and 3, payload bytes 6 and 7, carry statuses, a rejected PType and a reason code.
func controlFramesVector() Recipe {
	// The Select.rsp: Communication Already Active (SelectStatus 1).
	selectRsp := headerValues{session: controlSessionID, function: 1, stype: stypeSelectRsp, systemBytes: 0x0A}
	// The Deselect.rsp: Communication Busy (DeselectStatus 2), in session 2.
	deselectRsp := headerValues{session: 2, function: 2, stype: stypeDeselectRsp, systemBytes: 0x0B}
	// The Reject.req of a message of session 1 and PType 0x81: PType Not Supported (reason code 2),
	// byte 6 the rejected PType, read as stream 1 with W.
	rejectReq := headerValues{session: 1, stream: 1, w: true, function: 2, stype: stypeRejectReq, systemBytes: 0x0C}
	linktestReq := headerValues{session: controlSessionID, stype: stypeLinktestReq, systemBytes: 0x0D}

	return Recipe{
		ID: "hsms-control-frames", Title: "control frames whose payload bytes 6 and 7 are read at their positions",
		Cites: slices.Concat(hsmsCites, []string{"CORPUS §8"}), Class: ClassRead,
		Build: hsmsBuild(segmentMeta, func() ([]tracepack.Record, error) {
			h2e, e2h := tracepack.DirHostToEquipment, tracepack.DirEquipmentToHost

			return []tracepack.Record{
				newControl(0, e2h, hsmsFrame(controlSessionID, 0, 1, stypeSelectRsp, 0x0A, nil)),
				newControl(1, e2h, hsmsFrame(2, 0, 2, stypeDeselectRsp, 0x0B, nil)),
				newControl(2, h2e, hsmsFrame(1, 0x81, 2, stypeRejectReq, 0x0C, nil)),
				newControl(3, h2e, hsmsFrame(controlSessionID, 0, 0, stypeLinktestReq, 0x0D, nil)),
			}, nil
		},
			hsmsQuery("session-ids", Filter{SessionIDs: []uint16{controlSessionID}}),
			hsmsQuery("sf-reject", Filter{SF: []SF{{Stream: 1, Function: 2}}}),
			hsmsQuery("sf-statuses", Filter{SF: []SF{{Stream: 0, Function: 1}, {Stream: 0, Function: 2}}}),
			hsmsQuery("sf-zero", Filter{SF: []SF{{Stream: 0, Function: 0}}}),
			hsmsQuery("system-bytes", Filter{SystemBytes: systemBytes(0x0C)}),
		),
		// Every field is read at its position whatever SType says (the tracepack format specification §7.2):
		// the statuses as functions 1 and 2, the Reject.req's rejected PType 0x81 as stream 1 with W and its reason code as function 2.
		Expect: hsmsExpect(seqRange(0, 3), []Fields{
			headerFields(0, selectRsp, allFields...),
			headerFields(1, deselectRsp, allFields...),
			headerFields(2, rejectReq, allFields...),
			headerFields(3, linktestReq, allFields...),
		},
			QueryWant{ID: "session-ids", Seqs: []uint64{0, 3}},
			QueryWant{ID: "sf-reject", Seqs: []uint64{2}},
			QueryWant{ID: "sf-statuses", Seqs: []uint64{0, 1}},
			QueryWant{ID: "sf-zero", Seqs: []uint64{3}},
			QueryWant{ID: "system-bytes", Seqs: []uint64{2}},
		),
	}
}

// predicatesVector is hsms-predicates: data records whose fields match, differ from or are missing for each predicate,
// and an annotation, which has no available field.
//
//	seq 0  S1F1 with W, SessionID 1, System Bytes 1
//	seq 1  S1F2,        SessionID 1, System Bytes 1
//	seq 2  S2F1 with W, SessionID 1, System Bytes 2
//	seq 3  S1F1,        SessionID 2, System Bytes 3
//	seq 4  the first 7 bytes of S2F1 with W, SessionID 1: stream 2 available, function unavailable
//	seq 5  the first 7 bytes of S1F1 with W, SessionID 2: stream 1 available, function unavailable
//	seq 6  the first 5 bytes of a frame: no field available
//	seq 7  an annotation
func predicatesVector() Recipe {
	s1f1 := headerValues{session: 1, stream: 1, w: true, function: 1, systemBytes: 1}
	s1f2 := headerValues{session: 1, stream: 1, function: 2, systemBytes: 1}
	s2f1 := headerValues{session: 1, stream: 2, w: true, function: 1, systemBytes: 2}
	s1f1NoW := headerValues{session: 2, stream: 1, function: 1, systemBytes: 3}
	cut2 := headerValues{session: 1, stream: 2, w: true}
	cut1 := headerValues{session: 2, stream: 1, w: true}

	return Recipe{
		ID: "hsms-predicates", Title: "S/F, SessionID and System Bytes predicates with and without the request for unavailable fields",
		Cites: hsmsCites, Class: ClassRead,
		Build: hsmsBuild(segmentMeta, func() ([]tracepack.Record, error) {
			h2e, e2h := tracepack.DirHostToEquipment, tracepack.DirEquipmentToHost
			note, err := newNote(7, "predicates")
			if err != nil {
				return nil, err
			}

			return []tracepack.Record{
				newData(0, 1, h2e, dataFrame(1, 1, true, 1, nil)),
				newData(1, 1, e2h, dataFrame(1, 2, false, 1, nil)),
				newData(2, 1, h2e, dataFrame(2, 1, true, 2, nil)),
				newData(3, 1, h2e, hsmsFrame(2, 1, 1, stypeData, 3, nil)),
				newData(4, 1, h2e, dataFrame(2, 1, true, 4, nil)[:7]),
				newData(5, 1, h2e, hsmsFrame(2, 0x81, 1, stypeData, 5, nil)[:7]),
				newData(6, 1, h2e, dataFrame(1, 1, true, 6, nil)[:5]),
				note,
			}, nil
		},
			hsmsQuery("session-ids", Filter{SessionIDs: []uint16{1}}),
			hsmsQuery("session-ids-include-unavailable", Filter{SessionIDs: []uint16{1}, IncludeUnavailable: true}),
			hsmsQuery("sf", Filter{SF: []SF{{Stream: 1, Function: 1}}}),
			hsmsQuery("sf-include-unavailable", Filter{SF: []SF{{Stream: 1, Function: 1}}, IncludeUnavailable: true}),
			hsmsQuery("sf-pairs", Filter{SF: []SF{{Stream: 1, Function: 2}, {Stream: 2, Function: 1}}}),
			hsmsQuery("system-bytes", Filter{SystemBytes: systemBytes(1)}),
			hsmsQuery("system-bytes-include-unavailable", Filter{SystemBytes: systemBytes(1), IncludeUnavailable: true}),
		),
		// A predicate matches a record whose fields are available and equal to it; W is no part of S/F.
		// S/F needs stream and function both available: seq 5's stream 1 alone does not match S1F1.
		// On request, a record with any needed field unavailable satisfies the whole predicate, whatever its available fields hold:
		// seq 4 satisfies S1F1 although its stream is 2 (the tracepack format specification §7.2).
		// The annotation has no available field, so it is selected only on request.
		// A pair is matched as a pair: S1F1 is not among S1F2 and S2F1.
		Expect: hsmsExpect(seqRange(0, 7), []Fields{
			headerFields(0, s1f1, allFields...),
			headerFields(1, s1f2, allFields...),
			headerFields(2, s2f1, allFields...),
			headerFields(3, s1f1NoW, allFields...),
			headerFields(4, cut2, fieldSessionID, fieldStreamAndW),
			headerFields(5, cut1, fieldSessionID, fieldStreamAndW),
			headerFields(6, headerValues{}),
		},
			QueryWant{ID: "session-ids", Seqs: []uint64{0, 1, 2, 4}},
			QueryWant{ID: "session-ids-include-unavailable", Seqs: []uint64{0, 1, 2, 4, 6, 7}},
			QueryWant{ID: "sf", Seqs: []uint64{0, 3}},
			QueryWant{ID: "sf-include-unavailable", Seqs: []uint64{0, 3, 4, 5, 6, 7}},
			QueryWant{ID: "sf-pairs", Seqs: []uint64{1, 2}},
			QueryWant{ID: "system-bytes", Seqs: []uint64{0, 1}},
			QueryWant{ID: "system-bytes-include-unavailable", Seqs: []uint64{0, 1, 4, 5, 6, 7}},
		),
	}
}
