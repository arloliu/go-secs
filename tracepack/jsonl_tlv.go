package tracepack

import (
	"encoding/binary"
	"math/bits"

	"github.com/arloliu/go-secs/tracepack/internal/tlv"
)

// jsonlTagLimit bounds the known tags of the registries the export uses:
// every one is below it, so the known tags of an entry list fit a jsonlTagSet.
const jsonlTagLimit = 256

// jsonlPackMetadata10 is the format 1.0 view of the pack metadata registry, the one schema tracepack-jsonl/1 renders:
// a tag a later minor defines is unknown in it, at every nesting level,
// so a pack of minor 0 exports as a reader of format 1.0 sees it.
var jsonlPackMetadata10 = tlv.PackMetadata.AtMinor(0)

// jsonlPackMetadata11 is the format 1.1 view of the pack metadata registry, the one schema tracepack-jsonl/2 renders:
// a tag a minor after 1 defines is unknown in it, whatever this package knows,
// since naming it would change the output of the schema (the tracepack JSONL specification §3.1 and §9).
var jsonlPackMetadata11 = tlv.PackMetadata.AtMinor(1)

// Registries of the entry lists the export renders, with the enum each enum-typed tag holds
// (the tracepack JSONL specification §3.1) and the registry of each known tlv tag.
// Each nested registry is the one its schema's view of the pack metadata names,
// so the walk descends with the registry its validation uses.
// No later minor extends coverage or hsms_timers, so their registries serve both schemas.
var (
	jsonlCoverageRegistry        = &jsonlRegistry{reg: *jsonlPackMetadata10[tagCoverage].Nested}
	jsonlHSMSTimersRegistry      = &jsonlRegistry{reg: *jsonlPackMetadata10[tagHSMSTimers].Nested}
	jsonlRedactionPolicyRegistry = &jsonlRegistry{
		reg:   *jsonlPackMetadata10[tagRedactionPolicy].Nested,
		enums: map[uint16][]string{tagRedactionPolicyDigestAlgorithm: jsonlDigestAlgorithmNames},
	}
	jsonlRedactionRegistry = &jsonlRegistry{reg: *jsonlPackMetadata10[tagRedaction].Nested}

	// jsonlPackMetadataEnums holds the enum of each enum-typed tag of the pack metadata, the same under both schemas.
	jsonlPackMetadataEnums = map[uint16][]string{
		tagTransport:            jsonlTransportNames,
		tagCaptureMethod:        jsonlCaptureMethodNames,
		tagVantage:              jsonlVantageNames,
		tagTimeSource:           jsonlTimeSourceNames,
		tagLifecycleCoverage:    jsonlLifecycleCoverageNames,
		tagPackRole:             jsonlPackRoleNames,
		tagEquipmentConnectMode: jsonlSocketRoleNames,
	}

	jsonlPackMetadataRegistry = &jsonlRegistry{
		reg:   jsonlPackMetadata10,
		enums: jsonlPackMetadataEnums,
		nested: map[uint16]*jsonlRegistry{
			tagCoverage:        jsonlCoverageRegistry,
			tagHSMSTimers:      jsonlHSMSTimersRegistry,
			tagRedactionPolicy: jsonlRedactionPolicyRegistry,
			tagRedaction:       jsonlRedactionRegistry,
		},
	}

	jsonlRedactionPolicyRegistryV2 = &jsonlRegistry{
		reg: *jsonlPackMetadata11[tagRedactionPolicy].Nested,
		enums: map[uint16][]string{
			tagRedactionPolicyDigestAlgorithm:     jsonlDigestAlgorithmNames,
			tagRedactionPolicyEncryptionAlgorithm: jsonlEncryptionAlgorithmNames,
		},
	}
	jsonlRedactionRegistryV2 = &jsonlRegistry{reg: *jsonlPackMetadata11[tagRedaction].Nested}
	jsonlThinningRegistry    = &jsonlRegistry{reg: *jsonlPackMetadata11[tagThinning].Nested}

	jsonlPackMetadataRegistryV2 = &jsonlRegistry{
		reg:   jsonlPackMetadata11,
		enums: jsonlPackMetadataEnums,
		nested: map[uint16]*jsonlRegistry{
			tagCoverage:        jsonlCoverageRegistry,
			tagHSMSTimers:      jsonlHSMSTimersRegistry,
			tagRedactionPolicy: jsonlRedactionPolicyRegistryV2,
			tagRedaction:       jsonlRedactionRegistryV2,
			tagThinning:        jsonlThinningRegistry,
		},
	}

	jsonlTransportEventRegistry = &jsonlRegistry{
		reg: tlv.TransportEvent,
		enums: map[uint16][]string{
			tagEvent:        jsonlEventNames,
			tagPrevState:    jsonlStateNames,
			tagCurState:     jsonlStateNames,
			tagCause:        jsonlCauseNames,
			tagTimer:        jsonlTimerNames,
			tagSocketRole:   jsonlSocketRoleNames,
			tagBoundaryKind: jsonlBoundaryKindNames,
		},
	}

	jsonlAnnotationRegistry = &jsonlRegistry{
		reg:   tlv.Annotation,
		enums: map[uint16][]string{tagAnnotationKind: jsonlAnnotationKindNames},
	}
)

