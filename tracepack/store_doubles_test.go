package tracepack

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"maps"
	"slices"
	"strings"
	"sync"
	"sync/atomic"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// memStoreEvent is one operation of a memStore's event log, at the store's logical clock.
type memStoreEvent struct {
	at  int64
	op  string // "put", "delete", "list", "open", "read" or "close"
	key string // the key, or the listing prefix
	// token is a listing's token, "" for the first page of a traversal.
	token string
	// data holds the bytes a "put" stored.
	data []byte
	// page holds what a "list" returned, once afterList ran, and listErr the error returned with it;
	// returned is the instant List returned it, after afterList, while at is the instant the page was taken, before.
	page     ObjectPage
	listErr  error
	returned int64
	// checked is the instant a "read" checked that its object still exists, once readHook ran;
	// 0 when readHook failed the read first.
	checked int64
}

// testClock is a logical clock that a memStore and a fakeCatalog may share, so that their instants order against each other.
type testClock struct {
	now atomic.Int64
}

// tick returns the next instant.
func (c *testClock) tick() int64 {
	return c.now.Add(1)
}

// memStore is an ObjectStore over objects held in memory, with an event log and hooks for faults.
//
// Its listing follows the listing contract of ObjectStore: keys in byte order, pages of pageSize keys, a token naming the last key given.
// An object's bytes never change once put: a put under a key that exists replaces the object,
// which only a fuzzed schedule does, before the source opens anything;
// an Object opened from it reads its bytes until its key is deleted, after which every read fails with ErrObjectNotFound.
// Its hooks run without its lock held, so they may change the store.
type memStore struct {
	mu      sync.Mutex
	objects map[string][]byte
	clock   int64
	// shared, when set before use, gives the store's instants in place of its own clock.
	shared *testClock
	events []memStoreEvent
	// pageSize is the number of keys of a page; 0 means 1000.
	pageSize int
	// ignoreCtx makes List and Open ignore their context, so a test sees the source's own checks; set before use.
	ignoreCtx bool

	// beforeList, when set, runs first in every List, before the page is taken; it may change the store.
	beforeList func(prefix, token string)
	// afterList, when set, runs on every page List takes, before List returns it;
	// it may change the page or the store, and an error it returns is returned with the page.
	afterList func(prefix, token string, page *ObjectPage) error
	// openHook, when set, runs first in Open; when handled is set, Open returns its obj and err instead of opening key.
	openHook func(ctx context.Context, key string) (handled bool, obj Object, err error)
	// readHook, when set, runs first in every ReadAt of an object opened under key; an error it returns fails the read.
	readHook func(key string, off int64, n int) error
	// sizeDelta, by key, is added to the Size of the objects opened under the key.
	sizeDelta map[string]int64
	// closeErr, by key, is returned by Close of the objects opened under the key.
	closeErr map[string]error
	// closeHook, when set, runs first in every Close, without the store's lock held.
	closeHook func(key string)

	opens  map[string]int
	closes map[string]int
	reads  map[string]int
	// misuses counts the closes of an object already closed and the reads of a closed object.
	misuses int
}

var _ ObjectStore = (*memStore)(nil)

// memObject is an Object of a memStore.
// A second Close and a ReadAt after Close fail, and count as misuses of the store.
type memObject struct {
	s    *memStore
	key  string
	data []byte
	size int64
	// closed is set by the first Close; s.mu guards it.
	closed bool
}

var _ Object = (*memObject)(nil)

// fakeCatalog is a Catalog over scopes held in memory, for one tool, with a logical clock and a history of index-state changes,
// so ConfirmUnindexed answers its question exactly from the history.
//
// A scope it was told nothing about is indexed and empty.
// Snapshot returns deep copies, and its hooks run without its lock held.
type fakeCatalog struct {
	mu    sync.Mutex
	clock int64
	// shared, when set before use, gives the catalog's instants in place of its own clock.
	shared *testClock
	// log holds every snapshot, change of a scope's index state and confirmation, in order.
	log      []fakeCatalogEvent
	scopes   map[memScopeKey]*fakeCatalogScope
	evidence map[UUID]CaptureEvidence
	// barriers holds the tool's stop-unclean boundaries.
	barriers []Boundary
	// tokens maps each token Snapshot issued to the scope and the instant it was issued for.
	tokens map[string]fakeToken
	// historyLost makes ConfirmUnindexed answer false, as a catalog that lost its history must.
	historyLost bool
	// tokenLen, when longer than a token, pads every token Snapshot issues to that length.
	tokenLen int

	// snapshotErr, when set, is returned by every Snapshot.
	snapshotErr error
	// editSnapshot, when set, changes every snapshot before Snapshot returns it.
	editSnapshot func(snap *CatalogSnapshot)
	// afterSnapshot, when set, runs once a snapshot is taken, before Snapshot returns.
	afterSnapshot func()
	// beforeConfirm, when set, runs first in every ConfirmUnindexed;
	// an error it returns is returned in place of the answer.
	beforeConfirm func(hour int64) error

	// ignoreCtx makes Snapshot and ConfirmUnindexed ignore their context, so a test sees the source's own checks; set before use.
	ignoreCtx bool

	snapshots int
	confirms  int
}

