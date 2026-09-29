package tlv

import (
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// specRow is one row of a registry table, transcribed from the tracepack format specification.
type specRow struct {
	tag        uint16
	name       string
	vt         ValueType
	repeatable bool
	required   bool
}

// assertRegistry checks that reg holds exactly the rows, no more and no fewer.
func assertRegistry(t *testing.T, reg Registry, rows []specRow) {
	t.Helper()

	require.Len(t, reg, len(rows))
	for _, r := range rows {
		f, ok := reg[r.tag]
		if !assert.True(t, ok, "tag 0x%04X (%s) missing", r.tag, r.name) {
			continue
		}
		f.Nested = nil // checked by TestNestedRegistries
		assert.Equal(t, Field{Name: r.name, Type: r.vt, Repeatable: r.repeatable, Required: r.required}, f,
			"tag 0x%04X", r.tag)
	}
}

// Transcribed from the pack metadata tag registry of §5.
// Required is set only where the "Required when" column says "always";
// every conditional or optional row is false.
func TestPackMetadataRegistry(t *testing.T) {
	t.Parallel()

	rows := []specRow{
		{0x0002, "tool_id", TypeUTF8, false, true},
		{0x0003, "transport", TypeU8, false, true},
		{0x0004, "capture_method", TypeU8, false, true},
		{0x0005, "vantage", TypeU8, false, true},
		{0x0006, "recorder", TypeUTF8, false, true},
		{0x0007, "writer", TypeUTF8, false, true},
		{0x0008, "classifier", TypeUTF8, false, false},
		{0x0009, "time_source", TypeU8, false, true},
		{0x000A, "capture_origin_utc_ns", TypeI64, false, false},
		{0x000B, "capture_origin_mono_ns", TypeI64, false, false},
		{0x000C, "source_tz", TypeUTF8, false, false},
		{0x000D, "source_dialect", TypeUTF8, false, false},
		{0x000E, "source_ref", TypeUTF8, true, false},
		{0x000F, "max_frame_len", TypeU64, false, false},
		{0x0010, "period_start", TypeI64, false, true},
		{0x0011, "period_end", TypeI64, false, true},
		{0x0012, "lifecycle_coverage", TypeU8, false, true},
		{0x0013, "quality_evaluated", TypeBool, false, true},
		{0x0015, "supersedes", TypeUUID, true, false},
		{0x0016, "coverage", TypeTLV, true, false},
		{0x0017, "notes", TypeUTF8, false, false},
		{0x0018, "pack_role", TypeU8, false, true},
		{0x0019, "compaction_level", TypeU8, false, true},
		{0x001A, "compacted_from", TypeUUID, true, false},
		{0x001B, "recorder_instance_id", TypeUUID, false, true},
		{0x001C, "previous_capture_id", TypeUUID, false, false},
		{0x001D, "extract_filter", TypeUTF8, false, false},
		{0x001E, "site_id", TypeUTF8, false, false},
		{0x001F, "equipment_model", TypeUTF8, false, false},
		{0x0020, "equipment_sw_rev", TypeUTF8, false, false},
		{0x0021, "host_software", TypeUTF8, false, false},
		{0x0022, "host_endpoint", TypeUTF8, false, false},
		{0x0023, "equipment_endpoint", TypeUTF8, false, false},
		{0x0024, "equipment_connect_mode", TypeU8, false, false},
		{0x0025, "device_id", TypeU64, false, false},
		{0x0026, "hsms_timers", TypeTLV, false, false},
		{0x0027, "seq_start", TypeU64, false, true},
		{0x0028, "clock_step_tolerance_ns", TypeU64, false, false},
		{0x002C, "flush_interval_ns", TypeU64, false, false},
		{0x002D, "scope_generation", TypeU64, false, false},
		{0x002E, "publisher_epoch", TypeU64, false, false},
		{0x002F, "patch_base", TypeUUID, false, false},
		{0x0029, "replacement_set_id", TypeUUID, false, false},
		{0x002A, "replacement_set_size", TypeU64, false, false},
		{0x002B, "replacement_set_index", TypeU64, false, false},
		{0x0031, "redaction_policy", TypeTLV, false, false},
		{0x0032, "redaction", TypeTLV, true, false},
	}
	assertRegistry(t, PackMetadata, rows)

	for _, tag := range []uint16{0x0001, 0x0014, 0x0030} {
		_, retired := PackMetadata[tag]
		assert.False(t, retired, "the retired tag 0x%04X must stay unknown", tag)
	}
}

// Transcribed from "Nested tags of coverage" in §5; the specification marks none of them required.
func TestCoverageRegistry(t *testing.T) {
	t.Parallel()

	assertRegistry(t, Coverage, []specRow{
		{0x0001, "capture_id", TypeUUID, false, false},
		{0x0002, "seq_first", TypeU64, false, false},
		{0x0003, "seq_last", TypeU64, false, false},
		{0x0004, "time_start", TypeI64, false, false},
		{0x0005, "time_end", TypeI64, false, false},
	})
}

// Transcribed from "Nested tags of redaction_policy" in §5: "all required".
func TestRedactionPolicyRegistry(t *testing.T) {
	t.Parallel()

	assertRegistry(t, RedactionPolicy, []specRow{
		{0x0001, "policy_id", TypeUTF8, false, true},
		{0x0002, "policy_version", TypeU64, false, true},
		{0x0003, "key_id", TypeUTF8, false, true},
		{0x0004, "digest_algorithm", TypeU8, false, true},
	})
}

// Transcribed from "Nested tags of redaction" in §5: item_path is the only optional tag.
func TestRedactionRegistry(t *testing.T) {
	t.Parallel()

	assertRegistry(t, Redaction, []specRow{
		{0x0001, "seq", TypeU64, false, true},
		{0x0002, "item_path", TypeUTF8, false, false},
		{0x0003, "masked_ranges", TypeBytes, false, true},
		{0x0004, "domain", TypeUTF8, false, true},
		{0x0005, "digest", TypeBytes, false, true},
	})
}

// Transcribed from the hsms_timers row of §5: nested tag n (1–8) is Tn in milliseconds, u64.
func TestHSMSTimersRegistry(t *testing.T) {
	t.Parallel()

	assertRegistry(t, HSMSTimers, []specRow{
		{1, "T1", TypeU64, false, false},
		{2, "T2", TypeU64, false, false},
		{3, "T3", TypeU64, false, false},
		{4, "T4", TypeU64, false, false},
		{5, "T5", TypeU64, false, false},
		{6, "T6", TypeU64, false, false},
		{7, "T7", TypeU64, false, false},
		{8, "T8", TypeU64, false, false},
	})
}

// Transcribed from the transport-event table of §8; only event is marked required.
func TestTransportEventRegistry(t *testing.T) {
	t.Parallel()

	assertRegistry(t, TransportEvent, []specRow{
		{0x0001, "event", TypeU8, false, true},
		{0x0002, "prev_state", TypeU8, false, false},
		{0x0003, "cur_state", TypeU8, false, false},
		{0x0004, "cause", TypeU8, false, false},
		{0x0005, "cause_raw", TypeUTF8, false, false},
		{0x0006, "timer", TypeU8, false, false},
		{0x0007, "socket_role", TypeU8, false, false},
		{0x0008, "inferred", TypeBool, false, false},
		{0x0009, "primary_session_id", TypeU64, false, false},
		{0x000A, "primary_stream", TypeU64, false, false},
		{0x000B, "primary_function", TypeU64, false, false},
		{0x000C, "primary_system_bytes", TypeBytes, false, false},
		{0x000D, "boundary_kind", TypeU8, false, false},
		{0x000E, "boundary_seq_first", TypeU64, false, false},
		{0x000F, "boundary_seq_last", TypeU64, false, false},
		{0x0010, "gap_start", TypeI64, false, false},
		{0x0011, "gap_end", TypeI64, false, false},
		{0x0012, "detail", TypeUTF8, false, false},
		{0x0013, "clock_step_ns", TypeI64, false, false},
	})
}

// Transcribed from the annotation table of §8; only annotation_kind is marked required.
func TestAnnotationRegistry(t *testing.T) {
	t.Parallel()

	assertRegistry(t, Annotation, []specRow{
		{0x0001, "annotation_kind", TypeU8, false, true},
		{0x0002, "ref_capture_id", TypeUUID, false, false},
		{0x0003, "ref_seq_first", TypeU64, false, false},
		{0x0004, "ref_seq_last", TypeU64, false, false},
		{0x0005, "text", TypeUTF8, false, false},
		{0x0006, "raw", TypeBytes, false, false},
		{0x0007, "source_index", TypeU64, false, false},
		{0x0008, "source_offset", TypeU64, false, false},
		{0x0009, "source_len", TypeU64, false, false},
	})
}

// Transcribed from the F-3 block summary table of §10.
// A u32 array is a bytes value; the retired tags 0x0004 and 0x0006 and the reserved tags 0x0010–0x001F stay unknown.
func TestF3Registry(t *testing.T) {
	t.Parallel()

	assertRegistry(t, F3, []specRow{
		{0x0001, "kind_counts", TypeBytes, false, true},
		{0x0002, "dir_counts", TypeBytes, false, true},
		{0x0003, "decode_status_counts", TypeBytes, false, true},
		{0x0005, "quality_union", TypeU64, false, true},
		{0x0007, "epoch", TypeTLV, true, true},
		{0x0008, "boundary", TypeTLV, true, false},
		{0x0009, "seq_range", TypeBytes, true, false},
	})
	for tag := uint16(0x0010); tag <= 0x001F; tag++ {
		_, ok := F3[tag]
		assert.False(t, ok, "reserved tag 0x%04X", tag)
	}
}

// Transcribed from the F-5 pack statistics table of §10; no row is marked required.
func TestF5Registry(t *testing.T) {
	t.Parallel()

	assertRegistry(t, F5, []specRow{
		{0x0001, "record_count", TypeU64, false, false},
		{0x0002, "kind_counts", TypeBytes, false, false},
		{0x0003, "dir_counts", TypeBytes, false, false},
		{0x0004, "decode_status_counts", TypeBytes, false, false},
		{0x0005, "ts_min", TypeI64, false, false},
		{0x0006, "ts_max", TypeI64, false, false},
		{0x0007, "content_bytes", TypeU64, false, false},
		{0x0008, "quality_union", TypeU64, false, false},
		{0x0009, "seq_range", TypeBytes, true, false},
		{0x000A, "epoch", TypeTLV, true, false},
		{0x000B, "boundary", TypeTLV, true, false},
	})
}

// Transcribed from "Nested tags of epoch (F-3 and F-5)" in §10:
// the text marks only close_seq optional, so the six tags before it are required.
func TestEpochRegistry(t *testing.T) {
	t.Parallel()

	assertRegistry(t, Epoch, []specRow{
		{0x0001, "epoch", TypeU64, false, true},
		{0x0002, "record_count", TypeU64, false, true},
		{0x0003, "seq_first", TypeU64, false, true},
		{0x0004, "seq_last", TypeU64, false, true},
		{0x0005, "ts_min", TypeI64, false, true},
		{0x0006, "ts_max", TypeI64, false, true},
		{0x0007, "close_seq", TypeU64, false, false},
	})
}

// Transcribed from "Nested tags of boundary" in §10:
// seq, boundary_kind, ts and epoch are plain and required;
// gap_start and gap_end may be absent (an absent gap_start means unbounded), so they are optional.
func TestBoundaryRegistry(t *testing.T) {
	t.Parallel()

	assertRegistry(t, Boundary, []specRow{
		{0x0001, "seq", TypeU64, false, true},
		{0x0002, "boundary_kind", TypeU8, false, true},
		{0x0003, "ts", TypeI64, false, true},
		{0x0004, "epoch", TypeU64, false, true},
		{0x0005, "gap_start", TypeI64, false, false},
		{0x0006, "gap_end", TypeI64, false, false},
	})
}

// Every tlv tag names the registry of its nested entries, from the specification text:
// §5 "Nested tags of coverage", "of redaction_policy", "of redaction" and the hsms_timers row,
// and §10 "Nested tags of epoch (F-3 and F-5)" and "of boundary".
// No tag of another value type names one.
func TestNestedRegistries(t *testing.T) {
	t.Parallel()

	wiring := []struct {
		name   string
		reg    Registry
		tag    uint16
		nested *Registry
	}{
		{"pack metadata coverage", PackMetadata, 0x0016, &Coverage},
		{"pack metadata hsms_timers", PackMetadata, 0x0026, &HSMSTimers},
		{"pack metadata redaction_policy", PackMetadata, 0x0031, &RedactionPolicy},
		{"pack metadata redaction", PackMetadata, 0x0032, &Redaction},
		{"F-3 epoch", F3, 0x0007, &Epoch},
		{"F-3 boundary", F3, 0x0008, &Boundary},
		{"F-5 epoch", F5, 0x000A, &Epoch},
		{"F-5 boundary", F5, 0x000B, &Boundary},
	}
	for _, w := range wiring {
		assert.Same(t, w.nested, w.reg[w.tag].Nested, w.name)
	}

	all := []struct {
		name string
		reg  Registry
	}{
		{"pack metadata", PackMetadata}, {"coverage", Coverage}, {"redaction policy", RedactionPolicy},
		{"redaction", Redaction}, {"hsms timers", HSMSTimers}, {"transport event", TransportEvent},
		{"annotation", Annotation}, {"F-3", F3}, {"F-5", F5}, {"epoch", Epoch}, {"boundary", Boundary},
	}
	wired := 0
	for _, r := range all {
		for tag, f := range r.reg {
			if f.Type == TypeTLV {
				assert.NotNil(t, f.Nested, "%s tag 0x%04X is tlv without a nested registry", r.name, tag)
				wired++
			} else {
				assert.Nil(t, f.Nested, "%s tag 0x%04X is %s with a nested registry", r.name, tag, f.Type)
			}
		}
	}
	assert.Equal(t, len(wiring), wired, "every tlv tag is in the wiring table")
}
