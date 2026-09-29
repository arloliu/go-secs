package tracepack_test

import (
	"fmt"
	"testing"

	"github.com/arloliu/go-secs/tracepack"
	"github.com/stretchr/testify/require"
)

// u8Enum is the constraint satisfied by every tracepack enum type: a distinct uint8 with a String method.
type u8Enum interface {
	~uint8
	fmt.Stringer
}

// enumCase is one (numeric value, expected name) row transcribed from the tracepack format specification §9.
type enumCase struct {
	Value uint8
	Name  string
}

// checkEnumStrings asserts that every case in cases round-trips through T's String method,
// and that a value one past the last registered value formats as "unknown(<n>)".
func checkEnumStrings[T u8Enum](t *testing.T, cases []enumCase, pastLast uint8) {
	t.Helper()

	for _, c := range cases {
		t.Run(fmt.Sprintf("%d", c.Value), func(t *testing.T) {
			t.Parallel()
			require.Equal(t, c.Name, T(c.Value).String())
		})
	}

	t.Run("unknown", func(t *testing.T) {
		t.Parallel()
		require.Equal(t, fmt.Sprintf("unknown(%d)", pastLast), T(pastLast).String())
	})
}

func TestKind_String(t *testing.T) {
	t.Parallel()

	// Transcribed from the tracepack format specification §9, `kind` row.
	cases := []enumCase{
		{0, "unknown"},
		{1, "data"},
		{2, "control"},
		{3, "transport-event"},
		{4, "annotation"},
	}
	checkEnumStrings[tracepack.Kind](t, cases, 5)
}

func TestDir_String(t *testing.T) {
	t.Parallel()

	// Transcribed from the tracepack format specification §9, `dir` row.
	cases := []enumCase{
		{0, "unknown"},
		{1, "host-to-equipment"},
		{2, "equipment-to-host"},
		{3, "local"},
	}
	checkEnumStrings[tracepack.Dir](t, cases, 4)
}

func TestFidelity_String(t *testing.T) {
	t.Parallel()

	// Transcribed from the tracepack format specification §9, `fidelity` row.
	cases := []enumCase{
		{0, "unknown"},
		{1, "wire-exact"},
		{2, "re-encoded"},
		{3, "reconstructed"},
		{4, "synthesized"},
		{5, "not-applicable"},
	}
	checkEnumStrings[tracepack.Fidelity](t, cases, 6)
}

func TestDecodeStatus_String(t *testing.T) {
	t.Parallel()

	// Transcribed from the tracepack format specification §9, `decode_status` row.
	cases := []enumCase{
		{0, "not-attempted"},
		{1, "ok"},
		{2, "ok-with-trailing"},
		{3, "short-frame"},
		{4, "length-mismatch"},
		{5, "bad-ptype"},
		{6, "bad-stype"},
		{7, "control-with-body"},
		{8, "oversized"},
		{9, "item-decode-error"},
		{10, "reconstructed-ok"},
		{11, "parse-failed"},
		{12, "build-rejected"},
		{13, "not-applicable"},
	}
	checkEnumStrings[tracepack.DecodeStatus](t, cases, 14)
}

func TestDecodeStatus_MalformedAndClean(t *testing.T) {
	t.Parallel()

	// The malformed set is transcribed from the tracepack semantics specification §3:
	// decode_status ∈ {short-frame, length-mismatch, bad-ptype, bad-stype, control-with-body, oversized, item-decode-error}.
	// Every other known value is clean; a value outside the registry is neither (§6: its decode state is unknown).
	tests := []struct {
		status           tracepack.DecodeStatus
		malformed, clean bool
	}{
		{tracepack.DecodeStatusNotAttempted, false, true},
		{tracepack.DecodeStatusOK, false, true},
		{tracepack.DecodeStatusOKWithTrailing, false, true},
		{tracepack.DecodeStatusShortFrame, true, false},
		{tracepack.DecodeStatusLengthMismatch, true, false},
		{tracepack.DecodeStatusBadPType, true, false},
		{tracepack.DecodeStatusBadSType, true, false},
		{tracepack.DecodeStatusControlWithBody, true, false},
		{tracepack.DecodeStatusOversized, true, false},
		{tracepack.DecodeStatusItemDecodeError, true, false},
		{tracepack.DecodeStatusReconstructedOK, false, true},
		{tracepack.DecodeStatusParseFailed, false, true},
		{tracepack.DecodeStatusBuildRejected, false, true},
		{tracepack.DecodeStatusNotApplicable, false, true},
		{tracepack.DecodeStatus(14), false, false},
		{tracepack.DecodeStatus(255), false, false},
	}
	for _, tt := range tests {
		t.Run(tt.status.String(), func(t *testing.T) {
			t.Parallel()
			require.Equal(t, tt.malformed, tt.status.Malformed(), "Malformed")
			require.Equal(t, tt.clean, tt.status.Clean(), "Clean")
		})
	}
}

func TestTransport_String(t *testing.T) {
	t.Parallel()

	// Transcribed from the tracepack format specification §9, `transport` row.
	cases := []enumCase{
		{0, "unknown"},
		{1, "hsms-ss"},
		{2, "secs1-normalised"},
	}
	checkEnumStrings[tracepack.Transport](t, cases, 3)
}

func TestCaptureMethod_String(t *testing.T) {
	t.Parallel()

	// Transcribed from the tracepack format specification §9, `capture_method` row.
	cases := []enumCase{
		{0, "unknown"},
		{1, "raw-stream"},
		{2, "decoded-message"},
		{3, "log"},
		{4, "generator"},
	}
	checkEnumStrings[tracepack.CaptureMethod](t, cases, 5)
}

