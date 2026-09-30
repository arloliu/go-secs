package tracepack

import (
	"encoding"
	"errors"
	"fmt"
	"slices"

	"github.com/arloliu/go-secs/tracepack/internal/format"
	"github.com/arloliu/go-secs/tracepack/internal/tlv"
)

// Pack metadata tag registry (the tracepack format specification §5).
const (
	tagToolID               uint16 = 0x0002
	tagTransport            uint16 = 0x0003
	tagCaptureMethod        uint16 = 0x0004
	tagVantage              uint16 = 0x0005
	tagRecorder             uint16 = 0x0006
	tagWriter               uint16 = 0x0007
	tagClassifier           uint16 = 0x0008
	tagTimeSource           uint16 = 0x0009
	tagCaptureOriginUTCNs   uint16 = 0x000A
	tagCaptureOriginMonoNs  uint16 = 0x000B
	tagSourceTZ             uint16 = 0x000C
	tagSourceDialect        uint16 = 0x000D
	tagSourceRef            uint16 = 0x000E
	tagMaxFrameLen          uint16 = 0x000F
	tagPeriodStart          uint16 = 0x0010
	tagPeriodEnd            uint16 = 0x0011
	tagLifecycleCoverage    uint16 = 0x0012
	tagQualityEvaluated     uint16 = 0x0013
	tagSupersedes           uint16 = 0x0015
	tagCoverage             uint16 = 0x0016
	tagNotes                uint16 = 0x0017
	tagPackRole             uint16 = 0x0018
	tagCompactionLevel      uint16 = 0x0019
	tagCompactedFrom        uint16 = 0x001A
	tagRecorderInstanceID   uint16 = 0x001B
	tagPreviousCaptureID    uint16 = 0x001C
	tagExtractFilter        uint16 = 0x001D
	tagSiteID               uint16 = 0x001E
	tagEquipmentModel       uint16 = 0x001F
	tagEquipmentSWRev       uint16 = 0x0020
	tagHostSoftware         uint16 = 0x0021
	tagHostEndpoint         uint16 = 0x0022
	tagEquipmentEndpoint    uint16 = 0x0023
	tagEquipmentConnectMode uint16 = 0x0024
	tagDeviceID             uint16 = 0x0025
	tagHSMSTimers           uint16 = 0x0026
	tagSeqStart             uint16 = 0x0027
	tagClockStepToleranceNs uint16 = 0x0028
	tagReplacementSetID     uint16 = 0x0029
	tagReplacementSetSize   uint16 = 0x002A
	tagReplacementSetIndex  uint16 = 0x002B
	tagFlushIntervalNs      uint16 = 0x002C
	tagScopeGeneration      uint16 = 0x002D
	tagPublisherEpoch       uint16 = 0x002E
	tagPatchBase            uint16 = 0x002F
	tagRedactionPolicy      uint16 = 0x0031
	tagRedaction            uint16 = 0x0032
)

// retiredPackMetaTags are the retired pack metadata tag numbers (the tracepack format specification §5):
// a writer never writes them, and a reader treats them as unknown tags.
var retiredPackMetaTags = [...]uint16{0x0001, 0x0014, 0x0030}

// packRoleRetired is the retired pack_role value 5, which a writer never writes (the tracepack format specification §9).
const packRoleRetired = 5

// Nested tags of a pack metadata `coverage` entry (the tracepack format specification §5).
const (
	tagCoverageCaptureID uint16 = 0x0001
	tagCoverageSeqFirst  uint16 = 0x0002
	tagCoverageSeqLast   uint16 = 0x0003
	tagCoverageTimeStart uint16 = 0x0004
	tagCoverageTimeEnd   uint16 = 0x0005
)

// Nested tags of a pack metadata `redaction_policy` entry (the tracepack format specification §5).
const (
	tagRedactionPolicyPolicyID        uint16 = 0x0001
	tagRedactionPolicyPolicyVersion   uint16 = 0x0002
	tagRedactionPolicyKeyID           uint16 = 0x0003
	tagRedactionPolicyDigestAlgorithm uint16 = 0x0004
)

// Nested tags of a pack metadata `redaction` entry (the tracepack format specification §5).
const (
	tagRedactionEntrySeq          uint16 = 0x0001
	tagRedactionEntryItemPath     uint16 = 0x0002
	tagRedactionEntryMaskedRanges uint16 = 0x0003
	tagRedactionEntryDomain       uint16 = 0x0004
	tagRedactionEntryDigest       uint16 = 0x0005
)

// Sentinel errors returned by PackMeta, TransportEvent and Annotation.
var (
	// ErrRequiredTag reports that a "Required when" rule of the tracepack format specification §5 or §8 is not satisfied.
	ErrRequiredTag = errors.New("tracepack: required tag is missing")
	// ErrFieldValue reports a field holding a value the format does not allow (the tracepack format specification §5 and §9):
	// a replacement_set_size other than 1 or a replacement_set_index other than 0, read or written,
	// or the retired pack_role 5, which a writer never writes.
	ErrFieldValue = errors.New("tracepack: field value not allowed")
	// ErrAnnotationText reports an annotation whose text and raw tags are not exactly one of the two
	// (the tracepack format specification §8).
	ErrAnnotationText = errors.New("tracepack: annotation must carry exactly one of text or raw")
	// ErrFieldLength reports a field value whose length does not match what its tag requires.
	ErrFieldLength = errors.New("tracepack: field value has the wrong length")
	// ErrReservedTag reports an Unknown entry that names a tag this package already encodes as a typed field.
	ErrReservedTag = errors.New("tracepack: unknown entry names a tag this package already encodes")
)

// UUID is a 16-byte UUID in RFC 9562 byte order: the order of the hex digits in the canonical string, left to right.
//
// See the tracepack format specification §2.
// The zero value is the nil UUID.
type UUID [16]byte

var _ fmt.Stringer = UUID{}

