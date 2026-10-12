package corpus

import (
	"slices"

	"github.com/arloliu/go-secs/tracepack"
)

// Values of the captures a consumer of a durable bus writes (the tracepack storage specification §4).
const (
	// busClockStepTolerance is the clock_step_tolerance_ns of their descriptors: one millisecond.
	busClockStepTolerance uint64 = 1_000_000
	// fdcPollSource is the source of the thinning entries of thin-fdc-polls: where the sampling comes from.
	fdcPollSource = "fdc-poll-interval"
)

// thinVectors returns the recipes of the thin group (the tracepack corpus specification §9.4).
func thinVectors() []Recipe {
	return []Recipe{{
		ID:    "thin-fdc-polls",
		Title: "a raw-stream capture's thinned S1F3 and S1F4 polls, an S1F4 with an empty message text and an S9F7 kept in full",
		Cites: []string{"SEM §2", "FMT §5", "FMT §9", "STO §4", "FMT §14", "JSONL §5"}, Class: ClassRead,
		Cases: []Case{
			{ID: "unobserved-s1f3", Seq: 0, Cites: []string{"SEM §2", "FMT §9"}},
			{ID: "unobserved-s1f4", Seq: 1, Cites: []string{"SEM §2", "FMT §9"}},
			{ID: "empty-text-s1f4", Seq: 3, Cites: []string{"SEM §2"}},
			{ID: "abnormal-reply-s9f7", Seq: 4, Cites: []string{"SEM §2"}},
		},
		Build: thinBuild,
		// The pack holds thinning entries and unobserved records, both format 1.1 features, so it is of minor 1:
		// the unobserved records keep their header and length, a zero-filled text, not-attempted and no trailing bytes,
		// while a genuinely empty text and an abnormal reply are ordinary records, classified and kept in full.
		Expect: &Expectation{
			Outcome: tracepack.OutcomeFinalizedConsistent, FooterValid: true, Blocks: 1, Seqs: seqRange(0, 4), PrefixEnd: AtEnd(),
			ExportHas: []string{
				`"schema":"tracepack-jsonl/2","format_major":1,"format_minor":1`,
				`"thinning":[{"stream":"1","function":"3","source":"fdc-poll-interval"},{"stream":"1","function":"4","source":"fdc-poll-interval"}]`,
				`"seq":"0","ts_utc_ns":"1791000000000000000","mono_ns":"0","epoch":1,"payload_len":28,"trailing_bytes":0,"quality":["unobserved"],` +
					`"kind":"data","dir":"host-to-equipment","fidelity":"wire-exact","decode_status":"not-attempted"`,
				`"seq":"1","ts_utc_ns":"1791000000001000000","mono_ns":"0","epoch":1,"payload_len":24,"trailing_bytes":0,"quality":["unobserved"],` +
					`"kind":"data","dir":"equipment-to-host","fidelity":"wire-exact","decode_status":"not-attempted"`,
				`"seq":"3","ts_utc_ns":"1791000000003000000","mono_ns":"0","epoch":1,"payload_len":14,"trailing_bytes":0,"quality":[],` +
					`"kind":"data","dir":"equipment-to-host","fidelity":"wire-exact","decode_status":"ok"`,
				`"seq":"4","ts_utc_ns":"1791000000004000000","mono_ns":"0","epoch":1,"payload_len":26,"trailing_bytes":0,"quality":[],` +
					`"kind":"data","dir":"equipment-to-host","fidelity":"wire-exact","decode_status":"ok"`,
			},
		},
	}}
}

// thinBuild builds thin-fdc-polls: a raw-stream capture of one block of epoch 1 declaring S1F3 and S1F4 thinned:
// an S1F3 poll (seq 0) and its S1F4 reply (seq 1) unobserved by design, their texts zero-filled;
// a poll and reply observed in full (seqs 2 and 3), the reply's message text genuinely empty;
// and an S9F7 (seq 4) reporting illegal data, carrying the header of the message it rejects.
func thinBuild(seed string) (*Built, error) {
	meta := busCaptureMeta(seed, tracepack.CaptureMethodRawStream)
	meta.Thinning = []tracepack.Thinning{
		{Stream: 1, Function: 3, Source: fdcPollSource}, {Stream: 1, Function: 4, Source: fdcPollSource},
	}
	meta.Classifiers = []string{corpusClassifier}
	h2e, e2h := tracepack.DirHostToEquipment, tracepack.DirEquipmentToHost

	poll := slices.Concat(list(2), item(0o54, 0, 0, 0, 1), item(0o54, 0, 0, 0, 2))
	reply := slices.Concat(list(2), item(0o54, 0, 0, 0, 7), list(0))
	recs := []tracepack.Record{
		unobserved(newData(0, 1, h2e, dataFrame(1, 3, true, 1, poll))),
		unobserved(newData(1, 1, e2h, dataFrame(1, 4, false, 1, reply))),
		classified(newData(2, 1, h2e, dataFrame(1, 3, true, 2, poll))),
		classified(newData(3, 1, e2h, dataFrame(1, 4, false, 2, nil))),
		classified(newData(4, 1, e2h, dataFrame(9, 7, false, 3, item(0o10, dataFrame(1, 3, true, 2, nil)[4:]...)))),
	}
	pack, err := writeSpec(&packSpec{
		seed: seed, meta: meta, facts: tracepack.PackFacts{AnyClassified: true}, blocks: [][]tracepack.Record{recs},
	})

	return &Built{Pack: pack}, err
}

// unobserved returns r as a producer that thinned it writes it (the tracepack semantics specification §2):
// its message text zero-filled, quality.unobserved, not-attempted and no trailing bytes, its header and length kept.
func unobserved(r tracepack.Record) tracepack.Record {
	r.Payload = slices.Clone(r.Payload)
	clear(r.Payload[hsmsPrefixLen:])
	r.Quality |= tracepack.QualityUnobserved
	r.DecodeStatus, r.TrailingBytes = tracepack.DecodeStatusNotAttempted, 0

	return r
}

// classified returns r classified ok: a frame whose message text, empty or not, is one valid item,
// as the tracepack semantics specification §3 decides for the frames the recipes give it.
func classified(r tracepack.Record) tracepack.Record {
	r.DecodeStatus = tracepack.DecodeStatusOK

	return r
}

// hsmsPrefixLen is the length of an HSMS frame's 4-byte length and 10-byte message header, before the message text.
const hsmsPrefixLen = 14

// busCaptureMeta returns the pack metadata of a segment of seed written by a consumer of a durable bus
// (the tracepack storage specification §4): segmentMeta's, of capture_method method on the capture clock,
// its origin at TimeBase and a clock_step_tolerance_ns of busClockStepTolerance.
func busCaptureMeta(seed string, method tracepack.CaptureMethod) *tracepack.PackMeta {
	meta := segmentMeta(seed)
	meta.CaptureMethod, meta.TimeSource = method, tracepack.TimeSourceCaptureClock
	meta.CaptureOriginUTCNs, meta.CaptureOriginMonoNs, meta.ClockStepToleranceNs = new(TimeBase), new(int64(0)), new(busClockStepTolerance)

	return meta
}