// The schemas of the export (the tracepack JSONL specification §9).
// They differ only in the registry of the pack metadata and the names of the quality bits:
// the record bodies, the enums of the record header and the other bit sets are named alike under both.
var (
	jsonlSchemaV1 = &jsonlSchema{name: "tracepack-jsonl/1", meta: jsonlPackMetadataRegistry, quality: jsonlQualityNames}
	jsonlSchemaV2 = &jsonlSchema{name: "tracepack-jsonl/2", meta: jsonlPackMetadataRegistryV2, quality: jsonlQualityNamesV2}
)

// jsonlSchema is one schema of the export with the names it renders (the tracepack JSONL specification §3.1 and §3.2).
type jsonlSchema struct {
	// name is the schema value the header line writes.
	name string
	// meta is the registry of the pack metadata.
	meta *jsonlRegistry
	// quality holds the names of the quality bits.
	quality []string
}

// jsonlRegistry is the registry of one kind of entry list as the export uses it.
type jsonlRegistry struct {
	// reg names the known tags with their value types and repeatable and required flags.
	reg tlv.Registry
	// enums holds, for each u8 tag that holds an enum, the enum's names; every other u8 tag is a number.
	enums map[uint16][]string
	// nested holds the registry of the entries nested in each known tlv tag.
	nested map[uint16]*jsonlRegistry
}

// jsonlTagSet is a set of known tags, each below jsonlTagLimit.
type jsonlTagSet [jsonlTagLimit / 64]uint64

// jsonlSchemaOf returns the schema of the export of a pack of the format minor
// (the tracepack JSONL specification §5 and §9):
// tracepack-jsonl/1 for minor 0, and tracepack-jsonl/2 for every other minor, one this package does not know included.
func jsonlSchemaOf(minor uint16) *jsonlSchema {
	if minor == 0 {
		return jsonlSchemaV1
	}

	return jsonlSchemaV2
}

// writeJSONLHeader writes the header line of the export of the pack r reads (the tracepack JSONL specification §5)
// and returns the first write error of jw.
// The schema follows from the pack's format minor.
// The pack metadata is rendered from the bytes Open validated, not from the decoded PackMeta,
// so absent and present-but-zero values stay distinct and unknown entries keep their raw bytes.
func writeJSONLHeader(jw *jsonlWriter, r *Reader) error {
	schema := jsonlSchemaOf(r.hdr.FormatMinor)
	jw.beginObject()
	jw.key("schema")
	writeString(jw, schema.name)
	jw.key("format_major")
	jw.num(uint32(r.hdr.FormatMajor))
	jw.key("format_minor")
	jw.num(uint32(r.hdr.FormatMinor))
	jw.key("flags")
	jw.bitSet(jsonlHeaderFlagNames, r.hdr.Flags)
	jw.key("writer_start_utc_ns")
	jw.i64(r.hdr.WriterStartUTCNs)
	jw.key("pack_id")
	jw.uuid([16]byte(r.hdr.PackID))
	jw.key("capture_id")
	jw.uuid([16]byte(r.hdr.CaptureID))
	jw.key("metadata")
	schema.meta.render(jw, r.metaRaw)
	jw.endObject()

	return jw.endLine()
}

// jsonlValidBody reports whether payload is a valid TLV body of a record of kind (the tracepack format specification §8):
// an entry list that fills payload exactly and is valid under the kind's registry,
// with a present primary_system_bytes of exactly 4 bytes in a transport-event,
// and exactly one of text and raw, either possibly empty, in an annotation.
// Any kind other than transport-event is checked as an annotation, as checkEventPayload does,
// and the result is checkEventPayload(kind, payload) == nil, decided without decoding the payload into values.
func jsonlValidBody(kind Kind, payload []byte) bool {
	if kind != KindTransportEvent {
		seen, ok := jsonlAnnotationRegistry.valid(payload, 0)

		return ok && seen.has(tagText) != seen.has(tagRaw)
	}

	if _, ok := jsonlTransportEventRegistry.valid(payload, 0); !ok {
		return false
	}
	// The list is valid, so the walk yields no error.
	for e := range tlv.Entries(payload) {
		if e.Tag == tagPrimarySystemBytes && len(e.Value) != primarySystemBytesLen {
			return false
		}
	}

	return true
}

