package tlv

import (
	"errors"
	"maps"
	"slices"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// Required tags per registry, read from the specification text:
// §5 "Required when" = always, §5 nested redaction_policy ("all required") and redaction,
// §8 "required" rows, the §10 F-3 rows marked required,
// and the §10 nested tags of epoch and boundary that are not described as optional or possibly absent.
var specRequired = []struct {
	name string
	reg  Registry
	tags []uint16
}{
	{"pack metadata", PackMetadata, []uint16{
		0x0002, 0x0003, 0x0004, 0x0005, 0x0006, 0x0007, 0x0009,
		0x0010, 0x0011, 0x0012, 0x0013, 0x0018, 0x0019, 0x001B, 0x0027,
	}},
	{"coverage", Coverage, nil},
	{"redaction policy", RedactionPolicy, []uint16{0x0001, 0x0002, 0x0003, 0x0004}},
	{"redaction", Redaction, []uint16{0x0001, 0x0003, 0x0004, 0x0005}},
	{"hsms timers", HSMSTimers, nil},
	{"transport event", TransportEvent, []uint16{0x0001}},
	{"annotation", Annotation, []uint16{0x0001}},
	{"F-3", F3, []uint16{0x0001, 0x0002, 0x0003, 0x0005, 0x0007}},
	{"F-5", F5, nil},
	{"epoch", Epoch, []uint16{0x0001, 0x0002, 0x0003, 0x0004, 0x0005, 0x0006}},
	{"boundary", Boundary, []uint16{0x0001, 0x0002, 0x0003, 0x0004}},
}

// entryFor returns a valid entry for the field f of the given tag:
// a zero value of the field's type, or for a field with a nested registry,
// a nested value holding that registry's required tags.
func entryFor(tag uint16, f Field) Entry {
	if f.Nested != nil {
		return NestedEntry(tag, entriesFor(*f.Nested, requiredTags(*f.Nested)))
	}
	n, _ := f.Type.FixedLen()

	return Entry{Tag: tag, Type: f.Type, Value: make([]byte, n)}
}

func entriesFor(reg Registry, tags []uint16) []Entry {
	out := make([]Entry, 0, len(tags))
	for _, tag := range tags {
		out = append(out, entryFor(tag, reg[tag]))
	}

	return out
}

// requiredTags returns the tags reg marks Required, in tag order.
func requiredTags(reg Registry) []uint16 {
	var tags []uint16
	for _, tag := range slices.Sorted(maps.Keys(reg)) {
		if reg[tag].Required {
			tags = append(tags, tag)
		}
	}

	return tags
}

func TestValidateRequiredSets(t *testing.T) {
	t.Parallel()

	for _, tt := range specRequired {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			require.NoError(t, Validate(entriesFor(tt.reg, tt.tags), tt.reg), "all required tags present")

			for i, missing := range tt.tags {
				rest := make([]uint16, 0, len(tt.tags)-1)
				rest = append(rest, tt.tags[:i]...)
				rest = append(rest, tt.tags[i+1:]...)

				err := Validate(entriesFor(tt.reg, rest), tt.reg)
				require.ErrorIs(t, err, ErrMissing, "tag 0x%04X removed", missing)
				var ee *EntryError
				require.ErrorAs(t, err, &ee)
				assert.Equal(t, missing, ee.Tag)
				assert.Equal(t, -1, ee.Offset)
			}
		})
	}
}

// Every tag not in the required list may be omitted: an empty list misses exactly the required tags.
func TestValidateNoExtraRequired(t *testing.T) {
	t.Parallel()

	for _, tt := range specRequired {
		err := Validate(nil, tt.reg)
		if len(tt.tags) == 0 {
			assert.NoError(t, err, tt.name)
			continue
		}
		var ee *EntryError
		require.ErrorAs(t, err, &ee, tt.name)
		assert.Equal(t, tt.tags[0], ee.Tag, "%s: the lowest missing tag is reported", tt.name)
	}
}

