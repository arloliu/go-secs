package corpus

import (
	"fmt"
	"slices"

	"github.com/arloliu/go-secs/tracepack"
)

// Values of the sem group.
const (
	// semMaxFrameLen is the single max_frame_len of the packs of the sem group that list classify.json
	// (the tracepack semantics specification §3): frames of 40 bytes are within it, frames of 41 exceed it.
	semMaxFrameLen = 40
	// stypeUndefined is an SType SEMI E37 Table 5 does not define.
	stypeUndefined byte = 8
	// captureOriginMono is the capture_origin_mono_ns of sem-capture-origin-mono: a monotonic reading of about a day of uptime.
	captureOriginMono int64 = 86_400_123_456_789
)

// classifyCites are the clauses of a frame classify.json lists.
var classifyCites = []string{"SEM §3", "CORPUS §7"}

// semFrame is a record of a classifier vector: the case it exercises, the kind and frame it carries,
// and the decode_status and trailing_bytes written by hand from the tracepack semantics specification §3
// and the tracepack corpus specification §7, which the record stores and classify.json lists.
// A frame of nil is not a byte-predicate case: the record is built by build and stays out of classify.json.
type semFrame struct {
	id       string
	kind     tracepack.Kind
	frame    []byte
	status   tracepack.DecodeStatus
	trailing uint32
	// build makes a record that carries no classified frame.
	build func(seq uint64) (tracepack.Record, error)
}

// item returns a SECS-II item of the format code code (SEMI E5 §9.2, written in octal as E5 Table 1 writes it)
// with one length byte and the body body: the format byte, code × 4 + 1, then len(body), then body.
func item(code byte, body ...byte) []byte {
	return append([]byte{code<<2 | 1, byte(len(body))}, body...)
}

// list returns the header of a SECS-II list of n elements, with one length byte; the elements follow it.
func list(n byte) []byte {
	return []byte{0o00<<2 | 1, n}
}

// counting returns n bytes counting up from 1.
func counting(n int) []byte {
	b := make([]byte, n)
	for i := range b {
		b[i] = byte(i + 1)
	}

	return b
}

// textFrame returns the data message S1F3 with W, SessionID 1, whose message text is text.
func textFrame(text ...byte) []byte {
	return dataFrame(1, 3, true, 1, text)
}

// dataCase returns the semFrame of the case id: a data record carrying text as its message text, classified as status.
func dataCase(id string, status tracepack.DecodeStatus, text ...byte) semFrame {
	return semFrame{id: id, kind: tracepack.KindData, frame: textFrame(text...), status: status}
}

// Element widths of the multi-byte formats, by format code (the tracepack corpus specification §7).
var multiByteFormats = []struct {
	name  string
	code  byte
	width int
}{
	{"i2", 0o32, 2}, {"i4", 0o34, 4}, {"i8", 0o30, 8},
	{"u2", 0o52, 2}, {"u4", 0o54, 4}, {"u8", 0o50, 8},
	{"f4", 0o44, 4}, {"f8", 0o40, 8},
}

// Format codes of the single-byte formats and the list, with their names in the zero-length case ids.
var zeroLengthFormats = []struct {
	name string
	code byte
}{
	{"binary", 0o10}, {"boolean", 0o11}, {"ascii", 0o20}, {"jis8", 0o21}, {"localized", 0o22},
	{"i1", 0o31}, {"i2", 0o32}, {"i4", 0o34}, {"i8", 0o30},
	{"u1", 0o51}, {"u2", 0o52}, {"u4", 0o54}, {"u8", 0o50},
	{"f4", 0o44}, {"f8", 0o40},
}