// RawEntry preserves one TLV entry that PackMeta, TransportEvent or Annotation does not recognize:
// an unknown tag, or a private tag (0x8000-0xFFFF), per the tracepack format specification §5.
//
// MarshalBinary re-encodes every RawEntry after the typed fields, in the order it was decoded,
// so a round trip through UnmarshalPackMeta, UnmarshalTransportEvent or UnmarshalAnnotation reproduces it,
// except a retired pack metadata tag (0x0001, 0x0014, 0x0030), which PackMeta.MarshalBinary drops
// because a writer never writes one (the tracepack format specification §5).
type RawEntry struct {
	// Tag is the entry's tag.
	Tag uint16
	// Type is the entry's raw value_type byte; it may fall outside the registry's defined value types.
	Type uint8
	// Value is the entry's raw value bytes.
	Value []byte
}

// PackFacts carries the facts about a pack's records and its scope
// that PackMeta.Validate needs to evaluate the conditional "Required when" rules of the registry table
// that PackMeta's own fields cannot answer by themselves
// (the tracepack format specification §5).
//
// The zero value asserts none of these facts,
// so only the rules that depend solely on PackMeta's own fields are enforced.
type PackFacts struct {
	// AnyClassified reports that some record's decode_status is neither not-attempted nor not-applicable,
	// which requires the classifier tag (the tracepack format specification §3).
	AnyClassified bool
	// AnyOversized reports that some record was classified oversized,
	// which requires the classifier and max_frame_len tags.
	AnyOversized bool
	// AnyRedacted reports that some record was masked under a redaction policy,
	// which requires at least one redaction entry (the tracepack semantics specification §8).
	AnyRedacted bool
	// RedactionPolicyApplied reports that the pack was written under a redaction policy,
	// whether or not the policy matched any record,
	// which requires the redaction_policy tag.
	RedactionPolicyApplied bool
	// GenerationHasPredecessor reports that this pack's scope_generation replaces an earlier generation,
	// which, together with a non-zero scope_generation, requires the supersedes tag.
	GenerationHasPredecessor bool
	// CoverageLoss reports that a repair pack lost data it cannot recover,
	// which requires at least one coverage entry (the tracepack storage specification §6).
	CoverageLoss bool
	// ScopeHasGeneration reports that the pack's scope already has a published generation,
	// which, for a patch, requires the patch_base tag (the tracepack storage specification §4).
	ScopeHasGeneration bool
	// DurableRecorder reports that the writer is a recorder with a durable spool, or a consumer of a durable bus,
	// which requires the flush_interval_ns tag (the tracepack storage specification §4).
	DurableRecorder bool
}

// Coverage is one lost range of a pack metadata `coverage` entry:
// a repair pack's account of data it could not recover (the tracepack storage specification §6).
//
// See the tracepack format specification §5.
// Every field is optional on read.
// A writer declaring a new entry sets CaptureID, SeqFirst, TimeStart and TimeEnd, and SeqLast when the upper bound of the lost seqs is known;
// an entry a repair inherits is carried as it was read.
type Coverage struct {
	// CaptureID is the capture the lost range belongs to.
	CaptureID *UUID
	// SeqFirst is the first seq of the lost range.
	SeqFirst *uint64
	// SeqLast is the last seq of the lost range.
	SeqLast *uint64
	// TimeStart is the start of the lost range.
	TimeStart *int64
	// TimeEnd is the end of the lost range.
	TimeEnd *int64
	// Unknown holds the nested entries this package does not know, in stored order, a repeated tag included.
	// MarshalBinary writes them after the known nested tags; coverage matching ignores them.
	Unknown []RawEntry
}

// RedactionPolicy is the policy an extract's pack metadata `redaction_policy` entry names
// (the tracepack semantics specification §8).
//
// See the tracepack format specification §5.
// Every field is required whenever RedactionPolicy is present.
type RedactionPolicy struct {
	// PolicyID identifies the policy.
	PolicyID string
	// PolicyVersion is the policy's version.
	PolicyVersion uint64
	// KeyID identifies the key the digests were computed with.
	KeyID string
	// DigestAlgorithm is the algorithm the digests were computed with.
	DigestAlgorithm DigestAlgorithm
}

// MaskedRange is one (offset, length) pair of a RedactionEntry's masked ranges,
// counted from the start of the record payload (the tracepack semantics specification §8).
type MaskedRange struct {
	// Offset is the byte offset of the masked range from the start of the record payload.
	Offset uint64
	// Length is the byte length of the masked range.
	Length uint64
}

// RedactionEntry is one mask of a pack metadata `redaction` entry (the tracepack semantics specification §8).
//
// See the tracepack format specification §5.
// ItemPath is absent for a whole-text or annotation mask, and empty for the root item;
// the other fields are required.
type RedactionEntry struct {
	// Seq is the masked record's seq.
	Seq uint64
	// ItemPath locates the masked item within the record; nil for a whole-text or annotation mask.
	ItemPath *string
	// MaskedRanges lists the masked byte ranges of the record payload.
	MaskedRanges []MaskedRange
	// Domain names the redaction domain the mask belongs to.
	Domain string
	// Digest is the digest of the masked bytes, computed under RedactionPolicy.DigestAlgorithm.
	Digest []byte
}

// HSMSTimers carries the HSMS timers a pack metadata `hsms_timers` entry configures, in milliseconds.
//
// See the tracepack format specification §5.
// Every field is optional.
type HSMSTimers struct {
	T1 *uint64
	T2 *uint64
	T3 *uint64
	T4 *uint64
	T5 *uint64
	T6 *uint64
	T7 *uint64
	T8 *uint64
}

