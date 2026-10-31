package tracepack

import (
	"bytes"
	"encoding/binary"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/arloliu/go-secs/tracepack/internal/tlv"
)

// editedMetaBytes returns m encoded, with its entry list changed by edit:
// pack metadata that MarshalBinary would refuse to write.
func editedMetaBytes(t testing.TB, m *PackMeta, edit func(list []tlv.Entry) []tlv.Entry) []byte {
	t.Helper()

	raw, err := m.MarshalBinary()
	require.NoError(t, err)
	list, err := tlv.Decode(raw)
	require.NoError(t, err)

	var out []byte
	for _, e := range edit(list) {
		out = tlv.AppendEntry(out, e)
	}

	return out
}

// richMeta gives m a value in every pack metadata field a caller can reach through a pointer or a slice.
func richMeta(m *PackMeta) {
	m.Classifiers, m.MaxFrameLens, m.SourceRefs = []string{"c1", "c2"}, []uint64{4096}, []string{"a.log", "b.log"}
	m.SourceTZ, m.SourceDialect, m.Notes = new("UTC"), new("dialect"), new("note")
	m.SiteID, m.EquipmentModel, m.EquipmentSWRev = new("fab"), new("model"), new("rev")
	m.HostSoftware, m.HostEndpoint, m.EquipmentEndpoint = new("eap"), new("10.0.0.1:5000"), new("10.0.0.2:5000")
	m.EquipmentConnectMode, m.DeviceID, m.PreviousCaptureID = new(SocketRolePassive), new(uint64(7)), new(UUID{0xCB})
	m.CaptureOriginUTCNs, m.CaptureOriginMonoNs = new(blockTestHour), new(int64(5))
	m.ClockStepToleranceNs, m.FlushIntervalNs = new(uint64(1000)), new(uint64(60_000_000_000))
	m.HSMSTimers = &HSMSTimers{T1Ms: new(uint64(1)), T3Ms: new(uint64(45_000)), T8Ms: new(uint64(8))}
	m.Supersedes = []UUID{{0xAB}}
	m.Coverage = []Coverage{{
		CaptureID: new(UUID{0xC0}), SeqFirst: new(uint64(1)), SeqLast: new(uint64(2)),
		TimeStart: new(blockTestHour), TimeEnd: new(blockTestHour + 5),
		Unknown: []RawEntry{{Tag: 0x0050, Type: 7, Value: []byte{9}}},
	}}
	m.RedactionPolicy = &RedactionPolicy{PolicyID: "p", PolicyVersion: 1, KeyID: "k", DigestAlgorithm: DigestAlgorithmHMACSHA256}
	m.Redaction = []RedactionEntry{{
		Seq: 1, ItemPath: new(""), MaskedRanges: []MaskedRange{{Offset: 0, Length: 2}},
		Domain: "d", Digest: bytes.Repeat([]byte{0xD1}, 32),
	}}
	m.Unknown = []RawEntry{{Tag: 0x8001, Type: 7, Value: []byte{1, 2}}}
}

// scrambleMeta changes, in place, every value m reaches through a pointer or a slice,
// as a caller holding a PackMeta may.
func scrambleMeta(m *PackMeta) {
	m.ToolID += "!"
	scrambleValues(
		[]*uint64{
			m.DeviceID, m.ClockStepToleranceNs, m.ReplacementSetSize, m.ReplacementSetIndex,
			m.FlushIntervalNs, m.ScopeGeneration, m.PublisherEpoch,
		},
		[]*UUID{m.ReplacementSetID, m.PatchBase, m.PreviousCaptureID},
		[]*string{
			m.SourceTZ, m.SourceDialect, m.Notes, m.ExtractFilter, m.SiteID, m.EquipmentModel,
			m.EquipmentSWRev, m.HostSoftware, m.HostEndpoint, m.EquipmentEndpoint,
		},
		[]*int64{m.CaptureOriginUTCNs, m.CaptureOriginMonoNs},
	)
	if tm := m.HSMSTimers; tm != nil {
		scrambleValues([]*uint64{tm.T1Ms, tm.T2Ms, tm.T3Ms, tm.T4Ms, tm.T5Ms, tm.T6Ms, tm.T7Ms, tm.T8Ms}, nil, nil, nil)
	}
	for i := range m.Coverage {
		c := &m.Coverage[i]
		scrambleValues([]*uint64{c.SeqFirst, c.SeqLast}, []*UUID{c.CaptureID}, nil, []*int64{c.TimeStart, c.TimeEnd})
		scrambleRaw(c.Unknown)
	}
	for i := range m.Redaction {
		r := &m.Redaction[i]
		r.Seq++
		r.Domain += "!"
		scrambleValues(nil, nil, []*string{r.ItemPath}, nil)
		for k := range r.MaskedRanges {
			r.MaskedRanges[k].Offset++
		}
		if len(r.Digest) > 0 {
			r.Digest[0]++
		}
	}
	if p := m.RedactionPolicy; p != nil {
		p.PolicyVersion++
	}
	if m.EquipmentConnectMode != nil {
		*m.EquipmentConnectMode++
	}
	for i := range m.Classifiers {
		m.Classifiers[i] += "!"
	}
	for i := range m.SourceRefs {
		m.SourceRefs[i] += "!"
	}
	for i := range m.MaxFrameLens {
		m.MaxFrameLens[i]++
	}
	scrambleValues(nil, slicePointers(m.Supersedes), nil, nil)
	scrambleValues(nil, slicePointers(m.CompactedFrom), nil, nil)
	scrambleRaw(m.Unknown)
}

