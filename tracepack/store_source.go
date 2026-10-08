package tracepack

import (
	"context"
	"errors"
	"fmt"
	"io"
	"strings"
)

// Default StoreSourceOptions values, which a zero or negative field takes.
const (
	// DefaultMaxCommitListings is the default StoreSourceOptions.MaxCommitListings: 8 traversals per scope.
	DefaultMaxCommitListings = 8
	// DefaultMaxListPages is the default StoreSourceOptions.MaxListPages: 10000 pages per traversal.
	DefaultMaxListPages = 10000
	// DefaultMaxObjects is the default StoreSourceOptions.MaxObjects: 65536 objects per Observe.
	DefaultMaxObjects = 65536
	// DefaultMaxSourceBytes is the default StoreSourceOptions.MaxSourceBytes: 256 MiB per Observe.
	DefaultMaxSourceBytes = 256 << 20
)

// ErrObjectNotFound reports an object that does not exist:
// an ObjectStore returns an error wrapping it from Open, or from a read of an Object deleted since it was opened,
// and the error of an Observe that failed on such an object keeps it, so a caller may retry the lookup
// (the tracepack storage specification §5, Coherent observation).
var ErrObjectNotFound = errors.New("tracepack: object not found")

// ObjectStore is the bucket the source NewStoreSource returns reads (the tracepack storage specification §3).
//
// It must be safe for concurrent use.
// Every value a call returns is the caller's from the moment the call returns:
// the store never changes or reuses it afterwards, and a store that pools buffers copies them before returning.
type ObjectStore interface {
	// List returns one page of the keys under prefix, each with its object's size, after token: "" for the first page.
	// A traversal follows ObjectPage.Next from page to page until it is "".
	// The keys of a traversal are in strictly ascending byte order across its pages,
	// and a complete traversal returns every key that exists under prefix throughout the traversal: it is not a snapshot.
	// A token is opaque and valid only for its traversal.
	// An error, also one returned with keys, invalidates the traversal.
	List(ctx context.Context, prefix, token string) (ObjectPage, error)
	// Open opens the object key; a missing object is an error wrapping ErrObjectNotFound.
	// On error it returns a nil Object;
	// a non-nil Object returned with an error is closed by the source all the same, its close error joined after the open error.
	Open(ctx context.Context, key string) (Object, error)
}

// ObjectPage is one page of a listing of an ObjectStore.
type ObjectPage struct {
	// Objects holds the page's keys with their sizes, in ascending byte order of their keys.
	Objects []ObjectInfo
	// Next is the token of the next page; "" on the last page.
	Next string
}

// ObjectInfo is one key of a listing, with its object's size in bytes.
type ObjectInfo struct {
	Key  string
	Size int64
}

// Object is an object an ObjectStore opened.
//
// Its ReadAt calls are bounded by the store's own deadline, which the source does not interrupt:
// the source checks its context between reads, never during one.
// ReadAt supports concurrent calls,
// and a read of an object deleted since it was opened fails with an error wrapping ErrObjectNotFound.
// Close never waits on remote I/O.
type Object interface {
	io.ReaderAt
	// Size returns the object's size in bytes.
	Size() int64
	// Close releases the object; no ReadAt follows it.
	Close() error
}

// Catalog is the caller's catalog of one tool (the tracepack storage specification §5),
// which also provides the per-capture evidence and the tool's barriers.
//
// It must be safe for concurrent use.
// Every value a call returns, its nested slices, Boundary pointers and strings included, is the caller's from the moment the call returns:
// the catalog never changes or reuses it afterwards.
type Catalog interface {
	// Snapshot returns, from one consistent snapshot of the catalog,
	// the scope of capture in each hour of [from, to), the capture's per-capture evidence and the tool's barriers,
	// which reflect every registration the catalog completed before the snapshot.
	Snapshot(ctx context.Context, capture UUID, from, to int64) (CatalogSnapshot, error)
	// ConfirmUnindexed reports whether the scope (capture, hour) has not been indexed at any instant
	// from the snapshot that issued token to the call;
	// false when the catalog cannot tell, as after a rebuild, with its history lost, or for a token it did not issue for that scope.
	ConfirmUnindexed(ctx context.Context, capture UUID, hour int64, token string) (bool, error)
}

// CatalogSnapshot is what one Catalog.Snapshot returns.
type CatalogSnapshot struct {
	// Scopes holds exactly one scope per hour of [from, to), in ascending hour.
	Scopes []CatalogScope
	// Evidence is the capture's per-capture evidence.
	Evidence CaptureEvidence
	// Barriers holds every stop-unclean boundary of a capture of the tool whose gap interval meets the hours [from, to),
	// as Observation.Barriers describes the meeting.
	Barriers []Boundary
}

// CatalogScope is one scope of a CatalogSnapshot.
type CatalogScope struct {
	// Hour is the scope's UTC hour.
	Hour int64
	// Indexed reports whether the catalog indexes the scope.
	Indexed bool
	// Conflicted reports an indexed scope whose view is conflicted (the tracepack storage specification §4);
	// set only with Indexed, and such a scope has no Packs.
	Conflicted bool
	// Packs holds the packs of an indexed scope's active view, in view order, with distinct pack_ids and keys;
	// empty for a scope not indexed or conflicted.
	Packs []CatalogPack
	// Token is the confirmation token of a scope not indexed, which ConfirmUnindexed takes; not empty for such a scope.
	Token string
}

