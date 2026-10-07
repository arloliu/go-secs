package tracepack

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"maps"
	"slices"
)

// ReaderSourceOptions configures NewReaderSource.
type ReaderSourceOptions struct {
	// Complete asserts that the view readers are every accepted pack of the tool, of every capture and every hour,
	// and that the view and Evidence readers together are every pack a catalog of the tool would hold evidence of,
	// rejected registrations included (the tracepack storage specification §5, Per capture).
	// It is an assertion about that evidence, never "all the files I found":
	// a set of files from which a pack was removed, by retention or otherwise, cannot be Complete.
	// Only then can the source vouch for the barriers of captures other than the one observed.
	// The scopes are then indexed, except the scope of each Evidence reader.
	Complete bool
	// Evidence holds readers whose statistics are folded into their capture's evidence only,
	// never into a scope, a view or a commit set:
	// the segments and converter archives whose registration was rejected,
	// whose evidence the tracepack storage specification §5 Per capture keeps.
	// A catalog rejects a registration only for a scope that is not indexed,
	// so the scope of each Evidence reader, its capture_id and the UTC hour of its period,
	// is not indexed, even with Complete.
	Evidence []*Reader
}

// readerSource is the PackSource NewReaderSource returns: everything it fixed, never changed afterwards.
type readerSource struct {
	// indexed reports whether the source's scopes are indexed, all but those of unindexed: ReaderSourceOptions.Complete.
	indexed bool
	// unindexed holds the scope of each evidence reader, which is never indexed.
	unindexed map[readerScopeKey]struct{}
	// partial marks every capture's evidence partial, also a capture no reader holds.
	partial bool
	// scopes holds the view of each scope of the view readers.
	scopes map[readerScopeKey]readerScope
	// evidence holds the evidence of each capture of the readers, view and evidence alike.
	evidence map[UUID]CaptureEvidence
	// barriers holds the stop-unclean boundaries of every capture's evidence, by capture_id byte order, then in evidence order.
	barriers []Boundary
}

var _ PackSource = (*readerSource)(nil)

// readerScopeKey names one scope of a readerSource: a capture and a UTC hour.
type readerScopeKey struct {
	capture UUID
	hour    int64
}

// readerScope is the view of one scope of a readerSource.
type readerScope struct {
	// readers holds a Reader per pack of the view, in view order; nil for a conflicted scope.
	readers    []*Reader
	conflicted bool
}

// sourcePack is one reader given to NewReaderSource, with what the source needs of it.
type sourcePack struct {
	r    *Reader
	info PackInfo
	meta *PackMeta
	// view reports a reader of the view readers; false for an evidence reader.
	view bool
}

// readerObservation is the Observation a readerSource's Observe returns.
// It answers from the source, which never changes, so it holds no state of its own.
type readerObservation struct {
	src      *readerSource
	capture  UUID
	from, to int64
}

var _ Observation = (*readerObservation)(nil)

