package main

import (
	"bytes"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/arloliu/go-secs/tracepack"
)

// reportLines returns the values of the lines named name in a report, in order.
func reportLines(report, name string) []string {
	var values []string
	for line := range strings.Lines(report) {
		if v, ok := strings.CutPrefix(strings.TrimSuffix(line, "\n"), name+": "); ok {
			values = append(values, v)
		}
	}

	return values
}

func TestVerifyOutcomes(t *testing.T) {
	t.Parallel()

	tests := []struct {
		id       string
		outcome  string
		complete string
		code     int
		lines    []string // names of lines the report holds
	}{
		{id: "basic-codec-zstd", outcome: "finalized-consistent", complete: "true", code: exitClean},
		{id: "footer-invalid-count-sums", outcome: "finalized-inconsistent", complete: "true", code: exitNotClean, lines: []string{"footer-error"}},
		{id: "footer-boundary-ties", outcome: "finalized-inconsistent", complete: "false", code: exitNotClean, lines: []string{"disagreement"}},
		{id: "basic-seq-over-limit", outcome: "finalized-truncated", complete: "false", code: exitNotClean, lines: []string{"failed"}},
		{id: "basic-empty-pack", outcome: "unfinalized", complete: "true", code: exitNotClean, lines: []string{"footer-error"}},
		{id: "basic-corrupt-middle", outcome: "corrupt-middle", complete: "false", code: exitNotClean, lines: []string{"failed", "lost"}},
		{id: "verify-defect-seq-order", outcome: "finalized-inconsistent", complete: "true", code: exitNotClean, lines: []string{"writer-defect"}},
	}
	for _, tt := range tests {
		t.Run(tt.id, func(t *testing.T) {
			t.Parallel()
			code, stdout, stderr := runArgs(t, defaultConfig(), "verify", corpusPack(tt.id))
			assert.Equal(t, tt.code, code)
			assert.Empty(t, stderr)
			assert.Equal(t, []string{corpusPack(tt.id)}, reportLines(stdout, "pack"))
			assert.Equal(t, []string{tt.outcome}, reportLines(stdout, "outcome"))
			assert.Equal(t, []string{tt.complete}, reportLines(stdout, "complete"))
			for _, name := range tt.lines {
				assert.NotEmpty(t, reportLines(stdout, name), "a %s line", name)
			}
			for _, name := range []string{"finalized", "blocks", "records", "prefix-end"} {
				assert.Len(t, reportLines(stdout, name), 1, "one %s line", name)
			}
		})
	}
}

func TestVerifySeveralPacks(t *testing.T) {
	t.Parallel()

	packs := []string{corpusPack("basic-codec-zstd"), corpusPack("bootstrap-bad-magic"), corpusPack("basic-corrupt-middle")}
	o := &countingOpener{}
	code, stdout, stderr := runArgs(t, o.config(), append([]string{"verify"}, packs...)...)
	assert.Equal(t, exitFailed, code, "the largest code of the packs")
	assert.Equal(t, []string{packs[0], packs[2]}, reportLines(stdout, "pack"), "every pack that verified is reported")
	assert.Equal(t, []string{"finalized-consistent", "corrupt-middle"}, reportLines(stdout, "outcome"))
	assert.Contains(t, stderr, packs[1])
	assert.Equal(t, 1, strings.Count(stderr, "\n"), "one error line")

	opened, open := o.counts()
	assert.Equal(t, 3, opened)
	assert.Zero(t, open, "every file closed")
	assert.Equal(t, 1, o.maxOpen, "each pack closed before the next is opened")

	code, _, _ = runArgs(t, defaultConfig(), "verify", packs[0], packs[2])
	assert.Equal(t, exitNotClean, code)
}

// coveragePack writes a finalized pack without records whose pack metadata declares seqs 10 to 12 of its capture lost.
func coveragePack(t *testing.T) []byte {
	t.Helper()
	hourNs := time.Date(2026, 10, 18, 3, 0, 0, 0, time.UTC).UnixNano()
	capture := tracepack.UUID{0x01, 0x9a, 0, 0, 0, 0, 0x70, 0, 0x80, 0, 0, 0, 0, 0, 0, 1}
	first, last := uint64(10), uint64(12)
	start, end := hourNs+int64(time.Minute), hourNs+int64(2*time.Minute)
	meta := &tracepack.PackMeta{
		ToolID: "EQP-01", Recorder: "test-recorder/1.0", Writer: "test-writer/1.0",
		PackRole: tracepack.PackRoleSegment, ScopeGeneration: new(uint64(0)),
		PeriodStart: hourNs, PeriodEnd: hourNs + int64(time.Hour),
		LifecycleCoverage: tracepack.LifecycleCoverageSubscribed,
		Coverage: []tracepack.Coverage{{
			CaptureID: &capture, SeqFirst: &first, SeqLast: &last, TimeStart: &start, TimeEnd: &end,
		}},
	}
	var buf bytes.Buffer
	w, err := tracepack.NewWriter(&buf, tracepack.WriterOptions{Meta: meta, CaptureID: capture, Facts: tracepack.PackFacts{CoverageLoss: true}})
	require.NoError(t, err)
	_, err = w.Close()
	require.NoError(t, err)

	return buf.Bytes()
}

// TestVerifyCompleteLeavesOutCoverage verifies a consistent pack that declares a lost range:
// verify is clean and complete, while a read of the pack, and so dump, is incomplete.
func TestVerifyCompleteLeavesOutCoverage(t *testing.T) {
	t.Parallel()

	path := filepath.Join(t.TempDir(), "coverage.tpk")
	require.NoError(t, os.WriteFile(path, coveragePack(t), 0o600))

	code, stdout, _ := runArgs(t, defaultConfig(), "verify", path)
	assert.Equal(t, exitClean, code)
	assert.Equal(t, []string{"finalized-consistent"}, reportLines(stdout, "outcome"))
	assert.Equal(t, []string{"true"}, reportLines(stdout, "complete"))

	code, _, stderr := runArgs(t, defaultConfig(), "dump", "--jsonl", path)
	assert.Equal(t, exitNotClean, code)
	assert.Contains(t, stderr, "incomplete: coverage")

	// A stop-unclean boundary is the same: a crash gap of a read, outside verify's complete.
	code, _, _ = runArgs(t, defaultConfig(), "verify", corpusPack("close-plain"))
	assert.Equal(t, exitClean, code)
	code, _, stderr = runArgs(t, defaultConfig(), "dump", "--jsonl", corpusPack("close-plain"))
	assert.Equal(t, exitNotClean, code)
	assert.Contains(t, stderr, "incomplete: crash-gap")
}