// CatalogPack is one pack of an indexed scope's view, as the catalog records it.
type CatalogPack struct {
	// Key is the pack's object key: a segment's or an archive's (the tracepack storage specification §3).
	Key string
	// PackID is the pack's pack_id.
	PackID UUID
	// Size is the object's size in bytes.
	Size int64
}

// ExcludedPack is a listed pack that the source NewStoreSource returns excluded from a scope's view for its role
// (the tracepack storage specification §2), which StoreSourceOptions.OnExcluded reports.
type ExcludedPack struct {
	// Key is the pack's object key.
	Key string
	// PackID is the pack's pack_id.
	PackID UUID
	// Role is the pack's pack_role.
	Role PackRole
	// Hour is the UTC hour of the pack's period.
	Hour int64
}

// StoreSourceOptions configures NewStoreSource.
//
// A zero or negative limit takes its default.
type StoreSourceOptions struct {
	// Prefix is the bucket prefix of every key (the tracepack storage specification §3), used byte for byte:
	// empty, or not ending in '/'.
	Prefix string
	// Tool is the tool_id the source serves, as recorded; not "", "." or "..".
	Tool string
	// Reader configures the Open of every pack; its FooterOffset, a hint for one object, must be 0.
	Reader ReaderOptions
	// MaxCommitListings is the largest number of traversals of one scope's commit objects an Observe takes
	// while it waits for two consecutive complete ones that agree; default 8.
	// It must not be 1, which no Observe of a scope the catalog does not index could meet.
	MaxCommitListings int
	// MaxListPages is the largest number of pages of one traversal; default 10000.
	MaxListPages int
	// MaxObjects is the largest number of objects one Observe handles:
	// every key any of its listings returns, a key listed again counted again, and every object it opens;
	// default 65536.
	MaxObjects int
	// MaxSourceBytes bounds the state one Observe holds and its Observation keeps, in bytes; default 256 MiB.
	// The state is charged from the structures the source decodes and keeps, not from their encoded lengths:
	// the catalog's scope and pack descriptors while it uses them, a scope's token until its confirmation,
	// the state of each listed hour, the keys, tokens and commit ids of the listings,
	// and the slot of each object opened, while it holds them; the copied evidence and barriers,
	// every retained Reader, from its pack metadata, block index, footer summaries and statistics,
	// and the PackInfo it builds for ActiveView while ActiveView runs.
	// Not charged are the transient buffers and decode temporaries of one Open,
	// which ReaderOptions bounds and which the source holds for one Open at a time, so an Observe may exceed the limit by one Open's;
	// the workspace of ActiveView, released when it returns;
	// and whatever the ObjectStore and the Catalog allocate.
	MaxSourceBytes int64
	// OnExcluded reports each listed pack the source excludes from a scope's view for its role
	// (the tracepack storage specification §2), once per pack per Observe, after the source has closed it,
	// also when that Observe later fails; required.
	// It is called synchronously from the goroutine running Observe, possibly concurrently from concurrent Observe calls,
	// so it must be safe for concurrent use and return promptly, handing the report off rather than doing I/O.
	// The value it gets is its own.
	// Observe checks its context when it returns,
	// and a panic in it propagates after Observe has closed every object it opened.
	// A catalog reports the packs of the scopes it indexes itself.
	OnExcluded func(ExcludedPack)
	// Retention, when set, provides the retention boundary Observe applies (the tracepack storage specification §5, Retention);
	// nil removes nothing.
	// Observe asks it before every page of every listing, the commit listings included, before every object it opens,
	// after a listed scope's last listing and read and before its view is computed,
	// and once more after every scope is fixed.
	// Each answer is compared with every hour of the observation, against the largest boundary seen:
	// a scope whose hour is removed is reported as SourceScope.Removed, with no Readers,
	// its objects closed at once and their charges released, and nothing more is listed or opened for it;
	// a scope found removed by the last answer loses the view fixed for it.
	// The staging listing and its segments, which only the scopes the catalog does not index use,
	// stop once all of those scopes are removed.
	// When an object turns out gone from the store, Observe asks it again:
	// an object whose hour is known, a pack the catalog names, an archive key, or a segment whose head was read,
	// marks its scope removed when that answer shows its hour removed, instead of failing Observe;
	// a segment gone before its head was read, whose hour is unknown, fails Observe with an error wrapping ErrObjectNotFound
	// while a scope the catalog does not index is retained.
	// An error from it fails Observe, wrapped.
	// It promises nothing about the boundary advancing after Observe returns:
	// give TxOptions.Retention the same provider, so the lookup checks it from there on.
	Retention Retention
}

// storeSource is the PackSource NewStoreSource returns; each Observe owns its state.
// It is immutable once built, but for activeView, stopCommits and checkpoint, which only tests set, before they observe through it.
type storeSource struct {
	store ObjectStore
	cat   Catalog
	// opts holds the options with every limit defaulted.
	opts StoreSourceOptions
	// activeView computes a listed hour's view; nil means ActiveView.
	// Tests replace it to reach the checks of a view that ActiveView never returns.
	activeView func(packs []PackInfo, commits CommitSet) (View, error)
	// stopCommits, when set, ends a listed hour's commit listing after the traversal n, from 0, it reports true for,
	// keeping that traversal's ids; nil means two consecutive traversals must agree.
	// Tests set it to reach an acquisition that takes the commit objects of one traversal.
	stopCommits func(n int) bool
	// checkpoint, when set, is called with the acquisition, where it is called from and its listing state:
	// once the listings are taken ("listed"), after each listed hour's view is fixed ("viewed"), before each confirmation ("confirming"),
	// and while a view's PackInfo are prepared, after each is charged ("charged") and after each is built ("built").
	// Tests set it to measure what an acquisition holds against what it charged.
	checkpoint func(a *storeAcquisition, where string, held *storeListingState)
}

