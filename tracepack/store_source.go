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

// errListingNotImplemented reports a scope the catalog does not index, which the source NewStoreSource returns cannot observe yet.
var errListingNotImplemented = errors.New("tracepack: store source: listing views are not implemented")

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
	MaxCommitListings int
	// MaxListPages is the largest number of pages of one traversal; default 10000.
	MaxListPages int
	// MaxObjects is the largest number of objects one Observe handles:
	// every key any of its listings returns, a key listed again counted again, and every object it opens;
	// default 65536.
	MaxObjects int
	// MaxSourceBytes bounds the state one Observe holds and its Observation keeps, in bytes; default 256 MiB.
	// The state is charged from the structures the source decodes and keeps, not from their encoded lengths:
	// the catalog's scope and pack descriptors while it uses them, the copied evidence and barriers,
	// and every retained Reader, from its pack metadata, block index, footer summaries and statistics.
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
}

// storeSource is the PackSource NewStoreSource returns; it is immutable, and each Observe owns its state.
type storeSource struct {
	store ObjectStore
	cat   Catalog
	// opts holds the options with every limit defaulted.
	opts StoreSourceOptions
}

var _ PackSource = (*storeSource)(nil)

// storeAcquisition is the state of one Observe of a storeSource.
type storeAcquisition struct {
	src      *storeSource
	capture  UUID
	from, to int64
	budget   storeBudget
	// objects holds every object opened, in the order opened;
	// the Observation closes them, or Observe does when it fails.
	objects []Object
	// descriptors is the charge of the catalog's scope and pack descriptors, released once the observation is built.
	descriptors int64
	obs         *storeObservation
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
// A scope the catalog does not index is fixed by the coherent observation of the tracepack storage specification §5 and its listing view.
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
// which a caller may retry,
// and a key a listing returns or the catalog names that does not parse as a key of the tracepack storage specification §3,
// with an error wrapping ErrInvalidKey, which a retry does not cure.
// An Observe that fails closes every object it opened, and returns the close errors joined after its first error.
//
// The source's guarantees hold while every hour observed stays retained (the tracepack storage specification §5, Retention)
// from the start of Observe until the lookup's last use of the Observation;
// enforcing that is the caller's.
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
//     a Tool a key cannot hold (wrapping ErrInvalidKey), or a non-zero Reader.FooterOffset.
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
		return nil, err
	}
	if err := ctx.Err(); err != nil {
		return nil, fmt.Errorf("tracepack: store source: %w", err)
	}

	a := &storeAcquisition{
		src: s, capture: capture, from: from, to: to,
		budget: storeBudget{maxBytes: s.opts.MaxSourceBytes, maxObjects: s.opts.MaxObjects},
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

// checkObserve checks the arguments of Observe: a capture_id that is not zero, and hours [from, to) not empty,
// within [MinTxHour, MaxTxHour], the hours a lookup reads.
func checkObserve(capture UUID, from, to int64) error {
	switch {
	case capture == UUID{}:
		return errors.New("tracepack: store source: the capture_id is zero")
	case from >= to:
		return fmt.Errorf("tracepack: store source: hours [%d, %d) are empty", from, to)
	case from < MinTxHour || to-1 > MaxTxHour:
		return fmt.Errorf("tracepack: store source: hours [%d, %d) are not within [%d, %d]", from, to, MinTxHour, MaxTxHour)
	}

	return nil
}

// run takes the catalog snapshot, opens the packs of the indexed scopes, observes the others,
// and returns the observation built from them.
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

	a.budget.release(a.descriptors)
	a.descriptors = 0
	a.obs.objects = a.objects
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

	ids := make(map[UUID]struct{}, len(sc.Packs))
	keys := make(map[string]struct{}, len(sc.Packs))
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
func (a *storeAcquisition) openIndexed(ctx context.Context, scopes []CatalogScope) error {
	for i := range scopes {
		if err := ctx.Err(); err != nil {
			return fmt.Errorf("tracepack: store source: %w", err)
		}
		sc := &scopes[i]
		if !sc.Indexed {
			continue
		}
		if sc.Conflicted {
			a.obs.scopes[i] = storeScope{indexed: true, conflicted: true}

			continue
		}
		var readers []*Reader
		for j := range sc.Packs {
			if err := ctx.Err(); err != nil {
				return fmt.Errorf("tracepack: store source: %w", err)
			}
			r, err := a.openDescribed(ctx, &sc.Packs[j], sc.Hour)
			if err != nil {
				return err
			}
			readers = append(readers, r)
		}
		a.obs.scopes[i] = storeScope{readers: readers, indexed: true}
	}

	return nil
}

// openDescribed opens the pack the catalog describes as p, of hour's scope, and checks it against its key and p.
func (a *storeAcquisition) openDescribed(ctx context.Context, p *CatalogPack, hour int64) (*Reader, error) {
	k, err := a.packKey(p.Key, hour)
	if err != nil {
		return nil, err
	}
	r, err := a.openPack(ctx, p.Key, p.Size, hour)
	if err != nil {
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

// openPack opens the object key of size bytes, as the catalog or a listing gives it, and bootstraps a Reader over it,
// charging the object and the Reader.
// The object is kept for closing as soon as it is opened, and one returned with an error is closed at once.
func (a *storeAcquisition) openPack(ctx context.Context, key string, size, hour int64) (*Reader, error) {
	if err := a.budget.count(); err != nil {
		return nil, fmt.Errorf("tracepack: store source: hour %d: opening %q: %w", hour, key, err)
	}
	obj, err := a.src.store.Open(ctx, key)
	if err != nil {
		if obj != nil {
			if cerr := obj.Close(); cerr != nil {
				err = errors.Join(err, cerr)
			}
		}

		return nil, fmt.Errorf("tracepack: store source: hour %d: open %q: %w", hour, key, err)
	}
	if obj == nil {
		return nil, fmt.Errorf("tracepack: store source: hour %d: open %q returned no object", hour, key)
	}
	a.objects = append(a.objects, obj)

	if got := obj.Size(); got != size {
		return nil, fmt.Errorf("tracepack: store source: hour %d: object %q holds %d bytes, not the %d given for it", hour, key, got, size)
	}
	r, err := Open(ctx, obj, size, a.src.opts.Reader)
	if err != nil {
		return nil, fmt.Errorf("tracepack: store source: hour %d: pack %q: %w", hour, key, err)
	}
	if err := a.budget.reserve(readerCost(r)); err != nil {
		return nil, fmt.Errorf("tracepack: store source: hour %d: pack %q: %w", hour, key, err)
	}

	return r, nil
}

// checkPack checks the pack r opened from key, which names k, against k and hour's scope
// (the tracepack storage specification §2 and §3):
// its file header's pack_id and capture_id are the key's, its tool_id is the source's,
// its period lies in one UTC hour, which is hour;
// a segment is under staging/, its first seq, when known (firstSeq), the key's seq_first, and any other pack under archive/;
// and a pack whose role takes part in a scope's view has the scope_generation its role requires.
// A pack whose role takes no part in a view is an error when indexed is set.
func (a *storeAcquisition) checkPack(r *Reader, key string, k storePackKey, hour int64, indexed bool) error {
	m := r.meta
	fail := func(format string, args ...any) error {
		return fmt.Errorf("tracepack: store source: hour %d: pack %q: %s", hour, key, fmt.Sprintf(format, args...))
	}
	switch id, capture := UUID(r.hdr.PackID), UUID(r.hdr.CaptureID); {
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
	if first, known := firstSeq(r); k.segment && known && first != k.seqFirst {
		return fail("first seq %d, not the key's seq_first %d", first, k.seqFirst)
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

// observeListed fixes the scopes the snapshot reports not indexed.
func (a *storeAcquisition) observeListed(ctx context.Context, scopes []CatalogScope) error {
	for i := range scopes {
		if err := ctx.Err(); err != nil {
			return fmt.Errorf("tracepack: store source: %w", err)
		}
		if !scopes[i].Indexed {
			return fmt.Errorf("tracepack: store source: hour %d: %w", scopes[i].Hour, errListingNotImplemented)
		}
	}

	return nil
}

// abort closes every object the acquisition opened, each once, and returns err with the close errors joined after it.
// It runs on every path out of Observe but success, a panic included.
func (a *storeAcquisition) abort(err error) error {
	errs := []error{err}
	for _, obj := range a.objects {
		if cerr := obj.Close(); cerr != nil {
			errs = append(errs, fmt.Errorf("tracepack: store source: close: %w", cerr))
		}
	}
	a.objects = nil
	if len(errs) == 1 {
		return err
	}

	return errors.Join(errs...)
}