// valid reports whether the entry list b, nested depth levels below the list a check starts at,
// is valid under reg by the rules of the tracepack format specification §5,
// and returns the set of known tags b holds.
//
// Every entry must be framed;
// a known tag must carry its registry's value type and a value that passes the type's content rule,
// appear at most once unless it is repeatable, and, for a tlv tag, hold an entry list valid under its nested registry;
// every required tag must be present.
// Unknown tags are only framed: they may repeat and their values are never examined.
// Like tlv.Validate it descends at most tlv.MaxNestingDepth levels, and a known tlv value deeper than that makes b invalid.
// It walks b in place and allocates nothing for a valid list.
func (reg *jsonlRegistry) valid(b []byte, depth int) (jsonlTagSet, bool) {
	var seen jsonlTagSet
	for e, err := range tlv.Entries(b) {
		if err != nil {
			return seen, false
		}
		f, known := reg.reg[e.Tag]
		if !known {
			continue
		}
		if seen.has(e.Tag) && !f.Repeatable {
			return seen, false
		}
		seen.add(e.Tag)
		if e.CheckValue(f.Type) != nil {
			return seen, false
		}
		if nested := reg.nested[e.Tag]; nested != nil {
			if depth >= tlv.MaxNestingDepth {
				return seen, false
			}
			if _, ok := nested.valid(e.Value, depth+1); !ok {
				return seen, false
			}
		}
	}

	for tag, f := range reg.reg {
		if f.Required && !seen.has(tag) {
			return seen, false
		}
	}

	return seen, true
}

// render writes the entry list b as a TLV object (the tracepack JSONL specification §4).
//
// b must be valid under reg, as valid decides, before render is called:
// render checks nothing, every walk of it ranging over the entries alone,
// so it relies on every entry being framed and every known value having its registry's type and length.
//
// The object holds one key per known tag present, in ascending tag order, each from its own walk of b,
// so a repeatable tag's values come out in stored order;
// then, from one more walk, the unknown entries in stored order under the key "unknown", omitted when there is none.
// Nothing is copied or decoded into values: memory does not grow with the number of entries or the length of a value.
func (reg *jsonlRegistry) render(jw *jsonlWriter, b []byte) {
	var present jsonlTagSet
	hasUnknown := false
	for e := range tlv.Entries(b) {
		if _, known := reg.reg[e.Tag]; known {
			present.add(e.Tag)
		} else {
			hasUnknown = true
		}
	}

	jw.beginObject()
	for i, word := range present {
		for word != 0 {
			bit := bits.TrailingZeros64(word)
			word &^= 1 << bit
			reg.renderTag(jw, b, uint16(i*64+bit))
		}
	}
	if hasUnknown {
		reg.renderUnknown(jw, b)
	}
	jw.endObject()
}

// renderTag writes the key of the known tag and its value, or for a repeatable tag the array of its values, in stored order.
func (reg *jsonlRegistry) renderTag(jw *jsonlWriter, b []byte, tag uint16) {
	f := reg.reg[tag]
	jw.key(f.Name)
	if f.Repeatable {
		jw.beginArray()
	}
	for e := range tlv.Entries(b) {
		if e.Tag != tag {
			continue
		}
		reg.renderValue(jw, f.Type, e)
		if !f.Repeatable {
			break
		}
	}
	if f.Repeatable {
		jw.endArray()
	}
}

// renderValue writes the value of the known entry e, of value type vt, in its JSON form (the tracepack JSONL specification §3).
func (reg *jsonlRegistry) renderValue(jw *jsonlWriter, vt tlv.ValueType, e tlv.Entry) {
	switch vt {
	case tlv.TypeU8:
		if names, ok := reg.enums[e.Tag]; ok {
			jw.enum(names, e.Value[0])
		} else {
			jw.num(uint32(e.Value[0]))
		}
	case tlv.TypeBool:
		jw.boolean(e.Value[0] == 1)
	case tlv.TypeI64:
		jw.i64(int64(binary.LittleEndian.Uint64(e.Value)))
	case tlv.TypeU64:
		jw.u64(binary.LittleEndian.Uint64(e.Value))
	case tlv.TypeUUID:
		jw.uuid([16]byte(e.Value))
	case tlv.TypeUTF8:
		writeString(jw, e.Value)
	case tlv.TypeBytes:
		jw.base64(e.Value)
	case tlv.TypeTLV:
		reg.nested[e.Tag].render(jw, e.Value)
	default:
		// The registries of the export give their tags no other value type.
	}
}

// renderUnknown writes the key "unknown" and the array of the entries of b whose tags reg does not know, in stored order:
// each as its tag, its value type and its raw value in base64, whatever the value type says.
func (reg *jsonlRegistry) renderUnknown(jw *jsonlWriter, b []byte) {
	jw.key("unknown")
	jw.beginArray()
	for e := range tlv.Entries(b) {
		if _, known := reg.reg[e.Tag]; known {
			continue
		}
		jw.beginObject()
		jw.key("tag")
		jw.num(uint32(e.Tag))
		jw.key("value_type")
		jw.num(uint32(e.Type))
		jw.key("value")
		jw.base64(e.Value)
		jw.endObject()
	}
	jw.endArray()
}

// add adds tag to s.
func (s *jsonlTagSet) add(tag uint16) {
	s[tag/64] |= 1 << (tag % 64)
}

// has reports whether s holds tag.
func (s *jsonlTagSet) has(tag uint16) bool {
	return s[tag/64]&(1<<(tag%64)) != 0
}
