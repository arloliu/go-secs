# tracepack test data

## v0.1.0-rows.tpk

A pack written by the tracepack v0.1.0 Writer, before the format revision of spec v2.13:
56-byte record headers stored one after another in the header section.
It holds two zstd blocks: seqs 0-2 (three S1F3 records), then seq 3 (one record).
The redefined format 1.0 does not support it ([FMT §14]);
`reader_compat_test.go` reads it as a sample of that layout, not as a detection guarantee ([FMT §16]):
the three-record block is `corrupt`, and the one-record block reads with its fields from byte 36 on misread.

It was generated once, in a scratch module outside the repository with `go.mod` requiring
`github.com/arloliu/go-secs/tracepack v0.1.0`, by `go run . v0.1.0-rows.tpk` over this program:

```go
// Command v010gen writes a pack with the tracepack v0.1.0 Writer, whose record headers are 56 bytes stored row by row:
// a block of three data records, then a block of one data record, both zstd.
package main

import (
	"bytes"
	"log"
	"os"

	"github.com/arloliu/go-secs/tracepack"
)

func main() {
	const hour int64 = 1_789_999_200_000_000_000 // 2026-09-21T14:00:00Z
	scopeGen := uint64(0)
	classifier := "go-secs/test"
	var recorderID, packID, captureID tracepack.UUID
	for i := range recorderID {
		recorderID[i], packID[i], captureID[i] = byte(i), 0xA0|byte(i), 0xC0|byte(i)
	}
	meta := &tracepack.PackMeta{
		ToolID: "tool", Transport: tracepack.TransportHSMSSS, CaptureMethod: tracepack.CaptureMethodRawStream,
		Vantage: tracepack.VantageHost, Recorder: "rec", Writer: "tracepack/v0.1.0", Classifier: &classifier,
		TimeSource: tracepack.TimeSourceGenerator, PeriodStart: hour, PeriodEnd: hour + 3_600_000_000_000,
		LifecycleCoverage: tracepack.LifecycleCoverageNone, QualityEvaluated: true, PackRole: tracepack.PackRoleSegment,
		RecorderInstanceID: recorderID, ScopeGeneration: &scopeGen,
	}

	var buf bytes.Buffer
	w, err := tracepack.NewWriter(&buf, tracepack.WriterOptions{
		Meta: meta, Facts: tracepack.PackFacts{AnyClassified: true}, Codec: tracepack.CodecZstd,
		AssignSeq: true, PackID: packID, CaptureID: captureID,
	})
	if err != nil {
		log.Fatal(err)
	}

	appendFrame := func(i int) {
		// S1F3 W with an empty message text, session_id 0x1234, System Bytes 00 00 00 i+1.
		frame := []byte{0, 0, 0, 0x0A, 0x12, 0x34, 0x81, 0x03, 0x00, 0x00, 0x00, 0x00, 0x00, byte(i + 1)}
		r := tracepack.Record{
			TSUTCNs: hour + int64(i)*1_000_000_000, MonoNs: int64(i) * 1_000_000_000, MonoPresent: true, Epoch: 1,
			Kind: tracepack.KindData, Dir: tracepack.DirHostToEquipment, Fidelity: tracepack.FidelityWireExact,
			DecodeStatus: tracepack.DecodeStatusOK, Payload: frame,
		}
		r.SetHeaderCopies()
		if err := w.Append(&r); err != nil {
			log.Fatal(err)
		}
	}
	for i := range 3 {
		appendFrame(i)
	}
	if err := w.Flush(); err != nil {
		log.Fatal(err)
	}
	appendFrame(3)
	if _, err := w.Close(); err != nil {
		log.Fatal(err)
	}

	if err := os.WriteFile(os.Args[1], buf.Bytes(), 0o644); err != nil {
		log.Fatal(err)
	}
}
```

[FMT §14]: ../../docs/specs/tracepack/tracepack-format.md#14-versioning
[FMT §16]: ../../docs/specs/tracepack/tracepack-format.md#16-conformance-corpus