// PackMeta is the pack metadata of a tracepack file:
// the TLV entries the tracepack format specification §5 requires to fill pack_metadata_len bytes,
// decoded into typed fields.
//
// A pointer or slice field is present on the wire only when it is non-nil or non-empty;
// Validate reports which fields the specification's "Required when" column requires for the PackFacts passed to it.
// Unknown preserves every entry whose tag PackMeta does not recognize, private tags included,
// so MarshalBinary reproduces them on a round trip.
// A retired tag (the tracepack format specification §5) that UnmarshalPackMeta finds stays in Unknown like any unknown tag,
// but MarshalBinary never writes one, so a pack written from decoded metadata drops it.
type PackMeta struct {
	ToolID               string
	Transport            Transport
	CaptureMethod        CaptureMethod
	Vantage              Vantage
	Recorder             string
	Writer               string
	Classifier           *string
	TimeSource           TimeSource
	CaptureOriginUTCNs   *int64
	CaptureOriginMonoNs  *int64
	SourceTZ             *string
	SourceDialect        *string
	SourceRefs           []string
	MaxFrameLen          *uint64
	PeriodStart          int64
	PeriodEnd            int64
	LifecycleCoverage    LifecycleCoverage
	QualityEvaluated     bool
	Supersedes           []UUID
	Coverage             []Coverage
	Notes                *string
	PackRole             PackRole
	CompactionLevel      uint8
	CompactedFrom        []UUID
	RecorderInstanceID   UUID
	PreviousCaptureID    *UUID
	ExtractFilter        *string
	SiteID               *string
	EquipmentModel       *string
	EquipmentSWRev       *string
	HostSoftware         *string
	HostEndpoint         *string
	EquipmentEndpoint    *string
	EquipmentConnectMode *SocketRole
	DeviceID             *uint64
	HSMSTimers           *HSMSTimers
	SeqStart             uint64
	ClockStepToleranceNs *uint64
	ReplacementSetID     *UUID
	ReplacementSetSize   *uint64
	ReplacementSetIndex  *uint64
	FlushIntervalNs      *uint64
	ScopeGeneration      *uint64
	PublisherEpoch       *uint64
	PatchBase            *UUID
	RedactionPolicy      *RedactionPolicy
	Redaction            []RedactionEntry
	Unknown              []RawEntry
}

var _ encoding.BinaryMarshaler = (*PackMeta)(nil)

// FieldError reports which field a PackMeta, TransportEvent or Annotation rule failed for,
// or which commitment of the pack metadata or the file header a Writer call contradicts.
type FieldError struct {
	// Field is the failing tag's specification name, or a "0x%04X" tag for one PackMeta and Annotation do not name;
	// for a Writer's commitment it may also be "redaction-present", the file header flag.
	Field string
	// Err is the sentinel error describing the failure.
	Err error
}

var _ error = (*FieldError)(nil)

// optionalEntry is one optional tag of a TLV payload, encoded by appendPresent only when present;
// build dereferences the field, so it runs only for a present entry.
type optionalEntry struct {
	present bool
	build   func() tlv.Entry
}

// packMetaSetters decodes one known pack metadata tag into m, keyed by tag.
// A tag absent from this map is unknown or private, and is preserved in PackMeta.Unknown instead.
var packMetaSetters = map[uint16]func(m *PackMeta, e tlv.Entry) error{
	tagToolID:              setField(tlv.Entry.UTF8, func(m *PackMeta, v string) { m.ToolID = v }),
	tagTransport:           setField(u8As[Transport], func(m *PackMeta, v Transport) { m.Transport = v }),
	tagCaptureMethod:       setField(u8As[CaptureMethod], func(m *PackMeta, v CaptureMethod) { m.CaptureMethod = v }),
	tagVantage:             setField(u8As[Vantage], func(m *PackMeta, v Vantage) { m.Vantage = v }),
	tagRecorder:            setField(tlv.Entry.UTF8, func(m *PackMeta, v string) { m.Recorder = v }),
	tagWriter:              setField(tlv.Entry.UTF8, func(m *PackMeta, v string) { m.Writer = v }),
	tagClassifier:          setField(tlv.Entry.UTF8, func(m *PackMeta, v string) { m.Classifier = &v }),
	tagTimeSource:          setField(u8As[TimeSource], func(m *PackMeta, v TimeSource) { m.TimeSource = v }),
	tagCaptureOriginUTCNs:  setField(tlv.Entry.I64, func(m *PackMeta, v int64) { m.CaptureOriginUTCNs = &v }),
	tagCaptureOriginMonoNs: setField(tlv.Entry.I64, func(m *PackMeta, v int64) { m.CaptureOriginMonoNs = &v }),
	tagSourceTZ:            setField(tlv.Entry.UTF8, func(m *PackMeta, v string) { m.SourceTZ = &v }),
	tagSourceDialect:       setField(tlv.Entry.UTF8, func(m *PackMeta, v string) { m.SourceDialect = &v }),
	tagSourceRef: setField(tlv.Entry.UTF8, func(m *PackMeta, v string) {
		m.SourceRefs = append(m.SourceRefs, v)
	}),
	tagMaxFrameLen:       setField(tlv.Entry.U64, func(m *PackMeta, v uint64) { m.MaxFrameLen = &v }),
	tagPeriodStart:       setField(tlv.Entry.I64, func(m *PackMeta, v int64) { m.PeriodStart = v }),
	tagPeriodEnd:         setField(tlv.Entry.I64, func(m *PackMeta, v int64) { m.PeriodEnd = v }),
	tagLifecycleCoverage: setField(u8As[LifecycleCoverage], func(m *PackMeta, v LifecycleCoverage) { m.LifecycleCoverage = v }),
	tagQualityEvaluated:  setField(tlv.Entry.Bool, func(m *PackMeta, v bool) { m.QualityEvaluated = v }),
	tagSupersedes: setField(uuidOf, func(m *PackMeta, v UUID) {
		m.Supersedes = append(m.Supersedes, v)
	}),
	tagCoverage:        setNested(decodeCoverage, func(m *PackMeta, v Coverage) { m.Coverage = append(m.Coverage, v) }),
	tagNotes:           setField(tlv.Entry.UTF8, func(m *PackMeta, v string) { m.Notes = &v }),
	tagPackRole:        setField(u8As[PackRole], func(m *PackMeta, v PackRole) { m.PackRole = v }),
	tagCompactionLevel: setField(tlv.Entry.U8, func(m *PackMeta, v uint8) { m.CompactionLevel = v }),
	tagCompactedFrom: setField(uuidOf, func(m *PackMeta, v UUID) {
		m.CompactedFrom = append(m.CompactedFrom, v)
	}),
	tagRecorderInstanceID:   setField(uuidOf, func(m *PackMeta, v UUID) { m.RecorderInstanceID = v }),
	tagPreviousCaptureID:    setField(uuidOf, func(m *PackMeta, v UUID) { m.PreviousCaptureID = &v }),
	tagExtractFilter:        setField(tlv.Entry.UTF8, func(m *PackMeta, v string) { m.ExtractFilter = &v }),
	tagSiteID:               setField(tlv.Entry.UTF8, func(m *PackMeta, v string) { m.SiteID = &v }),
	tagEquipmentModel:       setField(tlv.Entry.UTF8, func(m *PackMeta, v string) { m.EquipmentModel = &v }),
	tagEquipmentSWRev:       setField(tlv.Entry.UTF8, func(m *PackMeta, v string) { m.EquipmentSWRev = &v }),
	tagHostSoftware:         setField(tlv.Entry.UTF8, func(m *PackMeta, v string) { m.HostSoftware = &v }),
	tagHostEndpoint:         setField(tlv.Entry.UTF8, func(m *PackMeta, v string) { m.HostEndpoint = &v }),
	tagEquipmentEndpoint:    setField(tlv.Entry.UTF8, func(m *PackMeta, v string) { m.EquipmentEndpoint = &v }),
	tagEquipmentConnectMode: setField(u8As[SocketRole], func(m *PackMeta, v SocketRole) { m.EquipmentConnectMode = &v }),
	tagDeviceID:             setField(tlv.Entry.U64, func(m *PackMeta, v uint64) { m.DeviceID = &v }),
	tagHSMSTimers:           setNested(decodeHSMSTimers, func(m *PackMeta, v HSMSTimers) { m.HSMSTimers = &v }),
	tagSeqStart:             setField(tlv.Entry.U64, func(m *PackMeta, v uint64) { m.SeqStart = v }),
	tagClockStepToleranceNs: setField(tlv.Entry.U64, func(m *PackMeta, v uint64) { m.ClockStepToleranceNs = &v }),
	tagReplacementSetID:     setField(uuidOf, func(m *PackMeta, v UUID) { m.ReplacementSetID = &v }),
	tagReplacementSetSize:   setField(tlv.Entry.U64, func(m *PackMeta, v uint64) { m.ReplacementSetSize = &v }),
	tagReplacementSetIndex:  setField(tlv.Entry.U64, func(m *PackMeta, v uint64) { m.ReplacementSetIndex = &v }),
	tagFlushIntervalNs:      setField(tlv.Entry.U64, func(m *PackMeta, v uint64) { m.FlushIntervalNs = &v }),
	tagScopeGeneration:      setField(tlv.Entry.U64, func(m *PackMeta, v uint64) { m.ScopeGeneration = &v }),
	tagPublisherEpoch:       setField(tlv.Entry.U64, func(m *PackMeta, v uint64) { m.PublisherEpoch = &v }),
	tagPatchBase:            setField(uuidOf, func(m *PackMeta, v UUID) { m.PatchBase = &v }),
	tagRedactionPolicy:      setNested(decodeNested(redactionPolicySetters), func(m *PackMeta, v RedactionPolicy) { m.RedactionPolicy = &v }),
	tagRedaction: setNested(decodeNested(redactionEntrySetters), func(m *PackMeta, v RedactionEntry) {
		m.Redaction = append(m.Redaction, v)
	}),
}

