package tracepack

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"maps"
	"slices"
	"strings"
)

// storeListed is a pack key a listing returned, kept until its pack is opened.
// Its key names the source's tool, which the listing's prefix fixes, so k leaves the tool empty.
type storeListed struct {
	key  string
	size int64
	k    storePackKey
}

// storeOpened is a pack of a listed hour that Observe opened, with the slot of its object among the acquisition's objects.
// It is charged storeOpenedCharge, beside its Reader and its listed key, until fixListed drops it.
type storeOpened struct {
	r    *Reader
	slot int
	key  string
	// kept marks a pack of the view, once it is fixed.
	kept bool
}

// storeListedHour is what Observe gathers of one listed hour, an hour the snapshot reports not indexed, before it fixes the hour's view.
type storeListedHour struct {
	// scope is the hour's index in the snapshot's scopes and the observation's hours.
	scope int
	hour  int64
	token string
	// commits holds the ids of the hour's commit objects, from the last of its two agreeing commit traversals;
	// commitCharge is their charge.
	commits      CommitSet
	commitCharge int64
	// archives holds the hour's archive keys of the capture, in listing order.
	archives []storeListed
	// packs holds the hour's packs opened whose role takes part in a view, ids their pack_ids and those of the packs excluded.
	packs []storeOpened
	ids   map[UUID]struct{}
	// held is the charge of packs and ids, with the listed keys of packs, until fixListed drops them.
	held int64
}

// observeListed fixes the scopes the snapshot reports not indexed (the tracepack storage specification §5, Coherent observation),
// in the order the observation of a lookup requires:
// for every such hour, its commit objects, listed until two consecutive complete traversals agree;
// then each hour's archive packs and the capture's staging segments, listed once;
// then each segment's head, which assigns it its hour, and the packs of the listed hours, opened in full;
// then each hour's view;
// and last each hour's confirmation by the catalog that the scope stayed not indexed since the snapshot.
// An hour found removed by retention, at any sample, is skipped from then on:
// its view is not computed nor its scope confirmed;
// the staging listing and the opening of segments stop once every listed hour is removed.
//
// Every structure it holds is charged before it is built, and released once nothing holds it:
// each listed hour, with its slots in the hours and their index, until Observe returns.
func (a *storeAcquisition) observeListed(ctx context.Context, scopes []CatalogScope) error {
	var hours []*storeListedHour
	byHour := map[int64]*storeListedHour{}
	var hoursCharge int64
	defer func() { a.budget.release(hoursCharge) }()
	for i := range scopes {
		if err := ctx.Err(); err != nil {
			return fmt.Errorf("tracepack: store source: %w", err)
		}
		if scopes[i].Indexed || a.needless(i) {
			continue
		}
		if err := a.budget.reserve(storeListedHourCharge); err != nil {
			return fmt.Errorf("tracepack: store source: hour %d: its listing state: %w", scopes[i].Hour, err)
		}
		hoursCharge += storeListedHourCharge
		lh := &storeListedHour{scope: i, hour: scopes[i].Hour, token: scopes[i].Token, ids: map[UUID]struct{}{}}
		hours = append(hours, lh)
		byHour[lh.hour] = lh
	}
	if len(hours) == 0 {
		return nil
	}
	a.hours = hours
	defer func() { a.hours = nil }()
	if a.src.checkpoint != nil {
		a.listing = &storeListingState{scopes: scopes, hours: hours, byHour: byHour}
		defer func() { a.listing = nil }()
	}

	for _, lh := range hours {
		if err := a.listCommits(ctx, lh); err != nil {
			return err
		}
	}
	for _, lh := range hours {
		if err := a.listArchives(ctx, lh); err != nil {
			return err
		}
	}
	segments, err := a.listSegments(ctx)
	if err != nil {
		return err
	}
	if a.listing != nil {
		a.listing.segments = segments
	}
	a.check("listed")

	for _, l := range segments {
		if err := a.openSegment(ctx, l, byHour); err != nil {
			return err
		}
	}
	// The segments' entries are dropped together, their charges with them: nothing reads segments after this.
	for _, l := range segments {
		a.budget.release(listedKeyCost(l.key))
	}
	if a.listing != nil {
		a.listing.segments = nil
	}
	for _, lh := range hours {
		if err := a.openArchives(ctx, lh); err != nil {
			return err
		}
	}
	if err := a.viewListed(ctx, hours); err != nil {
		return err
	}

	return a.confirm(ctx, scopes, hours, func() { a.check("confirming") })
}