// itemValidityFrames are the cases of the item-validity checklist of the tracepack corpus specification §7, in its order,
// each a data message whose message text is the item described.
func itemValidityFrames() []semFrame {
	const (
		itemErr = tracepack.DecodeStatusItemDecodeError
		ok      = tracepack.DecodeStatusOK
		ascii   = 0o20
	)
	frames := []semFrame{
		// Format code 0o77 is none of the codes of the table.
		dataCase("undefined-format", itemErr, item(0o77)...),
		// An ASCII format byte with no length byte: a reader taking it as an empty item reads ok.
		dataCase("length-bytes-zero", itemErr, ascii<<2),
		dataCase("length-bytes-cut-1", itemErr, ascii<<2|1),
		dataCase("length-bytes-cut-2", itemErr, ascii<<2|2, 0x00),
		dataCase("length-bytes-cut-3", itemErr, ascii<<2|3, 0x00, 0x00),
		dataCase("body-short", itemErr, ascii<<2|1, 3, 'A', 'B'),
	}
	for _, f := range multiByteFormats {
		frames = append(frames, dataCase("width-"+f.name, itemErr, item(f.code, counting(f.width+1)...)...))
	}
	frames = append(frames,
		// A list of two elements holding one U1.
		dataCase("list-missing-element", itemErr, slices.Concat(list(2), item(0o51, 5))...),
		// A list of one element, a U2 of one byte.
		dataCase("list-invalid-nested", itemErr, slices.Concat(list(1), item(0o52, 0))...),
		dataCase("localized-length-1", itemErr, item(0o22, 0x00)...),
		dataCase("empty-list", ok, list(0)...),
	)
	for _, f := range zeroLengthFormats {
		frames = append(frames, dataCase("zero-length-"+f.name, ok, item(f.code)...))
	}
	for _, f := range multiByteFormats {
		frames = append(frames, dataCase("whole-width-"+f.name, ok, item(f.code, counting(2*f.width)...)...))
	}
	frames = append(frames,
		// The ASCII item "A" with 1, 2 and 3 length bytes.
		dataCase("length-bytes-1", ok, ascii<<2|1, 0x01, 'A'),
		dataCase("length-bytes-2", ok, ascii<<2|2, 0x00, 0x01, 'A'),
		dataCase("length-bytes-3", ok, ascii<<2|3, 0x00, 0x00, 0x01, 'A'),
		dataCase("boolean-other-values", ok, item(0o11, 0x02, 0xFF)...),
		dataCase("ascii-high-bytes", ok, item(ascii, 0x80, 0xFF)...),
		// A localized string's 2-byte header naming UCS-2 (encoding 1), then one byte, an odd length under a 2-byte encoding.
		dataCase("localized-length-2", ok, item(0o22, 0x00, 0x01)...),
		dataCase("localized-length-3", ok, item(0o22, 0x00, 0x01, 'A')...),
	)
	trailing := dataCase("trailing", tracepack.DecodeStatusOKWithTrailing, slices.Concat(item(0o51, 5), []byte{0xFF, 0xFF})...)
	trailing.trailing = 2

	return append(frames, trailing, dataCase("empty-text", ok))
}