// coverageSetters decodes one nested tag of a pack metadata `coverage` entry into c, keyed by tag.
var coverageSetters = map[uint16]func(c *Coverage, e tlv.Entry) error{
	tagCoverageCaptureID: setField(uuidOf, func(c *Coverage, v UUID) { c.CaptureID = &v }),
	tagCoverageSeqFirst:  setField(tlv.Entry.U64, func(c *Coverage, v uint64) { c.SeqFirst = &v }),
	tagCoverageSeqLast:   setField(tlv.Entry.U64, func(c *Coverage, v uint64) { c.SeqLast = &v }),
	tagCoverageTimeStart: setField(tlv.Entry.I64, func(c *Coverage, v int64) { c.TimeStart = &v }),
	tagCoverageTimeEnd:   setField(tlv.Entry.I64, func(c *Coverage, v int64) { c.TimeEnd = &v }),
}

// redactionPolicySetters decodes one nested tag of a pack metadata `redaction_policy` entry into p, keyed by tag.
// Every field is required by the registry, so tlv.Validate has already confirmed each tag is present.
var redactionPolicySetters = map[uint16]func(p *RedactionPolicy, e tlv.Entry) error{
	tagRedactionPolicyPolicyID:      setField(tlv.Entry.UTF8, func(p *RedactionPolicy, v string) { p.PolicyID = v }),
	tagRedactionPolicyPolicyVersion: setField(tlv.Entry.U64, func(p *RedactionPolicy, v uint64) { p.PolicyVersion = v }),
	tagRedactionPolicyKeyID:         setField(tlv.Entry.UTF8, func(p *RedactionPolicy, v string) { p.KeyID = v }),
	tagRedactionPolicyDigestAlgorithm: setField(u8As[DigestAlgorithm], func(p *RedactionPolicy, v DigestAlgorithm) {
		p.DigestAlgorithm = v
	}),
}

// redactionEntrySetters decodes one nested tag of a pack metadata `redaction` entry into r, keyed by tag.
var redactionEntrySetters = map[uint16]func(r *RedactionEntry, e tlv.Entry) error{
	tagRedactionEntrySeq:          setField(tlv.Entry.U64, func(r *RedactionEntry, v uint64) { r.Seq = v }),
	tagRedactionEntryItemPath:     setField(tlv.Entry.UTF8, func(r *RedactionEntry, v string) { r.ItemPath = &v }),
	tagRedactionEntryMaskedRanges: setField(decodeMaskedRanges, func(r *RedactionEntry, v []MaskedRange) { r.MaskedRanges = v }),
	tagRedactionEntryDomain:       setField(tlv.Entry.UTF8, func(r *RedactionEntry, v string) { r.Domain = v }),
	tagRedactionEntryDigest: setField(tlv.Entry.Bytes, func(r *RedactionEntry, v []byte) {
		r.Digest = append([]byte(nil), v...)
	}),
}

