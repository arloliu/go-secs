package tracepack

import (
	"bytes"
	"cmp"
	"errors"
	"fmt"
)

// ErrViewConflicted reports a scope with two different complete, committed replacement sets of the highest rank,
// which the tracepack storage specification §4 makes conflicted instead of choosing one.
// ActiveView wraps it with the replacement_set_ids of two such sets.
var ErrViewConflicted = errors.New("tracepack: scope conflicted")

// PackInfo is what ActiveView needs of one pack of a scope: its pack_id, its capture_id and its decoded pack metadata.
//
// A PackInfo is built only from decoded bytes, by Reader.Info or NewPackInfo,
// never from a PackMeta a caller could have changed after it was decoded,
// so ActiveView never sees pack metadata the decoder would reject.
// Its accessors return copies.
// The zero PackInfo holds no pack metadata, and ActiveView rejects it.
type PackInfo struct {
	packID    UUID
	captureID UUID
	// raw holds the pack metadata bytes, which no caller can reach; Meta decodes a fresh copy from them on every call.
	raw []byte
	// meta is decoded from raw and never handed out.
	meta *PackMeta
}

// CommitSet holds the ids of a scope's commit objects (the tracepack storage specification §3):
// a generation's replacement_set_id, or a patch's pack_id.
type CommitSet map[UUID]struct{}

// Generation is one generation of a scope:
// a replacement set, whose one member is an archive (the tracepack storage specification §2 and §6).
type Generation struct {
	// ReplacementSetID identifies the set, and names its commit object.
	ReplacementSetID UUID
	// PublisherEpoch and ScopeGeneration are the generation's rank, compared lexicographically.
	PublisherEpoch  uint64
	ScopeGeneration uint64
	// Member is the pack_id of the set's one member.
	Member UUID
}

// View is the active view of one scope (the tracepack storage specification §4),
// with the lineage the next merge of the scope writes, the packs deletable under §4 Deletion,
// and the packs excluded for their role (§2).
//
// Each slice is nil when it has no element.
type View struct {
	// Capture is the scope's capture_id.
	Capture UUID
	// Hour is the scope's UTC hour, numbered in whole hours from 1970-01-01T00:00Z, negative before it.
	Hour int64
	// Generation is the current generation G; nil when the scope has none.
	Generation *Generation
	// Packs holds the packs whose records the view reads:
	// G's member, unless a patch based on G replaced it,
	// then the generation-0 packs in the view, in the order of ActiveView's packs.
	Packs []UUID
	// CompactedFrom is the compacted_from the next merge of the scope writes (the tracepack storage specification §4 Merge):
	// G's compacted_from in its stored order,
	// then every generation-0 pack in the view or named in the supersedes of a patch based on G, in the order of ActiveView's packs,
	// each pack once.
	CompactedFrom []UUID
	// Deletable holds the packs deletable under the tracepack storage specification §4 Deletion, in the order of ActiveView's packs.
	// Commit objects are not listed:
	// one is deleted by the component that deleted every pack it commits, from its own record of those deletions.
	Deletable []UUID
	// Excluded holds the packs whose pack_role takes no part in a scope's view (the tracepack storage specification §2),
	// in the order of ActiveView's packs.
	// They are reported to the operator and are never deletable.
	Excluded []UUID
}

// scopeView is the state ActiveView derives from the packs of one scope.
type scopeView struct {
	packs   []PackInfo
	commits CommitSet
	capture UUID
	hour    int64
	// gens maps the replacement_set_id of every generation among packs to that generation.
	gens map[UUID]Generation
	// g is the current generation; nil when the scope has none.
	g *Generation
	// gFrom is G's compacted_from, in its stored order.
	gFrom []UUID
	// folded holds the packs G's compacted_from lists.
	folded map[UUID]struct{}
	// replaced holds every pack named in the supersedes of a patch based on G.
	replaced map[UUID]struct{}
}

// NewPackInfo returns the PackInfo of a pack from its file header ids and its pack metadata as stored
// (the tracepack format specification §5), such as a catalog keeps for each pack.
//
// It copies metadata, then decodes the copy as UnmarshalPackMeta does, with the same registry checks and intrinsic rules.
// A pack_role this package does not know, the retired value included, decodes,
// and ActiveView then excludes the pack.
//
// Parameters:
//   - packID: the pack's pack_id.
//   - captureID: the pack's capture_id.
//   - metadata: the pack's pack metadata bytes; the caller may reuse the buffer afterwards.
//
// Returns:
//   - PackInfo: the pack's PackInfo; the zero PackInfo on error.
//   - error: the error of UnmarshalPackMeta.
func NewPackInfo(packID, captureID UUID, metadata []byte) (PackInfo, error) {
	raw := bytes.Clone(metadata)
	meta, err := UnmarshalPackMeta(raw)
	if err != nil {
		return PackInfo{}, err
	}

	return PackInfo{packID: packID, captureID: captureID, raw: raw, meta: meta}, nil
}