// NewReaderSource returns a PackSource over packs the caller opened, all of one tool
// (the tracepack storage specification §5, Observation of a lookup).
//
// It groups readers by capture_id and by the UTC hour of their period,
// and computes each scope's view once, with ActiveView,
// taking every generation and patch given as committed:
// giving one asserts that it was accepted (the tracepack storage specification §5, commit protocol),
// so a caller never gives a generation whose publication failed or was rejected, nor a patch that was not registered.
// A scope whose view is conflicted is reported Conflicted, without readers.
// It folds the statistics of every pack of a capture, readers and opts.Evidence alike,
// into the capture's evidence with AddPackEvidence,
// and takes as barriers the stop-unclean boundaries of the evidence of every capture.
//
// Without opts.Complete, its scopes are not indexed and every capture's evidence is Partial,
// so every scope a lookup reads is a TxGapCold gap and the lookup is never TxUnmatched.
// With it, the scopes are indexed, except the scope of each evidence reader,
// its capture_id and the UTC hour of its period, which is not indexed:
// a catalog rejects a registration only for a scope that is not indexed
// (the tracepack storage specification §5, No admissions outside the index),
// so a lookup reading that scope gets a TxGapCold gap, as it would through the catalog,
// and the scope's view still holds only view readers.
// With it too, every capture's evidence is Partial when any reader, view or evidence, has no used footer,
// since its statistics, and the boundaries and closures they hold, are then unknown;
// Complete never overrides such missing evidence.
//
// Observe takes a capture_id that is not zero and hours [from, to) with from < to within [MinTxHour, MaxTxHour+1),
// the hours a lookup reads, and answers from what NewReaderSource computed:
// an hour without packs is an empty scope, indexed only with Complete and when no evidence reader names it,
// and a capture without a view reader has only empty scopes beside its evidence.
// Every value an Observation returns is the caller's, as Observation states.
// The Close of an Observation closes nothing: the caller owns the readers, and keeps them open while the source is used.
// The source and its Observations are safe for concurrent use, as the Readers are.
//
// Parameters:
//   - readers: the packs of the scopes' views, of role segment, archive or repair; none at all leaves every scope empty.
//   - opts: see ReaderSourceOptions;
//     its Evidence readers are of role segment or archive, each with a period inside one UTC hour.
//
// Returns:
//   - PackSource: the source; nil on error.
//   - error: an error wrapping ErrInvalidQuery, over readers and opts.Evidence together:
//     for a nil reader, a Reader given twice, within either slice or across both, two readers of one pack_id,
//     readers of more than one tool_id, a view reader whose role is not segment, archive or repair,
//     an evidence reader whose role is not segment or archive,
//     or an evidence reader whose period is empty or not inside one UTC hour, whose scope would otherwise be unknown;
//     else the error of ActiveView for a scope it rejects, other than a conflicted one.
func NewReaderSource(readers []*Reader, opts ReaderSourceOptions) (PackSource, error) {
	packs, err := sourcePacks(readers, opts.Evidence)
	if err != nil {
		return nil, err
	}

	s := &readerSource{indexed: opts.Complete, partial: !opts.Complete}
	if err := s.computeViews(packs); err != nil {
		return nil, err
	}
	s.evidence = make(map[UUID]CaptureEvidence)
	s.unindexed = make(map[readerScopeKey]struct{})
	for i := range packs {
		p := &packs[i]
		if !p.view {
			// The hour as computeViews groups a view reader.
			s.unindexed[readerScopeKey{capture: p.info.CaptureID(), hour: hourOf(p.meta.PeriodStart)}] = struct{}{}
		}
		e := s.evidence[p.info.CaptureID()]
		if st, ok := p.r.Stats(); ok {
			AddPackEvidence(&e, &st)
		} else {
			AddPackEvidence(&e, nil)
			s.partial = true
		}
		s.evidence[p.info.CaptureID()] = e
	}
	captures := slices.SortedFunc(maps.Keys(s.evidence), func(a, b UUID) int { return bytes.Compare(a[:], b[:]) })
	for _, c := range captures {
		e := s.evidence[c]
		e.Partial = e.Partial || s.partial
		s.evidence[c] = e
		for i := range e.Boundaries {
			if e.Boundaries[i].Kind == BoundaryKindStopUnclean {
				s.barriers = append(s.barriers, cloneBoundary(e.Boundaries[i]))
			}
		}
	}

	return s, nil
}

// sourcePacks checks the readers of NewReaderSource, readers then evidence, as NewReaderSource states,
// and returns each with its PackInfo and pack metadata, in that order.
//
// Returns:
//   - []sourcePack: the packs; nil on error.
//   - error: an error wrapping ErrInvalidQuery that names the readers concerned.
func sourcePacks(readers, evidence []*Reader) ([]sourcePack, error) {
	packs := make([]sourcePack, 0, len(readers)+len(evidence))
	given := make(map[*Reader]string, cap(packs))
	ids := make(map[UUID]string, cap(packs))
	var tool, toolName string
	for _, set := range []struct {
		name    string
		readers []*Reader
		view    bool
	}{{name: "readers", readers: readers, view: true}, {name: "Evidence", readers: evidence}} {
		for i, r := range set.readers {
			name := fmt.Sprintf("%s[%d]", set.name, i)
			if r == nil {
				return nil, fmt.Errorf("%w: new reader source: %s is nil", ErrInvalidQuery, name)
			}
			if prev, ok := given[r]; ok {
				return nil, fmt.Errorf("%w: new reader source: %s and %s are the same Reader", ErrInvalidQuery, prev, name)
			}
			given[r] = name
			info := r.Info()
			if prev, ok := ids[info.PackID()]; ok {
				return nil, fmt.Errorf("%w: new reader source: %s and %s hold the same pack_id %s", ErrInvalidQuery, prev, name, info.PackID())
			}
			ids[info.PackID()] = name
			m := info.Meta()
			if toolName == "" {
				tool, toolName = m.ToolID, name
			} else if m.ToolID != tool {
				return nil, fmt.Errorf("%w: new reader source: %s holds tool_id %q, %s tool_id %q", ErrInvalidQuery, toolName, tool, name, m.ToolID)
			}
			if set.view && !tierRole(m.PackRole) {
				return nil, fmt.Errorf("%w: new reader source: %s is a pack of role %s, which takes no part in a scope's view",
					ErrInvalidQuery, name, m.PackRole)
			}
			if !set.view && m.PackRole != PackRoleSegment && m.PackRole != PackRoleArchive {
				return nil, fmt.Errorf("%w: new reader source: %s is a pack of role %s, not a segment or archive whose evidence a catalog keeps",
					ErrInvalidQuery, name, m.PackRole)
			}
			// An evidence reader's scope is not indexed, so it must be known:
			// ActiveView checks a view reader's period, nothing checks an evidence reader's.
			if _, ok := periodHour(m.PeriodStart, m.PeriodEnd); !set.view && !ok {
				return nil, fmt.Errorf("%w: new reader source: %s has the period [%d, %d), empty or not inside one UTC hour",
					ErrInvalidQuery, name, m.PeriodStart, m.PeriodEnd)
			}
			packs = append(packs, sourcePack{r: r, info: info, meta: m, view: set.view})
		}
	}

	return packs, nil
}