var _ Catalog = (*fakeCatalog)(nil)

// fakeCatalogScope is what a fakeCatalog holds of one scope.
type fakeCatalogScope struct {
	indexed    bool
	conflicted bool
	packs      []CatalogPack
	// changes holds the instant of every change of indexed, in ascending order.
	changes []int64
}

// fakeCatalogEvent is one operation of a fakeCatalog's log, at the catalog's logical clock.
type fakeCatalogEvent struct {
	at int64
	op string // "snapshot", "index" or "confirm"
	// capture and hour name the scope of an "index" or a "confirm".
	capture UUID
	hour    int64
	// indexed is the scope's new state for an "index", and the answer for a "confirm".
	indexed bool
	token   string
	// snap holds a deep copy of what a "snapshot" returned, once editSnapshot ran.
	snap CatalogSnapshot
}

// fakeToken is what a fakeCatalog recorded of a token it issued.
type fakeToken struct {
	scope memScopeKey
	at    int64
}

// doubleCtxErr returns ctx's error for a test double, or nil without asking ctx when ignore is set.
func doubleCtxErr(ctx context.Context, ignore bool) error {
	if ignore {
		return nil
	}

	return ctx.Err()
}

// newMemStore returns an empty memStore.
func newMemStore() *memStore {
	return &memStore{
		objects: map[string][]byte{}, sizeDelta: map[string]int64{}, closeErr: map[string]error{},
		opens: map[string]int{}, closes: map[string]int{}, reads: map[string]int{},
	}
}

// newFakeCatalog returns an empty fakeCatalog.
func newFakeCatalog() *fakeCatalog {
	return &fakeCatalog{scopes: map[memScopeKey]*fakeCatalogScope{}, evidence: map[UUID]CaptureEvidence{}, tokens: map[string]fakeToken{}}
}

// memStoreTool is the tool_id of the test packs, which repairTestMeta records.
const memStoreTool = "tool"

// packKeyOf returns the key of file under storeTestPrefix: a segment key for a segment, its seq_first its first seq,
// and an archive key in hour for any other pack.
func packKeyOf(t testing.TB, hour int64, file []byte) string {
	t.Helper()

	r := mustOpen(t, file, ReaderOptions{})
	h := r.Header()
	var (
		key string
		err error
	)
	if h.Meta.PackRole == PackRoleSegment {
		first := h.Meta.SeqStart
		if b := r.Blocks(); len(b) > 0 {
			first = b[0].FirstSeq
		}
		key, err = SegmentKey(storeTestPrefix, h.Meta.ToolID, h.CaptureID, first, h.PackID)
	} else {
		key, err = ArchiveKey(storeTestPrefix, h.Meta.ToolID, h.CaptureID, hour, h.PackID)
	}
	require.NoError(t, err)

	return key
}

// put stores data under key.
func (s *memStore) put(key string, data []byte) {
	s.mu.Lock()
	defer s.mu.Unlock()
	data = bytes.Clone(data)
	s.objects[key] = data
	s.log("put", key)
	s.events[len(s.events)-1].data = data
}

// putPack stores file under its key under storeTestPrefix (packKeyOf) and returns its descriptor.
func (s *memStore) putPack(t testing.TB, hour int64, file []byte) CatalogPack {
	t.Helper()

	key := packKeyOf(t, hour, file)
	s.put(key, file)

	return CatalogPack{Key: key, PackID: mustOpen(t, file, ReaderOptions{}).Header().PackID, Size: int64(len(file))}
}

// remove deletes the object key.
func (s *memStore) remove(key string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	delete(s.objects, key)
	s.log("delete", key)
}

// log appends an event at the next instant; s.mu is held.
func (s *memStore) log(op, key string) {
	s.events = append(s.events, memStoreEvent{at: s.tick(), op: op, key: key})
}