// ActiveView returns the active view of one scope (the tracepack storage specification §4):
// its current generation G, the packs whose records the view reads, the compacted_from the next merge writes,
// the packs deletable under §4 Deletion, and the packs excluded for their role (§2).
//
// packs must be a complete observation of the scope:
// every surviving pack of the scope that a reader could list, staging segments and replaced patches included,
// as a catalog snapshot or a coherent observation holds them (the tracepack storage specification §5);
// commits holds the ids of the scope's commit objects in the same observation.
// The Deletion rules are sound only under that premise:
// a patch left out of packs may be the one that keeps the packs it replaced out of the view.
// An id in commits that names no pack of packs, such as the commit object of an incomplete set, makes no pack deletable.
//
// Before any rule, ActiveView checks every pack:
// its period is non-empty and inside one UTC hour, and every pack has the same capture_id and hour;
// its pack_id appears once;
// a segment or repair is at scope_generation 0,
// and an archive at scope_generation 1 or more, with a publisher_epoch and a replacement_set_id;
// and no two archives hold one replacement_set_id, whatever their ranks.
// A pack whose pack_role is not segment, archive or repair takes no further part:
// it is listed in Excluded only.
// Its period, capture_id and pack_id are checked all the same,
// because the tracepack storage specification §2 places every pack except an extract in exactly one scope,
// and an extract never enters the tiers, so an extract in packs is the caller's error.
// A patch based on G is a repair with a commit object
// whose patch_base is G's replacement_set_id, or absent when the scope has no G;
// an absent patch_base differs from every present value, the nil UUID included.
//
// Parameters:
//   - packs: the packs of one scope, each built by Reader.Info or NewPackInfo.
//   - commits: the ids of the scope's commit objects; nil when it has none.
//
// Returns:
//   - View: the scope's view, its slices in the orders View states; the zero View on error.
//   - error: an error naming the pack and the check it fails, for a zero PackInfo, or for no packs;
//     or an error wrapping ErrViewConflicted for two different committed sets of the highest rank.
func ActiveView(packs []PackInfo, commits CommitSet) (View, error) {
	s, err := newScopeView(packs, commits)
	if err != nil {
		return View{}, err
	}
	if err := s.selectCurrent(); err != nil {
		return View{}, err
	}

	return s.view(), nil
}

// newScopeView checks packs as ActiveView states and indexes their generations.
//
// Returns:
//   - *scopeView: the scope, without its current generation; nil on error.
//   - error: an error naming the first pack that fails a check.
func newScopeView(packs []PackInfo, commits CommitSet) (*scopeView, error) {
	if len(packs) == 0 {
		return nil, errors.New("tracepack: active view: no packs")
	}

	s := &scopeView{packs: packs, commits: commits, gens: make(map[UUID]Generation)}
	ids := make(map[UUID]struct{}, len(packs))
	for i, p := range packs {
		if p.meta == nil {
			return nil, fmt.Errorf("tracepack: active view: pack %d holds no pack metadata; build it with NewPackInfo or Reader.Info", i)
		}
		if err := s.admit(i == 0, p, ids); err != nil {
			return nil, fmt.Errorf("tracepack: active view: pack %s: %w", p.packID, err)
		}
	}

	return s, nil
}

// tierRole reports whether a pack of role r takes part in a scope's view (the tracepack storage specification §2).
func tierRole(r PackRole) bool {
	return r == PackRoleSegment || r == PackRoleArchive || r == PackRoleRepair
}

// periodHour returns the UTC hour of the period [start, end),
// and false when the period is empty or not inside one UTC hour.
func periodHour(start, end int64) (int64, bool) {
	// start < end is checked first, so end-1 cannot overflow.
	if start >= end || hourOf(start) != hourOf(end-1) {
		return 0, false
	}

	return hourOf(start), true
}

