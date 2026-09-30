package tracepack

import (
	"bytes"
	"encoding/binary"
	"maps"
	"math"
	"slices"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/arloliu/go-secs/tracepack/internal/tlv"
)

// viewCapture is the capture of the view test scopes, whose UTC hour is the one of blockTestHour.
var viewCapture = UUID{0xCA, 0x97}

// Pack and replacement-set ids of the view test scopes:
// segments seg*, the members mem* of the replacement sets set*, and patches pat*.
var (
	seg0, seg1, seg2, seg3 = UUID{0x50}, UUID{0x51}, UUID{0x52}, UUID{0x53}
	setA, setB, setC, setD = UUID{0x5E, 0xA}, UUID{0x5E, 0xB}, UUID{0x5E, 0xC}, UUID{0x5E, 0xD}
	memA, memB, memC, memD = UUID{0xA0, 0xA}, UUID{0xA0, 0xB}, UUID{0xA0, 0xC}, UUID{0xA0, 0xD}
	patP1, patP2, patQ1    = UUID{0xB0, 1}, UUID{0xB0, 2}, UUID{0xB1, 1}
	patQ2                  = UUID{0xB1, 2}
	// setGone names a generation of which no pack survives.
	setGone = UUID{0x5E, 0xF}
)

// viewInfo returns the PackInfo of pack id of viewCapture, whose pack metadata is repairTestMeta changed by edit,
// encoded and then decoded by NewPackInfo, as a catalog holding the bytes would.
func viewInfo(t testing.TB, id UUID, edit func(m *PackMeta)) PackInfo {
	t.Helper()

	m := repairTestMeta()
	if edit != nil {
		edit(m)
	}
	raw, err := m.MarshalBinary()
	require.NoError(t, err)
	p, err := NewPackInfo(id, viewCapture, raw)
	require.NoError(t, err)

	return p
}

// segmentInfo returns segment id of the view test scope.
func segmentInfo(t testing.TB, id UUID) PackInfo {
	t.Helper()

	return viewInfo(t, id, nil)
}

// memberInfo returns archive id, the member of generation set, ranked (epoch, gen), whose compacted_from is from.
func memberInfo(t testing.TB, id, set UUID, epoch, gen uint64, from ...UUID) PackInfo {
	t.Helper()

	return viewInfo(t, id, func(m *PackMeta) { generationMeta(m, set, epoch, gen, from) })
}

// generationMeta makes m the pack metadata of the member of generation set, ranked (epoch, gen), whose compacted_from is from.
func generationMeta(m *PackMeta, set UUID, epoch, gen uint64, from []UUID) {
	m.PackRole = PackRoleArchive
	m.ScopeGeneration, m.PublisherEpoch = new(gen), new(epoch)
	m.ReplacementSetID, m.ReplacementSetSize, m.ReplacementSetIndex = new(set), new(uint64(1)), new(uint64(0))
	m.CompactionLevel, m.CompactedFrom = 1, from
}

// patchInfo returns patch id, registered against the generation base names (none when nil), replacing supersedes.
func patchInfo(t testing.TB, id UUID, base *UUID, supersedes ...UUID) PackInfo {
	t.Helper()

	return viewInfo(t, id, func(m *PackMeta) {
		m.PackRole = PackRoleRepair
		m.PatchBase, m.Supersedes = base, supersedes
	})
}

// retiredRoleInfo returns pack id of the view test scope with the retired pack_role 5,
// whose metadata only a reader accepts: MarshalBinary refuses to write it.
func retiredRoleInfo(t testing.TB, id UUID) PackInfo {
	t.Helper()

	raw := editedMetaBytes(t, repairTestMeta(), func(list []tlv.Entry) []tlv.Entry {
		return replaceEntry(t, list, tagPackRole, 0, tlv.U8Entry(tagPackRole, packRoleRetired))
	})
	p, err := NewPackInfo(id, viewCapture, raw)
	require.NoError(t, err)

	return p
}

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

// overwriteMetaValue overwrites, in place, the value of the first entry of tag in raw, pack metadata bytes.
func overwriteMetaValue(t testing.TB, raw []byte, tag uint16, value []byte) {
	t.Helper()

	list, err := tlv.Decode(raw)
	require.NoError(t, err)
	e := nthEntry(t, list, tag, 0)
	require.Len(t, value, len(e.Value))
	// e.Value aliases raw.
	copy(e.Value, value)
}

// commitSet returns a CommitSet holding ids.
func commitSet(ids ...UUID) CommitSet {
	c := make(CommitSet, len(ids))
	for _, id := range ids {
		c[id] = struct{}{}
	}

	return c
}

// commitOf returns the id of the commit object that commits p:
// an archive's replacement_set_id, or a patch's pack_id; false for any other pack, which has none.
func commitOf(p PackInfo) (UUID, bool) {
	if p.meta.PackRole == PackRoleArchive {
		return *p.meta.ReplacementSetID, true
	}
	if p.meta.PackRole == PackRoleRepair {
		return p.packID, true
	}

	return UUID{}, false
}