// tick advances the store's clock, or takes the shared clock's next instant, and returns it; s.mu is held.
func (s *memStore) tick() int64 {
	if s.shared != nil {
		s.clock = s.shared.tick()
	} else {
		s.clock++
	}

	return s.clock
}

// openCounts returns how many times each key was opened and how many times an object of it was closed for the first time.
func (s *memStore) openCounts() (opens, closes map[string]int) {
	s.mu.Lock()
	defer s.mu.Unlock()

	return maps.Clone(s.opens), maps.Clone(s.closes)
}

// misuseCount returns how many times an object was closed again or read after Close.
func (s *memStore) misuseCount() int {
	s.mu.Lock()
	defer s.mu.Unlock()

	return s.misuses
}

// List returns the page of keys under prefix after token, in byte order, between beforeList and afterList.
func (s *memStore) List(ctx context.Context, prefix, token string) (ObjectPage, error) {
	if err := doubleCtxErr(ctx, s.ignoreCtx); err != nil {
		return ObjectPage{}, err
	}
	s.mu.Lock()
	before, after := s.beforeList, s.afterList
	s.mu.Unlock()
	if before != nil {
		before(prefix, token)
	}
	page, at := s.page(prefix, token)
	var err error
	if after != nil {
		err = after(prefix, token, &page)
	}
	s.mu.Lock()
	s.events[at].page = ObjectPage{Objects: slices.Clone(page.Objects), Next: page.Next}
	s.events[at].listErr = err
	s.events[at].returned = s.tick()
	s.mu.Unlock()

	return page, err
}

// page takes the page of keys under prefix after token, logging the listing, and returns it with its event's index in the log.
func (s *memStore) page(prefix, token string) (ObjectPage, int) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.events = append(s.events, memStoreEvent{at: s.tick(), op: "list", key: prefix, token: token})
	at := len(s.events) - 1

	size := s.pageSize
	if size <= 0 {
		size = 1000
	}
	var page ObjectPage
	for _, key := range slices.Sorted(maps.Keys(s.objects)) {
		if !strings.HasPrefix(key, prefix) || key <= token {
			continue
		}
		if len(page.Objects) == size {
			page.Next = page.Objects[size-1].Key

			break
		}
		page.Objects = append(page.Objects, ObjectInfo{Key: key, Size: int64(len(s.objects[key]))})
	}

	return page, at
}

// Open opens the object key, or returns what openHook returns.
func (s *memStore) Open(ctx context.Context, key string) (Object, error) {
	s.mu.Lock()
	hook := s.openHook
	s.mu.Unlock()
	if hook != nil {
		if handled, obj, err := hook(ctx, key); handled {
			return obj, err
		}
	}
	if err := doubleCtxErr(ctx, s.ignoreCtx); err != nil {
		return nil, err
	}

	if obj := s.openObject(key); obj != nil {
		return obj, nil
	}

	return nil, fmt.Errorf("memStore: %q: %w", key, ErrObjectNotFound)
}

// openObject opens the object key, counting the open; nil when there is none.
// An openHook may call it to return an object with an error.
func (s *memStore) openObject(key string) *memObject {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.log("open", key)
	data, ok := s.objects[key]
	if !ok {
		return nil
	}
	s.opens[key]++

	return &memObject{s: s, key: key, data: data, size: int64(len(data)) + s.sizeDelta[key]}
}

// scope returns the scope of capture in hour, created indexed and empty when absent; c.mu is held.
func (c *fakeCatalog) scope(capture UUID, hour int64) *fakeCatalogScope {
	k := memScopeKey{capture: capture, hour: hour}
	sc := c.scopes[k]
	if sc == nil {
		sc = &fakeCatalogScope{indexed: true}
		c.scopes[k] = sc
	}

	return sc
}

// setView makes capture's scope in hour indexed, its view packs.
func (c *fakeCatalog) setView(capture UUID, hour int64, packs ...CatalogPack) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.setIndexedLocked(capture, hour, true)
	c.scope(capture, hour).packs = slices.Clone(packs)
}

// setConflicted makes capture's scope in hour indexed and conflicted, or not conflicted.
func (c *fakeCatalog) setConflicted(capture UUID, hour int64, conflicted bool) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.setIndexedLocked(capture, hour, true)
	c.scope(capture, hour).conflicted = conflicted
}