// checkRoleGeneration checks the scope_generation of a pack whose role takes part in a scope's view against its role
// (the tracepack storage specification §2):
// a segment or repair is at generation 0, and an archive is a generation, with its rank and its set.
func checkRoleGeneration(m *PackMeta) error {
	// Unreachable through NewPackInfo or Reader.Info, whose decoder requires scope_generation;
	// kept to guard the dereferences below if that changes.
	if m.ScopeGeneration == nil {
		return fmt.Errorf("pack_role %s without scope_generation", m.PackRole)
	}
	gen := *m.ScopeGeneration
	if m.PackRole != PackRoleArchive {
		if gen != 0 {
			return fmt.Errorf("pack_role %s at scope_generation %d, not 0", m.PackRole, gen)
		}

		return nil
	}

	switch {
	case gen == 0:
		return errors.New("pack_role archive at scope_generation 0")
	// The next two are unreachable through NewPackInfo or Reader.Info, whose decoder requires both;
	// kept to guard generationOf if that changes.
	case m.PublisherEpoch == nil:
		return errors.New("archive without publisher_epoch")
	case m.ReplacementSetID == nil:
		return errors.New("archive without replacement_set_id")
	}

	return nil
}

// generationOf returns the generation of which p, an archive that checkRoleGeneration accepted, is the member.
func generationOf(p PackInfo) Generation {
	m := p.meta

	return Generation{
		ReplacementSetID: *m.ReplacementSetID,
		PublisherEpoch:   *m.PublisherEpoch,
		ScopeGeneration:  *m.ScopeGeneration,
		Member:           p.packID,
	}
}

// compareRank compares the ranks of a and b, (publisher_epoch, scope_generation) lexicographically
// (the tracepack storage specification §2).
func compareRank(a, b Generation) int {
	if c := cmp.Compare(a.PublisherEpoch, b.PublisherEpoch); c != 0 {
		return c
	}

	return cmp.Compare(a.ScopeGeneration, b.ScopeGeneration)
}

// PackID returns the pack's pack_id.
func (p PackInfo) PackID() UUID {
	return p.packID
}

// CaptureID returns the pack's capture_id.
func (p PackInfo) CaptureID() UUID {
	return p.captureID
}

// Meta returns the pack's pack metadata, decoded afresh on every call,
// so a caller may modify the result without affecting p; nil for the zero PackInfo.
func (p PackInfo) Meta() *PackMeta {
	if p.meta == nil {
		return nil
	}
	// The same bytes were decoded without error when p was built, so decoding them again cannot fail.
	meta, _ := UnmarshalPackMeta(p.raw)

	return meta
}

// admit checks one pack as ActiveView states, against the scope that the first pack sets,
// and indexes it if it is a generation.
// ids holds the pack_ids admitted so far.
func (s *scopeView) admit(first bool, p PackInfo, ids map[UUID]struct{}) error {
	m := p.meta
	hour, ok := periodHour(m.PeriodStart, m.PeriodEnd)
	if !ok {
		return fmt.Errorf("period %d to %d is not inside one UTC hour", m.PeriodStart, m.PeriodEnd)
	}
	if first {
		s.capture, s.hour = p.captureID, hour
	}
	if p.captureID != s.capture {
		return fmt.Errorf("capture_id %s, not the scope's %s", p.captureID, s.capture)
	}
	if hour != s.hour {
		return fmt.Errorf("UTC hour %d, not the scope's %d", hour, s.hour)
	}
	if _, dup := ids[p.packID]; dup {
		return errors.New("pack_id appears more than once")
	}
	ids[p.packID] = struct{}{}

	if !tierRole(m.PackRole) {
		return nil
	}
	if err := checkRoleGeneration(m); err != nil {
		return err
	}
	if m.PackRole != PackRoleArchive {
		return nil
	}

	gen := generationOf(p)
	if other, dup := s.gens[gen.ReplacementSetID]; dup {
		return fmt.Errorf("replacement_set_id %s is also held by pack %s", gen.ReplacementSetID, other.Member)
	}
	s.gens[gen.ReplacementSetID] = gen

	return nil
}

// selectCurrent sets G, the highest-ranked generation with a commit object (every generation among the packs is complete),
// with its compacted_from, and the packs replaced by the patches based on it.
//
// Returns:
//   - error: an error wrapping ErrViewConflicted when two committed generations share the highest rank.
func (s *scopeView) selectCurrent() error {
	var tie *Generation
	gi := -1
	for i, p := range s.packs {
		if p.meta.PackRole != PackRoleArchive {
			continue
		}
		gen := s.gens[*p.meta.ReplacementSetID]
		if _, committed := s.commits[gen.ReplacementSetID]; !committed {
			continue
		}
		if s.g != nil {
			c := compareRank(gen, *s.g)
			if c < 0 {
				continue
			}
			if c == 0 {
				tie = &gen

				continue
			}
		}
		s.g, tie, gi = &gen, nil, i
	}
	if tie != nil {
		return fmt.Errorf("%w: replacement sets %s and %s both rank (%d, %d)",
			ErrViewConflicted, s.g.ReplacementSetID, tie.ReplacementSetID, tie.PublisherEpoch, tie.ScopeGeneration)
	}

	if s.g != nil {
		s.gFrom = s.packs[gi].meta.CompactedFrom
	}
	s.folded = make(map[UUID]struct{}, len(s.gFrom))
	for _, id := range s.gFrom {
		s.folded[id] = struct{}{}
	}
	s.replaced = make(map[UUID]struct{})
	for _, p := range s.packs {
		if s.basedOnG(p) {
			for _, id := range p.meta.Supersedes {
				s.replaced[id] = struct{}{}
			}
		}
	}

	return nil
}