// permutations returns every order of ids.
func permutations(ids []UUID) [][]UUID {
	if len(ids) <= 1 {
		return [][]UUID{slices.Clone(ids)}
	}

	out := make([][]UUID, 0, len(ids)*len(ids))
	for i := range ids {
		for _, rest := range permutations(slices.Concat(ids[:i], ids[i+1:])) {
			out = append(out, append([]UUID{ids[i]}, rest...))
		}
	}

	return out
}

// requireView requires the view of packs and commits to be want, in the capture and hour of the view test scopes,
// and the same view, its slices in another order, for every rotation of packs and the reverse of each;
// then it checks every state that deleting the deletable packs can reach with requireDeletionsKeepView.
func requireView(t *testing.T, want View, packs []PackInfo, commits CommitSet) {
	t.Helper()

	want.Capture, want.Hour = viewCapture, hourOf(blockTestHour)
	got, err := ActiveView(packs, commits)
	require.NoError(t, err)
	require.Equal(t, want, got)

	for k := range packs {
		rotated := slices.Concat(packs[k:], packs[:k])
		reversed := slices.Clone(rotated)
		slices.Reverse(reversed)
		for _, order := range [][]PackInfo{rotated, reversed} {
			got, err := ActiveView(order, commits)
			require.NoError(t, err, "rotation %d", k)
			requireSameView(t, want, got, "rotation %d", k)
		}
	}

	requireDeletionsKeepView(t, want, packs, commits)
}

// requireSameView requires got to be want up to the order of its slices, which follows the order of ActiveView's packs.
func requireSameView(t *testing.T, want, got View, msgAndArgs ...any) {
	t.Helper()

	require.Equal(t, want.Capture, got.Capture, msgAndArgs...)
	require.Equal(t, want.Hour, got.Hour, msgAndArgs...)
	require.Equal(t, want.Generation, got.Generation, msgAndArgs...)
	require.ElementsMatch(t, want.Packs, got.Packs, msgAndArgs...)
	require.ElementsMatch(t, want.CompactedFrom, got.CompactedFrom, msgAndArgs...)
	require.ElementsMatch(t, want.Deletable, got.Deletable, msgAndArgs...)
	require.ElementsMatch(t, want.Excluded, got.Excluded, msgAndArgs...)
}

// requireDeletionsKeepView checks every state that deleting the deletable packs of want, the view of packs and commits,
// can reach in any order that deletes each commit object after the pack it commits:
// each deletable pack is kept, deleted, or deleted together with its commit object.
// In every state the view recomputed over the surviving packs and commit objects must be want,
// less the deleted packs in Deletable;
// once everything is deleted, a rebuild from the survivors in reverse order must give the same view with nothing deletable.
func requireDeletionsKeepView(t *testing.T, want View, packs []PackInfo, commits CommitSet) {
	t.Helper()

	require.LessOrEqual(t, len(want.Deletable), 8, "the states of more packs take too long")
	requireSafeDeletions(t, want)

	// states[i] is the state of the i-th pack of want.Deletable:
	// 0 kept, 1 deleted, 2 deleted with its commit object; last[i] is the furthest state it has.
	states, last := make([]int, len(want.Deletable)), make([]int, len(want.Deletable))
	for i, id := range want.Deletable {
		last[i] = 1
		p := packs[slices.IndexFunc(packs, func(p PackInfo) bool { return p.packID == id })]
		if c, ok := commitOf(p); ok {
			if _, committed := commits[c]; committed {
				last[i] = 2
			}
		}
	}

	for {
		live, objs, kept := deletionState(want.Deletable, states, packs, commits)
		step := want
		step.Deletable = kept
		got, err := ActiveView(live, objs)
		require.NoError(t, err, "states %v of %v", states, want.Deletable)
		require.Equal(t, step, got, "states %v of %v", states, want.Deletable)
		requireSafeDeletions(t, got)
		if !nextState(states, last) {
			break
		}
	}

	live, objs, _ := deletionState(want.Deletable, last, packs, commits)
	slices.Reverse(live)
	got, err := ActiveView(live, objs)
	require.NoError(t, err)
	rebuilt := want
	rebuilt.Deletable = nil
	requireSameView(t, rebuilt, got, "rebuild")
}

// deletionState returns what survives when the i-th pack of deletable is in states[i]
// (0 kept, 1 deleted, 2 deleted with its commit object):
// the surviving packs in the order of packs, the surviving commit objects,
// and the kept packs of deletable, in order; nil when none is kept, as View's slices are.
func deletionState(deletable []UUID, states []int, packs []PackInfo, commits CommitSet) ([]PackInfo, CommitSet, []UUID) {
	state := make(map[UUID]int, len(deletable))
	var kept []UUID
	for i, id := range deletable {
		state[id] = states[i]
		if states[i] == 0 {
			kept = append(kept, id)
		}
	}

	live, objs := make([]PackInfo, 0, len(packs)), maps.Clone(commits)
	for _, p := range packs {
		s := state[p.packID]
		if s == 0 {
			live = append(live, p)
		}
		if c, ok := commitOf(p); ok && s == 2 {
			delete(objs, c)
		}
	}

	return live, objs, kept
}

// nextState advances states to the next combination with states[i] at most last[i], as an odometer does;
// it returns false, with every state back at 0, after the last combination.
func nextState(states, last []int) bool {
	for i := range states {
		if states[i] < last[i] {
			states[i]++

			return true
		}
		states[i] = 0
	}

	return false
}