// decodeStatusFrames are the records of sem-decode-status-classified:
// one per decode_status of the tracepack semantics specification §3 that a classifier writes, in the table's order,
// then the cases of the order of evaluation and of max_frame_len of the tracepack corpus specification §7.
// The frames are those of the table's predicates under semMaxFrameLen.
func decodeStatusFrames() []semFrame {
	const m = semMaxFrameLen
	withPType := func(f []byte, ptype byte) []byte {
		f[hsmsPTypeOff] = ptype
		return f
	}
	withLength := func(f []byte, length byte) []byte {
		f[3] = length
		return f
	}
	control := func(id string, status tracepack.DecodeStatus, f []byte) semFrame {
		return semFrame{id: id, kind: tracepack.KindControl, frame: f, status: status}
	}
	short := func(id string, f []byte) semFrame {
		return semFrame{id: id, kind: tracepack.KindData, frame: f, status: tracepack.DecodeStatusShortFrame}
	}
	trailing := dataCase(tracepack.DecodeStatusOKWithTrailing.String(), tracepack.DecodeStatusOKWithTrailing,
		slices.Concat(item(0o51, 5), []byte{0xFF, 0xFF, 0xFF})...)
	trailing.trailing = 3

	return []semFrame{
		{id: tracepack.DecodeStatusNotAttempted.String(), build: func(seq uint64) (tracepack.Record, error) {
			return newData(seq, 1, tracepack.DirHostToEquipment, textFrame(item(0o51, 5)...)), nil
		}},
		short(tracepack.DecodeStatusShortFrame.String(), textFrame()[:10]),
		// A length field of 11 over a 10-byte header.
		{id: tracepack.DecodeStatusLengthMismatch.String(), kind: tracepack.KindData, frame: withLength(textFrame(), 11), status: tracepack.DecodeStatusLengthMismatch},
		{id: tracepack.DecodeStatusBadPType.String(), kind: tracepack.KindData, frame: withPType(textFrame(), 1), status: tracepack.DecodeStatusBadPType},
		control(tracepack.DecodeStatusBadSType.String(), tracepack.DecodeStatusBadSType, hsmsFrame(controlSessionID, 0, 0, stypeUndefined, 2, nil)),
		// A Linktest.req with a body of one byte.
		control(tracepack.DecodeStatusControlWithBody.String(), tracepack.DecodeStatusControlWithBody,
			hsmsFrame(controlSessionID, 0, 0, stypeLinktestReq, 3, []byte{0})),
		// A binary item of 30 bytes: a frame of 46 bytes.
		dataCase(tracepack.DecodeStatusOversized.String(), tracepack.DecodeStatusOversized, item(0o10, counting(30)...)...),
		dataCase(tracepack.DecodeStatusItemDecodeError.String(), tracepack.DecodeStatusItemDecodeError, item(0o20, 'A', 'B')[:3]...),
		trailing,
		dataCase(tracepack.DecodeStatusOK.String(), tracepack.DecodeStatusOK, item(0o51, 5)...),
		{id: tracepack.DecodeStatusNotApplicable.String(), build: func(seq uint64) (tracepack.Record, error) {
			return newNote(seq, 1, "not classified")
		}},
		// The first 13 bytes of a frame of PType 1.
		short("short-and-bad-ptype", withPType(textFrame(), 1)[:13]),
		control("length-mismatch-and-bad-stype", tracepack.DecodeStatusLengthMismatch, withLength(hsmsFrame(controlSessionID, 0, 0, stypeUndefined, 4, nil), 11)),
		// A Select.req with a body of 27 bytes: a frame of m + 1 bytes.
		control("control-with-body-and-oversized", tracepack.DecodeStatusControlWithBody,
			hsmsFrame(controlSessionID, 0, 0, stypeSelectReq, 5, counting(m+1-14))),
		// Binary items of 24 and 25 bytes: frames of m and m + 1 bytes.
		dataCase("max-frame-len-equal", tracepack.DecodeStatusOK, item(0o10, counting(m-16)...)...),
		dataCase("max-frame-len-exceeded", tracepack.DecodeStatusOversized, item(0o10, counting(m-15)...)...),
	}
}

// hsmsPTypeOff is the offset of the PType byte in a frame: header byte 4, after the 4-byte length.
const hsmsPTypeOff = 8

// classifiedVector returns the recipe of a pack of one block holding frames at seqs 0, 1, …,
// classified under semMaxFrameLen, with classify.json listing each classified frame and a case per record.
func classifiedVector(id, title string, cites []string, frames []semFrame) Recipe {
	cases := make([]Case, 0, len(frames))
	seqs := make([]uint64, 0, len(frames))
	for i, f := range frames {
		cases = append(cases, Case{ID: f.id, Seq: U64(i), Cites: classifyCites})
		seqs = append(seqs, uint64(i))
	}

	return Recipe{
		ID: id, Title: title, Cites: cites, Class: ClassRead, Cases: cases,
		Build: func(seed string) (*Built, error) {
			recs := make([]tracepack.Record, 0, len(frames))
			c := &Classify{MaxFrameLen: semMaxFrameLen}
			for i, f := range frames {
				seq := uint64(i)
				if f.frame == nil {
					r, err := f.build(seq)
					if err != nil {
						return nil, err
					}
					recs = append(recs, r)

					continue
				}
				r := frameRecord(f.kind, seq, msAt(int64(seq)), 1, tracepack.DirHostToEquipment, f.frame)
				r.DecodeStatus, r.TrailingBytes = f.status, f.trailing
				recs = append(recs, r)
				row, err := NewClassifyFrame(seq, f.id, f.status, f.trailing)
				if err != nil {
					return nil, fmt.Errorf("case %s: %w", f.id, err)
				}
				c.Frames = append(c.Frames, row)
			}
			meta := segmentMeta(seed)
			meta.Classifiers, meta.MaxFrameLens = []string{corpusClassifier}, []uint64{semMaxFrameLen}
			pack, err := writeSpec(&packSpec{seed: seed, meta: meta, blocks: [][]tracepack.Record{recs}})

			return &Built{Pack: pack, Classify: c}, err
		},
		Expect: &Expectation{
			Outcome: tracepack.OutcomeFinalizedConsistent, FooterValid: true, Blocks: 1, Seqs: seqs, PrefixEnd: AtEnd(),
			ExportHas: []string{`"classifier":["tracepack-sem3"]`, `"max_frame_len":["40"]`},
		},
	}
}