var _ PackSource = (*storeSource)(nil)

// storeAcquisition is the state of one Observe of a storeSource.
type storeAcquisition struct {
	src      *storeSource
	capture  UUID
	from, to int64
	budget   storeBudget
	// objects holds every object opened, in the order opened, with a nil object in the slot of one discarded since;
	// the Observation closes them, or Observe does when it fails.
	objects []storeSlot
	// descriptors is the charge of the catalog's scope and pack descriptors, released once the observation is built.
	descriptors int64
	// slots is the charge of the slots of objects, released once the observation is built,
	// where a retained object's slot is its Reader's.
	slots int64
	obs   *storeObservation
	// listing is the listing state a test's checkpoint measures; nil without a checkpoint.
	listing *storeListingState
	// retention is what the acquisition knows of StoreSourceOptions.Retention's boundary.
	retention retentionState
	// removedTo is the number of the observation's hours, from the first, found removed by retention:
	// the boundary never moves back, so they are always the first ones.
	removedTo int
	// hours holds the listed hours, in ascending hour, while the listed scopes are observed; nil otherwise.
	hours []*storeListedHour
}

// storeSlot is the slot of an object the acquisition opened.
type storeSlot struct {
	// obj is the object; nil once it is discarded.
	obj Object
	// r is the Reader over obj once it is charged; nil before, and for a segment opened for its head only.
	r *Reader
	// scope is the index of the object's hour among the observation's hours;
	// -1 for a staging segment whose head is not read yet, or whose hour is not observed.
	scope int
}

// storePackKey is what the key of a pack names: a staging segment's, or an archive's.
type storePackKey struct {
	// segment reports a key under staging/; seqFirst is set only for it, hour only for an archive key.
	segment  bool
	tool     string
	capture  UUID
	pack     UUID
	seqFirst uint64
	hour     int64
}

// NewStoreSource returns the PackSource of one tool over a bucket and the caller's catalog (the tracepack storage specification §5).
//
// Each Observe takes one catalog snapshot.
// It fixes each scope the catalog indexes from that snapshot:
// it opens each pack of the scope's view, in view order, and checks it against its key and its descriptor.
// A scope the catalog does not index is fixed by the coherent observation of the tracepack storage specification §5 and its listing view:
// its commit objects are listed until two consecutive complete listings agree,
// then its archive packs and the capture's staging segments are listed once;
// each segment's head, its file header and pack metadata, is read to learn its hour,
// and a segment of an hour not observed this way is closed, its trailer and footer never decoded;
// the packs are opened and checked, and ActiveView takes them in ascending pack_id byte order;
// a conflicted view is fixed as conflicted, without readers;
// and last the catalog must confirm that the scope stayed not indexed since the snapshot.
// A listed pack whose role takes no part in a view is closed and reported through OnExcluded.
// Every key a listing returns is parsed before it is classified, and an archive key of another capture is skipped;
// a listing that breaks the contract of ObjectStore — a key outside its prefix or not above the one before it, a token that comes again —
// fails the Observe.
// Every pack opened must agree with its key, and with its descriptor when the catalog indexes its scope,
// on pack_id and capture_id; its tool_id is Tool; its period lies in one UTC hour, the scope's;
// a key under staging/ holds a segment, and a key under archive/ holds a pack of another role;
// a segment key's seq_first is the first seq of the first block found,
// or the seq_start of a segment known to hold no record,
// and is not compared when damage hides the first seq, which the reads over the scope then report;
// its size is the one the catalog or the listing gives;
// and its scope_generation suits its role, as ActiveView checks it.
// A pack the catalog indexes must have a role that takes part in a scope's view (the tracepack storage specification §2).
// Any disagreement fails the Observe, and so does a pack that is gone when opened or read, with an error wrapping ErrObjectNotFound,
// which a caller may retry, unless StoreSourceOptions.Retention shows its hour removed,
// and a key a listing returns or the catalog names that does not parse as a key of the tracepack storage specification §3,
// with an error wrapping ErrInvalidKey, which a retry does not cure.
// An Observe that fails closes every object it opened, and returns the close errors joined after its first error.
//
// The source's guarantees hold while every hour observed stays retained (the tracepack storage specification §5, Retention)
// from the start of Observe until the lookup's last use of the Observation;
// enforcing that is the caller's.
// With StoreSourceOptions.Retention set, Observe reports a scope whose hour it finds removed as SourceScope.Removed, with no Readers,
// so no view is ever formed from an hour partly deleted;
// from its return on, the lookup's own TxOptions.Retention checks the boundary.
// The source and its Observations are safe for concurrent use: each Observe owns its state.
// Observe takes a capture_id that is not zero and hours [from, to) with from < to within [MinTxHour, MaxTxHour+1),
// the hours a lookup reads; other arguments fail it before the catalog is asked.
//
// Parameters:
//   - store: the bucket; not nil.
//   - cat: the catalog of opts.Tool; not nil.
//   - opts: see StoreSourceOptions; OnExcluded is required.
//
// Returns:
//   - PackSource: the source; nil on error.
//   - error: non-nil for a nil store or cat, a nil OnExcluded, a Prefix ending in '/',
//     a Tool a key cannot hold (wrapping ErrInvalidKey), a MaxCommitListings of 1, or a non-zero Reader.FooterOffset.
func NewStoreSource(store ObjectStore, cat Catalog, opts StoreSourceOptions) (PackSource, error) {
	switch {
	case store == nil:
		return nil, errors.New("tracepack: new store source: the object store is nil")
	case cat == nil:
		return nil, errors.New("tracepack: new store source: the catalog is nil")
	case opts.OnExcluded == nil:
		return nil, errors.New("tracepack: new store source: OnExcluded is nil")
	case strings.HasSuffix(opts.Prefix, "/"):
		return nil, fmt.Errorf("tracepack: new store source: prefix %q ends in '/'", opts.Prefix)
	case opts.Reader.FooterOffset != 0:
		return nil, fmt.Errorf("tracepack: new store source: Reader.FooterOffset %d is a hint for one object, not for every pack", opts.Reader.FooterOffset)
	case opts.MaxCommitListings == 1:
		return nil, errors.New("tracepack: new store source: MaxCommitListings 1 leaves no second traversal to agree with the first")
	}
	if err := checkKeyTool(opts.Tool); err != nil {
		return nil, fmt.Errorf("tracepack: new store source: %w", keyArgError(err))
	}
	opts.MaxCommitListings = positiveOr(opts.MaxCommitListings, DefaultMaxCommitListings)
	opts.MaxListPages = positiveOr(opts.MaxListPages, DefaultMaxListPages)
	opts.MaxObjects = positiveOr(opts.MaxObjects, DefaultMaxObjects)
	opts.MaxSourceBytes = positiveOr(opts.MaxSourceBytes, DefaultMaxSourceBytes)

	return &storeSource{store: store, cat: cat, opts: opts}, nil
}