// requireSafeDeletions requires that no pack v.Deletable lists is in v.Packs, is G's member, or is in v.Excluded.
func requireSafeDeletions(t *testing.T, v View) {
	t.Helper()

	for _, id := range v.Deletable {
		require.NotContains(t, v.Packs, id)
		require.NotContains(t, v.Excluded, id)
		if v.Generation != nil {
			require.NotEqual(t, v.Generation.Member, id)
		}
	}
}

// patchChainScope returns a scope whose generation A is current:
// its member memA lists seg0, already deleted, in compacted_from;
// segment seg1 is replaced by patch patP1, itself replaced by patP2,
// and memA by patQ1, itself replaced by patQ2, every patch committed against A.
// With folded, the scope also holds generation B, the merge that folds them.
func patchChainScope(t testing.TB, folded bool) ([]PackInfo, CommitSet) {
	t.Helper()

	packs := []PackInfo{
		memberInfo(t, memA, setA, 1, 1, seg0),
		segmentInfo(t, seg1),
		patchInfo(t, patP1, new(setA), seg1),
		patchInfo(t, patP2, new(setA), patP1),
		patchInfo(t, patQ1, new(setA), memA),
		patchInfo(t, patQ2, new(setA), patQ1),
	}
	commits := commitSet(setA, patP1, patP2, patQ1, patQ2)
	if folded {
		packs = append(packs, viewInfo(t, memB, func(m *PackMeta) {
			generationMeta(m, setB, 1, 2, []UUID{seg0, seg1, patP1, patP2, patQ1, patQ2})
			m.Supersedes = []UUID{memA}
		}))
		commits[setB] = struct{}{}
	}

	return packs, commits
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
	m.HSMSTimers = &HSMSTimers{T1: new(uint64(1)), T3: new(uint64(45_000)), T8: new(uint64(8))}
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
		scrambleValues([]*uint64{tm.T1, tm.T2, tm.T3, tm.T4, tm.T5, tm.T6, tm.T7, tm.T8}, nil, nil, nil)
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

func TestNewPackInfoRejectsWhatTheDecoderRejects(t *testing.T) {
	t.Parallel()

	const tooLarge = uint64(1) << 63
	meta := func() *PackMeta {
		m := repairTestMeta()
		generationMeta(m, setA, 1, 1, nil)
		m.HSMSTimers = &HSMSTimers{T3: new(uint64(45_000))}
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

	_, err := NewPackInfo(memA, viewCapture, editedMetaBytes(t, meta(), func(list []tlv.Entry) []tlv.Entry { return list }))
	require.NoError(t, err, "the metadata is valid before an edit")

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()

			raw := editedMetaBytes(t, meta(), func(list []tlv.Entry) []tlv.Entry { return tt.edit(t, list) })
			_, decodeErr := UnmarshalPackMeta(raw)
			require.ErrorIs(t, decodeErr, tt.want)

			p, err := NewPackInfo(memA, viewCapture, raw)
			require.ErrorIs(t, err, tt.want)
			assert.EqualError(t, err, decodeErr.Error())
			assert.Equal(t, PackInfo{}, p)
		})
	}
}

func TestActiveViewExcludesRolesOutsideTheTiers(t *testing.T) {
	t.Parallel()

	retired, unknown, extract := UUID{0xE5}, UUID{0xE9}, UUID{0xE3}
	retiredPack := retiredRoleInfo(t, retired)
	require.Equal(t, PackRole(packRoleRetired), retiredPack.Meta().PackRole, "the retired role decodes")
	unknownPack := viewInfo(t, unknown, func(m *PackMeta) { m.PackRole = 9 })
	require.Equal(t, PackRole(9), unknownPack.Meta().PackRole, "an unknown role decodes")

	packs := []PackInfo{
		segmentInfo(t, seg0),
		retiredPack,
		// G's compacted_from names the retired pack too, which still never becomes deletable.
		memberInfo(t, memA, setA, 1, 1, seg0, retired),
		unknownPack,
		viewInfo(t, extract, func(m *PackMeta) {
			m.PackRole, m.ExtractFilter, m.ScopeGeneration = PackRoleExtract, new("seq 1-2"), nil
		}),
		segmentInfo(t, seg1),
	}
	requireView(t, View{
		Generation:    &Generation{ReplacementSetID: setA, PublisherEpoch: 1, ScopeGeneration: 1, Member: memA},
		Packs:         []UUID{memA, seg1},
		CompactedFrom: []UUID{seg0, retired, seg1},
		Deletable:     []UUID{seg0},
		Excluded:      []UUID{retired, unknown, extract},
	}, packs, commitSet(setA, retired, unknown, extract))
}