// viewListed fixes the view of each listed hour in turn, once its last listing and read are done,
// sampling the retention boundary first: an hour found removed gets no view.
func (a *storeAcquisition) viewListed(ctx context.Context, hours []*storeListedHour) error {
	for _, lh := range hours {
		if err := ctx.Err(); err != nil {
			return fmt.Errorf("tracepack: store source: %w", err)
		}
		if err := a.sample(ctx); err != nil {
			return err
		}
		if a.needless(lh.scope) {
			continue
		}
		if err := a.fixListed(ctx, lh); err != nil {
			return err
		}
		a.check("viewed")
	}

	return nil
}

// storeListingState is what observeListed holds while it observes the listed hours, which a test's checkpoint measures:
// the snapshot's scopes, the listed hours and their index, the listed segments, and the PackInfo built for a view.
type storeListingState struct {
	scopes   []CatalogScope
	hours    []*storeListedHour
	byHour   map[int64]*storeListedHour
	segments []storeListed
	infos    []PackInfo
}

// check calls the source's checkpoint, when a test set it, with where it is called from and the listing state.
func (a *storeAcquisition) check(where string) {
	if a.src.checkpoint != nil {
		a.src.checkpoint(a, where, a.listing)
	}
}

// traverse takes one complete traversal of the listing of prefix, following each page's Next until it is "",
// and passes each key to fn in order, once it has counted it against MaxObjects and checked the listing contract of ObjectStore:
// every key under prefix and above the one before it, no token repeated, and at most MaxListPages pages.
// It samples the retention boundary before each page, and stops, with no error, once stop reports true after a sample;
// the caller then checks stop again.
// It charges the tokens it holds until the traversal ends.
func (a *storeAcquisition) traverse(ctx context.Context, prefix string, stop func() bool, fn func(o ObjectInfo) error) (err error) {
	tokens := map[string]struct{}{}
	var charge int64
	defer func() { a.budget.release(charge) }()

	fail := func(format string, args ...any) error {
		return fmt.Errorf("tracepack: store source: listing %q: %s", prefix, fmt.Sprintf(format, args...))
	}
	prev, token := "", ""
	for pages := 0; ; pages++ {
		if err := ctx.Err(); err != nil {
			return fmt.Errorf("tracepack: store source: %w", err)
		}
		if err := a.sample(ctx); err != nil {
			return err
		}
		if stop() {
			return nil
		}
		if pages == a.src.opts.MaxListPages {
			return fmt.Errorf("tracepack: store source: listing %q: more than MaxListPages %d pages: %w", prefix, a.src.opts.MaxListPages, ErrReadLimit)
		}
		page, err := a.src.store.List(ctx, prefix, token)
		if err != nil {
			return fmt.Errorf("tracepack: store source: listing %q: %w", prefix, err)
		}
		for _, o := range page.Objects {
			if err := ctx.Err(); err != nil {
				return fmt.Errorf("tracepack: store source: %w", err)
			}
			if err := a.budget.count(); err != nil {
				return fmt.Errorf("tracepack: store source: listing %q: %w", prefix, err)
			}
			// A key under the prefix is never empty, so the first key is always above "".
			switch {
			case !strings.HasPrefix(o.Key, prefix):
				return fail("key %q is outside the prefix", o.Key)
			case o.Key <= prev:
				return fail("key %q is not above %q", o.Key, prev)
			}
			prev = o.Key
			if err := fn(o); err != nil {
				return err
			}
		}
		if page.Next == "" {
			return nil
		}
		if _, dup := tokens[page.Next]; dup {
			return fail("token %q comes again", page.Next)
		}
		c := storeStringCharge + int64(len(page.Next))
		if err := a.budget.reserve(c); err != nil {
			return fmt.Errorf("tracepack: store source: listing %q: a token: %w", prefix, err)
		}
		charge += c
		tokens[page.Next] = struct{}{}
		token = page.Next
	}
}