// scrambleValues changes the value behind every non-nil pointer it is given.
func scrambleValues(counters []*uint64, ids []*UUID, texts []*string, times []*int64) {
	for _, p := range counters {
		if p != nil {
			*p++
		}
	}
	for _, p := range ids {
		if p != nil {
			p[0]++
		}
	}
	for _, p := range texts {
		if p != nil {
			*p += "!"
		}
	}
	for _, p := range times {
		if p != nil {
			*p++
		}
	}
}

// slicePointers returns a pointer to every element of s.
func slicePointers[T any](s []T) []*T {
	out := make([]*T, len(s))
	for i := range s {
		out[i] = &s[i]
	}

	return out
}

// scrambleRaw changes the first value byte of every entry of raw.
func scrambleRaw(raw []RawEntry) {
	for i := range raw {
		if len(raw[i].Value) > 0 {
			raw[i].Value[0]++
		}
	}
}

func TestUnmarshalPackMetaRejectsInvalidValues(t *testing.T) {
	t.Parallel()

	const tooLarge = uint64(1) << 63
	meta := func() *PackMeta {
		m := repairTestMeta()
		generationMeta(m, setA, 1, 1, nil)
		m.HSMSTimers = &HSMSTimers{T3Ms: new(uint64(45_000))}
		m.Coverage = []Coverage{{SeqFirst: new(uint64(1)), SeqLast: new(uint64(2))}}
		m.RedactionPolicy = &RedactionPolicy{PolicyID: "p", PolicyVersion: 1, KeyID: "k", DigestAlgorithm: DigestAlgorithmHMACSHA256}

		return m
	}
	badPolicyID := tlv.Entry{Tag: tagRedactionPolicyPolicyID, Type: tlv.TypeUTF8, Value: []byte{0xC3}}

	tests := []struct {
		name string
		edit func(t testing.TB, list []tlv.Entry) []tlv.Entry
		want error
	}{
		{"publisher_epoch above 2^63-1", func(t testing.TB, list []tlv.Entry) []tlv.Entry {
			return replaceEntry(t, list, tagPublisherEpoch, 0, tlv.U64Entry(tagPublisherEpoch, tooLarge))
		}, tlv.ErrLimit},
		{"scope_generation above 2^63-1", func(t testing.TB, list []tlv.Entry) []tlv.Entry {
			return replaceEntry(t, list, tagScopeGeneration, 0, tlv.U64Entry(tagScopeGeneration, tooLarge))
		}, tlv.ErrLimit},
		{"seq_start above 2^63-1", func(t testing.TB, list []tlv.Entry) []tlv.Entry {
			return replaceEntry(t, list, tagSeqStart, 0, tlv.U64Entry(tagSeqStart, tooLarge))
		}, tlv.ErrLimit},
		{"nested timer above 2^63-1", func(t testing.TB, list []tlv.Entry) []tlv.Entry {
			return withNestedAt(t, list, tagHSMSTimers, 0, 3, tlv.U64Entry(3, tooLarge))
		}, tlv.ErrLimit},
		{"nested coverage value above 2^63-1", func(t testing.TB, list []tlv.Entry) []tlv.Entry {
			return withNestedAt(t, list, tagCoverage, 0, tagCoverageSeqFirst, tlv.U64Entry(tagCoverageSeqFirst, tooLarge))
		}, tlv.ErrLimit},
		{"tool_id not valid UTF-8", func(t testing.TB, list []tlv.Entry) []tlv.Entry {
			return replaceEntry(t, list, tagToolID, 0, tlv.Entry{Tag: tagToolID, Type: tlv.TypeUTF8, Value: []byte{0xFF}})
		}, tlv.ErrUTF8},
		{"nested policy_id not valid UTF-8", func(t testing.TB, list []tlv.Entry) []tlv.Entry {
			return withNestedAt(t, list, tagRedactionPolicy, 0, tagRedactionPolicyPolicyID, badPolicyID)
		}, tlv.ErrUTF8},
		{"publisher_epoch twice", func(_ testing.TB, list []tlv.Entry) []tlv.Entry {
			return append(list, tlv.U64Entry(tagPublisherEpoch, 2))
		}, tlv.ErrDuplicate},
	}

	_, err := UnmarshalPackMeta(editedMetaBytes(t, meta(), func(list []tlv.Entry) []tlv.Entry { return list }))
	require.NoError(t, err, "the metadata is valid before an edit")

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()

			raw := editedMetaBytes(t, meta(), func(list []tlv.Entry) []tlv.Entry { return tt.edit(t, list) })
			m, err := UnmarshalPackMeta(raw)
			require.ErrorIs(t, err, tt.want)
			assert.Nil(t, m)
		})
	}
}