// parsePackKey parses key as the key of a pack under prefix: a segment's under staging/, else an archive's.
// Its error wraps ErrInvalidKey.
func parsePackKey(prefix, key string) (storePackKey, error) {
	if strings.HasPrefix(key, keyArea(prefix, "staging")) {
		p, err := parseSegmentKey(prefix, key)
		if err != nil {
			return storePackKey{}, err
		}

		return storePackKey{segment: true, tool: p.tool, capture: p.capture, pack: p.pack, seqFirst: p.seqFirst}, nil
	}
	p, err := parseArchiveKey(prefix, key)
	if err != nil {
		return storePackKey{}, err
	}

	return storePackKey{tool: p.tool, capture: p.capture, pack: p.pack, hour: p.hour}, nil
}

// firstSeq returns the first seq of the pack r reads, the seq a segment key's seq_first states
// (the tracepack storage specification §3):
// the first seq of the first block found;
// else, for a pack known to hold no record, its seq_start;
// and false when damage hides it.
// A pack without blocks is known to hold no record when r carries no defect from opening it:
// it uses the footer, whose validation checked the blocks against the trailer,
// or its forward walk reached the end of a finalized pack and agrees with the trailer's block and record counts.
// A walk that stopped, or a pack that is not finalized, leaves its records unknown.
func firstSeq(r *Reader) (uint64, bool) {
	switch {
	case len(r.blocks) > 0:
		return r.blocks[0].FirstSeq, true
	case len(r.openDefects) == 0:
		return r.meta.SeqStart, true
	default:
		return 0, false
	}
}

// Observe fixes the scopes of capture over the hours [from, to), the capture's evidence and the tool's barriers.
// On error it has closed every object it opened, also when a callback it called panics.
func (s *storeSource) Observe(ctx context.Context, capture UUID, from, to int64) (obs Observation, err error) {
	if err := checkObserve(capture, from, to); err != nil {
		return nil, fmt.Errorf("tracepack: store source: %w", err)
	}
	if err := ctx.Err(); err != nil {
		return nil, fmt.Errorf("tracepack: store source: %w", err)
	}

	a := &storeAcquisition{
		src: s, capture: capture, from: from, to: to,
		budget:    storeBudget{maxBytes: s.opts.MaxSourceBytes, maxObjects: s.opts.MaxObjects},
		retention: newRetentionState(s.opts.Retention),
	}
	done := false
	defer func() {
		if !done {
			err = a.abort(err)
		}
	}()

	o, err := a.run(ctx)
	if err != nil {
		return nil, err
	}
	done = true

	return o, nil
}

// checkObserve checks the arguments of a PackSource's Observe: a capture_id that is not zero, and hours [from, to) not empty,
// within [MinTxHour, MaxTxHour], the hours a lookup reads.
// Its error names no source; the caller adds its own.
func checkObserve(capture UUID, from, to int64) error {
	switch {
	case capture == UUID{}:
		return errors.New("the capture_id is zero")
	case from >= to:
		return fmt.Errorf("hours [%d, %d) are empty", from, to)
	case from < MinTxHour || to-1 > MaxTxHour:
		return fmt.Errorf("hours [%d, %d) are not within [%d, %d]", from, to, MinTxHour, MaxTxHour)
	}

	return nil
}

