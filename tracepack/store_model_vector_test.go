package tracepack

import (
	"fmt"
	"slices"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// searchedIndexed returns, for each scope r searched, its hour relative to memTestHour and whether it is indexed.
func searchedIndexed(r *TxResult) map[int64]bool {
	out := map[int64]bool{}
	for _, s := range r.Searched {
		out[s.Hour-memTestHour] = s.Indexed
	}

	return out
}

// coldHours returns the cold gaps of gaps as gapHours writes them.
func coldHours(gaps []TxGap) []string {
	return slices.DeleteFunc(gapHours(gaps), func(g string) bool { return !strings.HasPrefix(g, "cold@") })
}

// TestStoreSourceModelLateSegment looks up the primary of an indexed hour beside the next hour, which the catalog does not index,
// where a segment lands after the hour's commit traversals, during its archive listing, holding the reply or another record.
// The observation matches the acquisition model, the late segment in the listed view;
// the lookup lists cold and is incomplete without the reply, and matched beside cold with it, the reply read from the late segment.
func TestStoreSourceModelLateSegment(t *testing.T) {
	t.Parallel()

	for _, reply := range []bool{false, true} {
		t.Run(fmt.Sprintf("reply %v", reply), func(t *testing.T) {
			t.Parallel()

			c := newStoreCase(2)
			c.cat.setView(captureLow, memTestHour, c.store.putPack(t, memTestHour, txPack(t, seg0, nil, txBlock(txRecord(12, nil)))))
			c.cat.setIndexed(captureLow, memTestHour+1, false)
			c.store.putPack(t, memTestHour+1, txPackIn(t, seg1, 1, nil, txBlock(txSeqs(20)...)))
			rec, want := txRecord(21, nil), TxIncomplete
			if reply {
				rec, want = txReply(13, nil), TxMatched
			}
			late := txPackIn(t, seg2, 1, nil, txBlock(rec))
			archives := archiveDir(t, memTestHour+1)
			c.store.afterList = func(prefix, token string, _ *ObjectPage) error {
				if prefix == archives && token == "" {
					c.store.putPack(t, memTestHour+1, late)
				}

				return nil
			}

			r := requireStoreCase(t, c)
			assert.Equal(t, 1, r.stats.lateSegments, "reply %v", reply)
			require.NoError(t, r.err)
			assert.Contains(t, gapHours(r.res.Gaps), "cold@1", "reply %v", reply)
			assert.Equal(t, want, r.res.Outcome, "reply %v", reply)
			assert.Equal(t, map[int64]bool{0: true, 1: false}, searchedIndexed(&r.res))
			assert.Equal(t, []UUID{seg1, seg2}, r.res.Searched[1].Packs, "the late segment in the listed view")
			if reply {
				i := slices.IndexFunc(r.res.Records, func(v TxRecord) bool { return v.Record.Seq == 13 })
				require.GreaterOrEqual(t, i, 0, "the reply")
				assert.Equal(t, seg2, r.res.Records[i].Pack, "the reply")
			}
		})
	}
}

// TestStoreSourceModelWindowBoundary looks up primaries over three hours of which the catalog indexes a window:
// the primary's hour before the window, the reply in it;
// and the primary's hour in the window, the reply in the hour after it;
// each with the reply and without it.
// The observations match the acquisition model;
// the window's scopes are read as indexed and the hour outside it as listed, cold;
// the outcome is matched beside cold with the reply, incomplete without it.
func TestStoreSourceModelWindowBoundary(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name string
		// listed is the hour outside the window, reply the hour of the reply.
		listed, reply int64
	}{
		{name: "the primary before the window", listed: 0, reply: 2},
		{name: "the reply after the window", listed: 2, reply: 2},
	}
	for _, tt := range tests {
		for _, withReply := range []bool{false, true} {
			t.Run(fmt.Sprintf("%s, reply %v", tt.name, withReply), func(t *testing.T) {
				t.Parallel()

				c := newStoreCase(3)
				for h := range int64(3) {
					hour := memTestHour + h
					steps := txBlock(txSeqs(20 + 10*uint64(h))...)
					switch {
					case h == 0:
						steps = txBlock(txRecord(12, nil))
					case h == tt.reply && withReply:
						steps = txBlock(txReply(13, nil))
					default:
					}
					desc := c.store.putPack(t, hour, txPackIn(t, UUID{0x50, byte(h)}, h, nil, steps))
					if h == tt.listed {
						c.cat.setIndexed(captureLow, hour, false)
					} else {
						c.cat.setView(captureLow, hour, desc)
					}
				}

				r := requireStoreCase(t, c)
				require.NoError(t, r.err, "%s, reply %v", tt.name, withReply)
				want := TxIncomplete
				if withReply {
					want = TxMatched
				}
				assert.Equal(t, want, r.res.Outcome, "%s, reply %v", tt.name, withReply)
				assert.Equal(t, []string{fmt.Sprintf("cold@%d", tt.listed)}, coldHours(r.res.Gaps), "%s, reply %v", tt.name, withReply)
				wantSearched := map[int64]bool{0: true, 1: true, 2: true}
				wantSearched[tt.listed] = false
				if withReply && tt.reply < 2 {
					delete(wantSearched, 2)
				}
				assert.Equal(t, wantSearched, searchedIndexed(&r.res), "%s, reply %v", tt.name, withReply)
			})
		}
	}
}

