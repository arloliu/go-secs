package main

import (
	"encoding/binary"
	"fmt"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/arloliu/go-secs/tracepack"
)

// findOrigin is the capture origin of the captures the find tests record, inside UTC hour findHour.
var findOrigin = time.Date(2026, 10, 6, 8, 0, 0, 0, time.UTC)

// findHour is the UTC hour of every record of those captures.
var findHour = tracepack.HourOf(findOrigin)

// s1Frame returns an HSMS data message frame of stream 1: session 0, the function, the W bit and system bytes, and body.
func s1Frame(function byte, wbit bool, system uint32, body []byte) []byte {
	f := make([]byte, 14, 14+len(body))
	binary.BigEndian.PutUint32(f[0:4], uint32(10+len(body)))
	f[6] = 1
	if wbit {
		f[6] |= 0x80
	}
	f[7] = function
	binary.BigEndian.PutUint32(f[10:14], system)

	return append(f, body...)
}

// capture describes a capture a find test records.
type capture struct {
	tool string
	// replies is the number of S1F2 replies recorded after the S1F1 primary.
	replies int
	// open leaves the capture without its stop: its segment is committed by a rotation instead of the close.
	open bool
}

// recordCapture records c into a directory sink at root, in hour findHour:
// the start record (seq 0), an S1F1 W primary (seq 1), its replies, and, unless c.open, the stop.
// It returns the capture's id.
func recordCapture(t *testing.T, root string, c capture) tracepack.UUID {
	t.Helper()
	ctx := t.Context()
	sink, err := tracepack.NewDirSink(root, "", tracepack.DirSinkOptions{})
	require.NoError(t, err)
	w, err := tracepack.NewSegmentWriter(ctx, tracepack.SegmentWriterOptions{
		Capture: tracepack.CaptureDescriptor{
			ToolID:             c.tool,
			Transport:          tracepack.TransportHSMSSS,
			CaptureMethod:      tracepack.CaptureMethodRawStream,
			Vantage:            tracepack.VantageHost,
			Recorder:           "test-recorder/1.0",
			RecorderInstanceID: tracepack.UUID{0x01, 0x9a, 0x2b, 0x3c, 0x4d, 0x5e, 0x70, 0x00, 0x80, 0x00, 0, 0, 0, 0, 0, 1},
			TimeSource:         tracepack.TimeSourceCaptureClock,
			CaptureOrigin:      findOrigin,
			LifecycleCoverage:  tracepack.LifecycleCoverageSubscribed,
			QualityEvaluated:   true,
		},
		Sink: sink,
		Now:  func() time.Time { return findOrigin.Add(10 * time.Minute) },
	})
	require.NoError(t, err)
	require.NoError(t, w.AppendFrame(ctx, findOrigin.Add(time.Second), tracepack.DirHostToEquipment, 1, s1Frame(1, true, 7, nil)))
	for k := range c.replies {
		at := findOrigin.Add(time.Duration(2+k) * time.Second)
		require.NoError(t, w.AppendFrame(ctx, at, tracepack.DirEquipmentToHost, 1, s1Frame(2, false, 7, nil)))
	}
	if c.open {
		require.NoError(t, w.Rotate(ctx))
	} else {
		require.NoError(t, w.Close(ctx))
	}

	return w.CaptureID()
}

// findArgs returns the find command line for the primary of capture id, seq 1, by --hour, over paths.
func findArgs(id tracepack.UUID, paths ...string) []string {
	return append([]string{"find", "--capture", id.String(), "--seq", "1", "--hour", strconv.FormatInt(findHour, 10)}, paths...)
}

// recordRoles returns the seq, roles and flags of each record line of a find report.
func recordRoles(report string) []string {
	lines := reportLines(report, "record")
	out := make([]string, 0, len(lines))
	for _, v := range lines {
		f := strings.Fields(v)
		out = append(out, fmt.Sprintf("%s %s %s", f[1], f[11], f[13]))
	}

	return out
}