// run takes the catalog snapshot, opens the packs of the indexed scopes, observes the others,
// samples the retention boundary a last time, and returns the observation built from them.
func (a *storeAcquisition) run(ctx context.Context) (*storeObservation, error) {
	snap, err := a.src.cat.Snapshot(ctx, a.capture, a.from, a.to)
	if err != nil {
		return nil, fmt.Errorf("tracepack: store source: catalog snapshot: %w", err)
	}
	if err := a.takeSnapshot(ctx, &snap); err != nil {
		return nil, err
	}
	if err := a.openIndexed(ctx, snap.Scopes); err != nil {
		return nil, err
	}
	if err := a.observeListed(ctx, snap.Scopes); err != nil {
		return nil, err
	}
	// Every scope is fixed: the final sample classifies each against one boundary, discarding the views of those removed.
	if err := a.sample(ctx); err != nil {
		return nil, err
	}

	a.budget.release(a.descriptors + a.slots)
	a.descriptors, a.slots = 0, 0
	for _, s := range a.objects {
		if s.obj != nil {
			a.obs.objects = append(a.obs.objects, s.obj)
		}
	}
	a.objects = nil
	a.obs.charged, a.obs.peak = a.budget.used, a.budget.peak

	return a.obs, nil
}

// takeSnapshot checks the snapshot's scopes and their descriptors, charging them,
// then builds the observation with a copy of the evidence and the barriers.
func (a *storeAcquisition) takeSnapshot(ctx context.Context, snap *CatalogSnapshot) error {
	n := a.to - a.from
	if int64(len(snap.Scopes)) != n {
		return fmt.Errorf("tracepack: store source: the catalog snapshot holds %d scopes for the %d hours [%d, %d)",
			len(snap.Scopes), n, a.from, a.to)
	}
	for i := range snap.Scopes {
		if err := ctx.Err(); err != nil {
			return fmt.Errorf("tracepack: store source: %w", err)
		}
		if err := a.checkScope(ctx, &snap.Scopes[i], a.from+int64(i)); err != nil {
			return err
		}
	}

	if err := a.budget.reserve(storeObservationCharge + n*storeScopeCharge); err != nil {
		return fmt.Errorf("tracepack: store source: the observation's scopes: %w", err)
	}
	a.obs = &storeObservation{from: a.from, to: a.to, scopes: make([]storeScope, n)}
	evidence, err := a.copyEvidence(ctx, &snap.Evidence)
	if err != nil {
		return err
	}
	a.obs.evidence = evidence
	barriers, err := a.copyBoundaries(ctx, snap.Barriers, "barrier")
	if err != nil {
		return err
	}
	a.obs.barriers = barriers
	// The copies are the observation's; the snapshot's own are dropped.
	snap.Evidence, snap.Barriers = CaptureEvidence{}, nil

	return nil
}

// checkScope checks the snapshot's scope sc of hour and the keys of its packs, charging its descriptors:
// Conflicted only with Indexed, Packs only for an indexed scope that is not conflicted, a Token for a scope not indexed,
// and within the scope distinct pack_ids and keys, each key a pack's of the source's tool, of the capture and, for an archive, of hour.
func (a *storeAcquisition) checkScope(ctx context.Context, sc *CatalogScope, hour int64) error {
	switch {
	case sc.Hour != hour:
		return fmt.Errorf("tracepack: store source: the catalog snapshot's scope of hour %d comes where hour %d's should", sc.Hour, hour)
	case sc.Conflicted && !sc.Indexed:
		return fmt.Errorf("tracepack: store source: hour %d: the catalog reports a scope it does not index as conflicted", hour)
	case len(sc.Packs) > 0 && (!sc.Indexed || sc.Conflicted):
		return fmt.Errorf("tracepack: store source: hour %d: the catalog lists packs of a scope it does not index or reports conflicted", hour)
	case !sc.Indexed && sc.Token == "":
		return fmt.Errorf("tracepack: store source: hour %d: the catalog gives no token for a scope it does not index", hour)
	}
	if err := a.chargeDescriptor(scopeDescriptorCost(sc), hour); err != nil {
		return err
	}
	if len(sc.Packs) == 0 {
		return nil
	}

	// The maps grow as each descriptor, its entries' charge included, is charged.
	ids := make(map[UUID]struct{})
	keys := make(map[string]struct{})
	for j := range sc.Packs {
		if err := ctx.Err(); err != nil {
			return fmt.Errorf("tracepack: store source: %w", err)
		}
		p := &sc.Packs[j]
		if err := a.chargeDescriptor(packDescriptorCost(p), hour); err != nil {
			return err
		}
		if _, dup := ids[p.PackID]; dup {
			return fmt.Errorf("tracepack: store source: hour %d: the catalog lists pack_id %s twice", hour, p.PackID)
		}
		ids[p.PackID] = struct{}{}
		if _, dup := keys[p.Key]; dup {
			return fmt.Errorf("tracepack: store source: hour %d: the catalog lists key %q twice", hour, p.Key)
		}
		keys[p.Key] = struct{}{}
		if _, err := a.packKey(p.Key, hour); err != nil {
			return err
		}
	}

	return nil
}

// chargeDescriptor charges n bytes of the catalog's descriptors of hour, held until the observation is built.
func (a *storeAcquisition) chargeDescriptor(n, hour int64) error {
	if err := a.budget.reserve(n); err != nil {
		return fmt.Errorf("tracepack: store source: hour %d: the catalog's descriptors: %w", hour, err)
	}
	a.descriptors += n

	return nil
}