// TestStoreSourceModelFixed fixes an observation of an indexed hour and a listed one, checked against the acquisition model,
// then, before the lookup reads, evicts the indexed scope,
// uploads a segment holding a valid reply to the listed hour, which the catalog then indexes with the segment registered,
// or leaves rejected, recording its evidence,
// records a closure and a stop, and adds a barrier.
// The lookup answers as the model's replay does, from what was fixed: incomplete, cold, without the reply.
func TestStoreSourceModelFixed(t *testing.T) {
	t.Parallel()

	for _, registered := range []bool{false, true} {
		t.Run(fmt.Sprintf("registered %v", registered), func(t *testing.T) {
			t.Parallel()

			c := newStoreCase(2)
			c.cat.setView(captureLow, memTestHour, c.store.putPack(t, memTestHour, txPack(t, seg0, nil, txBlock(txRecord(12, nil)))))
			c.cat.setIndexed(captureLow, memTestHour+1, false)
			listed := c.store.putPack(t, memTestHour+1, txPackIn(t, seg1, 1, nil, txBlock(txSeqs(20)...)))
			c.afterObserve = func() {
				c.cat.setIndexed(captureLow, memTestHour, false)
				late := c.store.putPack(t, memTestHour+1, txPackIn(t, seg2, 1, nil, txBlock(txReply(13, nil))))
				if registered {
					c.cat.setView(captureLow, memTestHour+1, listed, late)
				}
				c.cat.setEvidence(captureLow, CaptureEvidence{End: EndStopped, Closures: []EpochClosure{{Epoch: txTestEpoch, CloseSeq: 13}}})
				at := blockTestHour
				c.cat.addBarrier(Boundary{Capture: captureHigh, Seq: 1, Kind: BoundaryKindStopUnclean, GapStart: &at})
			}

			r := requireStoreCase(t, c)
			require.NoError(t, r.err)
			assert.Equal(t, TxIncomplete, r.res.Outcome, "registered %v", registered)
			assert.Equal(t, []string{"cold@1"}, coldHours(r.res.Gaps), "registered %v", registered)
			assert.Equal(t, []UUID{seg1}, r.res.Searched[1].Packs, "registered %v", registered)
		})
	}
}