// semVectors returns the recipes of the sem group (the tracepack corpus specification §9.4).
func semVectors() []Recipe {
	classified := classifiedVector("sem-decode-status-classified",
		"a record of each decode_status a classifier writes, the order of evaluation and the max_frame_len boundary",
		[]string{"SEM §3", "SEM §9", "CORPUS §5.9", "CORPUS §7"}, decodeStatusFrames())
	// The records store the statuses that are not byte predicates as they are, and the excess of ok-with-trailing.
	classified.Expect.ExportHas = append(classified.Expect.ExportHas,
		`"trailing_bytes":3,"quality":[],"kind":"data","dir":"host-to-equipment","fidelity":"wire-exact","decode_status":"ok-with-trailing"`,
		`"kind":"data","dir":"host-to-equipment","fidelity":"wire-exact","decode_status":"not-attempted"`,
		`"kind":"annotation","dir":"local","fidelity":"not-applicable","decode_status":"not-applicable"`,
	)
	validity := classifiedVector("sem-item-validity", "the item-validity checklist",
		[]string{"SEM §3", "SEM §9", "CORPUS §7", "CORPUS §5.9"}, itemValidityFrames())

	return []Recipe{classified, logStatusVector(), validity, captureOriginMonoVector()}
}

// logStatusVector is sem-decode-status-log, a log conversion (the tracepack storage specification §7):
// at seq 0 a message the converter parsed and built (reconstructed-ok);
// at seq 1 an entry it could not parse, kept as an unparsed-entry annotation (parse-failed);
// at seq 2 an entry it parsed whose metadata sets W on an even function, which the build rejects,
// kept as an unparsed-entry annotation (build-rejected, the tracepack semantics specification §2).
// The source has no connect or accept line, so every record is of epoch 0 and correlation-incomplete.
func logStatusVector() Recipe {
	entries := []struct {
		seq    uint64
		status tracepack.DecodeStatus
		text   string
	}{
		{1, tracepack.DecodeStatusParseFailed, `h2e sb=00000002 S1F1 W <A "unterminated>.`},
		{2, tracepack.DecodeStatusBuildRejected, `e2h sb=00000003 w=1 S1F2 <L>.`},
	}

	return Recipe{
		ID: "sem-decode-status-log", Title: "a log conversion's reconstructed-ok, parse-failed and build-rejected records",
		Cites: []string{"SEM §3", "SEM §2", "SEM §9", "STO §7"}, Class: ClassRead,
		Cases: []Case{
			{ID: "reconstructed-ok", Seq: 0, Cites: []string{"SEM §3"}},
			{ID: "parse-failed", Seq: 1, Cites: []string{"SEM §3", "STO §7"}},
			{ID: "build-rejected", Seq: 2, Cites: []string{"SEM §3", "SEM §2"}},
		},
		Build: func(seed string) (*Built, error) {
			recs := []tracepack.Record{newLogData(0, 0, tracepack.DirHostToEquipment, dataFrame(1, 1, true, 1, nil), 0)}
			for _, e := range entries {
				r, err := newAnnotation(e.seq, 0, &tracepack.Annotation{AnnotationKind: tracepack.AnnotationKindUnparsedEntry, Text: new(e.text)})
				if err != nil {
					return nil, err
				}
				r.DecodeStatus = e.status
				recs = append(recs, r)
			}
			pack, err := writeSpec(&packSpec{seed: seed, meta: logMeta(seed), blocks: [][]tracepack.Record{recs}})

			return &Built{Pack: pack}, err
		},
		Expect: &Expectation{
			Outcome: tracepack.OutcomeFinalizedConsistent, FooterValid: true, Blocks: 1, Seqs: seqRange(0, 2), PrefixEnd: AtEnd(),
			ExportHas: []string{
				`"capture_method":"log"`,
				`"epoch":0,"payload_len":14,"trailing_bytes":0,"quality":["correlation-incomplete"],` +
					`"kind":"data","dir":"host-to-equipment","fidelity":"reconstructed","decode_status":"reconstructed-ok"`,
				`"epoch":0,"payload_len":58,"trailing_bytes":0,"quality":["correlation-incomplete"],` +
					`"kind":"annotation","dir":"local","fidelity":"not-applicable","decode_status":"parse-failed"`,
				`"body":{"annotation_kind":"unparsed-entry","text":"h2e sb=00000002 S1F1 W <A \"unterminated>."}`,
				`"epoch":0,"payload_len":46,"trailing_bytes":0,"quality":["correlation-incomplete"],` +
					`"kind":"annotation","dir":"local","fidelity":"not-applicable","decode_status":"build-rejected"`,
				`"body":{"annotation_kind":"unparsed-entry","text":"e2h sb=00000003 w=1 S1F2 <L>."}`,
			},
		},
	}
}