func TestFindOutcomes(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name     string
		capture  capture
		outcome  string
		code     int
		records  []string
		gaps     []string // the reasons of the incomplete lines
		windowAt string
	}{
		{
			name: "matched", capture: capture{tool: "EQP-01", replies: 1}, outcome: "matched", code: exitClean,
			records:  []string{"1 primary none", "2 candidate in-window,decidable,eligible,valid", "3 closing bound"},
			windowAt: "3",
		},
		{
			name: "not found", capture: capture{tool: "EQP-01"}, outcome: "not-found", code: exitClean,
			records:  []string{"1 primary none", "2 closing bound"},
			windowAt: "2",
		},
		{
			name: "ambiguous", capture: capture{tool: "EQP-01", replies: 2}, outcome: "ambiguous", code: exitClean,
			records: []string{
				"1 primary none", "2 candidate in-window,decidable,eligible,valid", "3 candidate in-window,decidable,eligible,valid", "4 closing bound",
			},
			windowAt: "4",
		},
		{
			name: "open window", capture: capture{tool: "EQP-01", replies: 1, open: true}, outcome: "matched", code: exitNotClean,
			records:  []string{"1 primary none", "2 candidate in-window,decidable,eligible,valid"},
			gaps:     []string{"open-window"},
			windowAt: "open",
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			root := t.TempDir()
			id := recordCapture(t, root, tt.capture)
			o := &countingOpener{}
			code, stdout, stderr := runArgs(t, o.config(), findArgs(id, root)...)
			assert.Equal(t, tt.code, code, "stderr %s", stderr)
			assert.Empty(t, stderr)
			assert.Equal(t, []string{tt.outcome}, reportLines(stdout, "outcome"))
			assert.Equal(t, []string{strconv.FormatBool(tt.code == exitClean)}, reportLines(stdout, "complete"))
			assert.Equal(t, tt.records, recordRoles(stdout))
			assert.Equal(t, []string{tt.windowAt}, reportLines(stdout, "window-end"))
			var gaps []string
			for _, g := range reportLines(stdout, "incomplete") {
				gaps = append(gaps, strings.TrimSuffix(strings.Fields(g)[0], ":"))
			}
			assert.Equal(t, tt.gaps, gaps)
			assert.Len(t, reportLines(stdout, "searched"), tracepack.DefaultTxMaxScopes)
			opened, open := o.counts()
			assert.Positive(t, opened)
			assert.Zero(t, open, "every file closed")
		})
	}
}

// segments returns the committed segments under a directory sink's root, in lexical order.
func segments(t *testing.T, root string) []string {
	t.Helper()
	paths, err := packPaths([]string{root})
	require.NoError(t, err)

	return paths
}

func TestFindIncompleteReads(t *testing.T) {
	t.Parallel()

	root := t.TempDir()
	id := recordCapture(t, root, capture{tool: "EQP-01", replies: 1})
	paths := segments(t, root)
	require.Len(t, paths, 2, "the records and the stop, each in a segment")

	t.Run("cut inside the window", func(t *testing.T) {
		t.Parallel()
		// The first segment holds the primary and its reply; cut inside its block, it loses both.
		pack, err := os.ReadFile(paths[0])
		require.NoError(t, err)
		r := openBytes(t, pack)
		b := r.Blocks()[0]
		cut := pack[:b.Offset+uint64(b.OnDiskLen)/2]
		o := &countingOpener{mem: map[string][]byte{paths[0]: cut}}
		code, stdout, stderr := runArgs(t, o.config(), findArgs(id, paths...)...)
		assert.Equal(t, exitNotClean, code, "stderr %s", stderr)
		assert.Equal(t, []string{"not-found"}, reportLines(stdout, "outcome"))
		assert.NotEmpty(t, reportLines(stdout, "incomplete"))
	})
	t.Run("block over the reader budget", func(t *testing.T) {
		t.Parallel()
		cfg := defaultConfig()
		cfg.reader.MaxBlockLen = 1
		code, stdout, stderr := runArgs(t, cfg, findArgs(id, root)...)
		assert.Equal(t, exitNotClean, code, "a budget gap is a report, not an error; stderr %s", stderr)
		assert.Empty(t, stderr)
		reasons := reportLines(stdout, "incomplete")
		require.NotEmpty(t, reasons)
		assert.Regexp(t, `^read[ :]`, reasons[0], "a read gap")
		assert.Contains(t, reasons[0], tracepack.ErrReadLimit.Error())
	})
	t.Run("at instead of hour", func(t *testing.T) {
		t.Parallel()
		_, byHour, _ := runArgs(t, defaultConfig(), findArgs(id, root)...)
		code, byAt, stderr := runArgs(t, defaultConfig(), "find", "--capture", id.String(), "--seq", "1",
			"--at", findOrigin.Add(time.Second).Format(time.RFC3339Nano), root)
		assert.Equal(t, exitClean, code, "stderr %s", stderr)
		assert.Equal(t, byHour, byAt)
	})
	t.Run("hidden directory skipped", func(t *testing.T) {
		t.Parallel()
		// A copy of a segment in a hidden directory, as a crash leaves one in .partial/, would repeat its pack_id.
		dir := t.TempDir()
		for k, p := range paths {
			b, err := os.ReadFile(p)
			require.NoError(t, err)
			require.NoError(t, os.WriteFile(filepath.Join(dir, fmt.Sprintf("%d.tpk", k)), b, 0o600))
			require.NoError(t, os.MkdirAll(filepath.Join(dir, ".partial"), 0o700))
			require.NoError(t, os.WriteFile(filepath.Join(dir, ".partial", fmt.Sprintf("%d.tpk", k)), b, 0o600))
		}
		code, _, stderr := runArgs(t, defaultConfig(), findArgs(id, dir)...)
		assert.Equal(t, exitClean, code, "stderr %s", stderr)
		code, _, _ = runArgs(t, defaultConfig(), findArgs(id, filepath.Join(dir, ".partial"))...)
		assert.Equal(t, exitClean, code, "a hidden directory named as an argument is searched")
	})
}