func TestReaderInfoUnaffectedByHeaderMutation(t *testing.T) {
	t.Parallel()

	p := writeRepairPack(t, CodecZstd, func(m *PackMeta) {
		archiveMeta(m)
		m.SiteID = new("fab")
		m.HSMSTimers = &HSMSTimers{T3: new(uint64(45_000))}
	}, false, repairSteps(t))
	r := mustOpen(t, p.file, ReaderOptions{})
	want := r.Header().Meta
	before := r.Info()

	h := r.Header()
	*h.Meta.PublisherEpoch = 99
	h.Meta.ToolID = "changed"
	*h.Meta.SiteID = "changed"
	*h.Meta.HSMSTimers.T3 = 1
	h.Meta.CompactedFrom[0][0]++
	h.Meta.Coverage[0].Unknown[0].Value[0]++

	hdr := layoutHeader(t, p.file)
	for _, info := range []PackInfo{before, r.Info()} {
		assert.Equal(t, want, info.Meta())
		assert.Equal(t, UUID(hdr.PackID), info.PackID())
		assert.Equal(t, UUID(hdr.CaptureID), info.CaptureID())

		v, err := ActiveView([]PackInfo{info}, commitSet(*want.ReplacementSetID))
		require.NoError(t, err)
		assert.Equal(t, &Generation{ReplacementSetID: *want.ReplacementSetID, PublisherEpoch: 3, ScopeGeneration: 2, Member: info.PackID()}, v.Generation)
		assert.Equal(t, want.CompactedFrom, v.CompactedFrom)
	}
}

func TestPackInfoUnaffectedByCallerMutation(t *testing.T) {
	t.Parallel()

	memberMeta := repairTestMeta()
	generationMeta(memberMeta, setA, 4, 2, []UUID{seg0})
	richMeta(memberMeta)
	memberRaw, err := memberMeta.MarshalBinary()
	require.NoError(t, err)
	member, err := NewPackInfo(memA, viewCapture, memberRaw)
	require.NoError(t, err)

	pm := repairTestMeta()
	pm.PackRole, pm.PatchBase, pm.Supersedes = PackRoleRepair, new(setA), []UUID{memA}
	patchRaw, err := pm.MarshalBinary()
	require.NoError(t, err)
	patch, err := NewPackInfo(patP1, viewCapture, patchRaw)
	require.NoError(t, err)

	packs := []PackInfo{segmentInfo(t, seg1), member, patch}
	commits := commitSet(setA, patP1)
	wantMember, wantPatch := member.Meta(), patch.Meta()
	wantView, err := ActiveView(packs, commits)
	require.NoError(t, err)
	require.Equal(t, []UUID{seg1, patP1}, wantView.Packs, "the patch replaces the member")

	// The caller reuses the buffers it passed to NewPackInfo.
	overwriteMetaValue(t, memberRaw, tagPublisherEpoch, binary.LittleEndian.AppendUint64(nil, 9))
	overwriteMetaValue(t, patchRaw, tagPatchBase, setB[:])
	overwriteMetaValue(t, patchRaw, tagSupersedes, seg1[:])
	changed, err := UnmarshalPackMeta(memberRaw)
	require.NoError(t, err)
	require.Equal(t, uint64(9), *changed.PublisherEpoch, "the buffer holds another epoch now")

	// It also changes the PackMeta values Meta returned.
	for _, info := range []PackInfo{member, patch} {
		m := info.Meta()
		want := info.Meta()
		scrambleMeta(m)
		require.NotEqual(t, want, m)
	}

	assert.Equal(t, wantMember, member.Meta())
	assert.Equal(t, wantPatch, patch.Meta())
	assert.NotSame(t, member.Meta(), member.Meta(), "every call decodes a new PackMeta")
	got, err := ActiveView(packs, commits)
	require.NoError(t, err)
	assert.Equal(t, wantView, got)

	var zero PackInfo
	assert.Nil(t, zero.Meta())
}