// view returns the scope's View from its current generation.
func (s *scopeView) view() View {
	v := View{Capture: s.capture, Hour: s.hour}
	if s.g != nil {
		g := *s.g
		v.Generation = &g
		if _, replaced := s.replaced[g.Member]; !replaced {
			v.Packs = append(v.Packs, g.Member)
		}
	}

	listed := make(map[UUID]struct{}, len(s.gFrom))
	lineage := func(id UUID) {
		if _, dup := listed[id]; !dup {
			listed[id] = struct{}{}
			v.CompactedFrom = append(v.CompactedFrom, id)
		}
	}
	for _, id := range s.gFrom {
		lineage(id)
	}

	for _, p := range s.packs {
		role := p.meta.PackRole
		if !tierRole(role) {
			v.Excluded = append(v.Excluded, p.packID)

			continue
		}
		if s.deletable(p) {
			v.Deletable = append(v.Deletable, p.packID)
		}
		if role == PackRoleArchive {
			continue
		}

		inView := s.candidate(p)
		_, replaced := s.replaced[p.packID]
		if inView && !replaced {
			v.Packs = append(v.Packs, p.packID)
		}
		if inView || replaced {
			lineage(p.packID)
		}
	}

	return v
}

// basedOnG reports whether p is a patch based on G:
// a repair with a commit object whose patch_base matches G (the tracepack storage specification §4).
func (s *scopeView) basedOnG(p PackInfo) bool {
	if p.meta.PackRole != PackRoleRepair {
		return false
	}
	if _, committed := s.commits[p.packID]; !committed {
		return false
	}

	return s.matchesG(p.meta.PatchBase)
}

// matchesG reports whether base, a patch_base, names G:
// absent when the scope has no G, else G's replacement_set_id; an absent value never matches a present one.
func (s *scopeView) matchesG(base *UUID) bool {
	if s.g == nil {
		return base == nil
	}

	return base != nil && *base == s.g.ReplacementSetID
}

// candidate reports whether p, a generation-0 pack, joins the view before the patches based on G remove what they replace:
// it is not listed in G's compacted_from, and it is a segment or a patch based on G.
func (s *scopeView) candidate(p PackInfo) bool {
	if _, folded := s.folded[p.packID]; folded {
		return false
	}

	return p.meta.PackRole == PackRoleSegment || s.basedOnG(p)
}

// deletable reports whether p is deletable under the tracepack storage specification §4 Deletion;
// p's role takes part in the view.
// Only the current generation makes a pack redundant, so no pack is deletable without G, and G's own member never is.
func (s *scopeView) deletable(p PackInfo) bool {
	if s.g == nil || p.packID == s.g.Member {
		return false
	}

	m := p.meta
	if m.PackRole == PackRoleArchive {
		// A generation below G, committed or not; its rank is read from its own pack metadata.
		return compareRank(s.gens[*m.ReplacementSetID], *s.g) < 0
	}
	if _, folded := s.folded[p.packID]; folded {
		return true
	}
	if m.PackRole != PackRoleRepair {
		return false
	}

	return s.baseBelowG(m.PatchBase)
}

// baseBelowG reports whether a patch whose patch_base is base, committed or not, is based below G.
// A present patch_base that names no surviving generation, and is not G's, names one that was deleted,
// which happens only once a higher-ranked generation is G;
// an absent one was registered before the scope had any generation.
func (s *scopeView) baseBelowG(base *UUID) bool {
	switch {
	case base == nil:
		return true
	case *base == s.g.ReplacementSetID:
		// Committed, the patch is in the view or a witness of its replacements; uncommitted, it may be an admission in progress.
		return false
	}
	if gen, ok := s.gens[*base]; ok {
		return compareRank(gen, *s.g) < 0
	}

	return true
}
