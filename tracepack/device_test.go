package tracepack

import (
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/arloliu/go-secs/tracepack/internal/format"
	"github.com/arloliu/go-secs/tracepack/internal/tlv"
)

// Tag 0x0035 is device only in a pack of format 1.2 or later (the tracepack format specification §5 and §14):
// below minor 2 it stays unknown whatever its value type or value, a repeated entry included;
// from minor 2 a wrong type, invalid UTF-8 or a repeated entry rejects the metadata.
func TestDeviceByMinor(t *testing.T) {
	t.Parallel()

	valid := tlv.UTF8Entry(tagDevice, "main")
	entries := map[string][]tlv.Entry{
		"utf8":         {valid},
		"bytes":        {tlv.BytesEntry(tagDevice, []byte{0xFF, 0x00})},
		"invalid utf8": {{Tag: tagDevice, Type: tlv.TypeUTF8, Value: []byte{0xFF}}},
		"repeated":     {valid, tlv.UTF8Entry(tagDevice, "sensor")},
	}
	for name, es := range entries {
		raw := jsonlTLV(append(jsonlV2Required(), es...)...)
		want := make([]RawEntry, 0, len(es))
		for _, e := range es {
			want = append(want, RawEntry{Tag: e.Tag, Type: uint8(e.Type), Value: e.Value})
		}
		for _, minor := range []uint16{0, 1} {
			m, err := UnmarshalPackMetaMinor(raw, minor)
			require.NoError(t, err, "%s at minor %d", name, minor)
			assert.Nil(t, m.Device, "%s at minor %d", name, minor)
			assert.Equal(t, want, m.Unknown, "%s at minor %d", name, minor)
		}

		m, err := UnmarshalPackMetaMinor(raw, 2)
		if name == "utf8" {
			require.NoError(t, err)
			assert.Equal(t, new("main"), m.Device)
			assert.Empty(t, m.Unknown)

			continue
		}
		require.ErrorIs(t, err, ErrCorrupt, "%s at minor 2", name)
	}

	// The export names the tag only under the schema of its minor.
	raw := jsonlTLV(append(jsonlV2Required(), valid)...)
	for minor, want := range map[uint16]string{
		0: jsonlV2HeaderStart("1", 0) + jsonlV2RequiredJSON + `,"unknown":[{"tag":53,"value_type":6,"value":"bWFpbg=="}]}}` + "\n",
		1: jsonlV2HeaderStart("2", 1) + jsonlV2RequiredJSON + `,"unknown":[{"tag":53,"value_type":6,"value":"bWFpbg=="}]}}` + "\n",
		2: jsonlV2HeaderStart("3", 2) + jsonlV2RequiredJSON + `,"device":"main"}}` + "\n",
	} {
		r := jsonlTestReader(t, format.FileHeader{FormatMajor: 1, FormatMinor: minor}, raw)
		out, _, err := runExportJSONL(t, r)
		require.NoError(t, err)
		assert.Equal(t, want, out, "minor %d", minor)
	}
}

// A present empty device is kept apart from an absent one, through the pack metadata's encoding.
func TestDevicePresentEmpty(t *testing.T) {
	t.Parallel()

	for _, d := range []*string{nil, new(""), new("load port 2")} {
		m := repairTestMeta()
		m.Device = d
		b, err := m.MarshalBinary()
		require.NoError(t, err)
		got, err := UnmarshalPackMetaMinor(b, 2)
		require.NoError(t, err)
		assert.Equal(t, d, got.Device)
	}
}

// device alone, without recorder_instance_name, makes a pack format 1.2, for a Writer and for WriteClose.
func TestDeviceFormatMinor(t *testing.T) {
	t.Parallel()

	for _, d := range []string{"main", ""} {
		m := repairTestMeta()
		m.Device = new(d)
		require.Nil(t, m.RecorderInstanceName)
		assert.Equal(t, uint16(2), formatMinorOf(m), "device %q", d)
	}

	file := mergePack(t, seg0, func(m *PackMeta) { m.Device = new("main") }, hourSteps(10, 13, 2))
	h := mustOpen(t, file, ReaderOptions{}).Header()
	assert.Equal(t, uint16(2), h.FormatMinor)
	assert.Nil(t, h.Meta.RecorderInstanceName)
	assert.Equal(t, new("main"), h.Meta.Device)

	highest := int64(5_000)
	m := closeMeta(1000, false, false)
	m.Device = new("main")
	given := *m
	closed := writeCloseBytes(t, closeOptions(m, &highest))
	assert.Equal(t, given, *m, "WriteClose does not change the caller's metadata")
	ch := mustOpen(t, closed, ReaderOptions{}).Header()
	assert.Equal(t, uint16(2), ch.FormatMinor)
	assert.Equal(t, new("main"), ch.Meta.Device)
}

