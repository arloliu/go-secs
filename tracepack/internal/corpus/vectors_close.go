package corpus

import (
	"bytes"
	"math"
	"strconv"
	"time"

	"github.com/arloliu/go-secs/tracepack"
)

// Values of the consumer's close of a capture left open (the tracepack storage specification §4).
const (
	// closeSeq is the seq of the close record: the last seq of a capture.
	closeSeq uint64 = math.MaxInt64
	// closeTriggerMs is when the close happens, in milliseconds after TimeBase, inside the period of the capture's segments.
	closeTriggerMs = 30_000
	// closeStagedMs is the ts_utc_ns, in milliseconds after TimeBase, of the record with the highest seq the service staged.
	closeStagedMs = 2
)

// closeVectors returns the recipes of the close group (the tracepack corpus specification §9.4).
func closeVectors() []Recipe {
	staged := closeStagedMs

	return []Recipe{
		{
			ID: "close-plain", Title: "a segment holding only a consumer's close of a plain capture, at seq 2^63-1",
			Cites: []string{"STO §4", "FMT §2", "FMT §10", "FMT §14"}, Class: ClassRead,
			Build: func(seed string) (*Built, error) {
				pack, err := writeClose(seed, IDFor(rolePackID, seed), closeMeta(seed), &staged)

				return &Built{Pack: pack, Footer: true}, err
			},
			Expect: closeExpect(&staged, closeProjection(&staged)),
		},
		{
			ID: "close-masked-thinned", Title: "the close segment of a masked and thinned decoded-message capture",
			Cites: []string{"STO §4", "FMT §5", "FMT §14", "JSONL §5"}, Class: ClassRead,
			Build: func(seed string) (*Built, error) {
				policy, err := redactPolicy(DigestKeyID1)
				if err != nil {
					return nil, err
				}
				meta := maskedMeta(seed)
				meta.RedactionPolicy = new(policy.PackPolicy())
				meta.Thinning = []tracepack.Thinning{{Stream: 1, Function: 3, Source: fdcPollSource}}
				pack, err := writeClose(seed, IDFor(rolePackID, seed), meta, &staged)

				return &Built{Pack: pack}, err
			},
			// The capture's policy, classifier, max_frame_len and thinning are inherited, which are format 1.1 features:
			// minor 1, exported under /2, and no entry, since the close record is no data record.
			Expect: &Expectation{
				Outcome: tracepack.OutcomeFinalizedConsistent, FooterValid: true, Blocks: 1, Seqs: []uint64{closeSeq}, PrefixEnd: AtEnd(),
				ExportHas: []string{
					`"schema":"tracepack-jsonl/2","format_major":1,"format_minor":1,"flags":[]`,
					`"classifier":["tracepack-sem3"]`,
					`"max_frame_len":["256"]`,
					`"seq_start":"9223372036854775807"`,
					`"redaction_policy":{"policy_id":"corpus-baseline","policy_version":"1","key_id":"test-digest-1","digest_algorithm":"hmac-sha256",` +
						`"encryption_algorithm":"aes-siv-cmac-512","encryption_key_id":"test-enc-1"}`,
					`"thinning":[{"stream":"1","function":"3","source":"fdc-poll-interval"}]`,
					`"seq":"9223372036854775807"`,
				},
				Stats: closeStats(),
			},
		},
		{
			ID: "close-no-start-bound", Title: "a consumer's close of a capture that staged no record",
			Cites: []string{"STO §4", "FMT §10"}, Class: ClassRead,
			Build: func(seed string) (*Built, error) {
				pack, err := writeClose(seed, IDFor(rolePackID, seed), closeMeta(seed), nil)

				return &Built{Pack: pack, Footer: true}, err
			},
			Expect: closeExpect(nil, closeProjection(nil)),
		},
		{
			ID: "close-merged-archive", Title: "a capture's records and its close merged into an archive",
			Cites: []string{"STO §4", "FMT §10", "FMT §16"}, Class: ClassRead,
			Build: closeMergedBuild,
			// Two seq ranges, the capture's records and the close; the close counts as one record and ends no epoch.
			// Both input blocks are small, so the merge coalesces them into one block (the tracepack storage specification §4 Merge).
			Expect: closeMergedExpect(),
		},
	}
}

// closeMeta returns the pack metadata of a plain capture's segments, which its close takes:
// a raw-stream capture on the capture clock and the consumer's flush_interval_ns.
func closeMeta(seed string) *tracepack.PackMeta {
	meta := busCaptureMeta(seed, tracepack.CaptureMethodRawStream)
	meta.FlushIntervalNs = new(redactFlushInterval)

	return meta
}

// writeClose writes, as packID, the close segment of the capture of seed with WriteClose:
// the capture's pack metadata meta, the trigger closeTriggerMs after TimeBase,
// the timestamp stagedMs milliseconds after TimeBase of the record with the highest seq staged, or none when nil,
// the corpus clock and codec none.
func writeClose(seed string, packID tracepack.UUID, meta *tracepack.PackMeta, stagedMs *int) ([]byte, error) {
	opts := tracepack.CloseOptions{
		Meta: meta, CaptureID: IDFor(roleCaptureID, seed), Trigger: time.Unix(0, msAt(closeTriggerMs)).UTC(),
		PackID: packID, Now: corpusNow,
	}
	if stagedMs != nil {
		opts.HighestSeqTS = new(msAt(int64(*stagedMs)))
	}
	var buf bytes.Buffer
	if _, err := tracepack.WriteClose(&buf, opts); err != nil {
		return nil, err
	}

	return buf.Bytes(), nil
}

