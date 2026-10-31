package bus

import (
	"testing"

	"github.com/stretchr/testify/require"

	"github.com/arloliu/go-secs/tracepack"
	"github.com/arloliu/go-secs/tracepack/internal/tlv"
)

// Tags and bits the tests spell out, listed here independently of the root package
// (the tracepack format specification §9 and the tracepack storage specification §4).
const (
	// tagHSMSTimers is the hsms_timers tag of the pack metadata.
	tagHSMSTimers uint16 = 0x0026
	// fieldValidityMask holds the six defined field_validity bits.
	fieldValidityMask = tracepack.FieldValiditySessionID | tracepack.FieldValidityStreamAndW | tracepack.FieldValidityFunction |
		tracepack.FieldValidityPType | tracepack.FieldValiditySType | tracepack.FieldValiditySystemBytes
)

// fakeClassifier is a Classifier returning a fixed result, which records the frames it saw.
type fakeClassifier struct {
	name     string
	max      uint64
	status   tracepack.DecodeStatus
	trailing int
	seen     [][]byte
}

var _ tracepack.Classifier = (*fakeClassifier)(nil)

func (c *fakeClassifier) Name() string        { return c.name }
func (c *fakeClassifier) MaxFrameLen() uint64 { return c.max }

func (c *fakeClassifier) Frame(frame []byte) (tracepack.DecodeStatus, int) {
	c.seen = append(c.seen, frame)

	return c.status, c.trailing
}

// descriptorMeta returns a capture-clock raw-stream PackMeta holding every producer-owned entry a descriptor carries:
// a nonzero capture_origin_mono_ns, a zero clock_step_tolerance_ns,
// every optional site and equipment tag, and two thinning entries.
func descriptorMeta() *tracepack.PackMeta {
	role := tracepack.SocketRolePassive

	return &tracepack.PackMeta{
		ToolID:               "tool/A.1",
		Transport:            tracepack.TransportHSMSSS,
		CaptureMethod:        tracepack.CaptureMethodRawStream,
		Vantage:              tracepack.VantageHost,
		Recorder:             "gateway/1.2.3",
		TimeSource:           tracepack.TimeSourceCaptureClock,
		CaptureOriginUTCNs:   new(int64(1_700_000_000_000_000_000)),
		CaptureOriginMonoNs:  new(int64(987_654_321)),
		LifecycleCoverage:    tracepack.LifecycleCoverageSubscribed,
		QualityEvaluated:     true,
		RecorderInstanceID:   tracepack.UUID{0x01, 0x02, 0x03, 0x04, 0x05, 0x06, 0x47, 0x08, 0x89, 0x0A, 0x0B, 0x0C, 0x0D, 0x0E, 0x0F, 0x10},
		PreviousCaptureID:    &tracepack.UUID{0x11, 0x12, 0x13, 0x14, 0x15, 0x16, 0x47, 0x18, 0x89, 0x1A, 0x1B, 0x1C, 0x1D, 0x1E, 0x1F, 0x20},
		SiteID:               new("fab-1"),
		EquipmentModel:       new("model-x"),
		EquipmentSWRev:       new("rev-2"),
		HostSoftware:         new("eap 4.5"),
		HostEndpoint:         new("10.0.0.1:5000"),
		EquipmentEndpoint:    new("10.0.0.2:5000"),
		EquipmentConnectMode: &role,
		DeviceID:             new(uint64(7)),
		HSMSTimers:           &tracepack.HSMSTimers{T3: new(uint64(45_000)), T5: new(uint64(10_000))},
		ClockStepToleranceNs: new(uint64(0)),
		Thinning: []tracepack.Thinning{
			{Stream: 1, Function: 3, Source: "fdc interval poll"},
			{Stream: 1, Function: 4, Source: "fdc interval reply"},
		},
	}
}

// descriptorBytes returns the encoding of descriptorMeta.
func descriptorBytes(t testing.TB) []byte {
	t.Helper()

	b, err := tracepack.EncodeDescriptor(descriptorMeta())
	require.NoError(t, err)

	return b
}

// descriptorEntries returns the TLV entries of descriptorMeta in the order EncodeDescriptor writes them,
// so a test can drop, repeat or add an entry.
func descriptorEntries(t testing.TB) []tlv.Entry {
	t.Helper()

	entries, err := tlv.Decode(descriptorBytes(t))
	require.NoError(t, err)

	return entries
}

// encodeTLV encodes entries as they are, without any registry check.
func encodeTLV(entries []tlv.Entry) []byte {
	var b []byte
	for _, e := range entries {
		b = tlv.AppendEntry(b, e)
	}

	return b
}

// replaceDescriptorEntry returns entries with every entry of tag replaced by repl, in place.
func replaceDescriptorEntry(entries []tlv.Entry, tag uint16, repl tlv.Entry) []tlv.Entry {
	out := make([]tlv.Entry, 0, len(entries))
	for _, e := range entries {
		if e.Tag == tag {
			e = repl
		}
		out = append(out, e)
	}

	return out
}