// setIndexed indexes capture's scope in hour, or evicts it, recording the change at the next instant.
func (c *fakeCatalog) setIndexed(capture UUID, hour int64, indexed bool) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.setIndexedLocked(capture, hour, indexed)
}

// setIndexedLocked is setIndexed; c.mu is held.
func (c *fakeCatalog) setIndexedLocked(capture UUID, hour int64, indexed bool) {
	sc := c.scope(capture, hour)
	at := c.tick()
	if sc.indexed != indexed {
		sc.indexed = indexed
		sc.changes = append(sc.changes, at)
		c.log = append(c.log, fakeCatalogEvent{at: at, op: "index", capture: capture, hour: hour, indexed: indexed})
	}
}

// tick advances the catalog's clock, or takes the shared clock's next instant, and returns it; c.mu is held.
func (c *fakeCatalog) tick() int64 {
	if c.shared != nil {
		c.clock = c.shared.tick()
	} else {
		c.clock++
	}

	return c.clock
}

// catalogLog returns a copy of the catalog's log.
func (c *fakeCatalog) catalogLog() []fakeCatalogEvent {
	c.mu.Lock()
	defer c.mu.Unlock()

	return slices.Clone(c.log)
}

// cloneSnapshot returns a deep copy of snap.
func cloneSnapshot(snap *CatalogSnapshot) CatalogSnapshot {
	out := CatalogSnapshot{Barriers: cloneBoundaries(snap.Barriers)}
	for _, sc := range snap.Scopes {
		sc.Packs = slices.Clone(sc.Packs)
		out.Scopes = append(out.Scopes, sc)
	}
	e := snap.Evidence
	out.Evidence = CaptureEvidence{End: e.End, Boundaries: cloneBoundaries(e.Boundaries), Closures: slices.Clone(e.Closures), Partial: e.Partial}

	return out
}

// setEvidence makes e the per-capture evidence of capture.
func (c *fakeCatalog) setEvidence(capture UUID, e CaptureEvidence) {
	c.mu.Lock()
	defer c.mu.Unlock()
	e.Boundaries = cloneBoundaries(e.Boundaries)
	e.Closures = slices.Clone(e.Closures)
	c.evidence[capture] = e
}

// addBarrier records b as a stop-unclean boundary of the tool.
func (c *fakeCatalog) addBarrier(b Boundary) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.barriers = append(c.barriers, cloneBoundary(b))
}

// Snapshot returns capture's scopes over [from, to), its evidence and the tool's barriers meeting those hours, as deep copies;
// a scope not indexed gets a fresh token.
func (c *fakeCatalog) Snapshot(ctx context.Context, capture UUID, from, to int64) (CatalogSnapshot, error) {
	if err := doubleCtxErr(ctx, c.ignoreCtx); err != nil {
		return CatalogSnapshot{}, err
	}
	c.mu.Lock()
	c.snapshots++
	if err := c.snapshotErr; err != nil {
		c.mu.Unlock()

		return CatalogSnapshot{}, err
	}
	at := c.tick()
	snap := CatalogSnapshot{Scopes: make([]CatalogScope, 0, to-from)}
	for h := from; h < to; h++ {
		sc := c.scope(capture, h)
		cs := CatalogScope{Hour: h, Indexed: sc.indexed, Conflicted: sc.indexed && sc.conflicted}
		if !sc.indexed {
			cs.Token = fmt.Sprintf("token-%d-%d-", at, h)
			if pad := c.tokenLen - len(cs.Token); pad > 0 {
				cs.Token += strings.Repeat("x", pad)
			}
			c.tokens[cs.Token] = fakeToken{scope: memScopeKey{capture: capture, hour: h}, at: at}
		} else if !sc.conflicted {
			cs.Packs = slices.Clone(sc.packs)
		}
		snap.Scopes = append(snap.Scopes, cs)
	}
	e := c.evidence[capture]
	snap.Evidence = CaptureEvidence{End: e.End, Boundaries: cloneBoundaries(e.Boundaries), Closures: slices.Clone(e.Closures), Partial: e.Partial}
	for i := range c.barriers {
		if barrierMeets(&c.barriers[i], from*hourNs, to*hourNs) {
			snap.Barriers = append(snap.Barriers, cloneBoundary(c.barriers[i]))
		}
	}
	edit, after := c.editSnapshot, c.afterSnapshot
	c.mu.Unlock()

	if edit != nil {
		edit(&snap)
	}
	c.mu.Lock()
	c.log = append(c.log, fakeCatalogEvent{at: at, op: "snapshot", capture: capture, snap: cloneSnapshot(&snap)})
	c.mu.Unlock()
	if after != nil {
		after()
	}

	return snap, nil
}