// packKey parses key as the key of a pack of hour's scope:
// the source's tool, the observation's capture and, for an archive, hour.
func (a *storeAcquisition) packKey(key string, hour int64) (storePackKey, error) {
	k, err := parsePackKey(a.src.opts.Prefix, key)
	if err != nil {
		return storePackKey{}, fmt.Errorf("tracepack: store source: hour %d: %w", hour, err)
	}
	switch {
	case k.tool != a.src.opts.Tool:
		return storePackKey{}, fmt.Errorf("tracepack: store source: hour %d: key %q names tool %q, not %q", hour, key, k.tool, a.src.opts.Tool)
	case k.capture != a.capture:
		return storePackKey{}, fmt.Errorf("tracepack: store source: hour %d: key %q names capture %s, not %s", hour, key, k.capture, a.capture)
	case !k.segment && k.hour != hour:
		return storePackKey{}, fmt.Errorf("tracepack: store source: hour %d: key %q names hour %d", hour, key, k.hour)
	}

	return k, nil
}

// copyEvidence returns a deep copy of e, charging it.
func (a *storeAcquisition) copyEvidence(ctx context.Context, e *CaptureEvidence) (CaptureEvidence, error) {
	out := CaptureEvidence{End: e.End, Partial: e.Partial}
	bs, err := a.copyBoundaries(ctx, e.Boundaries, "evidence boundary")
	if err != nil {
		return CaptureEvidence{}, err
	}
	out.Boundaries = bs
	for i := range e.Closures {
		if err := ctx.Err(); err != nil {
			return CaptureEvidence{}, fmt.Errorf("tracepack: store source: %w", err)
		}
		if err := a.budget.reserve(storeClosureCharge); err != nil {
			return CaptureEvidence{}, fmt.Errorf("tracepack: store source: an evidence closure: %w", err)
		}
		out.Closures = append(out.Closures, e.Closures[i])
	}

	return out, nil
}

// copyBoundaries returns a deep copy of bs, nil for an empty bs, charging each entry before it is copied.
func (a *storeAcquisition) copyBoundaries(ctx context.Context, bs []Boundary, what string) ([]Boundary, error) {
	var out []Boundary
	for i := range bs {
		if err := ctx.Err(); err != nil {
			return nil, fmt.Errorf("tracepack: store source: %w", err)
		}
		if err := a.budget.reserve(boundaryCost(&bs[i])); err != nil {
			return nil, fmt.Errorf("tracepack: store source: a copied %s: %w", what, err)
		}
		out = append(out, cloneBoundary(bs[i]))
	}

	return out, nil
}

// openIndexed opens the packs of every indexed scope that is not conflicted, in the catalog's order, and checks each;
// a conflicted scope is fixed as conflicted, without readers.
// A scope found removed by retention is skipped, and the opening of its packs stops.
func (a *storeAcquisition) openIndexed(ctx context.Context, scopes []CatalogScope) error {
	for i := range scopes {
		if err := ctx.Err(); err != nil {
			return fmt.Errorf("tracepack: store source: %w", err)
		}
		sc := &scopes[i]
		if !sc.Indexed || a.needless(i) {
			continue
		}
		if sc.Conflicted {
			a.obs.scopes[i] = storeScope{indexed: true, conflicted: true}

			continue
		}
		// The readers join the scope as they are opened, so a removal of the scope meanwhile finds them.
		a.obs.scopes[i].indexed = true
		for j := range sc.Packs {
			if err := ctx.Err(); err != nil {
				return fmt.Errorf("tracepack: store source: %w", err)
			}
			r, err := a.openDescribed(ctx, &sc.Packs[j], i)
			if err != nil {
				return err
			}
			if r == nil {
				break
			}
			a.obs.scopes[i].readers = append(a.obs.scopes[i].readers, r)
		}
	}

	return nil
}

// openDescribed opens the pack the catalog describes as p, of the scope of index scope, and checks it against its key and p.
// It returns a nil Reader and no error when the scope is found removed by retention.
func (a *storeAcquisition) openDescribed(ctx context.Context, p *CatalogPack, scope int) (*Reader, error) {
	hour := a.from + int64(scope)
	k, err := a.packKey(p.Key, hour)
	if err != nil {
		return nil, err
	}
	r, _, err := a.openPack(ctx, p.Key, p.Size, scope)
	if err != nil || r == nil {
		return nil, err
	}
	if UUID(r.hdr.PackID) != p.PackID {
		return nil, fmt.Errorf("tracepack: store source: hour %d: pack %q: file header pack_id %s, the catalog's %s",
			hour, p.Key, UUID(r.hdr.PackID), p.PackID)
	}
	if err := a.checkPack(r, p.Key, k, hour, true); err != nil {
		return nil, err
	}

	return r, nil
}

// openPack opens the object key of size bytes, as the catalog or a listing gives it, of the scope of index scope,
// and bootstraps a Reader over it, charging the object and the Reader.
// It returns the slot of the object among the acquisition's objects,
// or a nil Reader and no error when the scope is found removed by retention.
func (a *storeAcquisition) openPack(ctx context.Context, key string, size int64, scope int) (*Reader, int, error) {
	where := fmt.Sprintf("hour %d", a.from+int64(scope))
	obj, slot, err := a.openObject(ctx, key, size, scope, where)
	if err != nil || obj == nil {
		return nil, 0, err
	}
	r, err := a.bootstrap(ctx, obj, key, size, where)
	if err != nil {
		return nil, 0, a.gone(ctx, scope, err)
	}
	a.objects[slot].r = r

	return r, slot, nil
}

