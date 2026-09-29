package tlv

// Tag registries of the tracepack format specification.
//
// Required is set only where the specification requires a tag unconditionally.
// A tag the specification requires under a condition on other values
// (for example `classifier`, required when a record's decode_status was computed)
// is not Required here; the caller that knows the condition checks it.
var (
	// PackMetadata is the pack metadata tag registry (the tracepack format specification §5).
	// The retired tags 0x0001, 0x0014 and 0x0030 are absent, so a reader skips them like any unknown tag.
	PackMetadata = Registry{
		0x0002: {Name: "tool_id", Type: TypeUTF8, Required: true},
		0x0003: {Name: "transport", Type: TypeU8, Required: true},
		0x0004: {Name: "capture_method", Type: TypeU8, Required: true},
		0x0005: {Name: "vantage", Type: TypeU8, Required: true},
		0x0006: {Name: "recorder", Type: TypeUTF8, Required: true},
		0x0007: {Name: "writer", Type: TypeUTF8, Required: true},
		0x0008: {Name: "classifier", Type: TypeUTF8},
		0x0009: {Name: "time_source", Type: TypeU8, Required: true},
		0x000A: {Name: "capture_origin_utc_ns", Type: TypeI64},
		0x000B: {Name: "capture_origin_mono_ns", Type: TypeI64},
		0x000C: {Name: "source_tz", Type: TypeUTF8},
		0x000D: {Name: "source_dialect", Type: TypeUTF8},
		0x000E: {Name: "source_ref", Type: TypeUTF8, Repeatable: true},
		0x000F: {Name: "max_frame_len", Type: TypeU64},
		0x0010: {Name: "period_start", Type: TypeI64, Required: true},
		0x0011: {Name: "period_end", Type: TypeI64, Required: true},
		0x0012: {Name: "lifecycle_coverage", Type: TypeU8, Required: true},
		0x0013: {Name: "quality_evaluated", Type: TypeBool, Required: true},
		0x0015: {Name: "supersedes", Type: TypeUUID, Repeatable: true},
		0x0016: {Name: "coverage", Type: TypeTLV, Repeatable: true, Nested: &Coverage},
		0x0017: {Name: "notes", Type: TypeUTF8},
		0x0018: {Name: "pack_role", Type: TypeU8, Required: true},
		0x0019: {Name: "compaction_level", Type: TypeU8, Required: true},
		0x001A: {Name: "compacted_from", Type: TypeUUID, Repeatable: true},
		0x001B: {Name: "recorder_instance_id", Type: TypeUUID, Required: true},
		0x001C: {Name: "previous_capture_id", Type: TypeUUID},
		0x001D: {Name: "extract_filter", Type: TypeUTF8},
		0x001E: {Name: "site_id", Type: TypeUTF8},
		0x001F: {Name: "equipment_model", Type: TypeUTF8},
		0x0020: {Name: "equipment_sw_rev", Type: TypeUTF8},
		0x0021: {Name: "host_software", Type: TypeUTF8},
		0x0022: {Name: "host_endpoint", Type: TypeUTF8},
		0x0023: {Name: "equipment_endpoint", Type: TypeUTF8},
		0x0024: {Name: "equipment_connect_mode", Type: TypeU8},
		0x0025: {Name: "device_id", Type: TypeU64},
		0x0026: {Name: "hsms_timers", Type: TypeTLV, Nested: &HSMSTimers},
		0x0027: {Name: "seq_start", Type: TypeU64, Required: true},
		0x0028: {Name: "clock_step_tolerance_ns", Type: TypeU64},
		0x0029: {Name: "replacement_set_id", Type: TypeUUID},
		0x002A: {Name: "replacement_set_size", Type: TypeU64},
		0x002B: {Name: "replacement_set_index", Type: TypeU64},
		0x002C: {Name: "flush_interval_ns", Type: TypeU64},
		0x002D: {Name: "scope_generation", Type: TypeU64},
		0x002E: {Name: "publisher_epoch", Type: TypeU64},
		0x002F: {Name: "patch_base", Type: TypeUUID},
		0x0031: {Name: "redaction_policy", Type: TypeTLV, Nested: &RedactionPolicy},
		0x0032: {Name: "redaction", Type: TypeTLV, Repeatable: true, Nested: &Redaction},
	}

	// Coverage is the registry of the entries nested in a pack metadata `coverage` entry.
	// The tracepack format specification §5 defines it.
	Coverage = Registry{
		0x0001: {Name: "capture_id", Type: TypeUUID},
		0x0002: {Name: "seq_first", Type: TypeU64},
		0x0003: {Name: "seq_last", Type: TypeU64},
		0x0004: {Name: "time_start", Type: TypeI64},
		0x0005: {Name: "time_end", Type: TypeI64},
	}

	// RedactionPolicy is the registry of the entries nested in a pack metadata `redaction_policy` entry.
	// The tracepack format specification §5 defines it.
	RedactionPolicy = Registry{
		0x0001: {Name: "policy_id", Type: TypeUTF8, Required: true},
		0x0002: {Name: "policy_version", Type: TypeU64, Required: true},
		0x0003: {Name: "key_id", Type: TypeUTF8, Required: true},
		0x0004: {Name: "digest_algorithm", Type: TypeU8, Required: true},
	}

	// Redaction is the registry of the entries nested in a pack metadata `redaction` entry.
	// The tracepack format specification §5 defines it.
	// The masked_ranges value is a u64 array of (offset, length) pairs.
	Redaction = Registry{
		0x0001: {Name: "seq", Type: TypeU64, Required: true},
		0x0002: {Name: "item_path", Type: TypeUTF8},
		0x0003: {Name: "masked_ranges", Type: TypeBytes, Required: true},
		0x0004: {Name: "domain", Type: TypeUTF8, Required: true},
		0x0005: {Name: "digest", Type: TypeBytes, Required: true},
	}

	// HSMSTimers is the registry of the entries nested in a pack metadata `hsms_timers` entry.
	// The tracepack format specification §5 defines it:
	// tag n is the configured Tn in milliseconds.
	HSMSTimers = Registry{
		1: {Name: "T1", Type: TypeU64},
		2: {Name: "T2", Type: TypeU64},
		3: {Name: "T3", Type: TypeU64},
		4: {Name: "T4", Type: TypeU64},
		5: {Name: "T5", Type: TypeU64},
		6: {Name: "T6", Type: TypeU64},
		7: {Name: "T7", Type: TypeU64},
		8: {Name: "T8", Type: TypeU64},
	}

	// TransportEvent is the registry of a transport-event record payload.
	// The tracepack format specification §8 defines it.
	TransportEvent = Registry{
		0x0001: {Name: "event", Type: TypeU8, Required: true},
		0x0002: {Name: "prev_state", Type: TypeU8},
		0x0003: {Name: "cur_state", Type: TypeU8},
		0x0004: {Name: "cause", Type: TypeU8},
		0x0005: {Name: "cause_raw", Type: TypeUTF8},
		0x0006: {Name: "timer", Type: TypeU8},
		0x0007: {Name: "socket_role", Type: TypeU8},
		0x0008: {Name: "inferred", Type: TypeBool},
		0x0009: {Name: "primary_session_id", Type: TypeU64},
		0x000A: {Name: "primary_stream", Type: TypeU64},
		0x000B: {Name: "primary_function", Type: TypeU64},
		0x000C: {Name: "primary_system_bytes", Type: TypeBytes},
		0x000D: {Name: "boundary_kind", Type: TypeU8},
		0x000E: {Name: "boundary_seq_first", Type: TypeU64},
		0x000F: {Name: "boundary_seq_last", Type: TypeU64},
		0x0010: {Name: "gap_start", Type: TypeI64},
		0x0011: {Name: "gap_end", Type: TypeI64},
		0x0012: {Name: "detail", Type: TypeUTF8},
		0x0013: {Name: "clock_step_ns", Type: TypeI64},
	}

	// Annotation is the registry of an annotation record payload (the tracepack format specification §8).
	Annotation = Registry{
		0x0001: {Name: "annotation_kind", Type: TypeU8, Required: true},
		0x0002: {Name: "ref_capture_id", Type: TypeUUID},
		0x0003: {Name: "ref_seq_first", Type: TypeU64},
		0x0004: {Name: "ref_seq_last", Type: TypeU64},
		0x0005: {Name: "text", Type: TypeUTF8},
		0x0006: {Name: "raw", Type: TypeBytes},
		0x0007: {Name: "source_index", Type: TypeU64},
		0x0008: {Name: "source_offset", Type: TypeU64},
		0x0009: {Name: "source_len", Type: TypeU64},
	}

	// F3 is the registry of a block summary entry list in footer section F-3.
	// The tracepack format specification §10 defines it.
	// The count arrays are bytes values holding u32 arrays;
	// the retired tags 0x0004 and 0x0006 and the reserved tags 0x0010–0x001F are absent, so a reader skips them.
	F3 = Registry{
		0x0001: {Name: "kind_counts", Type: TypeBytes, Required: true},
		0x0002: {Name: "dir_counts", Type: TypeBytes, Required: true},
		0x0003: {Name: "decode_status_counts", Type: TypeBytes, Required: true},
		0x0005: {Name: "quality_union", Type: TypeU64, Required: true},
		0x0007: {Name: "epoch", Type: TypeTLV, Repeatable: true, Required: true, Nested: &Epoch},
		0x0008: {Name: "boundary", Type: TypeTLV, Repeatable: true, Nested: &Boundary},
		0x0009: {Name: "seq_range", Type: TypeBytes, Repeatable: true},
	}

	// F5 is the registry of the pack statistics entry list in footer section F-5.
	// The tracepack format specification §10 defines it.
	// The count arrays are bytes values holding u64 arrays.
	F5 = Registry{
		0x0001: {Name: "record_count", Type: TypeU64},
		0x0002: {Name: "kind_counts", Type: TypeBytes},
		0x0003: {Name: "dir_counts", Type: TypeBytes},
		0x0004: {Name: "decode_status_counts", Type: TypeBytes},
		0x0005: {Name: "ts_min", Type: TypeI64},
		0x0006: {Name: "ts_max", Type: TypeI64},
		0x0007: {Name: "content_bytes", Type: TypeU64},
		0x0008: {Name: "quality_union", Type: TypeU64},
		0x0009: {Name: "seq_range", Type: TypeBytes, Repeatable: true},
		0x000A: {Name: "epoch", Type: TypeTLV, Repeatable: true, Nested: &Epoch},
		0x000B: {Name: "boundary", Type: TypeTLV, Repeatable: true, Nested: &Boundary},
	}

	// Epoch is the registry of the entries nested in an `epoch` entry of F-3 or F-5.
	// The tracepack format specification §10 defines it,
	// marking only close_seq optional.
	Epoch = Registry{
		0x0001: {Name: "epoch", Type: TypeU64, Required: true},
		0x0002: {Name: "record_count", Type: TypeU64, Required: true},
		0x0003: {Name: "seq_first", Type: TypeU64, Required: true},
		0x0004: {Name: "seq_last", Type: TypeU64, Required: true},
		0x0005: {Name: "ts_min", Type: TypeI64, Required: true},
		0x0006: {Name: "ts_max", Type: TypeI64, Required: true},
		0x0007: {Name: "close_seq", Type: TypeU64},
	}

	// Boundary is the registry of the entries nested in a `boundary` entry of F-3 or F-5.
	// The tracepack format specification §10 defines it:
	// gap_start and gap_end are copied from the boundary record and may be absent,
	// and the other tags are required.
	Boundary = Registry{
		0x0001: {Name: "seq", Type: TypeU64, Required: true},
		0x0002: {Name: "boundary_kind", Type: TypeU8, Required: true},
		0x0003: {Name: "ts", Type: TypeI64, Required: true},
		0x0004: {Name: "epoch", Type: TypeU64, Required: true},
		0x0005: {Name: "gap_start", Type: TypeI64},
		0x0006: {Name: "gap_end", Type: TypeI64},
	}
)

// Field describes one tag of a registry.
type Field struct {
	// Name is the tag's name in the specification.
	Name string
	// Type is the value type the tag must carry.
	Type ValueType
	// Repeatable reports whether the tag may appear more than once.
	Repeatable bool
	// Required reports whether the tag must be present in every entry list the registry applies to.
	Required bool
	// Nested is the registry of the entries nested in the value of a tlv tag,
	// which Validate decodes and validates against it;
	// nil leaves the value unexamined.
	Nested *Registry
}

// Registry maps the tags of one entry list to their fields.
// A tag absent from the registry is unknown: a reader skips it and preserves its raw value.
type Registry map[uint16]Field