// openBytes opens a pack held in memory.
func openBytes(t *testing.T, pack []byte) *tracepack.Reader {
	t.Helper()
	o := &countingOpener{mem: map[string][]byte{"p": pack}}
	f, size, err := o.openPath("p")
	require.NoError(t, err)
	r, err := tracepack.Open(t.Context(), f, size, tracepack.ReaderOptions{})
	require.NoError(t, err)

	return r
}

func TestFindFails(t *testing.T) {
	t.Parallel()

	root := t.TempDir()
	id := recordCapture(t, root, capture{tool: "EQP-01", replies: 1})
	other := t.TempDir()
	recordCapture(t, other, capture{tool: "EQP-02", replies: 1})
	paths := segments(t, root)
	hour := strconv.FormatInt(findHour, 10)

	tests := []struct {
		name   string
		args   []string
		stderr string
		opened int // the files opened, -1 for a usage error that opens none
	}{
		{name: "not a primary", args: append([]string{"find", "--capture", id.String(), "--seq", "2", "--hour", hour}, root), stderr: tracepack.ErrNotPrimary.Error(), opened: 2},
		{name: "two tools", args: findArgs(id, root, other), stderr: "tracepack find: ", opened: 4},
		{name: "a pack twice", args: findArgs(id, paths[0], paths[0]), stderr: "tracepack find: ", opened: 2},
		{name: "an extract", args: findArgs(id, corpusPack("basic-extract-policy-no-classifier-1-0")), stderr: "tracepack find: ", opened: 1},
		{name: "a rejected pack", args: findArgs(id, paths[0], corpusPack("bootstrap-bad-magic")), stderr: corpusPack("bootstrap-bad-magic"), opened: 2},
		{name: "no pack", args: findArgs(id, t.TempDir()), stderr: errNoPacks.Error(), opened: 0},
		{name: "hour and at", args: []string{"find", "--capture", id.String(), "--seq", "1", "--hour", hour, "--at", "2026-10-06T08:00:00Z", root}, stderr: "exactly one of --hour and --at", opened: -1},
		{name: "neither hour nor at", args: []string{"find", "--capture", id.String(), "--seq", "1", root}, stderr: "exactly one of --hour and --at", opened: -1},
		{name: "bad at", args: []string{"find", "--capture", id.String(), "--seq", "1", "--at", "today", root}, stderr: "--at: ", opened: -1},
		{name: "bad capture", args: []string{"find", "--capture", "x", "--seq", "1", "--hour", hour, root}, stderr: "--capture: ", opened: -1},
		{name: "no path", args: findArgs(id), stderr: "one or more paths", opened: -1},
		{name: "hour out of range", args: []string{"find", "--capture", id.String(), "--seq", "1", "--hour", strconv.FormatInt(tracepack.MaxTxHour+1, 10), root}, stderr: "tracepack find: ", opened: 2},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			o := &countingOpener{}
			code, stdout, stderr := runArgs(t, o.config(), tt.args...)
			assert.Equal(t, exitFailed, code)
			assert.Empty(t, stdout)
			assert.Contains(t, stderr, tt.stderr)
			opened, open := o.counts()
			assert.Equal(t, max(tt.opened, 0), opened)
			assert.Zero(t, open, "every file closed")
		})
	}
}