// Unknown tags, including private tags and unknown value types, pass validation and are preserved.
func TestValidateUnknownTagsPass(t *testing.T) {
	t.Parallel()

	b := append(mustHex(t, goldenUnknown), mustHex(t, goldenBytes)...)
	b = append(b, mustHex(t, goldenUnknown)...)
	entries, err := Decode(b)
	require.NoError(t, err)
	require.Len(t, entries, 3)

	require.NoError(t, Validate(entries, TransportEvent.withoutRequired()))
	assert.Equal(t, Entry{Tag: 0x7FFF, Type: 9, Value: []byte("xyz")}, entries[0])
	assert.Equal(t, Entry{Tag: 0x8001, Type: TypeBytes, Value: []byte("ab"), Offset: 11}, entries[1])
}

func TestValidateRules(t *testing.T) {
	t.Parallel()

	reg := Registry{
		0x0001: {Name: "once", Type: TypeU64},
		0x0002: {Name: "many", Type: TypeUTF8, Repeatable: true},
		0x0003: {Name: "must", Type: TypeU8, Required: true},
	}
	must := U8Entry(0x0003, 1)

	tests := []struct {
		name    string
		entries []Entry
		want    error
		tag     uint16
		offset  int
	}{
		{"valid", []Entry{U64Entry(1, 7), must}, nil, 0, 0},
		{"repeatable tag twice", []Entry{UTF8Entry(2, "a"), UTF8Entry(2, "b"), must}, nil, 0, 0},
		{"unknown tag twice", []Entry{BytesEntry(9, nil), BytesEntry(9, nil), must}, nil, 0, 0},
		{
			"known tag with another known type",
			[]Entry{must, at(UTF8Entry(1, "x"), 9)}, ErrType, 0x0001, 9,
		},
		{
			"known tag with an unknown type",
			[]Entry{must, at(Entry{Tag: 1, Type: 9, Value: make([]byte, 8)}, 9)}, ErrType, 0x0001, 9,
		},
		{
			"non-repeatable tag twice",
			[]Entry{at(U64Entry(1, 1), 0), at(must, 16), at(U64Entry(1, 2), 25)}, ErrDuplicate, 0x0001, 25,
		},
		{"required tag missing", []Entry{U64Entry(1, 7)}, ErrMissing, 0x0003, -1},
		{
			"the first offending entry is reported",
			[]Entry{at(UTF8Entry(1, "x"), 0), at(UTF8Entry(3, "y"), 9)}, ErrType, 0x0001, 0,
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			err := Validate(tt.entries, reg)
			if tt.want == nil {
				require.NoError(t, err)
				return
			}
			require.ErrorIs(t, err, tt.want)
			var ee *EntryError
			require.ErrorAs(t, err, &ee)
			assert.Equal(t, tt.tag, ee.Tag)
			assert.Equal(t, tt.offset, ee.Offset)
		})
	}
}

// A decoded hsms_timers value validates against its nested registry, which Validate descends into.
func TestValidateNestedHSMSTimers(t *testing.T) {
	t.Parallel()

	entries, err := Decode(mustHex(t, goldenNested))
	require.NoError(t, err)
	require.NoError(t, Validate(entries, Registry{0x0026: PackMetadata[0x0026]}))

	bad := NestedEntry(0x0026, []Entry{U64Entry(3, 1<<63)})
	require.ErrorIs(t, Validate([]Entry{bad}, Registry{0x0026: PackMetadata[0x0026]}), ErrLimit)
}