// computeViews groups the view packs of packs by scope, in the order of their first pack,
// and computes each scope's view with ActiveView,
// its commit set every replacement_set_id of an archive and pack_id of a repair among them.
//
// Returns:
//   - error: the error of ActiveView, wrapped with the scope, for a scope it rejects other than a conflicted one.
func (s *readerSource) computeViews(packs []sourcePack) error {
	var keys []readerScopeKey
	groups := make(map[readerScopeKey][]*sourcePack)
	for i := range packs {
		p := &packs[i]
		if !p.view {
			continue
		}
		// A period not inside one hour is grouped by its start, and ActiveView rejects it.
		key := readerScopeKey{capture: p.info.CaptureID(), hour: hourOf(p.meta.PeriodStart)}
		if _, ok := groups[key]; !ok {
			keys = append(keys, key)
		}
		groups[key] = append(groups[key], p)
	}

	s.scopes = make(map[readerScopeKey]readerScope, len(keys))
	for _, key := range keys {
		group := groups[key]
		infos := make([]PackInfo, len(group))
		commits := make(CommitSet)
		byID := make(map[UUID]*Reader, len(group))
		for i, p := range group {
			infos[i] = p.info
			byID[p.info.PackID()] = p.r
			if p.meta.PackRole == PackRoleArchive && p.meta.ReplacementSetID != nil {
				commits[*p.meta.ReplacementSetID] = struct{}{}
			} else if p.meta.PackRole == PackRoleRepair {
				commits[p.info.PackID()] = struct{}{}
			}
		}
		v, err := ActiveView(infos, commits)
		if errors.Is(err, ErrViewConflicted) {
			s.scopes[key] = readerScope{conflicted: true}

			continue
		}
		if err != nil {
			return fmt.Errorf("tracepack: new reader source: scope of capture %s, hour %d: %w", key.capture, key.hour, err)
		}
		sc := readerScope{readers: make([]*Reader, len(v.Packs))}
		for i, id := range v.Packs {
			sc.readers[i] = byID[id]
		}
		s.scopes[key] = sc
	}

	return nil
}

// Observe returns an observation of capture over the hours [from, to), which answers from what NewReaderSource computed.
func (s *readerSource) Observe(ctx context.Context, capture UUID, from, to int64) (Observation, error) {
	if err := checkObserve(capture, from, to); err != nil {
		return nil, fmt.Errorf("tracepack: reader source: %w", err)
	}
	if err := ctx.Err(); err != nil {
		return nil, fmt.Errorf("tracepack: reader source: %w", err)
	}

	return &readerObservation{src: s, capture: capture, from: from, to: to}, nil
}

// Scope returns the view of hour's scope, its Readers in a fresh slice; an hour without packs has none.
func (o *readerObservation) Scope(ctx context.Context, hour int64) (SourceScope, error) {
	if err := ctx.Err(); err != nil {
		return SourceScope{}, fmt.Errorf("tracepack: reader source: %w", err)
	}
	if hour < o.from || hour >= o.to {
		return SourceScope{}, fmt.Errorf("tracepack: reader source: hour %d outside the observation's hours [%d, %d)", hour, o.from, o.to)
	}
	key := readerScopeKey{capture: o.capture, hour: hour}
	sc := o.src.scopes[key]
	_, unindexed := o.src.unindexed[key]

	return SourceScope{Readers: slices.Clone(sc.readers), Indexed: o.src.indexed && !unindexed, Conflicted: sc.conflicted}, nil
}

// Evidence returns a deep copy of the capture's evidence;
// for a capture no reader holds, an empty evidence, Partial as every capture's is.
func (o *readerObservation) Evidence(ctx context.Context) (CaptureEvidence, error) {
	if err := ctx.Err(); err != nil {
		return CaptureEvidence{}, fmt.Errorf("tracepack: reader source: %w", err)
	}
	e, ok := o.src.evidence[o.capture]
	if !ok {
		return CaptureEvidence{Partial: o.src.partial}, nil
	}
	e.Boundaries = cloneBoundaries(e.Boundaries)
	e.Closures = slices.Clone(e.Closures)

	return e, nil
}

// Barriers returns deep copies of the barriers whose gap interval meets [from, to),
// which must be non-empty and lie within the observation's hours.
func (o *readerObservation) Barriers(ctx context.Context, from, to int64) ([]Boundary, error) {
	if err := ctx.Err(); err != nil {
		return nil, fmt.Errorf("tracepack: reader source: %w", err)
	}
	out, err := meetingBarriers(o.src.barriers, from, to, o.from, o.to)
	if err != nil {
		return nil, fmt.Errorf("tracepack: reader source: %w", err)
	}

	return out, nil
}

// Close does nothing: the caller owns the readers.
func (o *readerObservation) Close() error {
	return nil
}