// closeBoundary is the boundary of a close record, from the tracepack storage specification §4:
// gap_start the staged timestamp minus clock_step_tolerance_ns, absent without one,
// and gap_end the trigger plus clock_step_tolerance_ns.
func closeBoundary(stagedMs *int) Boundary {
	b := Boundary{Seq: U64(closeSeq), Kind: "stop-unclean", GapEnd: new(I64(msAt(closeTriggerMs) + int64(busClockStepTolerance)))}
	if stagedMs != nil {
		b.GapStart = new(I64(msAt(int64(*stagedMs)) - int64(busClockStepTolerance)))
	}

	return b
}

// closeProjection is the footer projection of a close segment: one block holding the close record of epoch 0,
// which ends no epoch, its boundary, and the one seq 2^63-1.
func closeProjection(stagedMs *int) FooterProjection {
	b := []Boundary{closeBoundary(stagedMs)}

	return FooterProjection{
		Blocks: []FooterBlock{{Block: 0, Boundaries: b}},
		F5:     FooterF5{Epochs: []F5Epoch{{Epoch: 0}}, Boundaries: b, SeqRanges: []SeqRange{{First: U64(closeSeq), Last: U64(closeSeq)}}},
	}
}

// closeStats are the statistics of a close segment: epoch 0 not ended, and the stop-unclean boundary.
func closeStats() *StatsWant {
	return &StatsWant{
		Available: true, Epochs: []EpochWant{{Epoch: 0}}, Boundaries: []BoundaryWant{{Seq: closeSeq, Kind: tracepack.BoundaryKindStopUnclean}},
	}
}

// closeExpect is the expectation of the close segment of a plain capture: minor 0, exported under /1,
// one record at seq 2^63-1, the segment's seq_start, its footer projection p.
func closeExpect(stagedMs *int, p FooterProjection) *Expectation {
	b := closeBoundary(stagedMs)
	gaps := `"gap_end":"` + itoa(int64(*b.GapEnd)) + `"`
	if b.GapStart != nil {
		gaps = `"gap_start":"` + itoa(int64(*b.GapStart)) + `",` + gaps
	}

	return &Expectation{
		Outcome: tracepack.OutcomeFinalizedConsistent, FooterValid: true, Blocks: 1, Seqs: []uint64{closeSeq}, PrefixEnd: AtEnd(),
		ExportHas: []string{
			`"schema":"tracepack-jsonl/1","format_major":1,"format_minor":0`,
			`"seq_start":"9223372036854775807"`,
			`"seq":"9223372036854775807","ts_utc_ns":"` + itoa(msAt(closeTriggerMs)) + `"`,
			`"event":"capture-boundary"`, `"boundary_kind":"stop-unclean"`, gaps,
		},
		Stats:  closeStats(),
		Footer: &Footer{Stored: &p, Accepted: true, Recomputed: p},
	}
}

// closeMergedBuild builds close-merged-archive: a segment of the plain capture of seed holding seqs 0 to 2 of epoch 1,
// the last of them staged closeStagedMs after TimeBase, and the capture's close, merged into an archive.
func closeMergedBuild(seed string) (*Built, error) {
	seg, err := writeSpec(&packSpec{
		seed: seed, packID: IDFor(rolePackID, seed+"/segment"), meta: closeMeta(seed), facts: tracepack.PackFacts{DurableRecorder: true},
		blocks: [][]tracepack.Record{{footerData(0, 1), footerData(1, 1), footerData(2, 1)}},
	})
	if err != nil {
		return nil, err
	}
	staged := closeStagedMs
	closed, err := writeClose(seed, IDFor(rolePackID, seed+"/close"), closeMeta(seed), &staged)
	if err != nil {
		return nil, err
	}
	pack, err := mergePacks(seed, [][]byte{seg, closed})

	return &Built{Pack: pack, Footer: true}, err
}

// closeMergedExpect is the expectation of close-merged-archive.
func closeMergedExpect() *Expectation {
	staged := closeStagedMs
	b := []Boundary{closeBoundary(&staged)}
	p := FooterProjection{
		Blocks: []FooterBlock{{Block: 0, Boundaries: b}},
		F5: FooterF5{
			Epochs: []F5Epoch{{Epoch: 0}, {Epoch: 1}}, Boundaries: b,
			SeqRanges: []SeqRange{{First: 0, Last: 2}, {First: U64(closeSeq), Last: U64(closeSeq)}},
		},
	}

	return &Expectation{
		Outcome: tracepack.OutcomeFinalizedConsistent, FooterValid: true, Blocks: 1, Seqs: []uint64{0, 1, 2, closeSeq}, PrefixEnd: AtEnd(),
		ExportHas: []string{`"pack_role":"archive"`, `"seq_start":"0"`},
		Stats: &StatsWant{
			Available: true, Epochs: []EpochWant{{Epoch: 0}, {Epoch: 1}},
			Boundaries: []BoundaryWant{{Seq: closeSeq, Kind: tracepack.BoundaryKindStopUnclean}},
		},
		Footer: &Footer{Stored: &p, Accepted: true, Recomputed: p},
	}
}

// itoa returns v in decimal, as an export writes an i64 value.
func itoa(v int64) string {
	return strconv.FormatInt(v, 10)
}
