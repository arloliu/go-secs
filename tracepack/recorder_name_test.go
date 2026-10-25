package tracepack

import (
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/arloliu/go-secs/tracepack/internal/format"
	"github.com/arloliu/go-secs/tracepack/internal/tlv"
)

// Tag 0x0034 is recorder_instance_name only in a pack of format 1.2 or later (the tracepack format specification §5 and §14):
// below minor 2 it stays unknown whatever its value type or value;
// from minor 2 a wrong type or invalid UTF-8 rejects the metadata.
func TestRecorderInstanceNameByMinor(t *testing.T) {
	t.Parallel()

	entries := map[string]tlv.Entry{
		"utf8":         tlv.UTF8Entry(tagRecorderInstanceName, "eqp"),
		"bytes":        tlv.BytesEntry(tagRecorderInstanceName, []byte{0xFF, 0x00}),
		"invalid utf8": {Tag: tagRecorderInstanceName, Type: tlv.TypeUTF8, Value: []byte{0xFF}},
	}
	for name, e := range entries {
		raw := jsonlTLV(append(jsonlV2Required(), e)...)
		for _, minor := range []uint16{0, 1} {
			p, err := NewPackInfoMinor(UUID{1}, UUID{2}, minor, raw)
			require.NoError(t, err, "%s at minor %d", name, minor)
			m := p.Meta()
			assert.Nil(t, m.RecorderInstanceName, "%s at minor %d", name, minor)
			assert.Equal(t, []RawEntry{{Tag: e.Tag, Type: uint8(e.Type), Value: e.Value}}, m.Unknown, "%s at minor %d", name, minor)
		}

		p, err := NewPackInfoMinor(UUID{1}, UUID{2}, 2, raw)
		if name == "utf8" {
			require.NoError(t, err)
			assert.Equal(t, new("eqp"), p.Meta().RecorderInstanceName)
			assert.Empty(t, p.Meta().Unknown)

			continue
		}
		require.Error(t, err, "%s at minor 2", name)
	}

	// The export names the tag only under the schema of its minor.
	raw := jsonlTLV(append(jsonlV2Required(), entries["utf8"])...)
	for minor, want := range map[uint16]string{
		0: jsonlV2HeaderStart("1", 0) + jsonlV2RequiredJSON + `,"unknown":[{"tag":52,"value_type":6,"value":"ZXFw"}]}}` + "\n",
		1: jsonlV2HeaderStart("2", 1) + jsonlV2RequiredJSON + `,"unknown":[{"tag":52,"value_type":6,"value":"ZXFw"}]}}` + "\n",
		2: jsonlV2HeaderStart("3", 2) + jsonlV2RequiredJSON + `,"recorder_instance_name":"eqp"}}` + "\n",
	} {
		r := jsonlTestReader(t, format.FileHeader{FormatMajor: 1, FormatMinor: minor}, raw)
		out, _, err := runExportJSONL(t, r)
		require.NoError(t, err)
		assert.Equal(t, want, out, "minor %d", minor)
	}
}

// A merge carries recorder_instance_name as a capture-level tag:
// packs of format 1.2 with equal names merge into a format 1.2 archive that carries the name,
// and a minor-1 input whose 0x0034 is an unknown entry, with the same bytes, does not carry a name;
// so the merge refuses it.
func TestMergeRecorderInstanceName(t *testing.T) {
	t.Parallel()

	a := mergePack(t, seg0, captureMeta, hourSteps(10, 13, 2))
	b := mergePack(t, seg1, captureMeta, hourSteps(20, 23, 2))

	out, _, err := mergeBytes(t.Context(), inputsOf(a, b), mergeOpts())
	require.NoError(t, err)
	r := mustOpen(t, out, ReaderOptions{})
	require.Equal(t, uint16(2), r.Header().FormatMinor)
	assert.Equal(t, new("eqp"), r.Header().Meta.RecorderInstanceName)
	export, _, err := runExportJSONL(t, r)
	require.NoError(t, err)
	assert.True(t, strings.HasPrefix(export, `{"schema":"tracepack-jsonl/3",`))
	assert.Contains(t, export, `"recorder_instance_name":"eqp"`)

	minor1 := withMinor(t, b, 1)
	require.Nil(t, mustOpen(t, minor1, ReaderOptions{}).Header().Meta.RecorderInstanceName)
	for _, inputs := range [][]MergeInput{inputsOf(a, minor1), inputsOf(minor1, a)} {
		requireMergeUnfinalized(t, inputs, mergeOpts(), "capture-level tag recorder_instance_name differs", ErrMergeInput)
	}
}