// Validate checks m against the conditional "Required when" rules of the pack metadata tag registry
// (the tracepack format specification §5):
// those m's own fields decide, which UnmarshalPackMeta and MarshalBinary also enforce,
// and those that depend on the pack's records or scope, which facts resolve.
// The unconditional "always" rules cannot fail: their fields are not pointers, so they are always encoded.
//
// Returns:
//   - error: nil, or a *FieldError for the first rule violated,
//     in the order the tracepack format specification §5 registry table lists the rows.
func (m *PackMeta) Validate(facts PackFacts) error {
	isPatch := m.PackRole == PackRoleRepair
	isGeneration := m.ScopeGeneration != nil && *m.ScopeGeneration >= 1

	checks := []struct {
		field    string
		required bool
		present  bool
	}{
		{"classifier", facts.AnyClassified || facts.AnyOversized, m.Classifier != nil},
		{"capture_origin_utc_ns", m.TimeSource == TimeSourceCaptureClock, m.CaptureOriginUTCNs != nil},
		{"capture_origin_mono_ns", m.TimeSource == TimeSourceCaptureClock, m.CaptureOriginMonoNs != nil},
		{"source_tz", m.TimeSource == TimeSourceSourceLog, m.SourceTZ != nil},
		{"source_dialect", m.CaptureMethod == CaptureMethodLog, m.SourceDialect != nil},
		{"source_ref", m.CaptureMethod == CaptureMethodLog, len(m.SourceRefs) > 0},
		{"max_frame_len", facts.AnyOversized, m.MaxFrameLen != nil},
		{"supersedes", isPatch || (isGeneration && facts.GenerationHasPredecessor), len(m.Supersedes) > 0},
		{"coverage", isPatch && facts.CoverageLoss, len(m.Coverage) > 0},
		{"compacted_from", m.CompactionLevel >= 1, len(m.CompactedFrom) > 0},
		{"extract_filter", m.PackRole == PackRoleExtract, m.ExtractFilter != nil},
		{"clock_step_tolerance_ns", m.TimeSource == TimeSourceCaptureClock, m.ClockStepToleranceNs != nil},
		{"flush_interval_ns", facts.DurableRecorder, m.FlushIntervalNs != nil},
		{"scope_generation", m.PackRole != PackRoleExtract, m.ScopeGeneration != nil},
		{"publisher_epoch", isGeneration, m.PublisherEpoch != nil},
		{"patch_base", isPatch && facts.ScopeHasGeneration, m.PatchBase != nil},
		{"replacement_set_id", isGeneration, m.ReplacementSetID != nil},
		{"replacement_set_size", m.ReplacementSetID != nil, m.ReplacementSetSize != nil},
		{"replacement_set_index", m.ReplacementSetID != nil, m.ReplacementSetIndex != nil},
		{"redaction_policy", facts.RedactionPolicyApplied, m.RedactionPolicy != nil},
		{"redaction", facts.AnyRedacted, len(m.Redaction) > 0},
	}

	for _, c := range checks {
		if c.required && !c.present {
			return &FieldError{Field: c.field, Err: ErrRequiredTag}
		}
	}

	// A replacement set has exactly one member (the tracepack storage specification §6).
	if m.ReplacementSetSize != nil && *m.ReplacementSetSize != 1 {
		return &FieldError{Field: "replacement_set_size", Err: ErrFieldValue}
	}
	if m.ReplacementSetIndex != nil && *m.ReplacementSetIndex != 0 {
		return &FieldError{Field: "replacement_set_index", Err: ErrFieldValue}
	}

	return nil
}

// MarshalBinary encodes m as the TLV bytes of the tracepack format specification §5.
//
// It first enforces the rules UnmarshalPackMeta enforces, those that do not depend on record or scope facts;
// a caller whose pack has additional facts (a classified record, a redacted record, a generation with a predecessor,
// and so on) must call Validate(facts) itself before calling MarshalBinary.
// Every RawEntry in m.Unknown is re-encoded after the typed fields, except a retired tag number, which is dropped,
// because a writer never writes one (the tracepack format specification §5).
// The Unknown entries of each coverage entry are re-encoded after its known nested tags.
// The retired pack_role 5 is rejected: a writer never writes it (the tracepack format specification §9).
//
// It rejects any value UnmarshalPackMeta would reject:
// a string that is not valid UTF-8, a u64 above 2^63-1, nested values included.
//
// Returns:
//   - []byte: the encoded pack metadata.
//   - error: a *FieldError from Validate, a *FieldError wrapping ErrFieldValue for pack_role 5,
//     a *FieldError wrapping ErrReservedTag for an Unknown entry, of the metadata or of a coverage entry,
//     that names a tag this package already encodes,
//     or an error naming the first value the decoder would reject.
func (m *PackMeta) MarshalBinary() ([]byte, error) {
	if err := m.validateIntrinsic(); err != nil {
		return nil, err
	}

	if m.PackRole == packRoleRetired {
		return nil, &FieldError{Field: "pack_role", Err: ErrFieldValue}
	}

	entries := m.appendAlwaysEntries(nil)
	entries = m.appendOptionalScalarEntries(entries)
	entries, err := m.appendRepeatableEntries(entries)
	if err != nil {
		return nil, err
	}
	entries = m.appendNestedEntries(entries)

	unknown := slices.DeleteFunc(slices.Clone(m.Unknown), func(r RawEntry) bool { return slices.Contains(retiredPackMetaTags[:], r.Tag) })
	entries, err = appendUnknownEntries(entries, unknown, tlv.PackMetadata)
	if err != nil {
		return nil, err
	}

	return encodeEntries(entries, tlv.PackMetadata, "pack metadata")
}

// UnmarshalPackMeta decodes the pack metadata of the tracepack format specification §5 from b.
//
// It checks b against the pack metadata tag registry:
// every unconditionally required tag, every value type and every nested entry.
// It then evaluates the conditional "Required when" rules the metadata answers by itself,
// the rules Validate enforces with the zero PackFacts,
// such as the capture origins a capture-clock pack requires, or the scope_generation every pack but an extract requires.
// The rules that depend on facts about the pack's records or scope are left to the reader,
// which calls Validate itself when it has PackFacts to check against.
// An entry whose tag is not in the registry, a retired tag included, is preserved in the returned PackMeta's Unknown field.
//
// Returns:
//   - *PackMeta: the decoded pack metadata; nil on error.
//   - error: non-nil if b is not valid TLV or fails the registry check;
//     a *FieldError wrapping ErrRequiredTag for the first "Required when" rule the metadata itself violates,
//     or ErrFieldValue for a replacement-set value other than size 1 and index 0.
func UnmarshalPackMeta(b []byte) (*PackMeta, error) {
	entries, err := tlv.Decode(b)
	if err != nil {
		return nil, fmt.Errorf("tracepack: decode pack metadata: %w", err)
	}
	if err := tlv.Validate(entries, tlv.PackMetadata); err != nil {
		return nil, fmt.Errorf("tracepack: validate pack metadata: %w", err)
	}

	m := &PackMeta{}
	for _, e := range entries {
		if set, known := packMetaSetters[e.Tag]; known {
			if err := set(m, e); err != nil {
				return nil, err
			}

			continue
		}

		m.Unknown = append(m.Unknown, newRawEntry(e))
	}

	if err := m.validateIntrinsic(); err != nil {
		return nil, err
	}

	return m, nil
}