func TestVantage_String(t *testing.T) {
	t.Parallel()

	// Transcribed from the tracepack format specification §9, `vantage` row.
	cases := []enumCase{
		{0, "unknown"},
		{1, "host"},
		{2, "equipment"},
		{3, "intermediary"},
		{4, "network"},
		{5, "none"},
	}
	checkEnumStrings[tracepack.Vantage](t, cases, 6)
}

func TestTimeSource_String(t *testing.T) {
	t.Parallel()

	// Transcribed from the tracepack format specification §9, `time_source` row.
	cases := []enumCase{
		{0, "unknown"},
		{1, "capture-clock"},
		{2, "source-log"},
		{3, "generator"},
	}
	checkEnumStrings[tracepack.TimeSource](t, cases, 4)
}

func TestLifecycleCoverage_String(t *testing.T) {
	t.Parallel()

	// Transcribed from the tracepack format specification §9, `lifecycle_coverage` row.
	cases := []enumCase{
		{0, "unknown"},
		{1, "subscribed"},
		{2, "none"},
	}
	checkEnumStrings[tracepack.LifecycleCoverage](t, cases, 3)
}

func TestPackRole_String(t *testing.T) {
	t.Parallel()

	// Transcribed from the tracepack format specification §9, `pack_role` row.
	cases := []enumCase{
		{0, "unknown"},
		{1, "segment"},
		{2, "archive"},
		{3, "extract"},
		{4, "repair"},
	}
	// 5 is the retired correction role, reported like any unknown value.
	checkEnumStrings[tracepack.PackRole](t, cases, 5)
}

func TestDigestAlgorithm_String(t *testing.T) {
	t.Parallel()

	// Transcribed from the tracepack format specification §9, `digest_algorithm` row.
	cases := []enumCase{
		{0, "unknown"},
		{1, "hmac-sha256"},
	}
	checkEnumStrings[tracepack.DigestAlgorithm](t, cases, 2)
}

func TestEvent_String(t *testing.T) {
	t.Parallel()

	// Transcribed from the tracepack format specification §9, `event` row.
	cases := []enumCase{
		{0, "unknown"},
		{1, "state-transition"},
		{2, "timer-expiry"},
		{3, "socket-accept"},
		{4, "socket-connect"},
		{5, "socket-close"},
		{6, "capture-boundary"},
		{7, "clock-step"},
	}
	checkEnumStrings[tracepack.Event](t, cases, 8)
}

func TestState_String(t *testing.T) {
	t.Parallel()

	// Transcribed from the tracepack format specification §9, `state` row.
	cases := []enumCase{
		{0, "unknown"},
		{1, "not-connected"},
		{2, "not-selected"},
		{3, "selected"},
	}
	checkEnumStrings[tracepack.State](t, cases, 4)
}

func TestTimer_String(t *testing.T) {
	t.Parallel()

	// Transcribed from the tracepack format specification §9, `timer` row: 0 none; 1-8 = T1..T8.
	cases := []enumCase{
		{0, "none"},
		{1, "T1"},
		{2, "T2"},
		{3, "T3"},
		{4, "T4"},
		{5, "T5"},
		{6, "T6"},
		{7, "T7"},
		{8, "T8"},
	}
	checkEnumStrings[tracepack.Timer](t, cases, 9)
}

func TestCause_String(t *testing.T) {
	t.Parallel()

	// Transcribed from the tracepack format specification §9, `cause` row.
	cases := []enumCase{
		{0, "unknown"},
		{1, "local-open"},
		{2, "local-close"},
		{3, "select-accepted"},
		{4, "select-rejected"},
		{5, "local-deselect"},
		{6, "peer-deselect"},
		{7, "local-separate"},
		{8, "peer-separate"},
		{9, "timer-expiry"},
		{10, "linktest-failure"},
		{11, "transport-error"},
		{12, "peer-close"},
		{13, "implementation-fault"},
	}
	checkEnumStrings[tracepack.Cause](t, cases, 14)
}

func TestSocketRole_String(t *testing.T) {
	t.Parallel()

	// Transcribed from the tracepack format specification §9, `socket_role` row.
	cases := []enumCase{
		{0, "unknown"},
		{1, "passive"},
		{2, "active"},
	}
	checkEnumStrings[tracepack.SocketRole](t, cases, 3)
}

func TestBoundaryKind_String(t *testing.T) {
	t.Parallel()

	// Transcribed from the tracepack format specification §9, `boundary_kind` row.
	cases := []enumCase{
		{0, "unknown"},
		{1, "start"},
		{2, "stop"},
		{3, "gap"},
		{4, "stop-unclean"},
	}
	checkEnumStrings[tracepack.BoundaryKind](t, cases, 5)
}

func TestAnnotationKind_String(t *testing.T) {
	t.Parallel()

	// Transcribed from the tracepack format specification §9, `annotation_kind` row.
	cases := []enumCase{
		{0, "unknown"},
		{1, "note"},
		{2, "unparsed-entry"},
		{3, "skipped-bytes"},
		{4, "unrecognised-line"},
	}
	checkEnumStrings[tracepack.AnnotationKind](t, cases, 5)
}

func TestCodec_String(t *testing.T) {
	t.Parallel()

	// Transcribed from the tracepack format specification §2, codec table.
	cases := []enumCase{
		{0, "none"},
		{1, "zstd"},
	}
	checkEnumStrings[tracepack.Codec](t, cases, 2)
}
