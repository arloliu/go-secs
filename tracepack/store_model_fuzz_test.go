package tracepack

import (
	"bytes"
	"errors"
	"maps"
	"slices"
	"strings"
	"testing"

	"github.com/stretchr/testify/require"

	"github.com/arloliu/go-secs/tracepack/internal/format"
)

// Kinds of the mutations FuzzStoreSource applies to a generated schedule,
// each chosen by a control byte modulo fuzzStoreKinds.
const (
	// fuzzStoreKey puts an object under a key derived from one of the schedule's keys,
	// one that does not parse or one that names another hour or seq_first.
	fuzzStoreKey = iota
	// fuzzStorePage changes one page a listing returns:
	// a key repeated or out of order, a key outside the prefix, a token repeated, an error, or a size changed.
	fuzzStorePage
	// fuzzStorePack replaces the bytes of one of the schedule's objects with the fuzzed pack, or flips one of their bytes.
	fuzzStorePack
	// fuzzStorePageSize changes the store's page size.
	fuzzStorePageSize
	// fuzzStoreHead damages the head of one of the schedule's segments:
	// its pack_metadata_crc, the file header's own CRC kept valid, or a byte of its pack metadata.
	fuzzStoreHead
	// fuzzStoreLate adds an action at a moment of the observation:
	// a pack of the pool or a commit object put, a segment of an hour outside the lookup deleted, or a scope indexed or evicted.
	fuzzStoreLate
	fuzzStoreKinds
)

// keys returns the keys of the store's objects, in byte order.
func (s *memStore) keys() []string {
	s.mu.Lock()
	defer s.mu.Unlock()

	return slices.Sorted(maps.Keys(s.objects))
}

// object returns the bytes of the object key, nil when there is none.
func (s *memStore) object(key string) []byte {
	s.mu.Lock()
	defer s.mu.Unlock()

	return s.objects[key]
}

// fuzzStoreKeyVariant returns a variant of key, a key of the schedule's under storeTestPrefix, chosen by v:
// with a suffix, an uppercase extension, an extra component,
// or, each under the prefix the key is listed under, an archive key's capture in uppercase,
// a segment key's seq_first one digit short, or a commit key's id in uppercase, none of which parses;
// or, for a pack key, the key of the same pack in the next hour or with the next seq_first, which parse.
func fuzzStoreKeyVariant(t testing.TB, key string, v byte) string {
	t.Helper()

	dir, base := key[:strings.LastIndexByte(key, '/')+1], key[strings.LastIndexByte(key, '/')+1:]
	switch v % 6 {
	case 0:
		return key + "x"
	case 1:
		return strings.TrimSuffix(key, ".tpk") + ".TPK"
	case 2:
		switch {
		case strings.Contains(key, "/archive/"):
			return dir + strings.ToUpper(base[:36]) + base[36:]
		case strings.Contains(key, "/staging/"):
			return dir + base[1:]
		default:
			return dir + strings.ToUpper(base)
		}
	case 3:
		return dir + "x/" + base
	default:
	}
	if s, err := parseSegmentKey(storeTestPrefix, key); err == nil {
		next, err := SegmentKey(storeTestPrefix, memStoreTool, s.capture, s.seqFirst+1, s.pack)
		require.NoError(t, err)

		return next
	}
	if a, err := parseArchiveKey(storeTestPrefix, key); err == nil {
		next, err := ArchiveKey(storeTestPrefix, memStoreTool, a.capture, a.hour+1, a.pack)
		require.NoError(t, err)

		return next
	}

	return key + "x"
}