func TestActiveViewRejectsInvalidScopes(t *testing.T) {
	t.Parallel()

	h := blockTestHour
	period := func(id UUID, start, end int64) func(t *testing.T) PackInfo {
		return func(t *testing.T) PackInfo {
			return viewInfo(t, id, func(m *PackMeta) { m.PeriodStart, m.PeriodEnd = start, end })
		}
	}
	otherCapture := func(t *testing.T) PackInfo {
		raw, err := repairTestMeta().MarshalBinary()
		require.NoError(t, err)
		p, err := NewPackInfo(seg1, UUID{0xCA, 0xFF}, raw)
		require.NoError(t, err)

		return p
	}
	segment := func(id UUID) func(t *testing.T) PackInfo {
		return func(t *testing.T) PackInfo { return segmentInfo(t, id) }
	}
	member := func(id, set UUID, epoch, gen uint64) func(t *testing.T) PackInfo {
		return func(t *testing.T) PackInfo { return memberInfo(t, id, set, epoch, gen) }
	}
	atGeneration := func(role PackRole, gen uint64) func(t *testing.T) PackInfo {
		return func(t *testing.T) PackInfo {
			return viewInfo(t, seg1, func(m *PackMeta) {
				generationMeta(m, setB, 1, gen, nil)
				m.PackRole, m.CompactionLevel, m.Supersedes = role, 0, []UUID{seg0}
			})
		}
	}

	tests := []struct {
		name  string
		packs []func(t *testing.T) PackInfo
		msg   string
	}{
		{"no packs", nil, "no packs"},
		{"a zero PackInfo", []func(t *testing.T) PackInfo{segment(seg0), func(*testing.T) PackInfo { return PackInfo{} }}, "holds no pack metadata"},
		{"an empty period", []func(t *testing.T) PackInfo{segment(seg0), period(seg1, h+10, h+10)}, "is not inside one UTC hour"},
		{"a reversed period", []func(t *testing.T) PackInfo{period(seg1, h+10, h+5)}, "is not inside one UTC hour"},
		{"a period across two hours", []func(t *testing.T) PackInfo{period(seg1, h+10, h+hourNs+1)}, "is not inside one UTC hour"},
		{"a malformed period outside the tiers", []func(t *testing.T) PackInfo{segment(seg0), func(t *testing.T) PackInfo {
			return viewInfo(t, seg1, func(m *PackMeta) { m.PackRole, m.PeriodEnd = 9, m.PeriodStart })
		}}, "is not inside one UTC hour"},
		{"another hour", []func(t *testing.T) PackInfo{segment(seg0), period(seg1, h+hourNs, h+2*hourNs)}, "not the scope's"},
		{"another capture", []func(t *testing.T) PackInfo{segment(seg0), otherCapture}, "not the scope's"},
		{"a repeated pack_id", []func(t *testing.T) PackInfo{segment(seg0), segment(seg0)}, "pack_id appears more than once"},
		{"one set at equal ranks", []func(t *testing.T) PackInfo{member(memA, setA, 1, 1), member(memB, setA, 1, 1)}, "is also held by pack"},
		{"one set at unequal ranks", []func(t *testing.T) PackInfo{member(memA, setA, 1, 1), member(memB, setA, 2, 5)}, "is also held by pack"},
		{"a segment at generation 1", []func(t *testing.T) PackInfo{atGeneration(PackRoleSegment, 1)}, "pack_role segment at scope_generation 1"},
		{"a repair at generation 2", []func(t *testing.T) PackInfo{atGeneration(PackRoleRepair, 2)}, "pack_role repair at scope_generation 2"},
		{"an archive at generation 0", []func(t *testing.T) PackInfo{func(t *testing.T) PackInfo {
			return viewInfo(t, memA, func(m *PackMeta) { m.PackRole = PackRoleArchive })
		}}, "pack_role archive at scope_generation 0"},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()

			var packs []PackInfo
			for _, build := range tt.packs {
				packs = append(packs, build(t))
			}
			v, err := ActiveView(packs, commitSet(setA, setB))
			require.ErrorContains(t, err, tt.msg)
			require.NotErrorIs(t, err, ErrViewConflicted)
			assert.Equal(t, View{}, v)
		})
	}
}

func TestActiveViewHourOfPeriod(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name       string
		start, end int64
		hour       int64
	}{
		{"a whole hour", blockTestHour, blockTestHour + hourNs, hourOf(blockTestHour)},
		{"the hour before 1970", -hourNs, 0, -1},
		{"one nanosecond before 1970", -1, 0, -1},
		{"the lowest time", math.MinInt64, math.MinInt64 + 1, hourOf(math.MinInt64)},
		{"the highest time", math.MaxInt64 - 1, math.MaxInt64, hourOf(math.MaxInt64 - 1)},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()

			p := viewInfo(t, seg1, func(m *PackMeta) { m.PeriodStart, m.PeriodEnd = tt.start, tt.end })
			v, err := ActiveView([]PackInfo{p}, nil)
			require.NoError(t, err)
			assert.Equal(t, tt.hour, v.Hour)
			assert.Equal(t, []UUID{seg1}, v.Packs)
		})
	}
}

func TestActiveViewTellsAbsentPatchBaseFromNilUUID(t *testing.T) {
	t.Parallel()

	nilUUID := UUID{}
	patches := func(t *testing.T) []PackInfo {
		return []PackInfo{
			segmentInfo(t, seg1),
			segmentInfo(t, seg2),
			patchInfo(t, patP1, nil, seg1),
			patchInfo(t, patQ1, new(nilUUID), seg2),
		}
	}
	commits := commitSet(nilUUID, setA, patP1, patQ1)

	t.Run("without a generation", func(t *testing.T) {
		t.Parallel()

		// Only the patch without patch_base is based on the absent generation.
		requireView(t, View{
			Packs:         []UUID{seg2, patP1},
			CompactedFrom: []UUID{seg1, seg2, patP1},
		}, patches(t), commits)
	})
	t.Run("under a generation whose set is the nil UUID", func(t *testing.T) {
		t.Parallel()

		// Only the patch whose patch_base is the nil UUID is based on it; the other ranks below it.
		requireView(t, View{
			Generation:    &Generation{ReplacementSetID: nilUUID, PublisherEpoch: 1, ScopeGeneration: 1, Member: memA},
			Packs:         []UUID{memA, seg1, patQ1},
			CompactedFrom: []UUID{seg1, seg2, patQ1},
			Deletable:     []UUID{patP1},
		}, append(patches(t), memberInfo(t, memA, nilUUID, 1, 1)), commits)
	})
	t.Run("under another generation", func(t *testing.T) {
		t.Parallel()

		requireView(t, View{
			Generation:    &Generation{ReplacementSetID: setA, PublisherEpoch: 1, ScopeGeneration: 1, Member: memA},
			Packs:         []UUID{memA, seg1, seg2},
			CompactedFrom: []UUID{seg1, seg2},
			Deletable:     []UUID{patP1, patQ1},
		}, append(patches(t), memberInfo(t, memA, setA, 1, 1)), commits)
	})
}