// holdKey charges the key o of a listing while the source holds it.
func (a *storeAcquisition) holdKey(o ObjectInfo) (int64, error) {
	c := listedKeyCost(o.Key)
	if err := a.budget.reserve(c); err != nil {
		return 0, fmt.Errorf("tracepack: store source: listed key %q: %w", o.Key, err)
	}

	return c, nil
}

// listCommits takes traversals of lh's commit objects until two consecutive complete ones return the same keys,
// at most MaxCommitListings, parsing every key, and keeps the ids of the last one;
// it stops, keeping none, once lh is found removed by retention.
func (a *storeAcquisition) listCommits(ctx context.Context, lh *storeListedHour) error {
	if a.needless(lh.scope) {
		return nil
	}
	prefix, err := commitKeyDir(a.src.opts.Prefix, a.src.opts.Tool, a.capture, lh.hour)
	if err != nil {
		return fmt.Errorf("tracepack: store source: hour %d: %w", lh.hour, err)
	}
	var prev CommitSet
	var prevCharge int64
	for n := 0; ; n++ {
		if err := ctx.Err(); err != nil {
			return fmt.Errorf("tracepack: store source: %w", err)
		}
		if n == a.src.opts.MaxCommitListings {
			// The traversals may disagree because retention deleted the hour's commit objects meanwhile.
			if err := a.sample(ctx); err != nil {
				return err
			}
			if a.needless(lh.scope) {
				a.budget.release(prevCharge)

				return nil
			}

			return fmt.Errorf("tracepack: store source: hour %d: no two consecutive traversals of the commit objects agree within MaxCommitListings %d: %w",
				lh.hour, a.src.opts.MaxCommitListings, ErrReadLimit)
		}
		ids := CommitSet{}
		var charge int64
		removed := func() bool { return a.needless(lh.scope) }
		err := a.traverse(ctx, prefix, removed, func(o ObjectInfo) error {
			c, err := a.holdKey(o)
			if err != nil {
				return err
			}
			p, err := parseCommitKey(a.src.opts.Prefix, o.Key)
			a.budget.release(c)
			if err != nil {
				return fmt.Errorf("tracepack: store source: hour %d: %w", lh.hour, err)
			}
			// An id is charged its UUID; the map's overhead is not, as no map overhead is (the charges' comment).
			if err := a.budget.reserve(storeUUIDCharge); err != nil {
				return fmt.Errorf("tracepack: store source: hour %d: commit id %s: %w", lh.hour, p.id, err)
			}
			charge += storeUUIDCharge
			ids[p.id] = struct{}{}

			return nil
		})
		if err != nil {
			return err
		}
		if removed() {
			a.budget.release(charge + prevCharge)

			return nil
		}
		a.budget.release(prevCharge)
		if (n > 0 && maps.Equal(prev, ids)) || (a.src.stopCommits != nil && a.src.stopCommits(n)) {
			lh.commits, lh.commitCharge = ids, charge

			return nil
		}
		prev, prevCharge = ids, charge
	}
}

// listArchives traverses lh's archive packs once, parsing every key, and keeps those of the capture;
// a key of another capture belongs to another scope.
// It skips an hour found removed by retention, and stops once lh is found so.
func (a *storeAcquisition) listArchives(ctx context.Context, lh *storeListedHour) error {
	if a.needless(lh.scope) {
		return nil
	}
	prefix, err := archiveKeyDir(a.src.opts.Prefix, a.src.opts.Tool, lh.hour)
	if err != nil {
		return fmt.Errorf("tracepack: store source: hour %d: %w", lh.hour, err)
	}

	return a.traverse(ctx, prefix, func() bool { return a.needless(lh.scope) }, func(o ObjectInfo) error {
		c, err := a.holdKey(o)
		if err != nil {
			return err
		}
		p, err := parseArchiveKey(a.src.opts.Prefix, o.Key)
		if err != nil {
			return fmt.Errorf("tracepack: store source: hour %d: %w", lh.hour, err)
		}
		if p.capture != a.capture {
			a.budget.release(c)

			return nil
		}
		lh.archives = append(lh.archives, storeListed{key: o.Key, size: o.Size, k: storePackKey{capture: p.capture, pack: p.pack, hour: p.hour}})

		return nil
	})
}