// fuzzStorePageEdit returns an afterList edit that changes a page, chosen by v.
func fuzzStorePageEdit(v byte) func(prefix, token string, page *ObjectPage) error {
	return func(prefix, token string, page *ObjectPage) error {
		n := len(page.Objects)
		switch v % 6 {
		case 0:
			if n > 0 {
				page.Objects = slices.Insert(page.Objects, 1, page.Objects[0])
			}
		case 1:
			if n > 1 {
				page.Objects[0], page.Objects[1] = page.Objects[1], page.Objects[0]
			}
		case 2:
			page.Objects = append(page.Objects, ObjectInfo{Key: prefix[:len(prefix)-1] + "~/x", Size: 1})
		case 3:
			// A token repeated; on a traversal's first page there is none to repeat, and an empty Next would drop keys unseen.
			if token != "" {
				page.Next = token
			}
		case 4:
			return errors.New("fuzzed listing error")
		default:
			if n > 0 {
				page.Objects[n-1].Size++
			}
		}

		return nil
	}
}

// mutateStoreSchedule applies to s the mutations ctl names, three bytes each: the kind, then two arguments;
// pack, when not empty, is the bytes a pack mutation puts.
func mutateStoreSchedule(t testing.TB, pool *modelPool, s *modelSchedule, ctl, pack []byte) {
	t.Helper()

	pages := map[int]func(prefix, token string, page *ObjectPage) error{}
	for i := 0; i+2 < len(ctl) && i < 12; i += 3 {
		a, b := ctl[i+1], ctl[i+2]
		keys := s.c.store.keys()
		switch ctl[i] % fuzzStoreKinds {
		case fuzzStoreKey:
			if len(keys) > 0 {
				key := keys[int(a)%len(keys)]
				s.c.store.put(fuzzStoreKeyVariant(t, key, b), s.c.store.object(key))
			}
		case fuzzStorePage:
			pages[1+int(a)%24] = fuzzStorePageEdit(b)
		case fuzzStorePack:
			if len(keys) == 0 {
				continue
			}
			key := keys[int(a)%len(keys)]
			data := slices.Clone(s.c.store.object(key))
			switch {
			case len(pack) > 0 && b%2 == 0:
				data = pack
			case len(data) > 0:
				data[(int(b)*131)%len(data)] ^= 1 << (b % 8)
			default:
			}
			s.c.store.put(key, data)
		case fuzzStorePageSize:
			s.c.store.pageSize = 1 + int(a)%4
		case fuzzStoreHead:
			fuzzStoreDamageHead(t, s, a, b)
		default:
			fuzzStoreLateAction(t, pool, s, a, b)
		}
	}
	if len(pages) == 0 {
		return
	}
	inner, calls := s.c.store.afterList, 0
	s.c.store.afterList = func(prefix, token string, page *ObjectPage) error {
		if err := inner(prefix, token, page); err != nil {
			return err
		}
		calls++
		if edit := pages[calls]; edit != nil {
			return edit(prefix, token, page)
		}

		return nil
	}
}

// fuzzStoreDamageHead damages the head of the schedule's segment a chosen by a, as b chooses:
// its pack_metadata_crc changed, the file header's CRC recomputed so that only the pack metadata's check fails,
// or a byte of its pack metadata flipped; a head an earlier mutation damaged is left alone.
func fuzzStoreDamageHead(t testing.TB, s *modelSchedule, a, b byte) {
	t.Helper()

	var segments []string
	for _, key := range s.c.store.keys() {
		if strings.Contains(key, "/staging/") {
			segments = append(segments, key)
		}
	}
	if len(segments) == 0 {
		return
	}
	key := segments[int(a)%len(segments)]
	data := slices.Clone(s.c.store.object(key))
	if len(data) < format.FileHeaderLen {
		return
	}
	hdr, err := format.UnmarshalFileHeader(data[:format.FileHeaderLen])
	if err != nil || uint64(hdr.PackMetadataLen) > uint64(len(data)-format.FileHeaderLen) {
		return
	}
	if b%2 == 0 || hdr.PackMetadataLen == 0 {
		hdr.PackMetadataCRC ^= 1 << (b % 32)
		copy(data, format.AppendFileHeader(nil, &hdr))
	} else {
		data[format.FileHeaderLen+int(b)%int(hdr.PackMetadataLen)] ^= 1 << (b % 8)
	}
	s.c.store.put(key, data)
}