// Error implements the error interface.
func (e *FieldError) Error() string {
	return fmt.Sprintf("tracepack: %s: %v", e.Field, e.Err)
}

// Unwrap returns the sentinel error.
func (e *FieldError) Unwrap() error {
	return e.Err
}

// ParseUUID parses the canonical 8-4-4-4-12 string form of a UUID, such as "f47ac10b-58cc-4372-a567-0e02b2c3d479".
//
// Returns:
//   - UUID: the parsed value; the zero UUID on error.
//   - error: non-nil if s is not a canonical UUID string.
func ParseUUID(s string) (UUID, error) {
	u, err := format.ParseUUID(s)

	return UUID(u), err
}

// String returns the canonical 8-4-4-4-12 lowercase hex form of u.
func (u UUID) String() string {
	return format.UUID(u).String()
}

// IsZero reports whether u is the nil UUID (every byte zero).
func (u UUID) IsZero() bool {
	return u == UUID{}
}

// entry returns c as the nested entry list of a pack metadata `coverage` entry:
// its known nested tags in tag order, then its Unknown entries in order.
//
// Returns:
//   - tlv.Entry: the coverage entry.
//   - error: a *FieldError wrapping ErrReservedTag for an Unknown entry that names a known nested tag,
//     or the error of an Unknown entry the decoder would reject.
func (c Coverage) entry() (tlv.Entry, error) {
	var sub []tlv.Entry
	if c.CaptureID != nil {
		sub = append(sub, tlv.UUIDEntry(tagCoverageCaptureID, [16]byte(*c.CaptureID)))
	}
	if c.SeqFirst != nil {
		sub = append(sub, tlv.U64Entry(tagCoverageSeqFirst, *c.SeqFirst))
	}
	if c.SeqLast != nil {
		sub = append(sub, tlv.U64Entry(tagCoverageSeqLast, *c.SeqLast))
	}
	if c.TimeStart != nil {
		sub = append(sub, tlv.I64Entry(tagCoverageTimeStart, *c.TimeStart))
	}
	if c.TimeEnd != nil {
		sub = append(sub, tlv.I64Entry(tagCoverageTimeEnd, *c.TimeEnd))
	}

	sub, err := appendUnknownEntries(sub, c.Unknown, tlv.Coverage)
	if err != nil {
		return tlv.Entry{}, err
	}

	return tlv.NestedEntry(tagCoverage, sub), nil
}

// entry returns p as the nested entry list of a pack metadata `redaction_policy` entry.
func (p RedactionPolicy) entry() tlv.Entry {
	sub := []tlv.Entry{
		tlv.UTF8Entry(tagRedactionPolicyPolicyID, p.PolicyID),
		tlv.U64Entry(tagRedactionPolicyPolicyVersion, p.PolicyVersion),
		tlv.UTF8Entry(tagRedactionPolicyKeyID, p.KeyID),
		tlv.U8Entry(tagRedactionPolicyDigestAlgorithm, uint8(p.DigestAlgorithm)),
	}

	return tlv.NestedEntry(tagRedactionPolicy, sub)
}

// entry returns r as the nested entry list of a pack metadata `redaction` entry.
func (r RedactionEntry) entry() tlv.Entry {
	sub := []tlv.Entry{tlv.U64Entry(tagRedactionEntrySeq, r.Seq)}
	if r.ItemPath != nil {
		sub = append(sub, tlv.UTF8Entry(tagRedactionEntryItemPath, *r.ItemPath))
	}

	flat := make([]uint64, 0, 2*len(r.MaskedRanges))
	for _, mr := range r.MaskedRanges {
		flat = append(flat, mr.Offset, mr.Length)
	}
	sub = append(sub,
		tlv.BytesEntry(tagRedactionEntryMaskedRanges, tlv.AppendU64Array(nil, flat)),
		tlv.UTF8Entry(tagRedactionEntryDomain, r.Domain),
		tlv.BytesEntry(tagRedactionEntryDigest, r.Digest),
	)

	return tlv.NestedEntry(tagRedaction, sub)
}

// entry returns t as the nested entry list of a pack metadata `hsms_timers` entry.
func (t HSMSTimers) entry() tlv.Entry {
	fields := []struct {
		tag uint16
		v   *uint64
	}{
		{1, t.T1}, {2, t.T2}, {3, t.T3}, {4, t.T4},
		{5, t.T5}, {6, t.T6}, {7, t.T7}, {8, t.T8},
	}

	var sub []tlv.Entry
	for _, f := range fields {
		if f.v != nil {
			sub = append(sub, tlv.U64Entry(f.tag, *f.v))
		}
	}

	return tlv.NestedEntry(tagHSMSTimers, sub)
}

// validateIntrinsic checks the "Required when" rules that m's own fields decide,
// which UnmarshalPackMeta and MarshalBinary both enforce.
// Every PackFacts field only adds requirements, so under the zero PackFacts no fact-dependent rule applies,
// and Validate(PackFacts{}) checks exactly the intrinsic rules, in the registry table's order.
func (m *PackMeta) validateIntrinsic() error {
	return m.Validate(PackFacts{})
}

