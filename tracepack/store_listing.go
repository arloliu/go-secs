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
// Its key names the source's tool, which the listing's prefix fixes, so k holds the source's own tool string.
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
func (a *storeAcquisition) observeListed(ctx context.Context, scopes []CatalogScope) error {
	var hours []*storeListedHour
	byHour := map[int64]*storeListedHour{}
	for i := range scopes {
		if !scopes[i].Indexed {
			lh := &storeListedHour{scope: i, hour: scopes[i].Hour, token: scopes[i].Token, ids: map[UUID]struct{}{}}
			hours = append(hours, lh)
			byHour[lh.hour] = lh
		}
	}
	if len(hours) == 0 {
		return nil
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

	for _, l := range segments {
		if err := a.openSegment(ctx, l, byHour); err != nil {
			return err
		}
	}
	for _, lh := range hours {
		if err := a.openArchives(ctx, lh); err != nil {
			return err
		}
	}
	for _, lh := range hours {
		if err := ctx.Err(); err != nil {
			return fmt.Errorf("tracepack: store source: %w", err)
		}
		if err := a.fixListed(ctx, lh); err != nil {
			return err
		}
	}

	return a.confirm(ctx, scopes, hours)
}

// traverse takes one complete traversal of the listing of prefix, following each page's Next until it is "",
// and passes each key to fn in order, once it has counted it against MaxObjects and checked the listing contract of ObjectStore:
// every key under prefix and above the one before it, no token repeated, and at most MaxListPages pages.
// It charges the tokens it holds until the traversal ends.
func (a *storeAcquisition) traverse(ctx context.Context, prefix string, fn func(o ObjectInfo) error) (err error) {
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
// at most MaxCommitListings, parsing every key, and keeps the ids of the last one.
func (a *storeAcquisition) listCommits(ctx context.Context, lh *storeListedHour) error {
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
			return fmt.Errorf("tracepack: store source: hour %d: no two consecutive traversals of the commit objects agree within MaxCommitListings %d: %w",
				lh.hour, a.src.opts.MaxCommitListings, ErrReadLimit)
		}
		ids := CommitSet{}
		var charge int64
		err := a.traverse(ctx, prefix, func(o ObjectInfo) error {
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
		a.budget.release(prevCharge)
		if n > 0 && maps.Equal(prev, ids) {
			lh.commits, lh.commitCharge = ids, charge

			return nil
		}
		prev, prevCharge = ids, charge
	}
}

// listArchives traverses lh's archive packs once, parsing every key, and keeps those of the capture;
// a key of another capture belongs to another scope.
func (a *storeAcquisition) listArchives(ctx context.Context, lh *storeListedHour) error {
	prefix, err := archiveKeyDir(a.src.opts.Prefix, a.src.opts.Tool, lh.hour)
	if err != nil {
		return fmt.Errorf("tracepack: store source: hour %d: %w", lh.hour, err)
	}

	return a.traverse(ctx, prefix, func(o ObjectInfo) error {
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
		lh.archives = append(lh.archives, storeListed{key: o.Key, size: o.Size, k: storePackKey{tool: a.src.opts.Tool, capture: p.capture, pack: p.pack, hour: p.hour}})

		return nil
	})
}

// listSegments traverses the capture's staging segments once, parsing every key, and returns them in listing order.
func (a *storeAcquisition) listSegments(ctx context.Context) ([]storeListed, error) {
	prefix, err := stagingKeyDir(a.src.opts.Prefix, a.src.opts.Tool, a.capture)
	if err != nil {
		return nil, fmt.Errorf("tracepack: store source: %w", err)
	}
	var out []storeListed
	err = a.traverse(ctx, prefix, func(o ObjectInfo) error {
		if _, err := a.holdKey(o); err != nil {
			return err
		}
		p, err := parseSegmentKey(a.src.opts.Prefix, o.Key)
		if err != nil {
			return fmt.Errorf("tracepack: store source: %w", err)
		}
		out = append(out, storeListed{key: o.Key, size: o.Size, k: storePackKey{segment: true, tool: a.src.opts.Tool, capture: p.capture, pack: p.pack, seqFirst: p.seqFirst}})

		return nil
	})

	return out, err
}

// openSegment opens the segment l, reads its head, which assigns it its hour, and checks the head against l's key.
// A segment of an hour that is not listed is closed, its trailer and footer never decoded, and its key's charge released;
// one of a listed hour is opened in full over the same object, checked, and kept with its hour's packs, its key's charge with it.
func (a *storeAcquisition) openSegment(ctx context.Context, l storeListed, byHour map[int64]*storeListedHour) error {
	if err := ctx.Err(); err != nil {
		return fmt.Errorf("tracepack: store source: %w", err)
	}

	obj, slot, err := a.openObject(ctx, l.key, l.size, "staging")
	if err != nil {
		return err
	}
	h, err := openHead(ctx, obj, l.size, a.src.opts.Reader)
	if err != nil {
		return fmt.Errorf("tracepack: store source: staging: segment %q: %w", l.key, err)
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
	if lh == nil {
		a.budget.release(listedKeyCost(l.key))

		return a.discard(slot, nil, l.key)
	}

	r, err := a.bootstrap(ctx, obj, l.key, l.size, fmt.Sprintf("hour %d", hour))
	if err != nil {
		return err
	}
	if err := a.checkPack(r, l.key, l.k, hour, false); err != nil {
		return err
	}

	return a.keep(lh, storeOpened{r: r, slot: slot, key: l.key})
}

// openArchives opens each archive pack of lh in full and checks it.
// A pack whose role takes no part in a view is closed, then reported through OnExcluded.
func (a *storeAcquisition) openArchives(ctx context.Context, lh *storeListedHour) error {
	for _, l := range lh.archives {
		if err := ctx.Err(); err != nil {
			return fmt.Errorf("tracepack: store source: %w", err)
		}
		if err := a.openArchive(ctx, lh, l); err != nil {
			return err
		}
	}
	lh.archives = nil

	return nil
}

// openArchive opens the archive pack l of lh in full, checks it, and keeps it with lh's packs, its key's charge with it,
// or excludes it for its role, its key's charge released.
func (a *storeAcquisition) openArchive(ctx context.Context, lh *storeListedHour, l storeListed) error {
	r, slot, err := a.openPack(ctx, l.key, l.size, lh.hour)
	if err != nil {
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
	a.budget.release(listedKeyCost(l.key))
	ex := ExcludedPack{Key: l.key, PackID: UUID(r.hdr.PackID), Role: r.meta.PackRole, Hour: lh.hour}
	cerr := a.discard(slot, r, l.key)
	a.src.opts.OnExcluded(ex)
	if cerr != nil {
		return cerr
	}
	if err := ctx.Err(); err != nil {
		return fmt.Errorf("tracepack: store source: %w", err)
	}

	return nil
}

// keep keeps p among lh's packs, charging its entry; its pack_id must be new to the hour.
// The charge of its listed key, held since the listing, stays held with it.
func (a *storeAcquisition) keep(lh *storeListedHour, p storeOpened) error {
	if err := a.addID(lh, UUID(p.r.hdr.PackID), p.key); err != nil {
		return err
	}
	if err := a.budget.reserve(storeOpenedCharge); err != nil {
		return fmt.Errorf("tracepack: store source: hour %d: pack %q: %w", lh.hour, p.key, err)
	}
	lh.held += storeOpenedCharge + listedKeyCost(p.key)
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
// It releases the charges of lh's commit set, packs and pack_ids.
func (a *storeAcquisition) fixListed(ctx context.Context, lh *storeListedHour) error {
	charge := lh.commitCharge + lh.held
	lh.commitCharge, lh.held = 0, 0
	defer a.budget.release(charge)

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
			if err := a.discard(p.slot, p.r, p.key); err != nil {
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

	byID := make(map[UUID]int, len(lh.packs))
	for i := range lh.packs {
		if err := ctx.Err(); err != nil {
			return fmt.Errorf("tracepack: store source: %w", err)
		}
		byID[UUID(lh.packs[i].r.hdr.PackID)] = i
	}
	readers := make([]*Reader, 0, len(v.Packs))
	kept := make(map[UUID]struct{}, len(v.Packs))
	for _, id := range v.Packs {
		if err := ctx.Err(); err != nil {
			return fmt.Errorf("tracepack: store source: %w", err)
		}
		i, ok := byID[id]
		if _, dup := kept[id]; !ok || dup {
			return fmt.Errorf("tracepack: store source: hour %d: the active view names pack %s, not one opened, or twice", lh.hour, id)
		}
		kept[id] = struct{}{}
		readers = append(readers, lh.packs[i].r)
	}
	for _, p := range lh.packs {
		if err := ctx.Err(); err != nil {
			return fmt.Errorf("tracepack: store source: %w", err)
		}
		if _, in := kept[UUID(p.r.hdr.PackID)]; in {
			continue
		}
		if err := a.discard(p.slot, p.r, p.key); err != nil {
			return err
		}
	}
	lh.packs = nil
	a.obs.scopes[lh.scope] = storeScope{readers: readers}

	return nil
}

// view returns the active view of lh's packs, in their order, under its commit set, charging their PackInfo while it runs.
func (a *storeAcquisition) view(ctx context.Context, lh *storeListedHour) (View, error) {
	infos := make([]PackInfo, len(lh.packs))
	var charge int64
	defer func() { a.budget.release(charge) }()
	for i, p := range lh.packs {
		if err := ctx.Err(); err != nil {
			return View{}, fmt.Errorf("tracepack: store source: %w", err)
		}
		infos[i] = p.r.Info()
		c := packInfoCost(&infos[i])
		if err := a.budget.reserve(c); err != nil {
			return View{}, fmt.Errorf("tracepack: store source: hour %d: pack %q: its PackInfo: %w", lh.hour, p.key, err)
		}
		charge += c
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
// whether its scope stayed not indexed since the snapshot, and releases the charge of its descriptor and token.
// An error or a scope not confirmed fails Observe.
func (a *storeAcquisition) confirm(ctx context.Context, scopes []CatalogScope, hours []*storeListedHour) error {
	for _, lh := range hours {
		if err := ctx.Err(); err != nil {
			return fmt.Errorf("tracepack: store source: %w", err)
		}
		ok, err := a.src.cat.ConfirmUnindexed(ctx, a.capture, lh.hour, lh.token)
		if err != nil {
			return fmt.Errorf("tracepack: store source: hour %d: confirming the scope not indexed: %w", lh.hour, err)
		}
		if !ok {
			return fmt.Errorf("tracepack: store source: hour %d: the catalog does not confirm that the scope stayed not indexed since its snapshot", lh.hour)
		}
		c := scopeDescriptorCost(&scopes[lh.scope])
		a.budget.release(c)
		a.descriptors -= c
	}

	return nil
}