// Validate applies the content rules of the typed accessors to known tags,
// and leaves the values of unknown and private tags unexamined.
func TestValidateValueContents(t *testing.T) {
	t.Parallel()

	reg := Registry{
		0x0001: {Name: "flag", Type: TypeBool},
		0x0002: {Name: "count", Type: TypeU64},
		0x0003: {Name: "text", Type: TypeUTF8},
		0x0004: {Name: "opaque", Type: TypeTLV},
	}

	tests := []struct {
		name  string
		entry Entry
		want  error
	}{
		{"bool 0", BoolEntry(1, false), nil},
		{"bool 1", BoolEntry(1, true), nil},
		{"bool 2", Entry{Tag: 1, Type: TypeBool, Value: []byte{2}}, ErrBool},
		{"u64 at the limit", U64Entry(2, MaxU64), nil},
		{"u64 above the limit", U64Entry(2, 1<<63), ErrLimit},
		{"u64 of the wrong length", Entry{Tag: 2, Type: TypeU64, Value: make([]byte, 3)}, ErrLength},
		{"utf8 valid", UTF8Entry(3, "EQ-01 \u00e9"), nil},
		{"utf8 empty", UTF8Entry(3, ""), nil},
		{"utf8 invalid", Entry{Tag: 3, Type: TypeUTF8, Value: []byte{'a', 0xFF}}, ErrUTF8},
		// A BOM is valid UTF-8; the specification's "without BOM" binds writers.
		{"utf8 with a leading BOM", UTF8Entry(3, "\uFEFFEQ-01"), nil},
		{"tlv without a nested registry is not decoded", Entry{Tag: 4, Type: TypeTLV, Value: []byte{1, 2, 3}}, nil},
		{"unknown tag with a bool-typed 2", Entry{Tag: 0x7000, Type: TypeBool, Value: []byte{2}}, nil},
		{"unknown tag with a u64 above the limit", U64Entry(0x7001, 1<<63), nil},
		{"unknown tag with invalid utf8", Entry{Tag: 0x7002, Type: TypeUTF8, Value: []byte{0xFF}}, nil},
		{"unknown tag with a malformed tlv", Entry{Tag: 0x7003, Type: TypeTLV, Value: []byte{1, 2, 3}}, nil},
		{"private tag with a malformed tlv", Entry{Tag: 0x8001, Type: TypeTLV, Value: []byte{0, 0}}, nil},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			entries := []Entry{U8Entry(0x7FFF, 0), at(tt.entry, 9)}
			err := Validate(entries, reg)
			if tt.want == nil {
				require.NoError(t, err)
				return
			}
			require.ErrorIs(t, err, tt.want)
			var ee *EntryError
			require.ErrorAs(t, err, &ee)
			assert.Equal(t, tt.entry.Tag, ee.Tag)
			assert.Equal(t, 9, ee.Offset)
		})
	}
}