// A merge carries device as a capture-level tag:
// equal devices merge into a format 1.2 archive that carries it, and so do two inputs without one;
// a different device, a present device beside an absent one, and a minor-1 input whose 0x0035 is an unknown entry
// are refused whatever the order of the inputs.
func TestMergeDevice(t *testing.T) {
	t.Parallel()

	with := func(d *string) func(m *PackMeta) {
		return func(m *PackMeta) {
			captureMeta(m)
			m.Device = d
		}
	}
	a := mergePack(t, seg0, with(new("main")), hourSteps(10, 13, 2))
	b := mergePack(t, seg1, with(new("main")), hourSteps(20, 23, 2))

	out, _, err := mergeBytes(t.Context(), inputsOf(a, b), mergeOpts())
	require.NoError(t, err)
	r := mustOpen(t, out, ReaderOptions{})
	require.Equal(t, uint16(2), r.Header().FormatMinor)
	assert.Equal(t, new("main"), r.Header().Meta.Device)
	export, _, err := runExportJSONL(t, r)
	require.NoError(t, err)
	assert.True(t, strings.HasPrefix(export, `{"schema":"tracepack-jsonl/3",`))
	assert.Contains(t, export, `"device":"main"`)

	noneA := mergePack(t, seg0, with(nil), hourSteps(10, 13, 2))
	noneB := mergePack(t, seg1, with(nil), hourSteps(20, 23, 2))
	out, _, err = mergeBytes(t.Context(), inputsOf(noneA, noneB), mergeOpts())
	require.NoError(t, err)
	assert.Nil(t, mustOpen(t, out, ReaderOptions{}).Header().Meta.Device)

	other := mergePack(t, seg1, with(new("sensor")), hourSteps(20, 23, 2))
	empty := mergePack(t, seg1, with(new("")), hourSteps(20, 23, 2))
	minor1 := withMinor(t, b, 1)
	require.Nil(t, mustOpen(t, minor1, ReaderOptions{}).Header().Meta.Device)
	for _, x := range [][]byte{other, noneB, empty} {
		for _, inputs := range [][]MergeInput{inputsOf(a, x), inputsOf(x, a)} {
			requireMergeRefused(t, inputs, mergeOpts(), "capture-level tag device differs", ErrMergeInput)
		}
	}
	for _, inputs := range [][]MergeInput{inputsOf(a, minor1), inputsOf(minor1, a)} {
		requireMergeUnfinalized(t, inputs, mergeOpts(), "differs", ErrMergeInput)
	}
}

// device, of format 1.2, is a producer-owned value of the capture descriptor:
// it round-trips, the empty string included, and its entry follows recorder_instance_name in tag order.
func TestDescriptorDevice(t *testing.T) {
	t.Parallel()

	for _, d := range []string{"main", "", "load port/2"} {
		m := descriptorMeta()
		m.Device = new(d)
		b, err := EncodeDescriptor(m)
		require.NoError(t, err)

		got, err := DecodeDescriptor(b)
		require.NoError(t, err)
		assert.Equal(t, m, got, "device %q", d)

		entries, err := tlv.Decode(b)
		require.NoError(t, err)
		last := entries[len(entries)-1]
		assert.Equal(t, tagDevice, last.Tag, "device %q", d)
		assert.Equal(t, tlv.TypeUTF8, last.Type, "device %q", d)
	}

	m := descriptorMeta()
	m.Device = new("\xff")
	b, err := EncodeDescriptor(m)
	var fe *FieldError
	require.ErrorAs(t, err, &fe)
	assert.Equal(t, "device", fe.Field)
	assert.Nil(t, b)
}