// listSegments traverses the capture's staging segments once, parsing every key, and returns them in listing order;
// it stops once every listed hour is found removed by retention.
func (a *storeAcquisition) listSegments(ctx context.Context) ([]storeListed, error) {
	if a.needless(-1) {
		return nil, nil
	}
	prefix, err := stagingKeyDir(a.src.opts.Prefix, a.src.opts.Tool, a.capture)
	if err != nil {
		return nil, fmt.Errorf("tracepack: store source: %w", err)
	}
	var out []storeListed
	err = a.traverse(ctx, prefix, func() bool { return a.needless(-1) }, func(o ObjectInfo) error {
		if _, err := a.holdKey(o); err != nil {
			return err
		}
		p, err := parseSegmentKey(a.src.opts.Prefix, o.Key)
		if err != nil {
			return fmt.Errorf("tracepack: store source: %w", err)
		}
		out = append(out, storeListed{key: o.Key, size: o.Size, k: storePackKey{segment: true, capture: p.capture, pack: p.pack, seqFirst: p.seqFirst}})

		return nil
	})

	return out, err
}

// openSegment opens the segment l, reads its head, which assigns it its hour, and checks the head against l's key.
// A segment of an hour that is not listed, or removed by retention, is closed, its trailer and footer never decoded;
// one of a listed hour is opened in full over the same object, checked, and kept with its hour's packs.
// A segment gone before its head is read has no known hour, and fails Observe while a listed hour is retained.
// The charge of l stays held while the segments' entries are; a kept pack's key is charged again with its entry.
func (a *storeAcquisition) openSegment(ctx context.Context, l storeListed, byHour map[int64]*storeListedHour) error {
	if err := ctx.Err(); err != nil {
		return fmt.Errorf("tracepack: store source: %w", err)
	}

	obj, slot, err := a.openObject(ctx, l.key, l.size, -1, "staging")
	if err != nil || obj == nil {
		return err
	}
	h, err := openHead(ctx, obj, l.size, a.src.opts.Reader)
	if err != nil {
		if err := a.gone(ctx, -1, fmt.Errorf("tracepack: store source: staging: segment %q: %w", l.key, err)); err != nil {
			return err
		}

		return a.discard(slot, l.key)
	}
	hour, ok := periodHour(h.meta.PeriodStart, h.meta.PeriodEnd)
	if !ok {
		return fmt.Errorf("tracepack: store source: staging: segment %q: period %d to %d is not inside one UTC hour",
			l.key, h.meta.PeriodStart, h.meta.PeriodEnd)
	}
	if err := a.checkMeta(h.packID, h.captureID, h.meta, l.key, l.k, hour, false); err != nil {
		return err
	}
	lh := byHour[hour]
	if lh == nil || a.needless(lh.scope) {
		return a.discard(slot, l.key)
	}

	// The segment's hour is known from here on.
	a.objects[slot].scope = lh.scope
	r, err := a.bootstrap(ctx, obj, l.key, l.size, fmt.Sprintf("hour %d", hour))
	if err != nil {
		return a.gone(ctx, lh.scope, err)
	}
	a.objects[slot].r = r
	if err := a.checkPack(r, l.key, l.k, hour, false); err != nil {
		return err
	}

	return a.keep(lh, storeOpened{r: r, slot: slot, key: l.key})
}

// openArchives opens each archive pack of lh in full and checks it.
// A pack whose role takes no part in a view is closed, then reported through OnExcluded.
// The archives' entries are then dropped, their charges with them.
// It stops once lh is found removed by retention, which has dropped them already.
func (a *storeAcquisition) openArchives(ctx context.Context, lh *storeListedHour) error {
	for _, l := range lh.archives {
		if err := ctx.Err(); err != nil {
			return fmt.Errorf("tracepack: store source: %w", err)
		}
		if err := a.openArchive(ctx, lh, l); err != nil {
			return err
		}
		if a.needless(lh.scope) {
			return nil
		}
	}
	for _, l := range lh.archives {
		a.budget.release(listedKeyCost(l.key))
	}
	lh.archives = nil

	return nil
}