// appendAlwaysEntries appends the entries of m's unconditionally required tags, which are never absent
// because their fields are not pointers.
func (m *PackMeta) appendAlwaysEntries(dst []tlv.Entry) []tlv.Entry {
	return append(dst,
		tlv.UTF8Entry(tagToolID, m.ToolID),
		tlv.U8Entry(tagTransport, uint8(m.Transport)),
		tlv.U8Entry(tagCaptureMethod, uint8(m.CaptureMethod)),
		tlv.U8Entry(tagVantage, uint8(m.Vantage)),
		tlv.UTF8Entry(tagRecorder, m.Recorder),
		tlv.UTF8Entry(tagWriter, m.Writer),
		tlv.U8Entry(tagTimeSource, uint8(m.TimeSource)),
		tlv.I64Entry(tagPeriodStart, m.PeriodStart),
		tlv.I64Entry(tagPeriodEnd, m.PeriodEnd),
		tlv.U8Entry(tagLifecycleCoverage, uint8(m.LifecycleCoverage)),
		tlv.BoolEntry(tagQualityEvaluated, m.QualityEvaluated),
		tlv.U8Entry(tagPackRole, uint8(m.PackRole)),
		tlv.U8Entry(tagCompactionLevel, m.CompactionLevel),
		tlv.UUIDEntry(tagRecorderInstanceID, [16]byte(m.RecorderInstanceID)),
		tlv.U64Entry(tagSeqStart, m.SeqStart),
	)
}

// appendOptionalScalarEntries appends the entries of m's optional and conditionally required scalar tags
// that are present.
func (m *PackMeta) appendOptionalScalarEntries(dst []tlv.Entry) []tlv.Entry {
	return appendPresent(dst, []optionalEntry{
		{m.Classifier != nil, func() tlv.Entry { return tlv.UTF8Entry(tagClassifier, *m.Classifier) }},
		{m.CaptureOriginUTCNs != nil, func() tlv.Entry { return tlv.I64Entry(tagCaptureOriginUTCNs, *m.CaptureOriginUTCNs) }},
		{m.CaptureOriginMonoNs != nil, func() tlv.Entry { return tlv.I64Entry(tagCaptureOriginMonoNs, *m.CaptureOriginMonoNs) }},
		{m.SourceTZ != nil, func() tlv.Entry { return tlv.UTF8Entry(tagSourceTZ, *m.SourceTZ) }},
		{m.SourceDialect != nil, func() tlv.Entry { return tlv.UTF8Entry(tagSourceDialect, *m.SourceDialect) }},
		{m.MaxFrameLen != nil, func() tlv.Entry { return tlv.U64Entry(tagMaxFrameLen, *m.MaxFrameLen) }},
		{m.Notes != nil, func() tlv.Entry { return tlv.UTF8Entry(tagNotes, *m.Notes) }},
		{m.PreviousCaptureID != nil, func() tlv.Entry { return tlv.UUIDEntry(tagPreviousCaptureID, [16]byte(*m.PreviousCaptureID)) }},
		{m.ExtractFilter != nil, func() tlv.Entry { return tlv.UTF8Entry(tagExtractFilter, *m.ExtractFilter) }},
		{m.SiteID != nil, func() tlv.Entry { return tlv.UTF8Entry(tagSiteID, *m.SiteID) }},
		{m.EquipmentModel != nil, func() tlv.Entry { return tlv.UTF8Entry(tagEquipmentModel, *m.EquipmentModel) }},
		{m.EquipmentSWRev != nil, func() tlv.Entry { return tlv.UTF8Entry(tagEquipmentSWRev, *m.EquipmentSWRev) }},
		{m.HostSoftware != nil, func() tlv.Entry { return tlv.UTF8Entry(tagHostSoftware, *m.HostSoftware) }},
		{m.HostEndpoint != nil, func() tlv.Entry { return tlv.UTF8Entry(tagHostEndpoint, *m.HostEndpoint) }},
		{m.EquipmentEndpoint != nil, func() tlv.Entry { return tlv.UTF8Entry(tagEquipmentEndpoint, *m.EquipmentEndpoint) }},
		{m.EquipmentConnectMode != nil, func() tlv.Entry { return tlv.U8Entry(tagEquipmentConnectMode, uint8(*m.EquipmentConnectMode)) }},
		{m.DeviceID != nil, func() tlv.Entry { return tlv.U64Entry(tagDeviceID, *m.DeviceID) }},
		{m.ClockStepToleranceNs != nil, func() tlv.Entry { return tlv.U64Entry(tagClockStepToleranceNs, *m.ClockStepToleranceNs) }},
		{m.ReplacementSetID != nil, func() tlv.Entry { return tlv.UUIDEntry(tagReplacementSetID, [16]byte(*m.ReplacementSetID)) }},
		{m.ReplacementSetSize != nil, func() tlv.Entry { return tlv.U64Entry(tagReplacementSetSize, *m.ReplacementSetSize) }},
		{m.ReplacementSetIndex != nil, func() tlv.Entry { return tlv.U64Entry(tagReplacementSetIndex, *m.ReplacementSetIndex) }},
		{m.FlushIntervalNs != nil, func() tlv.Entry { return tlv.U64Entry(tagFlushIntervalNs, *m.FlushIntervalNs) }},
		{m.ScopeGeneration != nil, func() tlv.Entry { return tlv.U64Entry(tagScopeGeneration, *m.ScopeGeneration) }},
		{m.PublisherEpoch != nil, func() tlv.Entry { return tlv.U64Entry(tagPublisherEpoch, *m.PublisherEpoch) }},
		{m.PatchBase != nil, func() tlv.Entry { return tlv.UUIDEntry(tagPatchBase, [16]byte(*m.PatchBase)) }},
	})
}

// appendRepeatableEntries appends the entries of m's repeatable tags.
// It returns the error of the first coverage entry that cannot be encoded, naming its index.
func (m *PackMeta) appendRepeatableEntries(dst []tlv.Entry) ([]tlv.Entry, error) {
	for _, s := range m.SourceRefs {
		dst = append(dst, tlv.UTF8Entry(tagSourceRef, s))
	}
	for _, id := range m.Supersedes {
		dst = append(dst, tlv.UUIDEntry(tagSupersedes, [16]byte(id)))
	}
	for _, id := range m.CompactedFrom {
		dst = append(dst, tlv.UUIDEntry(tagCompactedFrom, [16]byte(id)))
	}
	for i, c := range m.Coverage {
		e, err := c.entry()
		if err != nil {
			return nil, fmt.Errorf("tracepack: coverage entry %d: %w", i, err)
		}
		dst = append(dst, e)
	}
	for _, r := range m.Redaction {
		dst = append(dst, r.entry())
	}

	return dst, nil
}