// captureOriginMonoVector is sem-capture-origin-mono: a raw capture on the host, timed by the capture clock,
// whose capture_origin_mono_ns is not 0 (the tracepack semantics specification §4).
// Its records' mono_ns count from that origin, so each record's ts_utc_ns is capture_origin_utc_ns plus its mono_ns.
func captureOriginMonoVector() Recipe {
	return Recipe{
		ID: "sem-capture-origin-mono", Title: "a capture-clock pack whose capture_origin_mono_ns is not 0",
		Cites: []string{"SEM §4", "SEM §9", "FMT §5"}, Class: ClassRead,
		Build: func(seed string) (*Built, error) {
			meta := segmentMeta(seed)
			meta.CaptureMethod, meta.Vantage, meta.TimeSource = tracepack.CaptureMethodRawStream, tracepack.VantageHost, tracepack.TimeSourceCaptureClock
			meta.CaptureOriginUTCNs, meta.CaptureOriginMonoNs = new(TimeBase), new(captureOriginMono)
			meta.ClockStepToleranceNs = new(uint64(1_000_000))
			recs := []tracepack.Record{
				newData(0, 1, tracepack.DirHostToEquipment, dataFrame(1, 1, true, 1, nil)),
				newData(1, 1, tracepack.DirEquipmentToHost, dataFrame(1, 2, false, 1, nil)),
			}
			for k := range recs {
				recs[k].MonoPresent, recs[k].MonoNs = true, recs[k].TSUTCNs-TimeBase
			}
			pack, err := writeSpec(&packSpec{seed: seed, meta: meta, blocks: [][]tracepack.Record{recs}})

			return &Built{Pack: pack}, err
		},
		Expect: &Expectation{
			Outcome: tracepack.OutcomeFinalizedConsistent, FooterValid: true, Blocks: 1, Seqs: seqRange(0, 1), PrefixEnd: AtEnd(),
			ExportHas: []string{
				`"time_source":"capture-clock","capture_origin_utc_ns":"1791000000000000000","capture_origin_mono_ns":"86400123456789"`,
				`"seq":"0","ts_utc_ns":"1791000000000000000","mono_ns":"0"`,
				`"seq":"1","ts_utc_ns":"1791000000001000000","mono_ns":"1000000"`,
				`"record_flags":["mono_present"]`,
			},
		},
	}
}