// openArchive opens the archive pack l of lh in full, checks it, and keeps it with lh's packs, or excludes it for its role;
// it does nothing once lh is found removed by retention.
func (a *storeAcquisition) openArchive(ctx context.Context, lh *storeListedHour, l storeListed) error {
	r, slot, err := a.openPack(ctx, l.key, l.size, lh.scope)
	if err != nil || r == nil {
		return err
	}
	if err := a.checkPack(r, l.key, l.k, lh.hour, false); err != nil {
		return err
	}
	if tierRole(r.meta.PackRole) {
		return a.keep(lh, storeOpened{r: r, slot: slot, key: l.key})
	}

	if err := a.addID(lh, UUID(r.hdr.PackID), l.key); err != nil {
		return err
	}
	ex := ExcludedPack{Key: l.key, PackID: UUID(r.hdr.PackID), Role: r.meta.PackRole, Hour: lh.hour}
	cerr := a.discard(slot, l.key)
	a.src.opts.OnExcluded(ex)
	if cerr != nil {
		return cerr
	}
	if err := ctx.Err(); err != nil {
		return fmt.Errorf("tracepack: store source: %w", err)
	}

	return nil
}

// keep keeps p among lh's packs, charging its entry and its key; its pack_id must be new to the hour.
func (a *storeAcquisition) keep(lh *storeListedHour, p storeOpened) error {
	if err := a.addID(lh, UUID(p.r.hdr.PackID), p.key); err != nil {
		return err
	}
	c := storeOpenedCharge + int64(len(p.key))
	if err := a.budget.reserve(c); err != nil {
		return fmt.Errorf("tracepack: store source: hour %d: pack %q: %w", lh.hour, p.key, err)
	}
	lh.held += c
	lh.packs = append(lh.packs, p)

	return nil
}

// addID records the pack_id id of the pack key among lh's, charging it, and fails for one recorded already.
func (a *storeAcquisition) addID(lh *storeListedHour, id UUID, key string) error {
	if _, dup := lh.ids[id]; dup {
		return fmt.Errorf("tracepack: store source: hour %d: pack %q: pack_id %s is listed twice in the scope", lh.hour, key, id)
	}
	if err := a.budget.reserve(storeUUIDCharge); err != nil {
		return fmt.Errorf("tracepack: store source: hour %d: pack %q: %w", lh.hour, key, err)
	}
	lh.held += storeUUIDCharge
	lh.ids[id] = struct{}{}

	return nil
}

// fixListed fixes lh's view: none for an hour without packs;
// else the active view of its packs, given in ascending pack_id byte order, under its commit set,
// a conflicted view fixed as conflicted with every pack closed, and every pack outside the view closed.
// It drops lh's commit set, packs and pack_ids, and releases their charges.
func (a *storeAcquisition) fixListed(ctx context.Context, lh *storeListedHour) error {
	charge := lh.commitCharge + lh.held
	lh.commitCharge, lh.held = 0, 0
	defer func() {
		lh.commits, lh.packs, lh.ids = nil, nil, nil
		a.budget.release(charge)
	}()

	if len(lh.packs) == 0 {
		a.obs.scopes[lh.scope] = storeScope{}

		return nil
	}
	slices.SortFunc(lh.packs, func(x, y storeOpened) int { return bytes.Compare(x.r.hdr.PackID[:], y.r.hdr.PackID[:]) })
	v, err := a.view(ctx, lh)
	if errors.Is(err, ErrViewConflicted) {
		for _, p := range lh.packs {
			if err := ctx.Err(); err != nil {
				return fmt.Errorf("tracepack: store source: %w", err)
			}
			if err := a.discard(p.slot, p.key); err != nil {
				return err
			}
		}
		lh.packs = nil
		a.obs.scopes[lh.scope] = storeScope{conflicted: true}

		return nil
	}
	if err != nil {
		return err
	}
	if v.Capture != a.capture || v.Hour != lh.hour {
		return fmt.Errorf("tracepack: store source: hour %d: the active view is of capture %s in hour %d", lh.hour, v.Capture, v.Hour)
	}

	// lh.packs is in pack_id order, so each pack of the view is found by a search, and marked kept in place;
	// the readers' slots are their Readers' charge.
	readers := make([]*Reader, 0, min(len(v.Packs), len(lh.packs)))
	for _, id := range v.Packs {
		if err := ctx.Err(); err != nil {
			return fmt.Errorf("tracepack: store source: %w", err)
		}
		i, ok := slices.BinarySearchFunc(lh.packs, id, func(p storeOpened, id UUID) int { return bytes.Compare(p.r.hdr.PackID[:], id[:]) })
		if !ok || lh.packs[i].kept {
			return fmt.Errorf("tracepack: store source: hour %d: the active view names pack %s, not one opened, or twice", lh.hour, id)
		}
		lh.packs[i].kept = true
		readers = append(readers, lh.packs[i].r)
	}
	for _, p := range lh.packs {
		if err := ctx.Err(); err != nil {
			return fmt.Errorf("tracepack: store source: %w", err)
		}
		if p.kept {
			continue
		}
		if err := a.discard(p.slot, p.key); err != nil {
			return err
		}
	}
	lh.packs = nil
	a.obs.scopes[lh.scope] = storeScope{readers: readers}

	return nil
}

