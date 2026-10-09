package corpus

import (
	"bytes"
	"crypto/sha256"
	"encoding/binary"
	"fmt"
	"time"

	"github.com/arloliu/go-secs/tracepack"
	"github.com/arloliu/go-secs/tracepack/internal/format"
)

// Deterministic inputs of every generated pack.
const (
	// TimeBase is the corpus's time base, in nanoseconds since the Unix epoch: an hour-aligned instant.
	// Every Writer and Repair of the corpus stamps it as writer_start_utc_ns,
	// and every generated identifier carries its millisecond.
	TimeBase int64 = 1_791_000_000_000_000_000

	// corpusWriter is the writer tag of every generated pack and patch.
	corpusWriter = "tracepack-corpus"
	// corpusClassifier is the classifier tag of a pack holding classified records.
	// It is fixed, never the classifier package's own name, which depends on build information.
	corpusClassifier = "tracepack-sem3"
	// corpusToolID is the tool_id of every generated pack.
	corpusToolID = "corpus-tool"
	// corpusRecorder is the recorder tag of every generated pack.
	corpusRecorder = "tracepack-corpus"

	// segmentPeriod is the length of a generated segment's period, from TimeBase.
	segmentPeriod = int64(time.Minute)
)

// Roles of the identifiers the corpus derives from an identity seed.
const (
	rolePackID             = "pack_id"
	roleCaptureID          = "capture_id"
	roleRecorderInstanceID = "recorder_instance_id"
	roleReplacementSetID   = "replacement_set_id"
	roleRepairPackID       = "repair_pack_id"
)

// HSMS SType values of the control frames the recipes build (SEMI E37 §8.3).
const (
	stypeData      byte = 0
	stypeSelectReq byte = 1
	stypeSelectRsp byte = 2
)

// controlSessionID is the SessionID of a control frame that names no session (SEMI E37 §8.3).
const controlSessionID uint16 = 0xFFFF

// corpusNow is the clock of every Writer and Repair of the corpus: it always returns TimeBase.
func corpusNow() time.Time {
	return time.Unix(0, TimeBase).UTC()
}

// IDFor returns the identifier of role for the identity seed seed:
// the UUIDv7 of TimeBase's millisecond whose other bits are the first 10 bytes of SHA-256(role + "/" + seed).
// Vectors that share a seed share every identifier.
func IDFor(role, seed string) tracepack.UUID {
	sum := sha256.Sum256([]byte(role + "/" + seed))

	return tracepack.UUID(format.NewUUIDv7(TimeBase/int64(time.Millisecond), [10]byte(sum[:10])))
}

// msAt returns the ts_utc_ns ms milliseconds after TimeBase.
func msAt(ms int64) int64 {
	return TimeBase + ms*int64(time.Millisecond)
}

// segmentMeta returns the pack metadata of a generator segment of seed:
// its period the first minute of TimeBase's hour, seq_start 0, scope generation 0,
// the fixed tool, recorder and writer, and the recorder instance of seed.
func segmentMeta(seed string) *tracepack.PackMeta {
	return &tracepack.PackMeta{
		ToolID:             corpusToolID,
		Transport:          tracepack.TransportHSMSSS,
		CaptureMethod:      tracepack.CaptureMethodGenerator,
		Vantage:            tracepack.VantageNone,
		Recorder:           corpusRecorder,
		Writer:             corpusWriter,
		TimeSource:         tracepack.TimeSourceGenerator,
		PeriodStart:        TimeBase,
		PeriodEnd:          TimeBase + segmentPeriod,
		LifecycleCoverage:  tracepack.LifecycleCoverageNone,
		PackRole:           tracepack.PackRoleSegment,
		RecorderInstanceID: IDFor(roleRecorderInstanceID, seed),
		ScopeGeneration:    new(uint64(0)),
	}
}

// packSpec is what writeSpec writes.
type packSpec struct {
	// seed is the identity seed of the pack's identifiers.
	seed string
	// meta is the pack metadata.
	meta *tracepack.PackMeta
	// codec compresses the blocks and the footer.
	codec tracepack.Codec
	// blocks holds the records of each block, one Flush after each.
	blocks [][]tracepack.Record
	// open leaves the pack unfinalized: no footer and no trailer.
	open bool
	// packID, when not zero, replaces the pack_id seed gives.
	packID tracepack.UUID
	// threshold is the Writer's block size threshold; zero means the default.
	threshold int
}

// writeSpec writes the pack of s with the public Writer:
// pack_id and capture_id from the seed unless s.packID is set, the corpus clock,
// each element of s.blocks flushed as one block, then the pack closed unless s.open.
func writeSpec(s *packSpec) ([]byte, error) {
	var buf bytes.Buffer
	w, err := tracepack.NewWriter(&buf, s.options())
	if err != nil {
		return nil, err
	}
	for i, b := range s.blocks {
		for j := range b {
			if err := w.Append(&b[j]); err != nil {
				return nil, fmt.Errorf("corpus: block %d, record %d: %w", i, j, err)
			}
		}
		if err := w.Flush(); err != nil {
			return nil, fmt.Errorf("corpus: block %d: %w", i, err)
		}
	}
	if !s.open {
		if _, err := w.Close(); err != nil {
			return nil, err
		}
	}

	return buf.Bytes(), nil
}