// openObject opens the object key of size bytes, as the catalog or a listing gives it, of the scope of index scope,
// -1 for a staging segment, counting it, and checks its size;
// where names the scope or the listing it belongs to in errors.
// It first samples the retention boundary, and opens nothing, returning a nil Object and no error,
// when the acquisition no longer needs the object (needless).
// The object is kept for closing as soon as it is opened, in the slot returned, and one returned with an error is closed at once.
func (a *storeAcquisition) openObject(ctx context.Context, key string, size int64, scope int, where string) (Object, int, error) {
	if err := a.sample(ctx); err != nil {
		return nil, 0, err
	}
	if a.needless(scope) {
		return nil, 0, nil
	}
	if err := a.budget.count(); err != nil {
		return nil, 0, fmt.Errorf("tracepack: store source: %s: opening %q: %w", where, key, err)
	}
	obj, err := a.src.store.Open(ctx, key)
	if err != nil {
		var cerr error
		if obj != nil {
			cerr = obj.Close()
		}
		if err := a.gone(ctx, scope, fmt.Errorf("tracepack: store source: %s: open %q: %w", where, key, errors.Join(err, cerr))); err != nil {
			return nil, 0, err
		}
		// The object is gone with its removed hour, but a close that failed still fails Observe.
		if cerr != nil {
			return nil, 0, fmt.Errorf("tracepack: store source: %s: close %q: %w", where, key, cerr)
		}

		return nil, 0, nil
	}
	if obj == nil {
		return nil, 0, fmt.Errorf("tracepack: store source: %s: open %q returned no object", where, key)
	}
	// The slot is charged before the object is kept, so a failure here closes it at once.
	if err := a.budget.reserve(storeObjectSlotCharge); err != nil {
		if cerr := obj.Close(); cerr != nil {
			err = errors.Join(err, cerr)
		}

		return nil, 0, fmt.Errorf("tracepack: store source: %s: opening %q: %w", where, key, err)
	}
	a.slots += storeObjectSlotCharge
	slot := len(a.objects)
	a.objects = append(a.objects, storeSlot{obj: obj, scope: scope})

	if got := obj.Size(); got != size {
		return nil, 0, fmt.Errorf("tracepack: store source: %s: object %q holds %d bytes, not the %d given for it", where, key, got, size)
	}

	return obj, slot, nil
}

// bootstrap opens a Reader over obj, the object key of size bytes, and charges it; where names its scope in errors.
func (a *storeAcquisition) bootstrap(ctx context.Context, obj Object, key string, size int64, where string) (*Reader, error) {
	r, err := Open(ctx, obj, size, a.src.opts.Reader)
	if err != nil {
		return nil, fmt.Errorf("tracepack: store source: %s: pack %q: %w", where, key, err)
	}
	if err := a.budget.reserve(readerCost(r)); err != nil {
		return nil, fmt.Errorf("tracepack: store source: %s: pack %q: %w", where, key, err)
	}

	return r, nil
}

// discard closes the object in slot, of key, which the acquisition no longer keeps, releasing the charge of its Reader when it has one.
// Its close error fails Observe.
func (a *storeAcquisition) discard(slot int, key string) error {
	if err := a.closeSlot(slot); err != nil {
		return fmt.Errorf("tracepack: store source: close %q: %w", key, err)
	}

	return nil
}

// closeSlot closes the object in slot, which the acquisition no longer keeps, releasing the charge of its Reader when it has one,
// and returns the close error.
func (a *storeAcquisition) closeSlot(slot int) error {
	s := &a.objects[slot]
	obj := s.obj
	if s.r != nil {
		a.budget.release(readerCost(s.r))
	}
	*s = storeSlot{scope: -1}

	return obj.Close()
}

// checkPack checks the pack r opened from key, which names k, against k and hour's scope
// (the tracepack storage specification §2 and §3), as checkMeta does,
// and checks a segment's first seq, when known (firstSeq), against the key's seq_first.
func (a *storeAcquisition) checkPack(r *Reader, key string, k storePackKey, hour int64, indexed bool) error {
	if err := a.checkMeta(UUID(r.hdr.PackID), UUID(r.hdr.CaptureID), r.meta, key, k, hour, indexed); err != nil {
		return err
	}
	if first, known := firstSeq(r); k.segment && known && first != k.seqFirst {
		return fmt.Errorf("tracepack: store source: hour %d: pack %q: first seq %d, not the key's seq_first %d", hour, key, first, k.seqFirst)
	}

	return nil
}