// TestStoreSourceModelLateExcluded observes a listed scope where an archive of the retired role lands after the witness instant,
// during the second commit traversal, and the archive listing returns it:
// the acquisition model expects it reported, and the source reports it once.
// A source whose reports go nowhere fails the check.
func TestStoreSourceModelLateExcluded(t *testing.T) {
	t.Parallel()

	for _, lost := range []bool{false, true} {
		t.Run(fmt.Sprintf("lost %v", lost), func(t *testing.T) {
			t.Parallel()

			c := newStoreCase(1)
			c.cat.setIndexed(captureLow, memTestHour, false)
			c.store.putPack(t, memTestHour, txPack(t, seg0, nil, txBlock(txRecord(12, nil))))
			retired := setMetaRole(t, storeArchive(t, memC, setC, 7), packRoleRetired)
			dir := commitDir(t, memTestHour)
			traversals := 0
			c.store.afterList = func(prefix, token string, _ *ObjectPage) error {
				// A traversal is one page, so the first one has ended, and the witness instant passed, when the second starts.
				if prefix == dir && token == "" {
					if traversals++; traversals == 2 {
						c.store.putPack(t, memTestHour, retired)
					}
				}

				return nil
			}
			if lost {
				c.edit = func(o *StoreSourceOptions) { o.OnExcluded = func(ExcludedPack) {} }
			}

			r := checkStoreCase(t, c)
			if lost {
				require.Len(t, r.problems, 1)
				assert.Contains(t, r.problems[0], "the source reported [] excluded")

				return
			}
			require.Empty(t, r.problems)
			assert.Equal(t, 1, r.stats.excluded)
			assert.Equal(t, 2, c.store.listCount(dir))
		})
	}
}

// TestStoreSourceModelGivenView reads an indexed scope whose view, as the catalog selected it, is a patch alone,
// the generation it is based on not among its packs, so ActiveView over the scope's packs would not select it:
// the acquisition model takes the snapshot's list as given, and the observation and the lookup match it.
func TestStoreSourceModelGivenView(t *testing.T) {
	t.Parallel()

	c := newStoreCase(1)
	patch := listedPatch(t, patP1, 0, new(setA), []UUID{memA}, txBlock(txRecord(12, nil)))
	c.cat.setView(captureLow, memTestHour, c.store.putPack(t, memTestHour, patch))
	computed, err := viewFiles(t.Context(), map[UUID][]byte{patP1: patch}, CommitSet{patP1: {}})
	require.NoError(t, err)
	require.Empty(t, computed, "ActiveView does not select the patch")

	r := requireStoreCase(t, c)
	require.NoError(t, r.err)
	assert.Equal(t, []UUID{patP1}, r.res.Searched[0].Packs)
}

// TestStoreSourceModelOrder reads a listed scope holding a segment and a committed patch that hold one identical record
// in blocks with equal first seq, the patch's pack_id below the segment's, though the source opens the segment first:
// the observation matches the acquisition model, whose replay takes the packs in ascending pack_id order,
// and the record's representative is the patch, the first reader holding it.
func TestStoreSourceModelOrder(t *testing.T) {
	t.Parallel()

	patch := UUID{0x4F}
	c := newStoreCase(1)
	c.cat.setIndexed(captureLow, memTestHour, false)
	c.store.putPack(t, memTestHour, txPack(t, seg1, nil, txBlock(txRecord(12, nil))))
	c.store.putPack(t, memTestHour, txPack(t, patch, func(m *PackMeta) { m.PackRole, m.Supersedes = PackRoleRepair, []UUID{memD} }, txBlock(txRecord(12, nil))))
	c.store.putCommit(t, captureLow, memTestHour, patch)

	r := requireStoreCase(t, c)
	require.NoError(t, r.err)
	assert.Equal(t, []UUID{patch, seg1}, r.res.Searched[0].Packs)
	require.NotEmpty(t, r.res.Records)
	assert.Equal(t, patch, r.res.Records[0].Pack)
}