// options returns the options of the Writer of s:
// pack_id and capture_id from the seed unless s.packID is set, the codec, the block size threshold and the corpus clock.
func (s *packSpec) options() tracepack.WriterOptions {
	packID := s.packID
	if packID.IsZero() {
		packID = IDFor(rolePackID, s.seed)
	}

	return tracepack.WriterOptions{
		Meta: s.meta, PackID: packID, CaptureID: IDFor(roleCaptureID, s.seed), Codec: s.codec, BlockThreshold: s.threshold, Now: corpusNow,
	}
}

// hsmsFrame returns an HSMS frame: the 4-byte length, the 10-byte header and text.
// b6 and b7 are header bytes 6 and 7: stream with W and function for a data message,
// whatever the control message's SType gives them otherwise.
func hsmsFrame(sessionID uint16, b6, b7, ptype, stype byte, systemBytes uint32, text []byte) []byte {
	f := binary.BigEndian.AppendUint32(nil, uint32(10+len(text)))
	f = binary.BigEndian.AppendUint16(f, sessionID)
	f = append(f, b6, b7, ptype, stype)
	f = binary.BigEndian.AppendUint32(f, systemBytes)

	return append(f, text...)
}

// dataFrame returns the frame of the data message SsFf of SessionID 1, W set when wbit, with text.
func dataFrame(stream, function byte, wbit bool, systemBytes uint32, text []byte) []byte {
	b6 := stream
	if wbit {
		b6 |= 0x80
	}

	return hsmsFrame(1, b6, function, 0, stypeData, systemBytes, text)
}

// controlFrame returns the frame of a control message of stype, header bytes 6 and 7 as given.
func controlFrame(stype, b6, b7 byte, systemBytes uint32) []byte {
	return hsmsFrame(controlSessionID, b6, b7, 0, stype, systemBytes, nil)
}

// frameRecord returns a record of kind data or control carrying frame, at seq and ts, wire-exact,
// with the field_validity of the captured bytes.
func frameRecord(kind tracepack.Kind, seq uint64, ts int64, epoch uint32, dir tracepack.Dir, frame []byte) tracepack.Record {
	r := tracepack.Record{
		Seq: seq, TSUTCNs: ts, Epoch: epoch, Kind: kind, Dir: dir,
		Fidelity: tracepack.FidelityWireExact, Payload: frame,
	}
	r.SetCapturedFieldValidity()

	return r
}

// newData returns a data record of seq at msAt(seq) carrying frame.
func newData(seq uint64, epoch uint32, dir tracepack.Dir, frame []byte) tracepack.Record {
	return frameRecord(tracepack.KindData, seq, msAt(int64(seq)), epoch, dir, frame)
}

// newControl returns a control record of seq at msAt(seq) carrying frame.
func newControl(seq uint64, epoch uint32, dir tracepack.Dir, frame []byte) tracepack.Record {
	return frameRecord(tracepack.KindControl, seq, msAt(int64(seq)), epoch, dir, frame)
}

// newEvent returns a transport-event record of seq at msAt(seq) carrying ev, local and synthesized.
func newEvent(seq uint64, epoch uint32, ev *tracepack.TransportEvent) (tracepack.Record, error) {
	payload, err := ev.MarshalBinary()
	if err != nil {
		return tracepack.Record{}, err
	}

	return tracepack.Record{
		Seq: seq, TSUTCNs: msAt(int64(seq)), Epoch: epoch,
		Kind: tracepack.KindTransportEvent, Dir: tracepack.DirLocal, Fidelity: tracepack.FidelitySynthesized,
		DecodeStatus: tracepack.DecodeStatusNotApplicable, Payload: payload,
	}, nil
}

// newAnnotation returns an annotation record of seq at msAt(seq) carrying a, local and synthesized.
func newAnnotation(seq uint64, epoch uint32, a *tracepack.Annotation) (tracepack.Record, error) {
	payload, err := a.MarshalBinary()
	if err != nil {
		return tracepack.Record{}, err
	}

	return tracepack.Record{
		Seq: seq, TSUTCNs: msAt(int64(seq)), Epoch: epoch,
		Kind: tracepack.KindAnnotation, Dir: tracepack.DirLocal, Fidelity: tracepack.FidelitySynthesized,
		DecodeStatus: tracepack.DecodeStatusNotApplicable, Payload: payload,
	}, nil
}

// newNote returns an annotation record of seq holding the note text.
func newNote(seq uint64, epoch uint32, text string) (tracepack.Record, error) {
	return newAnnotation(seq, epoch, &tracepack.Annotation{AnnotationKind: tracepack.AnnotationKindNote, Text: &text})
}

// newSocketEvent returns a transport-event record of seq for a socket event of epoch: a socket-connect or a socket-close.
func newSocketEvent(seq uint64, epoch uint32, ev tracepack.Event) (tracepack.Record, error) {
	return newEvent(seq, epoch, &tracepack.TransportEvent{Event: ev, SocketRole: new(tracepack.SocketRoleActive)})
}