// fuzzStoreLateAction adds to s an action at a moment of the observation chosen by a, doing what b chooses:
// a pack of the pool's hours put, a commit object put, a segment of an hour outside the lookup deleted,
// or a scope of the lookup indexed or evicted.
// Its actions keep the schedule within the coherent observation's premises (the tracepack storage specification §5),
// whatever the earlier mutations put under a key:
// a segment is deleted only while it holds the pool's bytes, of an hour outside the lookup,
// and a pack is put over other bytes only when neither is a pack of a scope the snapshot does not index.
func fuzzStoreLateAction(t testing.TB, pool *modelPool, s *modelSchedule, a, b byte) {
	t.Helper()

	n := s.c.opts.MaxScopes
	k := int64(a/8) % int64(n)
	hour := memTestHour + k
	at := []modelPoint{
		{op: "snapshot"},
		{op: "list", key: commitDir(t, hour), n: 1},
		{op: "list", key: commitDir(t, hour), n: 2},
		{op: "list", key: archiveDir(t, hour), n: 1},
		{op: "list", key: stagingDir(t), n: 1},
		{op: "list", key: stagingDir(t), n: 1, page: 1},
		{op: "confirm", hour: memTestHour + int64(n) - 1},
	}[int(a%8)%7]
	hp := &pool.hours[k]
	switch b % 4 {
	case 0:
		files := [][]byte{hp.segA, hp.segB, hp.reply, hp.empty, hp.memA, hp.patch, hp.memB, hp.memC, hp.retired}
		file := files[int(b/4)%len(files)]
		key := packKeyOf(t, hour, file)
		s.on(at, func() {
			if old, ok := s.c.store.lookup(key); ok && !bytes.Equal(old, file) &&
				(fuzzStoreListedPack(s, key, old) || fuzzStoreListedPack(s, key, file)) {
				return
			}
			s.c.store.put(key, file)
		})
	case 1:
		id := []UUID{setA, setB, setC, {0xB0, byte(k), 1}}[int(b/4)%4]
		s.on(at, func() { s.c.store.putCommit(t, captureLow, hour, id) })
	case 2:
		file := pool.before
		if b&4 != 0 {
			file = pool.after
		}
		key := packKeyOf(t, hourOf(mustOpen(t, file, ReaderOptions{}).Header().Meta.PeriodStart), file)
		s.on(at, func() {
			if old, _ := s.c.store.lookup(key); bytes.Equal(old, file) {
				s.c.store.remove(key)
			}
		})
	default:
		indexed := b&4 != 0
		s.on(at, func() { s.c.cat.setIndexed(captureLow, hour, indexed) })
	}
}

// lookup returns the bytes of the object key and whether it exists.
func (s *memStore) lookup(key string) ([]byte, bool) {
	s.mu.Lock()
	defer s.mu.Unlock()
	data, ok := s.objects[key]

	return data, ok
}

// fuzzStoreListedPack reports whether data, held under key, is a pack of a scope that s's catalog snapshot reports not indexed:
// key is an archive key of such an hour, or data's pack metadata, decoded without checks, puts its period in one.
func fuzzStoreListedPack(s *modelSchedule, key string, data []byte) bool {
	listed := map[int64]bool{}
	for _, e := range s.c.cat.catalogLog() {
		if e.op == "snapshot" {
			for _, sc := range e.snap.Scopes {
				listed[sc.Hour] = !sc.Indexed
			}
		}
	}
	if a, err := parseArchiveKey(storeTestPrefix, key); err == nil && listed[a.hour] {
		return true
	}
	if len(data) < format.FileHeaderLen {
		return false
	}
	hdr, err := format.UnmarshalFileHeader(data[:format.FileHeaderLen])
	if err != nil || uint64(hdr.PackMetadataLen) > uint64(len(data)-format.FileHeaderLen) {
		return false
	}
	meta, err := UnmarshalPackMeta(data[format.FileHeaderLen : format.FileHeaderLen+int(hdr.PackMetadataLen)])
	if err != nil {
		return false
	}
	hour, ok := periodHour(meta.PeriodStart, meta.PeriodEnd)

	return ok && listed[hour]
}

