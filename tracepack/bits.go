package tracepack

import "strings"

// Quality is the record-header quality bit set (bit 0 = least significant).
//
// Zero means none of the quality conditions apply.
// See the tracepack format specification §9 for the full registry;
// bits 3 and 6 are retired and bits 7-15 reserved: they never appear in [Quality.Names] or [Quality.String].
type Quality uint16

const (
	// QualityCaptureBoundary marks a capture-boundary transport event itself, the record that delimits a capture or a gap;
	// the Writer sets it on such a record and clears it on every other.
	QualityCaptureBoundary Quality = 1 << 0
	// QualityOrderingUncertain marks a record whose position relative to others is not certain.
	QualityOrderingUncertain Quality = 1 << 1
	// QualityCorrelationIncomplete marks a record whose transport connection could not be fully correlated.
	QualityCorrelationIncomplete Quality = 1 << 2
	// QualityDirectionInferred marks a record whose [Dir] was inferred rather than observed.
	QualityDirectionInferred Quality = 1 << 4
	// QualityRedacted marks a record whose payload was masked by a redaction policy.
	QualityRedacted Quality = 1 << 5
)

// qualityMask holds the defined quality bits; bits 3 and 6 are retired and bits 7-15 reserved, all zero on write (I-9).
const qualityMask = QualityCaptureBoundary | QualityOrderingUncertain | QualityCorrelationIncomplete |
	QualityDirectionInferred | QualityRedacted

// FieldValidity is the record-header bit set marking which HSMS header fields of a record's payload are real
// (the tracepack format specification §7.2).
//
// Zero means every field is unavailable, as for a very short capture or a record without an HSMS frame.
// See the tracepack format specification §9 for the full registry;
// bits 6-7 are reserved and never appear in [FieldValidity.Names] or [FieldValidity.String].
type FieldValidity uint8

const (
	// FieldValiditySessionID marks SessionID, payload bytes 4-5, as real.
	FieldValiditySessionID FieldValidity = 1 << 0
	// FieldValidityStreamAndW marks stream and the W bit as real;
	// the two share one bit because both live in payload byte 6.
	FieldValidityStreamAndW FieldValidity = 1 << 1
	// FieldValidityFunction marks function, payload byte 7, as real.
	FieldValidityFunction FieldValidity = 1 << 2
	// FieldValidityPType marks PType, payload byte 8, as real.
	FieldValidityPType FieldValidity = 1 << 3
	// FieldValiditySType marks SType, payload byte 9, as real.
	FieldValiditySType FieldValidity = 1 << 4
	// FieldValiditySystemBytes marks System Bytes, payload bytes 10-13, as real.
	FieldValiditySystemBytes FieldValidity = 1 << 5
)

// fieldValidityMask holds the defined field_validity bits; bits 6-7 are reserved and ignored (I-9).
const fieldValidityMask = FieldValiditySessionID | FieldValidityStreamAndW | FieldValidityFunction |
	FieldValidityPType | FieldValiditySType | FieldValiditySystemBytes

// RecordFlags is the record-header bit set carrying mono_ns availability.
//
// Zero means mono_ns is not present.
// See the tracepack format specification §9 for the full registry;
// bit 0 is retired and bits 2-7 reserved: they never appear in [RecordFlags.Names] or [RecordFlags.String].
type RecordFlags uint8

const (
	// RecordFlagsMonoPresent marks that mono_ns is valid for this record.
	RecordFlagsMonoPresent RecordFlags = 1 << 1
)

// Has reports whether any bit set in bit is also set in q.
func (q Quality) Has(bit Quality) bool {
	return q&bit != 0
}

// Names returns the tracepack format specification's names for q's set bits, in ascending bit order.
//
// Retired and reserved bits, if set, are not represented: they carry no name to return.
func (q Quality) Names() []string {
	var names []string

	for _, b := range []struct {
		bit  Quality
		name string
	}{
		{QualityCaptureBoundary, "capture-boundary"},
		{QualityOrderingUncertain, "ordering-uncertain"},
		{QualityCorrelationIncomplete, "correlation-incomplete"},
		{QualityDirectionInferred, "direction-inferred"},
		{QualityRedacted, "redacted"},
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
// Retired and reserved bits, if set, are not represented: they carry no name to return.
func (f RecordFlags) Names() []string {
	var names []string

	for _, b := range []struct {
		bit  RecordFlags
		name string
	}{
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