func TestActiveViewVectors(t *testing.T) {
	t.Parallel()

	genA := &Generation{ReplacementSetID: setA, PublisherEpoch: 1, ScopeGeneration: 1, Member: memA}
	genB := &Generation{ReplacementSetID: setB, PublisherEpoch: 1, ScopeGeneration: 2, Member: memB}
	onA := new(setA)
	successor := func(t testing.TB, id, set UUID, epoch, gen uint64, prev UUID, from ...UUID) PackInfo {
		return viewInfo(t, id, func(m *PackMeta) {
			generationMeta(m, set, epoch, gen, from)
			m.Supersedes = []UUID{prev}
		})
	}

	tests := []struct {
		name  string
		scope func(t *testing.T) ([]PackInfo, CommitSet)
		want  View
	}{
		{
			name: "no generation, a patch chain without patch_base",
			scope: func(t *testing.T) ([]PackInfo, CommitSet) {
				return []PackInfo{
					segmentInfo(t, seg1), segmentInfo(t, seg2),
					patchInfo(t, patP1, nil, seg1), patchInfo(t, patP2, nil, patP1),
					// Uncommitted: outside the view, and not deletable without a generation.
					patchInfo(t, patQ1, nil, seg2),
				}, commitSet(patP1, patP2)
			},
			want: View{Packs: []UUID{seg2, patP2}, CompactedFrom: []UUID{seg1, seg2, patP1, patP2}},
		},
		{
			name: "a generation with segments",
			scope: func(t *testing.T) ([]PackInfo, CommitSet) {
				return []PackInfo{
					segmentInfo(t, seg1), segmentInfo(t, seg2), memberInfo(t, memA, setA, 1, 1, seg1, seg2), segmentInfo(t, seg3),
				}, commitSet(setA)
			},
			want: View{Generation: genA, Packs: []UUID{memA, seg3}, CompactedFrom: []UUID{seg1, seg2, seg3}, Deletable: []UUID{seg1, seg2}},
		},
		{
			name: "patches replacing a segment and the member",
			scope: func(t *testing.T) ([]PackInfo, CommitSet) {
				return []PackInfo{
					memberInfo(t, memA, setA, 1, 1, seg1), segmentInfo(t, seg1), segmentInfo(t, seg2),
					patchInfo(t, patP1, onA, seg2), patchInfo(t, patQ1, onA, memA),
				}, commitSet(setA, patP1, patQ1)
			},
			want: View{Generation: genA, Packs: []UUID{patP1, patQ1}, CompactedFrom: []UUID{seg1, seg2, patP1, patQ1}, Deletable: []UUID{seg1}},
		},
		{
			name:  "a patch of a patch before the folding merge",
			scope: func(t *testing.T) ([]PackInfo, CommitSet) { return patchChainScope(t, false) },
			want: View{
				Generation: genA, Packs: []UUID{patP2, patQ2},
				CompactedFrom: []UUID{seg0, seg1, patP1, patP2, patQ1, patQ2},
			},
		},
		{
			name:  "a patch of a patch after the folding merge",
			scope: func(t *testing.T) ([]PackInfo, CommitSet) { return patchChainScope(t, true) },
			want: View{
				Generation: genB, Packs: []UUID{memB},
				CompactedFrom: []UUID{seg0, seg1, patP1, patP2, patQ1, patQ2},
				Deletable:     []UUID{memA, seg1, patP1, patP2, patQ1, patQ2},
			},
		},
		{
			name: "patches based on G, committed and not",
			scope: func(t *testing.T) ([]PackInfo, CommitSet) {
				return []PackInfo{
					memberInfo(t, memA, setA, 1, 1), segmentInfo(t, seg1), segmentInfo(t, seg2),
					// The uncommitted patch may be an admission in progress.
					patchInfo(t, patP1, onA, seg1), patchInfo(t, patQ1, onA, seg2),
				}, commitSet(setA, patP1)
			},
			want: View{Generation: genA, Packs: []UUID{memA, seg2, patP1}, CompactedFrom: []UUID{seg1, seg2, patP1}},
		},
		{
			// A catalog admits no such patch, but the lineage rule lists every generation-0 pack a patch based on G names.
			name: "a patch based on G naming a pack outside the view",
			scope: func(t *testing.T) ([]PackInfo, CommitSet) {
				return []PackInfo{
					memberInfo(t, memA, setA, 1, 1), segmentInfo(t, seg1),
					patchInfo(t, patQ1, onA, seg1), patchInfo(t, patP1, onA, patQ1),
				}, commitSet(setA, patP1)
			},
			want: View{Generation: genA, Packs: []UUID{memA, seg1, patP1}, CompactedFrom: []UUID{seg1, patQ1, patP1}},
		},
		{
			name: "patches based below G, committed and not",
			scope: func(t *testing.T) ([]PackInfo, CommitSet) {
				return []PackInfo{
					memberInfo(t, memA, setA, 1, 1), successor(t, memB, setB, 1, 2, memA, seg1),
					segmentInfo(t, seg1), segmentInfo(t, seg2),
					patchInfo(t, patP1, onA, seg2), patchInfo(t, patQ1, onA, seg2),
				}, commitSet(setA, setB, patP1)
			},
			want: View{Generation: genB, Packs: []UUID{memB, seg2}, CompactedFrom: []UUID{seg1, seg2}, Deletable: []UUID{memA, seg1, patP1, patQ1}},
		},
		{
			name: "patches on a missing generation or without patch_base",
			scope: func(t *testing.T) ([]PackInfo, CommitSet) {
				return []PackInfo{
					memberInfo(t, memB, setB, 1, 2, seg1), segmentInfo(t, seg1), segmentInfo(t, seg2),
					patchInfo(t, patP1, new(setGone), seg2), patchInfo(t, patQ1, new(setGone), seg2),
					// Registered before the scope had a generation.
					patchInfo(t, patP2, nil, seg2), patchInfo(t, patQ2, nil, seg2),
				}, commitSet(setB, patP1, patP2)
			},
			want: View{Generation: genB, Packs: []UUID{memB, seg2}, CompactedFrom: []UUID{seg1, seg2}, Deletable: []UUID{seg1, patP1, patQ1, patP2, patQ2}},
		},
		{
			name: "a patch and a generation whose commits landed after a cancellation generation",
			scope: func(t *testing.T) ([]PackInfo, CommitSet) {
				return []PackInfo{
					memberInfo(t, memA, setA, 1, 1, seg0), segmentInfo(t, seg1),
					// The uncertain admissions under epoch 1, cancelled under epoch 2 by C.
					patchInfo(t, patP1, onA, seg1),
					// An uncommitted patch against A, and a committed one registered before the scope had a generation.
					patchInfo(t, patQ1, onA, seg1), patchInfo(t, patQ2, nil, seg1),
					successor(t, memB, setB, 1, 2, memA, seg0, seg1),
					successor(t, memC, setC, 2, 2, memA, seg0, seg1),
				}, commitSet(setA, setB, setC, patP1, patQ2)
			},
			want: View{
				Generation: &Generation{ReplacementSetID: setC, PublisherEpoch: 2, ScopeGeneration: 2, Member: memC},
				Packs:      []UUID{memC}, CompactedFrom: []UUID{seg0, seg1}, Deletable: []UUID{memA, seg1, patP1, patQ1, patQ2, memB},
			},
		},
		{
			name: "uncommitted and lower generations",
			scope: func(t *testing.T) ([]PackInfo, CommitSet) {
				return []PackInfo{
					// Uncommitted and below G: deletable all the same.
					memberInfo(t, memA, setA, 1, 1),
					successor(t, memB, setB, 1, 2, memA, seg0),
					// Uncommitted and above G: its compacted_from folds nothing.
					successor(t, memC, setC, 1, 3, memB, seg0, seg1, seg2),
					// Committed with a higher scope_generation, but an older epoch.
					memberInfo(t, memD, setD, 0, 5),
					segmentInfo(t, seg1), segmentInfo(t, seg2),
				}, commitSet(setB, setD)
			},
			want: View{Generation: genB, Packs: []UUID{memB, seg1, seg2}, CompactedFrom: []UUID{seg0, seg1, seg2}, Deletable: []UUID{memA, memD}},
		},
		{
			name: "an incomplete set beside a generation",
			scope: func(t *testing.T) ([]PackInfo, CommitSet) {
				return []PackInfo{memberInfo(t, memA, setA, 1, 1, seg0), segmentInfo(t, seg1), segmentInfo(t, seg2)}, commitSet(setA, setGone)
			},
			want: View{Generation: genA, Packs: []UUID{memA, seg1, seg2}, CompactedFrom: []UUID{seg0, seg1, seg2}},
		},
		{
			name: "an incomplete set alone",
			scope: func(t *testing.T) ([]PackInfo, CommitSet) {
				return []PackInfo{segmentInfo(t, seg1), segmentInfo(t, seg2)}, commitSet(setGone)
			},
			want: View{Packs: []UUID{seg1, seg2}, CompactedFrom: []UUID{seg1, seg2}},
		},
		{
			name: "generations A, B and C",
			scope: func(t *testing.T) ([]PackInfo, CommitSet) {
				return []PackInfo{
					segmentInfo(t, seg1), segmentInfo(t, seg2), segmentInfo(t, seg3),
					memberInfo(t, memA, setA, 1, 1, seg1),
					successor(t, memB, setB, 1, 2, memA, seg1, seg2),
					successor(t, memC, setC, 1, 3, memB, seg1, seg2, seg3),
				}, commitSet(setA, setB, setC)
			},
			want: View{
				Generation: &Generation{ReplacementSetID: setC, PublisherEpoch: 1, ScopeGeneration: 3, Member: memC},
				Packs:      []UUID{memC}, CompactedFrom: []UUID{seg1, seg2, seg3}, Deletable: []UUID{seg1, seg2, seg3, memA, memB},
			},
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()

			packs, commits := tt.scope(t)
			requireView(t, tt.want, packs, commits)
		})
	}
}