// FuzzStoreSource checks a lookup through a store source over a generated schedule, mutated as the control bytes say —
// keys that do not parse or disagree with their packs, pages that break the listing contract, packs replaced or damaged,
// segment heads damaged, page sizes, and actions added during the observation — against the acquisition model:
// an observation that succeeds holds what the model derives,
// and the lookup returns what the lookup over the model's replay returns;
// a failed one returns no observation and a zero outcome; every object opened is closed once.
func FuzzStoreSource(f *testing.F) {
	pool := newModelPool(f)
	// Schedules unchanged; then keys with a suffix, of another hour or seq_first, among them a listed hour's archive key;
	// a page of the first commit listing with a key repeated, pages with keys out of order, a token repeated, an error;
	// packs damaged, or replaced by another pack;
	// an indexed segment damaged, then put again whole after the source opened it;
	// the heads of segments of hours outside the lookup with a wrong pack_metadata_crc, or a damaged pack metadata;
	// a segment of an hour outside the lookup deleted, and a scope indexed, during the observation;
	// the outside segment's key given a segment of the observed hour, then deleted while the first commit traversal returns,
	// before the witness instant, which leaves the premises whole;
	// or during the second commit traversal or the archive listing, after it, which must not happen;
	// a listed hour's segment damaged, then put whole again after the witness instant, which must not happen either.
	for seed := range uint64(8) {
		f.Add(seed, []byte(nil), []byte(nil))
	}
	f.Add(uint64(1), []byte{fuzzStoreKey, 0, 0, fuzzStoreKey, 3, 4}, []byte(nil))
	f.Add(uint64(111), []byte{fuzzStoreKey, 48, 0}, []byte(nil))
	f.Add(uint64(46), []byte{fuzzStorePage, 0, 0}, []byte(nil))
	f.Add(uint64(2), []byte{fuzzStorePage, 2, 0, fuzzStorePageSize, 0, 0}, []byte(nil))
	f.Add(uint64(3), []byte{fuzzStorePage, 1, 3, fuzzStorePage, 4, 5}, []byte(nil))
	f.Add(uint64(4), []byte{fuzzStorePack, 0, 101}, []byte(nil))
	f.Add(uint64(5), []byte{fuzzStorePack, 1, 0}, pool.hours[0].segB)
	f.Add(uint64(6), []byte{fuzzStorePack, 2, 0}, pool.hours[1].retired)
	f.Add(uint64(297), []byte{fuzzStorePack, 48, 49, fuzzStoreLate, 36, 36}, []byte("0"))
	f.Add(uint64(2), []byte{fuzzStoreHead, 0, 0}, []byte(nil))
	f.Add(uint64(4), []byte{fuzzStoreHead, 0, 0, fuzzStoreHead, 1, 1}, []byte(nil))
	f.Add(uint64(9), []byte{fuzzStoreLate, 4, 2, fuzzStoreLate, 9, 3}, []byte(nil))
	f.Add(uint64(2), []byte{fuzzStoreLate, 1, 0xc6, fuzzStorePack, 27, 2}, pool.hours[0].segB)
	f.Add(uint64(2), []byte{fuzzStoreLate, 2, 0xc6, fuzzStorePack, 27, 2}, pool.hours[0].segB)
	f.Add(uint64(2), []byte{fuzzStoreLate, 3, 0xc6, fuzzStorePack, 27, 2}, pool.hours[0].segB)
	f.Add(uint64(3), []byte{fuzzStorePack, 1, 3, fuzzStoreLate, 2, 0}, []byte(nil))

	f.Fuzz(func(t *testing.T, seed uint64, ctl, pack []byte) {
		s := genModelSchedule(t, pool, seed)
		mutateStoreSchedule(t, pool, s, ctl, pack)
		r := checkStoreCase(t, s.c)
		require.Empty(t, r.problems)
	})
}