// ConfirmUnindexed reports, after beforeConfirm, whether the scope token was issued for is capture's in hour
// and has not been indexed at any instant since the token was issued; false once the history is lost.
func (c *fakeCatalog) ConfirmUnindexed(ctx context.Context, capture UUID, hour int64, token string) (bool, error) {
	if err := doubleCtxErr(ctx, c.ignoreCtx); err != nil {
		return false, err
	}
	c.mu.Lock()
	before := c.beforeConfirm
	c.mu.Unlock()
	if before != nil {
		if err := before(hour); err != nil {
			return false, err
		}
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	c.confirms++
	at := c.tick()
	ok := c.answer(capture, hour, token)
	c.log = append(c.log, fakeCatalogEvent{at: at, op: "confirm", capture: capture, hour: hour, indexed: ok, token: token})

	return ok, nil
}

// answer reports whether the scope token was issued for is capture's in hour and has not been indexed at any instant since;
// c.mu is held.
func (c *fakeCatalog) answer(capture UUID, hour int64, token string) bool {
	tok, ok := c.tokens[token]
	if !ok || c.historyLost || tok.scope != (memScopeKey{capture: capture, hour: hour}) {
		return false
	}
	sc := c.scope(capture, hour)
	// The token was issued while the scope was not indexed, so any later change indexed it at some instant.
	if n := len(sc.changes); n > 0 && sc.changes[n-1] > tok.at {
		return false
	}

	return !sc.indexed
}

// ReadAt reads from the object's bytes, after readHook, and fails with ErrObjectNotFound once the object is deleted.
// Its "read" event is logged before readHook runs; the instant it checks that the object exists, after readHook, is the event's checked.
func (o *memObject) ReadAt(p []byte, off int64) (int, error) {
	o.s.mu.Lock()
	if o.closed {
		o.s.misuses++
		o.s.mu.Unlock()

		return 0, fmt.Errorf("memStore: read %q after Close", o.key)
	}
	hook := o.s.readHook
	o.s.reads[o.key]++
	o.s.log("read", o.key)
	at := len(o.s.events) - 1
	o.s.mu.Unlock()
	if hook != nil {
		if err := hook(o.key, off, len(p)); err != nil {
			return 0, err
		}
	}

	o.s.mu.Lock()
	_, present := o.s.objects[o.key]
	o.s.events[at].checked = o.s.tick()
	o.s.mu.Unlock()
	if !present {
		return 0, fmt.Errorf("memStore: read %q: %w", o.key, ErrObjectNotFound)
	}
	if off < 0 || off > int64(len(o.data)) {
		return 0, errors.New("memStore: offset out of range")
	}
	n := copy(p, o.data[off:])
	if n < len(p) {
		return n, errors.New("memStore: short read")
	}

	return n, nil
}

// Size returns the object's size, changed by the store's sizeDelta for its key.
func (o *memObject) Size() int64 {
	return o.size
}

// Close counts the call and returns the store's closeErr for the key; a second Close fails and counts as a misuse.
func (o *memObject) Close() error {
	o.s.mu.Lock()
	hook := o.s.closeHook
	o.s.mu.Unlock()
	if hook != nil {
		hook(o.key)
	}

	o.s.mu.Lock()
	defer o.s.mu.Unlock()
	if o.closed {
		o.s.misuses++

		return fmt.Errorf("memStore: %q closed twice", o.key)
	}
	o.closed = true
	o.s.closes[o.key]++
	o.s.log("close", o.key)

	return o.s.closeErr[o.key]
}

// TestMemStoreList lists a memStore in pages of two: keys under the prefix only, in byte order, each once, the last page without a token.
func TestMemStoreList(t *testing.T) {
	t.Parallel()

	s := newMemStore()
	s.pageSize = 2
	for _, key := range []string{"a/3", "a/1", "b/1", "a/2", "a/4", "a/5"} {
		s.put(key, []byte(key))
	}
	var keys []string
	pages := 0
	for token := ""; ; {
		page, err := s.List(t.Context(), "a/", token)
		require.NoError(t, err)
		pages++
		for _, o := range page.Objects {
			keys = append(keys, o.Key)
			require.Equal(t, int64(len(o.Key)), o.Size)
		}
		if page.Next == "" {
			break
		}
		token = page.Next
	}
	require.Equal(t, []string{"a/1", "a/2", "a/3", "a/4", "a/5"}, keys)
	require.Equal(t, 3, pages)
}

// TestFakeCatalogConfirmUnindexed confirms tokens of a fakeCatalog:
// a scope not indexed since its token was issued is confirmed;
// one indexed meanwhile, also when evicted again, a token of another scope, an unknown token and lost history are not.
func TestFakeCatalogConfirmUnindexed(t *testing.T) {
	t.Parallel()

	c := newFakeCatalog()
	c.setIndexed(captureLow, 1, false)
	c.setIndexed(captureLow, 2, false)
	snap, err := c.Snapshot(t.Context(), captureLow, 0, 3)
	require.NoError(t, err)
	require.True(t, snap.Scopes[0].Indexed)
	require.Empty(t, snap.Scopes[0].Token)
	tok1, tok2 := snap.Scopes[1].Token, snap.Scopes[2].Token
	require.NotEmpty(t, tok1)

	confirm := func(hour int64, token string) bool {
		t.Helper()

		ok, err := c.ConfirmUnindexed(t.Context(), captureLow, hour, token)
		require.NoError(t, err)

		return ok
	}
	assert.True(t, confirm(1, tok1))
	assert.False(t, confirm(2, tok1), "another scope's token")
	assert.False(t, confirm(1, "unknown"))
	c.setIndexed(captureLow, 2, true)
	c.setIndexed(captureLow, 2, false)
	assert.False(t, confirm(2, tok2), "indexed and evicted again since the token")
	c.historyLost = true
	assert.False(t, confirm(1, tok1), "history lost")
}

// TestMemObjectMisuse reads a memStore object after Close and closes it twice: both fail and count as misuses.
func TestMemObjectMisuse(t *testing.T) {
	t.Parallel()

	s := newMemStore()
	s.put("k", []byte("data"))
	obj, err := s.Open(t.Context(), "k")
	require.NoError(t, err)
	require.NoError(t, obj.Close())
	_, err = obj.ReadAt(make([]byte, 1), 0)
	require.ErrorContains(t, err, "after Close")
	require.ErrorContains(t, obj.Close(), "closed twice")
	assert.Equal(t, 2, s.misuseCount())
	_, closes := s.openCounts()
	assert.Equal(t, map[string]int{"k": 1}, closes)
}

// TestStoreDoublesLog shares a clock between a memStore and a fakeCatalog and logs a put, a snapshot, an eviction, a listing
// whose afterList changes the page and fails, and a confirmation:
// the instants ascend across both logs in the order of the calls;
// the put holds its bytes, the listing the page and error the store returned,
// the snapshot what Snapshot returned, and the confirmation its answer.
func TestStoreDoublesLog(t *testing.T) {
	t.Parallel()

	var clock testClock
	store, cat := newMemStore(), newFakeCatalog()
	store.shared, cat.shared = &clock, &clock
	store.put("a/1", []byte("one"))
	cat.setIndexed(captureLow, 1, false)
	snap, err := cat.Snapshot(t.Context(), captureLow, 1, 2)
	require.NoError(t, err)
	cat.setIndexed(captureLow, 1, true)
	failed := errors.New("page failed")
	store.afterList = func(_, _ string, page *ObjectPage) error {
		page.Objects = append(page.Objects, ObjectInfo{Key: "a/2", Size: 2})

		return failed
	}
	page, err := store.List(t.Context(), "a/", "")
	require.ErrorIs(t, err, failed)
	ok, err := cat.ConfirmUnindexed(t.Context(), captureLow, 1, snap.Scopes[0].Token)
	require.NoError(t, err)
	require.False(t, ok)

	events, log := store.eventLog(), cat.catalogLog()
	require.Len(t, events, 2)
	require.Len(t, log, 4)
	assert.Equal(t, []byte("one"), events[0].data)
	assert.Equal(t, page, events[1].page)
	assert.ErrorIs(t, events[1].listErr, failed)
	assert.Equal(t, snap, log[1].snap)
	assert.Equal(t, fakeCatalogEvent{at: log[3].at, op: "confirm", capture: captureLow, hour: 1, token: snap.Scopes[0].Token}, log[3])
	instants := []int64{events[0].at, log[0].at, log[1].at, log[2].at, events[1].at, log[3].at}
	assert.True(t, slices.IsSorted(instants) && len(slices.Compact(slices.Clone(instants))) == len(instants), "instants %v", instants)
}