// TestStoreSourceModelDeletedAtRead observes an indexed hour whose view is empty beside a listed hour without packs,
// where a segment of the indexed hour, outside its view, is deleted during the read of its head,
// after the store logged the read and before it checked the object:
// the source fails with ErrObjectNotFound, and the acquisition model, which takes the instant the store checked the object,
// expects that failure.
func TestStoreSourceModelDeletedAtRead(t *testing.T) {
	t.Parallel()

	c := newStoreCase(2)
	c.cat.setView(captureLow, memTestHour)
	c.cat.setIndexed(captureLow, memTestHour+1, false)
	key := c.store.putPack(t, memTestHour, txPack(t, seg0, nil, txBlock(txRecord(12, nil)))).Key
	deleted := false
	c.store.readHook = func(k string, _ int64, _ int) error {
		if k == key && !deleted {
			deleted = true
			c.store.remove(key)
		}

		return nil
	}

	r := requireStoreCase(t, c)
	require.ErrorIs(t, r.observeErr, ErrObjectNotFound)
	assert.False(t, r.observed)
	assert.Equal(t, 1, r.stats.goneAtRead)
}

// TestStoreSourceModelWitness changes a listed hour's segment from a listing hook, which runs after the store took a page
// and before List returns it:
// a segment deleted, or uploaded, during the last page of the first commit traversal is judged at the instant that traversal returned,
// after the change, so the witness instant follows it, and the observation matches the acquisition model,
// the deleted segment outside the view and the uploaded one in it, present at the witness instant.
// A segment deleted, or replaced by other bytes under its key, during the second commit traversal, after the witness instant,
// takes the schedule outside the coherent observation's premises, and the model rejects it rather than accept a smaller view.
func TestStoreSourceModelWitness(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name string
		// traversal is the commit traversal whose page hook changes the segment.
		traversal int
		change    func(c *storeCase, key string)
		// present reports whether the segment is there before the change; want is the hour's view, nil when the model rejects.
		present bool
		want    []UUID
	}{
		{name: "deleted while the first traversal returns", traversal: 1, present: true, want: []UUID{seg0},
			change: func(c *storeCase, key string) { c.store.remove(key) }},
		{name: "uploaded while the first traversal returns", traversal: 1, want: []UUID{seg0, seg1},
			change: func(c *storeCase, key string) { c.store.put(key, txPack(t, seg1, nil, txBlock(txSeqs(20, 21)...))) }},
		{name: "deleted after the witness instant", traversal: 2, present: true,
			change: func(c *storeCase, key string) { c.store.remove(key) }},
		{name: "replaced after the witness instant", traversal: 2, present: true,
			change: func(c *storeCase, key string) { c.store.put(key, txPack(t, seg1, nil, txBlock(txSeqs(20, 22)...))) }},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()

			c := newStoreCase(1)
			c.cat.setIndexed(captureLow, memTestHour, false)
			c.store.putPack(t, memTestHour, txPack(t, seg0, nil, txBlock(txRecord(12, nil))))
			segment := txPack(t, seg1, nil, txBlock(txSeqs(20, 21)...))
			key := packKeyOf(t, memTestHour, segment)
			if tt.present {
				c.store.put(key, segment)
			}
			dir := commitDir(t, memTestHour)
			traversals := 0
			c.store.afterList = func(prefix, token string, _ *ObjectPage) error {
				// Each traversal is one page.
				if prefix == dir && token == "" {
					if traversals++; traversals == tt.traversal {
						tt.change(c, key)
					}
				}

				return nil
			}

			r := checkStoreCase(t, c)
			if tt.want == nil {
				require.True(t, r.observed)
				require.NotEmpty(t, r.problems)
				assert.Contains(t, r.problems[0], "outside the coherent observation's premises")

				return
			}
			require.Empty(t, r.problems)
			require.NoError(t, r.err)
			assert.Equal(t, tt.want, r.res.Searched[0].Packs)
			assert.Zero(t, r.stats.lateSegments, "present at the witness instant")
		})
	}
}