// Validate decodes the value of a known tlv tag whose field names a nested registry
// and validates it against that registry,
// reporting a failure as an *EntryError for the tlv entry that wraps the nested entry's *EntryError.
func TestValidateNested(t *testing.T) {
	t.Parallel()

	policy := Registry{0x0031: PackMetadata[0x0031]}
	f3 := Registry{0x0007: F3[0x0007], 0x0008: F3[0x0008]}
	policyEntries := func(entries ...Entry) Entry { return NestedEntry(0x0031, entries) }
	validPolicy := []Entry{UTF8Entry(1, "p"), U64Entry(2, 3), UTF8Entry(3, "k"), U8Entry(4, 1)}
	epoch := func(skip uint16) Entry {
		var entries []Entry
		for tag := uint16(1); tag <= 7; tag++ {
			if tag != skip {
				entries = append(entries, entryFor(tag, Epoch[tag]))
			}
		}

		return NestedEntry(0x0007, entries)
	}

	tests := []struct {
		name        string
		reg         Registry
		entry       Entry
		want        error
		innerTag    uint16
		innerOffset int
	}{
		{"valid redaction policy", policy, policyEntries(validPolicy...), nil, 0, 0},
		{
			"nested entry of the wrong type", policy,
			policyEntries(UTF8Entry(1, "p"), UTF8Entry(2, "3"), UTF8Entry(3, "k"), U8Entry(4, 1)),
			ErrType, 0x0002, 9,
		},
		{
			"nested required tag missing", policy,
			policyEntries(UTF8Entry(1, "p"), U64Entry(2, 3), U8Entry(4, 1)),
			ErrMissing, 0x0003, -1,
		},
		{
			"nested u64 above the limit", policy,
			policyEntries(UTF8Entry(1, "p"), U64Entry(2, 1<<63), UTF8Entry(3, "k"), U8Entry(4, 1)),
			ErrLimit, 0x0002, 9,
		},
		{
			"nested value that does not decode", policy,
			Entry{Tag: 0x0031, Type: TypeTLV, Value: []byte{1, 0, 6, 0, 9, 0, 0, 0, 'p'}},
			ErrTruncated, 0x0001, 0,
		},
		{"valid epoch", f3, epoch(0), nil, 0, 0},
		{"epoch without record_count", f3, epoch(2), ErrMissing, 0x0002, -1},
		{"epoch without close_seq", f3, epoch(7), nil, 0, 0},
		{
			"boundary without gap_start and gap_end", f3,
			NestedEntry(0x0008, entriesFor(Boundary, []uint16{1, 2, 3, 4})), nil, 0, 0,
		},
		{
			"boundary without ts", f3,
			NestedEntry(0x0008, entriesFor(Boundary, []uint16{1, 2, 4})), ErrMissing, 0x0003, -1,
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			entries := []Entry{at(U8Entry(0x7000, 0), 0), at(tt.entry, 9)}
			if _, ok := tt.reg[0x0007]; ok && tt.entry.Tag != 0x0007 {
				entries = append(entries, at(epoch(0), 9+HeaderLen+len(tt.entry.Value)))
			}
			err := Validate(entries, tt.reg)
			if tt.want == nil {
				require.NoError(t, err)
				return
			}
			require.ErrorIs(t, err, tt.want)

			var outer *EntryError
			require.ErrorAs(t, err, &outer)
			assert.Equal(t, tt.entry.Tag, outer.Tag)
			assert.Equal(t, 9, outer.Offset)

			var inner *EntryError
			require.ErrorAs(t, outer.Err, &inner, "the nested entry's error is wrapped")
			assert.Equal(t, tt.innerTag, inner.Tag)
			assert.Equal(t, tt.innerOffset, inner.Offset, "the nested offset is relative to the nested value")
		})
	}
}

// Validate descends at most MaxNestingDepth levels of nested tlv values,
// so a registry that names itself cannot make it recurse without limit.
func TestValidateNestingDepthLimit(t *testing.T) {
	t.Parallel()

	chain := Registry{}
	chain[1] = Field{Name: "next", Type: TypeTLV, Nested: &chain}
	nest := func(levels int) []Entry {
		e := NestedEntry(1, nil)
		for range levels - 1 {
			e = NestedEntry(1, []Entry{e})
		}

		return []Entry{e}
	}

	require.NoError(t, Validate(nest(MaxNestingDepth), chain))

	err := Validate(nest(MaxNestingDepth+1), chain)
	require.ErrorIs(t, err, ErrDepth)
	levels := 0
	for ee := (*EntryError)(nil); errors.As(err, &ee); err = ee.Err {
		levels++
	}
	assert.Equal(t, MaxNestingDepth+1, levels, "the error names each tlv entry down to the one too deep to descend")
}

// Every registry accepts the entry list the fuzz seed builds for it, nested values included.
func TestValidateRegistrySeeds(t *testing.T) {
	t.Parallel()

	for i, reg := range fuzzRegistries {
		entries, err := Decode(registrySeed(reg))
		require.NoError(t, err)
		require.NoError(t, Validate(entries, reg), "registry %d", i)
	}
}

// at returns e with its Offset set, as Decode would.
func at(e Entry, offset int) Entry {
	e.Offset = offset

	return e
}

// withoutRequired returns a copy of r in which no tag is required.
func (r Registry) withoutRequired() Registry {
	out := make(Registry, len(r))
	for tag, f := range r {
		f.Required = false
		out[tag] = f
	}

	return out
}