// checkMeta checks the file header ids id and capture and the pack metadata m of the pack opened from key, which names k,
// against k and hour's scope (the tracepack storage specification §2 and §3):
// the file header's pack_id and capture_id are the key's, its tool_id is the source's,
// its period lies in one UTC hour, which is hour;
// a segment is under staging/, and any other pack under archive/;
// and a pack whose role takes part in a scope's view has the scope_generation its role requires.
// A pack whose role takes no part in a view is an error when indexed is set.
func (a *storeAcquisition) checkMeta(id, capture UUID, m *PackMeta, key string, k storePackKey, hour int64, indexed bool) error {
	fail := func(format string, args ...any) error {
		return fmt.Errorf("tracepack: store source: hour %d: pack %q: %s", hour, key, fmt.Sprintf(format, args...))
	}
	switch {
	case id != k.pack:
		return fail("file header pack_id %s, not the key's %s", id, k.pack)
	case capture != k.capture:
		return fail("file header capture_id %s, not the key's %s", capture, k.capture)
	case m.ToolID != a.src.opts.Tool:
		return fail("tool_id %q, not %q", m.ToolID, a.src.opts.Tool)
	}
	ph, ok := periodHour(m.PeriodStart, m.PeriodEnd)
	switch {
	case !ok:
		return fail("period %d to %d is not inside one UTC hour", m.PeriodStart, m.PeriodEnd)
	case ph != hour:
		return fail("period in hour %d", ph)
	case k.segment && m.PackRole != PackRoleSegment:
		return fail("pack_role %s under staging/", m.PackRole)
	case !k.segment && m.PackRole == PackRoleSegment:
		return fail("pack_role %s under archive/", m.PackRole)
	}
	if !tierRole(m.PackRole) {
		if indexed {
			return fail("pack_role %s takes no part in a scope's view", m.PackRole)
		}

		return nil
	}
	if err := checkRoleGeneration(m); err != nil {
		return fail("%v", err)
	}

	return nil
}

// abort closes every object the acquisition opened, each once, and returns err with the close errors joined after it.
// It runs on every path out of Observe but success, a panic included.
func (a *storeAcquisition) abort(err error) error {
	errs := []error{err}
	for _, s := range a.objects {
		if s.obj == nil {
			continue
		}
		if cerr := s.obj.Close(); cerr != nil {
			errs = append(errs, fmt.Errorf("tracepack: store source: close: %w", cerr))
		}
	}
	a.objects = nil
	if len(errs) == 1 {
		return err
	}

	return errors.Join(errs...)
}

// sample asks StoreSourceOptions.Retention for the boundary, when set (the tracepack storage specification §5, Retention),
// and classifies every hour of the observation against the largest boundary seen:
// each hour newly found removed is fixed as removed at once, before any more acquisition,
// its objects closed and the charges of its readers and its listing state released.
//
// Returns:
//   - error: nil; the provider's error, wrapped; or the error of closing an object of a removed hour.
func (a *storeAcquisition) sample(ctx context.Context) error {
	if err := a.retention.sample(ctx); err != nil {
		return fmt.Errorf("tracepack: store source: %w", err)
	}
	lo := a.removedTo
	for a.removedTo < len(a.obs.scopes) && a.retention.removedHour(a.from+int64(a.removedTo)) {
		a.removedTo++
	}
	if a.removedTo == lo {
		return nil
	}

	return a.remove(lo, a.removedTo)
}

// remove fixes the hours of index [lo, hi) as removed, with no readers:
// it drops their listing state, releasing its charges,
// and closes every object of theirs, each once, releasing the charges of their Readers.
//
// Returns:
//   - error: nil, or the first close error.
func (a *storeAcquisition) remove(lo, hi int) error {
	for i := lo; i < hi; i++ {
		a.obs.scopes[i] = storeScope{removed: true}
	}
	for _, lh := range a.hours {
		if lh.scope >= lo && lh.scope < hi {
			a.dropListed(lh)
		}
	}
	for slot := range a.objects {
		s := &a.objects[slot]
		if s.obj == nil || s.scope < lo || s.scope >= hi {
			continue
		}
		hour := a.from + int64(s.scope)
		if err := a.closeSlot(slot); err != nil {
			return fmt.Errorf("tracepack: store source: hour %d: removed by retention: close: %w", hour, err)
		}
	}

	return nil
}

// dropListed drops what Observe gathered of the listed hour lh, removed by retention, and releases its charges:
// its commit set, its archive keys, its packs and their pack_ids.
// The objects of its packs are closed by the caller.
// Its token stays charged with the catalog's descriptors.
func (a *storeAcquisition) dropListed(lh *storeListedHour) {
	charge := lh.commitCharge + lh.held
	for _, l := range lh.archives {
		charge += listedKeyCost(l.key)
	}
	a.budget.release(charge)
	lh.commitCharge, lh.held = 0, 0
	lh.commits, lh.archives, lh.packs, lh.ids = nil, nil, nil, nil
}

// needless reports whether the acquisition no longer needs the work of the hour of index scope, found removed by retention;
// for scope -1, the capture-wide staging listing and its segments, whether no listed hour is still retained.
func (a *storeAcquisition) needless(scope int) bool {
	if scope >= 0 {
		return scope < a.removedTo
	}
	// The listed hours are in ascending hour, and the removed hours are the first ones.
	return len(a.hours) == 0 || a.hours[len(a.hours)-1].scope < a.removedTo
}

// gone returns what err becomes,
// the failure to open or read an object of the hour of index scope, -1 for one whose hour is unknown.
// An object that is gone (ErrObjectNotFound) may be gone because retention removed its hour:
// when a boundary is set, gone samples it, and the failure stands only while the acquisition still needs the object (needless).
//
// Returns:
//   - error: nil when the object is gone and no longer needed;
//     else err, joined with the provider's error, wrapped, when sampling fails.
func (a *storeAcquisition) gone(ctx context.Context, scope int, err error) error {
	if a.retention.provider == nil || !errors.Is(err, ErrObjectNotFound) {
		return err
	}
	if serr := a.sample(ctx); serr != nil {
		return errors.Join(err, serr)
	}
	if a.needless(scope) {
		return nil
	}

	return err
}
