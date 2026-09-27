package tracepack

import "strings"

// Quality is the record-header quality bit set (bit 0 = least significant).
//
// Zero means none of the quality conditions apply.
// See the tracepack format specification §9 for the full registry;
// bits 7-15 are reserved and never appear in [Quality.Names] or [Quality.String].
type Quality uint16

const (
	// QualityCaptureBoundary marks a record adjacent to a capture-boundary event.
	QualityCaptureBoundary Quality = 1 << 0
	// QualityOrderingUncertain marks a record whose position relative to others is not certain.
	QualityOrderingUncertain Quality = 1 << 1
	// QualityCorrelationIncomplete marks a record whose transport connection could not be fully correlated.
	QualityCorrelationIncomplete Quality = 1 << 2
	// QualityDecodeFailed mirrors a malformed [DecodeStatus].
	QualityDecodeFailed Quality = 1 << 3
	// QualityDirectionInferred marks a record whose [Dir] was inferred rather than observed.
	QualityDirectionInferred Quality = 1 << 4
	// QualityRedacted marks a record whose payload was masked by a redaction policy.
	QualityRedacted Quality = 1 << 5
	// QualityNoMono marks a record with no valid mono_ns; it is set iff record_flags.mono_present is clear.
	QualityNoMono Quality = 1 << 6
)

// FieldValidity is the record-header bit set marking which copy fields were present in the captured bytes.
//
// Zero means every copy field is unavailable, as for a very short capture.
// See the tracepack format specification §9 for the full registry;
// bits 6-7 are reserved and never appear in [FieldValidity.Names] or [FieldValidity.String].
type FieldValidity uint8

const (
	// FieldValiditySessionID marks that session_id was present in the captured bytes.
	FieldValiditySessionID FieldValidity = 1 << 0
	// FieldValidityStreamAndW marks that stream and the W bit were present in the captured bytes;
	// the two share one bit because both live in HSMS header byte 2.
	FieldValidityStreamAndW FieldValidity = 1 << 1
	// FieldValidityFunction marks that function was present in the captured bytes.
	FieldValidityFunction FieldValidity = 1 << 2
	// FieldValidityPType marks that PType was present in the captured bytes.
	FieldValidityPType FieldValidity = 1 << 3
	// FieldValiditySType marks that SType was present in the captured bytes.
	FieldValiditySType FieldValidity = 1 << 4
	// FieldValiditySystemBytes marks that system_bytes was present in the captured bytes.
	FieldValiditySystemBytes FieldValidity = 1 << 5
)

// RecordFlags is the record-header bit set carrying the raw W bit and mono_ns availability.
//
// Zero means the W bit is clear and mono_ns is not present.
// See the tracepack format specification §9 for the full registry;
// bits 2-7 are reserved and never appear in [RecordFlags.Names] or [RecordFlags.String].
type RecordFlags uint8

const (
	// RecordFlagsW is the raw W (wait) bit copied from HSMS header byte 2, bit 7.
	RecordFlagsW RecordFlags = 1 << 0
	// RecordFlagsMonoPresent marks that mono_ns is valid for this record.
	RecordFlagsMonoPresent RecordFlags = 1 << 1
)

// Has reports whether any bit set in bit is also set in q.
func (q Quality) Has(bit Quality) bool {
	return q&bit != 0
}

// Names returns the tracepack format specification's names for q's set bits, in ascending bit order.
//
// Reserved bits, if set, are not represented: they carry no name to return.
func (q Quality) Names() []string {
	var names []string

	for _, b := range []struct {
		bit  Quality
		name string
	}{
		{QualityCaptureBoundary, "capture-boundary"},
		{QualityOrderingUncertain, "ordering-uncertain"},
		{QualityCorrelationIncomplete, "correlation-incomplete"},
		{QualityDecodeFailed, "decode-failed"},
		{QualityDirectionInferred, "direction-inferred"},
		{QualityRedacted, "redacted"},
		{QualityNoMono, "no-mono"},
	} {
		if q.Has(b.bit) {
			names = append(names, b.name)
		}
	}

	return names
}

// String returns q's set bit names joined by "|", in ascending bit order, or "" for zero.
func (q Quality) String() string {
	return strings.Join(q.Names(), "|")
}

// Has reports whether any bit set in bit is also set in v.
func (v FieldValidity) Has(bit FieldValidity) bool {
	return v&bit != 0
}

// Names returns the tracepack format specification's names for v's set bits, in ascending bit order.
//
// Reserved bits, if set, are not represented: they carry no name to return.
func (v FieldValidity) Names() []string {
	var names []string

	for _, b := range []struct {
		bit  FieldValidity
		name string
	}{
		{FieldValiditySessionID, "session_id"},
		{FieldValidityStreamAndW, "stream_and_w"},
		{FieldValidityFunction, "function"},
		{FieldValidityPType, "ptype"},
		{FieldValiditySType, "stype"},
		{FieldValiditySystemBytes, "system_bytes"},
	} {
		if v.Has(b.bit) {
			names = append(names, b.name)
		}
	}

	return names
}

// String returns v's set bit names joined by "|", in ascending bit order, or "" for zero.
func (v FieldValidity) String() string {
	return strings.Join(v.Names(), "|")
}

// Has reports whether any bit set in bit is also set in f.
func (f RecordFlags) Has(bit RecordFlags) bool {
	return f&bit != 0
}

// Names returns the tracepack format specification's names for f's set bits, in ascending bit order.
//
// Reserved bits, if set, are not represented: they carry no name to return.
func (f RecordFlags) Names() []string {
	var names []string

	for _, b := range []struct {
		bit  RecordFlags
		name string
	}{
		{RecordFlagsW, "W"},
		{RecordFlagsMonoPresent, "mono_present"},
	} {
		if f.Has(b.bit) {
			names = append(names, b.name)
		}
	}

	return names
}

// String returns f's set bit names joined by "|", in ascending bit order, or "" for zero.
func (f RecordFlags) String() string {
	return strings.Join(f.Names(), "|")
}