// appendNestedEntries appends the entries of m's single nested tags that are present.
func (m *PackMeta) appendNestedEntries(dst []tlv.Entry) []tlv.Entry {
	if m.HSMSTimers != nil {
		dst = append(dst, m.HSMSTimers.entry())
	}
	if m.RedactionPolicy != nil {
		dst = append(dst, m.RedactionPolicy.entry())
	}

	return dst
}

// newRawEntry copies e into a RawEntry, so the result outlives the buffer e.Value aliases.
func newRawEntry(e tlv.Entry) RawEntry {
	return RawEntry{Tag: e.Tag, Type: uint8(e.Type), Value: append([]byte(nil), e.Value...)}
}

// appendPresent appends the entry of every present optional tag in opts, in order.
func appendPresent(dst []tlv.Entry, opts []optionalEntry) []tlv.Entry {
	for _, o := range opts {
		if o.present {
			dst = append(dst, o.build())
		}
	}

	return dst
}

// appendUnknownEntries appends the entries of unknown, rejecting one whose tag reg already assigns a field to.
func appendUnknownEntries(dst []tlv.Entry, unknown []RawEntry, reg tlv.Registry) ([]tlv.Entry, error) {
	for _, r := range unknown {
		field := fmt.Sprintf("0x%04X", r.Tag)
		if _, known := reg[r.Tag]; known {
			return nil, &FieldError{Field: field, Err: ErrReservedTag}
		}
		// Registry validation skips unknown tags, so the two rules the decoder applies to every entry
		// (the tracepack format specification §5) are checked here before the entry is encoded.
		if r.Tag == 0 {
			return nil, &FieldError{Field: field, Err: tlv.ErrZeroTag}
		}
		if n, fixed := tlv.ValueType(r.Type).FixedLen(); fixed && len(r.Value) != n {
			return nil, &FieldError{Field: field, Err: tlv.ErrLength}
		}
		dst = append(dst, tlv.Entry{Tag: r.Tag, Type: tlv.ValueType(r.Type), Value: r.Value})
	}

	return dst, nil
}

// encodeEntries checks entries against reg with the same registry validation the decoder of what applies,
// then encodes them, so a marshaler never returns bytes its own decoder rejects.
func encodeEntries(entries []tlv.Entry, reg tlv.Registry, what string) ([]byte, error) {
	if err := tlv.Validate(entries, reg); err != nil {
		return nil, fmt.Errorf("tracepack: encode %s: %w", what, err)
	}

	var buf []byte
	for _, e := range entries {
		buf = tlv.AppendEntry(buf, e)
	}

	return buf, nil
}

// setField returns a setter-map entry for R that decodes e with decode and passes the result to assign.
func setField[R, T any](decode func(tlv.Entry) (T, error), assign func(*R, T)) func(*R, tlv.Entry) error {
	return func(r *R, e tlv.Entry) error {
		v, err := decode(e)
		if err != nil {
			return err
		}
		assign(r, v)

		return nil
	}
}

// setNested returns a packMetaSetters entry that decodes e's nested entries with decode and passes the result to assign.
func setNested[T any](decode func([]tlv.Entry) (T, error), assign func(*PackMeta, T)) func(*PackMeta, tlv.Entry) error {
	return func(m *PackMeta, e tlv.Entry) error {
		nested, err := e.Nested()
		if err != nil {
			return err
		}
		v, err := decode(nested)
		if err != nil {
			return err
		}
		assign(m, v)

		return nil
	}
}

// decodeNested returns a setNested decoder that fills an R from its nested entries with setters.
// A nested tag without a setter is skipped; decodeCoverage keeps them for a coverage entry instead.
func decodeNested[R any](setters map[uint16]func(*R, tlv.Entry) error) func([]tlv.Entry) (R, error) {
	return func(nested []tlv.Entry) (R, error) {
		var r R
		for _, e := range nested {
			set, known := setters[e.Tag]
			if !known {
				continue
			}
			if err := set(&r, e); err != nil {
				var zero R
				return zero, err
			}
		}

		return r, nil
	}
}

// decodeCoverage decodes the nested entries of a pack metadata `coverage` entry,
// keeping every nested entry without a setter in Coverage.Unknown, in order.
func decodeCoverage(nested []tlv.Entry) (Coverage, error) {
	var c Coverage
	for _, e := range nested {
		set, known := coverageSetters[e.Tag]
		if !known {
			c.Unknown = append(c.Unknown, newRawEntry(e))

			continue
		}
		if err := set(&c, e); err != nil {
			return Coverage{}, err
		}
	}

	return c, nil
}

// u8As decodes e's u8 value as the enum type T.
func u8As[T ~uint8](e tlv.Entry) (T, error) {
	v, err := e.U8()

	return T(v), err
}

// uuidOf decodes e's uuid value as UUID.
func uuidOf(e tlv.Entry) (UUID, error) {
	v, err := e.UUID()

	return UUID(v), err
}

// decodeMaskedRanges decodes a redaction entry's masked_ranges value: a u64 array of (offset, length) pairs.
func decodeMaskedRanges(e tlv.Entry) ([]MaskedRange, error) {
	b, err := e.Bytes()
	if err != nil {
		return nil, err
	}
	arr, err := tlv.DecodeU64Array(b)
	if err != nil {
		return nil, err
	}
	if len(arr)%2 != 0 {
		return nil, fmt.Errorf("tracepack: masked_ranges holds %d elements: %w", len(arr), ErrFieldLength)
	}

	ranges := make([]MaskedRange, 0, len(arr)/2)
	for i := 0; i < len(arr); i += 2 {
		ranges = append(ranges, MaskedRange{Offset: arr[i], Length: arr[i+1]})
	}

	return ranges, nil
}

// decodeHSMSTimers decodes the nested entries of a pack metadata `hsms_timers` entry.
func decodeHSMSTimers(nested []tlv.Entry) (HSMSTimers, error) {
	var t HSMSTimers
	fields := [8]**uint64{&t.T1, &t.T2, &t.T3, &t.T4, &t.T5, &t.T6, &t.T7, &t.T8}
	for _, e := range nested {
		if e.Tag < 1 || e.Tag > 8 {
			continue
		}
		v, err := e.U64()
		if err != nil {
			return HSMSTimers{}, err
		}
		*fields[e.Tag-1] = &v
	}

	return t, nil
}