// view returns the active view of lh's packs, in their order, under its commit set, charging their PackInfo while it runs.
// Every PackInfo is charged before any is built:
// its cost is its Reader's decoded pack metadata's, which a PackInfo decodes again from the same bytes.
func (a *storeAcquisition) view(ctx context.Context, lh *storeListedHour) (View, error) {
	var charge int64
	defer func() { a.budget.release(charge) }()
	for _, p := range lh.packs {
		if err := ctx.Err(); err != nil {
			return View{}, fmt.Errorf("tracepack: store source: %w", err)
		}
		c := storePackInfoCharge + packMetaCost(p.r.meta)
		if err := a.budget.reserve(c); err != nil {
			return View{}, fmt.Errorf("tracepack: store source: hour %d: pack %q: its PackInfo: %w", lh.hour, p.key, err)
		}
		charge += c
		a.check("charged")
	}
	infos := make([]PackInfo, 0, len(lh.packs))
	if a.listing != nil {
		defer func() { a.listing.infos = nil }()
	}
	for _, p := range lh.packs {
		if err := ctx.Err(); err != nil {
			return View{}, fmt.Errorf("tracepack: store source: %w", err)
		}
		infos = append(infos, p.r.Info())
		if a.listing != nil {
			a.listing.infos = infos
		}
		a.check("built")
	}
	if err := ctx.Err(); err != nil {
		return View{}, fmt.Errorf("tracepack: store source: %w", err)
	}
	activeView := a.src.activeView
	if activeView == nil {
		activeView = ActiveView
	}
	v, err := activeView(infos, lh.commits)
	if err != nil && !errors.Is(err, ErrViewConflicted) {
		return View{}, fmt.Errorf("tracepack: store source: hour %d: %w", lh.hour, err)
	}

	return v, err
}

// confirm asks the catalog, for each listed hour in turn, after every acquisition read of it,
// whether its scope stayed not indexed since the snapshot, then drops its token and releases its charge;
// before is called before each confirmation.
// An hour found removed by retention is not confirmed.
// An error or a scope not confirmed fails Observe.
func (a *storeAcquisition) confirm(ctx context.Context, scopes []CatalogScope, hours []*storeListedHour, before func()) error {
	for _, lh := range hours {
		if err := ctx.Err(); err != nil {
			return fmt.Errorf("tracepack: store source: %w", err)
		}
		if a.needless(lh.scope) {
			continue
		}
		before()
		ok, err := a.src.cat.ConfirmUnindexed(ctx, a.capture, lh.hour, lh.token)
		if err != nil {
			return fmt.Errorf("tracepack: store source: hour %d: confirming the scope not indexed: %w", lh.hour, err)
		}
		if !ok {
			return fmt.Errorf("tracepack: store source: hour %d: the catalog does not confirm that the scope stayed not indexed since its snapshot", lh.hour)
		}
		c := int64(len(lh.token))
		scopes[lh.scope].Token, lh.token = "", ""
		a.budget.release(c)
		a.descriptors -= c
	}

	return nil
}