func TestUnmarshalPackMetaDecodesEveryPackRole(t *testing.T) {
	t.Parallel()

	retired := editedMetaBytes(t, repairTestMeta(), func(list []tlv.Entry) []tlv.Entry {
		return replaceEntry(t, list, tagPackRole, 0, tlv.U8Entry(tagPackRole, packRoleRetired))
	})
	unknown := editedMetaBytes(t, repairTestMeta(), func(list []tlv.Entry) []tlv.Entry {
		return replaceEntry(t, list, tagPackRole, 0, tlv.U8Entry(tagPackRole, 9))
	})
	repair := repairTestMeta()
	repair.PackRole, repair.Supersedes = PackRoleRepair, []UUID{seg0}
	repairRaw, err := repair.MarshalBinary()
	require.NoError(t, err)

	for _, tt := range []struct {
		role PackRole
		raw  []byte
	}{{packRoleRetired, retired}, {9, unknown}, {PackRoleRepair, repairRaw}} {
		m, err := UnmarshalPackMeta(tt.raw)
		require.NoError(t, err, "role %d", tt.role)
		assert.Equal(t, tt.role, m.PackRole)
	}
}

func TestHeaderUnaffectedByHeaderMutation(t *testing.T) {
	t.Parallel()

	p := writeRepairPack(t, CodecZstd, func(m *PackMeta) {
		archiveMeta(m)
		m.SiteID = new("fab")
		m.HSMSTimers = &HSMSTimers{T3Ms: new(uint64(45_000))}
	}, false, repairSteps(t))
	r := mustOpen(t, p.file, ReaderOptions{})
	want := r.Header()

	h := r.Header()
	*h.Meta.PublisherEpoch = 99
	h.Meta.ToolID = "changed"
	*h.Meta.SiteID = "changed"
	*h.Meta.HSMSTimers.T3Ms = 1
	h.Meta.CompactedFrom[0][0]++
	h.Meta.Coverage[0].Unknown[0].Value[0]++

	assert.Equal(t, want, r.Header())
}

func TestUnmarshalPackMetaSharesNoMemoryWithItsInput(t *testing.T) {
	t.Parallel()

	m := repairTestMeta()
	generationMeta(m, setA, 4, 2, []UUID{seg0})
	richMeta(m)
	raw, err := m.MarshalBinary()
	require.NoError(t, err)
	stored := bytes.Clone(raw)
	decoded, err := UnmarshalPackMeta(raw)
	require.NoError(t, err)
	want, err := UnmarshalPackMeta(stored)
	require.NoError(t, err)
	require.Equal(t, want, decoded)

	// The caller changes the PackMeta it was given: the bytes it decoded stay as they were.
	scrambleMeta(decoded)
	require.NotEqual(t, want, decoded)
	assert.Equal(t, stored, raw)

	// The caller reuses its buffer: a PackMeta decoded from it before stays as it was.
	again, err := UnmarshalPackMeta(raw)
	require.NoError(t, err)
	overwriteMetaValue(t, raw, tagPublisherEpoch, binary.LittleEndian.AppendUint64(nil, 9))
	changed, err := UnmarshalPackMeta(raw)
	require.NoError(t, err)
	require.Equal(t, uint64(9), *changed.PublisherEpoch, "the buffer holds another epoch now")
	assert.Equal(t, want, again)
}