func TestActiveViewReportsConflictedScope(t *testing.T) {
	t.Parallel()

	a, b := memberInfo(t, memA, setA, 1, 2), memberInfo(t, memB, setB, 1, 2)
	lower, higher := memberInfo(t, memD, setD, 1, 1), memberInfo(t, memC, setC, 1, 3)
	seg := segmentInfo(t, seg1)

	for _, order := range permutations([]UUID{memA, memB, memD, seg1}) {
		byID := map[UUID]PackInfo{memA: a, memB: b, memD: lower, seg1: seg}
		packs := make([]PackInfo, 0, len(order))
		for _, id := range order {
			packs = append(packs, byID[id])
		}
		v, err := ActiveView(packs, commitSet(setA, setB, setD))
		require.ErrorIs(t, err, ErrViewConflicted, "order %v", order)
		require.ErrorContains(t, err, setA.String())
		require.ErrorContains(t, err, setB.String())
		assert.Equal(t, View{}, v)
	}

	// With one of the two sets uncommitted there is no conflict, and a set at G's rank is not below it,
	// nor is a patch based on it, or on an uncommitted set above G.
	onTie, onHigher := patchInfo(t, patP1, new(setB), seg1), patchInfo(t, patQ1, new(setC), seg1)
	requireView(t, View{
		Generation: &Generation{ReplacementSetID: setA, PublisherEpoch: 1, ScopeGeneration: 2, Member: memA},
		Packs:      []UUID{memA, seg1}, CompactedFrom: []UUID{seg1}, Deletable: []UUID{memD},
	}, []PackInfo{a, b, lower, higher, seg, onTie, onHigher}, commitSet(setA, setD, patP1))

	// A higher generation outranks the two sets.
	requireView(t, View{
		Generation: &Generation{ReplacementSetID: setC, PublisherEpoch: 1, ScopeGeneration: 3, Member: memC},
		Packs:      []UUID{memC, seg1}, CompactedFrom: []UUID{seg1}, Deletable: []UUID{memA, memB, memD},
	}, []PackInfo{a, b, lower, higher, seg}, commitSet(setA, setB, setC, setD))
}

