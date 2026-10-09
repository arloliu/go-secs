package corpus

import (
	"os"

	"github.com/arloliu/go-secs/tracepack"
)

// v010SamplePath is the pack tracepack v0.1.0 wrote, from this package's directory, where Generate runs:
// the sample sample-v010-rows copies, never regenerated.
const v010SamplePath = "../../testdata/v0.1.0-rows.tpk"

// v010Hour is the hour of the records of the v0.1.0 sample.
const v010Hour int64 = 1_789_999_200_000_000_000

// sampleVectors returns the recipes of the sample group (the tracepack corpus specification §9.4).
func sampleVectors() []Recipe {
	return []Recipe{v010SampleVector()}
}

// v010SampleVector is sample-v010-rows: the pack of testdata/v0.1.0-rows.tpk, written by tracepack v0.1.0,
// whose 56-byte record headers are stored one after another, a layout the format before v2.13 defined
// (the tracepack format specification §14).
// It holds two zstd blocks, seqs 0 to 2 (S1F3 with W, SessionID 0x1234, System Bytes 1 to 3) then seq 3 (System Bytes 4),
// each record at the hour plus seq seconds with mono_ns seq seconds.
// Each record's 56-byte header holds seq, ts_utc_ns, mono_ns, epoch, payload_len and trailing_bytes,
// then at byte 36 the System Bytes, the SessionID (little-endian) and quality,
// and at byte 44 stream, function, PType, SType, kind, dir, fidelity, decode_status, field_validity, record_flags and two reserved bytes.
//
// A reader of the current format reads the header section as columns of 44-byte headers:
// the three-record block gathers its seqs from other fields' bytes and fails its integrity check,
// so its F-3 range 0 to 2 is lost over the block's F-2 time range.
// The one-record block is laid out alike in both formats and is read from its 56-byte header,
// bytes 36 on as the current header's quality (System Bytes 0 0), kind (0, unknown), dir (4), fidelity (0x34), decode_status (0x12),
// field_validity and record_flags (the old quality, 0), and bytes 44 to 55 as its extension area.
// Its kind then disagrees with the block's F-3 kind_counts, which state one data record.
// The footer's retired F-3 tags are skipped, so the footer is valid,
// and the failed block before a validated one makes the pack corrupt-middle (the tracepack format specification §13).
func v010SampleVector() Recipe {
	return Recipe{
		ID: "sample-v010-rows", Title: "the tracepack v0.1.0 pack of a multi-record block, read under the current format",
		Cites: []string{"FMT §14", "FMT §13", "FMT §16"}, Class: ClassRead, Labels: []string{LabelPreV213Sample},
		Codec: CodecZstd, Source: SourceFixed,
		Build: func(string) (*Built, error) {
			pack, err := os.ReadFile(v010SamplePath)

			return &Built{Pack: pack}, err
		},
		Expect: &Expectation{
			Outcome: tracepack.OutcomeCorruptMiddle, FooterValid: true, Blocks: 2, Seqs: []uint64{3},
			Failed: []FailedWant{{Block: 0, Cause: tracepack.ReasonCorruptBlock}}, PrefixEnd: AtBlock(0),
			Disagreeing: []int{1},
			Lost:        []LostWant{{First: 0, Last: 2, TimeStart: v010Hour, TimeEnd: v010Hour + 2_000_000_000}},
			ExportHas: []string{
				`{"seq":"3","ts_utc_ns":"1789999203000000000","mono_ns":"3000000000","epoch":1,"payload_len":14,"trailing_bytes":0,` +
					`"quality":[],"kind":"unknown","dir":"unknown(4)","fidelity":"unknown(52)","decode_status":"unknown(18)",` +
					`"field_validity":[],"record_flags":[],"record_header_len":56,"header_extra":"AQMAAAEBAQE/AwAA",` +
					`"payload":"AAAAChI0gQMAAAAAAAQ="}`,
			},
		},
	}
}
