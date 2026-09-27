package tlv

import (
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// Required tags per registry, read from the specification text:
// §5 "Required when" = always, §5 nested redaction_policy ("all required") and redaction,
// §8 "required" rows, and the §10 F-3 rows marked required.
var specRequired = []struct {
	name string
	reg  Registry
	tags []uint16
}{
	{"pack metadata", PackMetadata, []uint16{
		0x0001, 0x0002, 0x0003, 0x0004, 0x0005, 0x0006, 0x0007, 0x0009,
		0x0010, 0x0011, 0x0012, 0x0013, 0x0018, 0x0019, 0x001B, 0x0027,
	}},
	{"coverage", Coverage, nil},
	{"redaction policy", RedactionPolicy, []uint16{0x0001, 0x0002, 0x0003, 0x0004}},
	{"redaction", Redaction, []uint16{0x0001, 0x0003, 0x0004, 0x0005}},
	{"hsms timers", HSMSTimers, nil},
	{"transport event", TransportEvent, []uint16{0x0001}},
	{"annotation", Annotation, []uint16{0x0001}},
	{"F-3", F3, []uint16{0x0001, 0x0002, 0x0003, 0x0004, 0x0005, 0x0006, 0x0007}},
	{"F-5", F5, nil},
	{"epoch", Epoch, nil},
	{"boundary", Boundary, nil},
}

// entryOfType returns an entry of the given tag and type with a value of valid length.
func entryOfType(tag uint16, vt ValueType) Entry {
	n, _ := vt.FixedLen()

	return Entry{Tag: tag, Type: vt, Value: make([]byte, n)}
}

func entriesFor(reg Registry, tags []uint16) []Entry {
	out := make([]Entry, 0, len(tags))
	for _, tag := range tags {
		out = append(out, entryOfType(tag, reg[tag].Type))
	}

	return out
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

// A decoded hsms_timers value validates against its nested registry.
func TestValidateNestedHSMSTimers(t *testing.T) {
	t.Parallel()

	entries, err := Decode(mustHex(t, goldenNested))
	require.NoError(t, err)
	require.NoError(t, Validate(entries, Registry{0x0026: PackMetadata[0x0026]}))

	nested, err := entries[0].Nested()
	require.NoError(t, err)
	require.NoError(t, Validate(nested, HSMSTimers))
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