func TestActiveViewFallsBackWhenCommitObjectRemovedFirst(t *testing.T) {
	t.Parallel()

	packs := []PackInfo{
		segmentInfo(t, seg1), segmentInfo(t, seg2),
		memberInfo(t, memA, setA, 1, 1, seg1),
		viewInfo(t, memB, func(m *PackMeta) {
			generationMeta(m, setB, 1, 2, []UUID{seg1, seg2})
			m.Supersedes = []UUID{memA}
		}),
	}
	// Deleting A's member and then its commit object keeps the view at B throughout.
	requireView(t, View{
		Generation: &Generation{ReplacementSetID: setB, PublisherEpoch: 1, ScopeGeneration: 2, Member: memB},
		Packs:      []UUID{memB}, CompactedFrom: []UUID{seg1, seg2}, Deletable: []UUID{seg1, seg2, memA},
	}, packs, commitSet(setA, setB))

	// B's commit object deleted before the packs that depend on it,
	// an order the Deletion rules of the tracepack storage specification §4 exclude, makes the view fall back to A:
	// seg2 returns to the view, and B's member, uncommitted above A, is not deletable.
	v, err := ActiveView(packs, commitSet(setA))
	require.NoError(t, err)
	assert.Equal(t, View{
		Capture: viewCapture, Hour: hourOf(blockTestHour),
		Generation: &Generation{ReplacementSetID: setA, PublisherEpoch: 1, ScopeGeneration: 1, Member: memA},
		Packs:      []UUID{memA, seg2}, CompactedFrom: []UUID{seg1, seg2}, Deletable: []UUID{seg1},
	}, v)
}

func TestActiveViewKeepsReplacedPatchesAsWitnesses(t *testing.T) {
	t.Parallel()

	packs, commits := patchChainScope(t, false)
	v, err := ActiveView(packs, commits)
	require.NoError(t, err)
	require.Equal(t, []UUID{patP2, patQ2}, v.Packs)
	require.Nil(t, v.Deletable, "before the folding merge no pack of the chains is deletable")

	// A replaced patch is what keeps the pack it replaced out of the view:
	// were patP1 or patQ1 deleted before the folding merge, seg1 or memA would return.
	for _, tt := range []struct{ witness, target UUID }{{patP1, seg1}, {patQ1, memA}} {
		rest := slices.DeleteFunc(slices.Clone(packs), func(p PackInfo) bool { return p.PackID() == tt.witness })
		got, err := ActiveView(rest, commits)
		require.NoError(t, err)
		assert.Contains(t, got.Packs, tt.target, "without %s", tt.witness)
	}

	// After the folding merge witnesses and targets are all deletable;
	// TestActiveViewVectors checks every state their deletion in any order reaches, then rebuilds the scope.
	packs, commits = patchChainScope(t, true)
	v, err = ActiveView(packs, commits)
	require.NoError(t, err)
	assert.Equal(t, []UUID{memB}, v.Packs)
	assert.Equal(t, []UUID{memA, seg1, patP1, patP2, patQ1, patQ2}, v.Deletable)
}
