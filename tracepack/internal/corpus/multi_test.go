package corpus

import (
	"bytes"
	"slices"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/arloliu/go-secs/tracepack"
)

// TestHourClock checks that the generator can place a pack in any hour:
// a pack of the hour before TimeBase's and one of the hour after it are written, open,
// and hold their period and records where the clock puts them; hour 0, minute 0 is the period of segmentMeta.
func TestHourClock(t *testing.T) {
	t.Parallel()

	require.Equal(t, msAt(7), hourAt(0, 7))
	require.Equal(t, TimeBase-hourNs+5, hourAt(-1, 0)+5)
	require.Equal(t, segmentMeta("s"), segmentMetaIn("s", 0, 0))

	const seed = "hour-clock"
	packs, err := writeMultiPacks(seed, []multiPack{
		{capture: 0, hour: -1, minute: 59, blocks: [][]tracepack.Record{{multiData(4, hourAt(-1, 59*60_000+1), nil)}}},
		{capture: 0, hour: 1, minute: 2, blocks: [][]tracepack.Record{{multiData(5, hourAt(1, 3), nil), multiData(6, hourAt(1, 4), nil)}}},
	})
	require.NoError(t, err)
	for n, p := range packs {
		v := verifyPack(t, p.Bytes)
		require.Equal(t, tracepack.OutcomeFinalizedConsistent, v.Outcome, "pack %d", n)
		h := openPack(t, p.Bytes).Header()
		require.Equal(t, captureOf(seed, 0), h.CaptureID)
		require.Equal(t, IDFor(rolePackID, multiSeed(seed, multiPackRole, n)), h.PackID)
		require.Equal(t, IDFor(roleRecorderInstanceID, multiSeed(seed, multiRecorderRole, 0)), h.Meta.RecorderInstanceID)
	}
	h0, h1 := openPack(t, packs[0].Bytes).Header().Meta, openPack(t, packs[1].Bytes).Header().Meta
	require.Equal(t, TimeBase-hourNs+59*segmentPeriod, h0.PeriodStart)
	require.Equal(t, TimeBase, h0.PeriodEnd)
	require.Equal(t, uint64(4), h0.SeqStart)
	require.Equal(t, TimeBase+hourNs+2*segmentPeriod, h1.PeriodStart)
	require.Equal(t, uint64(5), h1.SeqStart)

	// A record outside its pack's hour is refused, as for any pack that is not an extract.
	_, err = writeMultiPacks(seed, []multiPack{{capture: 0, hour: 1, blocks: [][]tracepack.Record{{multiData(1, hourAt(0, 1), nil)}}}})
	require.ErrorIs(t, err, tracepack.ErrScopeBreach)
}

// TestMultiRecipesAreChecked checks that Generate refuses a multi-pack recipe that does not fit its class
// and fails, before writing anything, when a read or a pack disagrees with the recipe's expectation:
// a changed item, conflict or footer error, a read or a pack missing.
func TestMultiRecipesAreChecked(t *testing.T) {
	t.Parallel()

	refused := map[string]func(r *Recipe){
		"no read expectations": func(r *Recipe) { r.Expect.Reads = nil },
		"no pack expectations": func(r *Recipe) { r.Expect.Packs = nil },
		"a pack's outcome":     func(r *Recipe) { r.Expect.Packs = []*Expectation{{PrefixEnd: AtEnd()}, r.Expect.Packs[1]} },
		"a single-pack field":  func(r *Recipe) { r.Expect.Outcome = tracepack.OutcomeFinalizedConsistent },
		"a pack's queries": func(r *Recipe) {
			p := *r.Expect.Packs[0]
			p.Queries = []QueryWant{{ID: "all"}}
			r.Expect.Packs = []*Expectation{&p, r.Expect.Packs[1]}
		},
		"reads on a read vector": func(r *Recipe) { r.Class = ClassRead },
	}
	for name, edit := range refused {
		t.Run(name, func(t *testing.T) {
			t.Parallel()

			r := multiRecipe(t, "multi-payload-only-conflict")
			edit(&r)
			require.Error(t, r.check())
			_, err := generateRecipes(t.Context(), []Recipe{r}, "test")
			require.Error(t, err)
		})
	}

	disagreeing := []struct {
		id, name string
		edit     func(e *Expectation)
	}{
		{"multi-payload-only-conflict", "an item's conflict", func(e *Expectation) { e.Reads[0].Items[0].Conflict = true }},
		{"multi-payload-only-conflict", "an item's pack", func(e *Expectation) { e.Reads[1].Items[2].Pack = 0 }},
		{"multi-payload-only-conflict", "an item missing", func(e *Expectation) { e.Reads[0].Items = e.Reads[0].Items[1:] }},
		{"multi-payload-only-conflict", "a conflict's versions", func(e *Expectation) {
			e.Reads[0].Conflicts = []ConflictWant{{Capture: 0, Seq: 2, Versions: [][]int{{1}, {0}}}}
		}},
		{"multi-payload-only-conflict", "a conflict's capture", func(e *Expectation) {
			e.Reads[0].Conflicts = []ConflictWant{{Capture: 1, Seq: 2, Versions: [][]int{{0}, {1}}}}
		}},
		{"multi-payload-only-conflict", "a conflict missing", func(e *Expectation) { e.Reads[1].Conflicts = nil }},
		{"multi-identical-copy", "a conflict", func(e *Expectation) {
			e.Reads[0].Conflicts = []ConflictWant{{Capture: 0, Seq: 2, Versions: [][]int{{0}, {1}}}}
		}},
		{"multi-identical-copy", "a footer error", func(e *Expectation) { e.Reads[1].FooterErrors = []int{1} }},
		{"multi-identical-copy", "an incomplete reason", func(e *Expectation) {
			e.Reads[0].Incomplete = []PackIncompleteWant{{Pack: 0, IncompleteWant: Truncated(Pos{})}}
		}},
		{"multi-identical-copy", "the error form", func(e *Expectation) { e.Reads[0] = ReadWant{ID: "given-order", Error: ErrorConflictLimit} }},
		{"multi-capture-interleave", "the interleaving", func(e *Expectation) {
			items := e.Reads[1].Items
			e.Reads[1].Items = slices.Concat(items[:4], items[6:], items[4:6])
		}},
		{"multi-capture-interleave", "a read missing", func(e *Expectation) { e.Reads = e.Reads[1:] }},
		{"multi-capture-interleave", "a pack's blocks", func(e *Expectation) { e.Packs[1] = finalizedPack(3, 4) }},
		{"multi-capture-interleave", "a pack's seqs", func(e *Expectation) { e.Packs[0] = finalizedPack(3, 5) }},
	}
	for _, tt := range disagreeing {
		t.Run(tt.id+" "+tt.name, func(t *testing.T) {
			t.Parallel()

			r := multiRecipe(t, tt.id)
			tt.edit(r.Expect)
			_, err := generateRecipes(t.Context(), []Recipe{r}, "test")
			require.ErrorContains(t, err, "disagrees with its Expect")
		})
	}
}

// TestCheckRecordContents checks that a record a read returns must be the stored record its pack and block name:
// a changed record header field, payload byte or extension byte, a payload returned unrequested,
// or a block or seq the pack does not hold fails the check.
func TestCheckRecordContents(t *testing.T) {
	t.Parallel()

	// framing-header-extension holds records with extension areas.
	r := recipeByID(t, "framing-header-extension")
	pack := must(r.Build(r.seed()))(t).Pack
	stored := []map[storedKey]storedRecord{must(storedRecords(t.Context(), pack))(t)}
	read := func(payloads bool) []tracepack.Item {
		var items []tracepack.Item
		_, err := tracepack.MergeIterate(t.Context(), []*tracepack.Reader{openPack(t, pack)}, tracepack.Query{Payloads: payloads},
			tracepack.MergeIterateOptions{Order: tracepack.OrderCapture}, func(it *tracepack.Item) error {
				c := *it
				c.Record.Payload, c.HeaderExtra = bytes.Clone(it.Record.Payload), bytes.Clone(it.HeaderExtra)
				items = append(items, c)

				return nil
			})
		require.NoError(t, err)

		return items
	}
	with, without := read(true), read(false)
	require.NoError(t, checkRecordContents(with, []int{0}, stored, true))
	require.NoError(t, checkRecordContents(without, []int{0}, stored, false))
	extended := slices.IndexFunc(with, func(it tracepack.Item) bool { return len(it.HeaderExtra) > 0 })
	require.GreaterOrEqual(t, extended, 0, "a record with an extension area")

	tests := []struct {
		name     string
		payloads bool
		edit     func(items []tracepack.Item)
	}{
		{"a payload byte", true, func(items []tracepack.Item) { items[extended].Record.Payload[0] ^= 1 }},
		{"an extension byte", true, func(items []tracepack.Item) { items[extended].HeaderExtra[0] ^= 1 }},
		{"an extension dropped", true, func(items []tracepack.Item) { items[extended].HeaderExtra = nil }},
		{"an extension byte without payloads", false, func(items []tracepack.Item) { items[extended].HeaderExtra[0] ^= 1 }},
		{"a record header field", false, func(items []tracepack.Item) { items[0].Record.TSUTCNs++ }},
		{"the field validity", true, func(items []tracepack.Item) { items[extended].Record.FieldValidity ^= 1 }},
		{"a payload returned unrequested", false, func(items []tracepack.Item) { items[0].Record.Payload = []byte{} }},
		{"another block", true, func(items []tracepack.Item) { items[extended].Block++ }},
		{"another reader", true, func(items []tracepack.Item) { items[0].Pack = 1 }},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()

			items := read(tt.payloads)
			tt.edit(items)
			require.Error(t, checkRecordContents(items, []int{0}, stored, tt.payloads))
		})
	}
}

// TestGroupBudgets checks the committed corpus against the ceiling of each vector group and root file,
// and that a group or root file over its ceiling, a vector of no group and a root file of no ceiling each fail the check.
func TestGroupBudgets(t *testing.T) {
	t.Parallel()

	committed, err := readTree(corpusDir)
	require.NoError(t, err)
	assert.Empty(t, checkBudgets(committed))

	// Every group of the corpus has a ceiling, and the ceilings sum to no more than the corpus's.
	total := 0
	for _, g := range vectorGroups {
		c, ok := vectorGroupCeilings[g]
		require.True(t, ok, "group %s has no ceiling", g)
		total += c
	}
	require.Len(t, vectorGroupCeilings, len(vectorGroups))
	for _, c := range rootFileCeilings {
		total += c.ceiling
	}
	require.LessOrEqual(t, total, maxCorpusBytes)

	tests := []struct {
		name string
		edit func(c map[string][]byte)
	}{
		{"a group over its ceiling", func(c map[string][]byte) { c["multi-x/pack-0.tpk"] = make([]byte, vectorGroupCeilings["multi-"]+1) }},
		{"a group of single-pack vectors over its ceiling", func(c map[string][]byte) { c["sample-x/pack.tpk"] = make([]byte, vectorGroupCeilings["sample-"]+1) }},
		{"the manifest over its ceiling", func(c map[string][]byte) { c[FileManifest] = make([]byte, 120_001) }},
		{"the README over its ceiling", func(c map[string][]byte) {
			c["README.md"] = make([]byte, 20_001-len(c[".gitattributes"]))
		}},
		{"a vector of no group", func(c map[string][]byte) { c["other-x/pack.tpk"] = nil }},
		{"a root file of no ceiling", func(c map[string][]byte) { c["notes.txt"] = nil }},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()

			problems := checkBudgets(edits(committed, tt.edit))
			require.NotEmpty(t, problems)
		})
	}
}

// TestMultiPackReadPath checks that each committed golden of a multi-pack vector binds the read path:
// a changed verification report, read result or pack fails it, and the read path finds every numbered pack.
func TestMultiPackReadPath(t *testing.T) {
	t.Parallel()

	gen, err := generateRecipes(t.Context(), mustRecipes(t), UnknownEncoder)
	require.NoError(t, err)
	var m Manifest
	require.NoError(t, Unmarshal(gen[FileManifest], &m))
	k := slices.IndexFunc(m.Vectors, func(v Vector) bool { return v.ID == "multi-payload-only-conflict" })
	require.GreaterOrEqual(t, k, 0)
	v := &m.Vectors[k]
	require.NoError(t, readCommitted(t.Context(), v, gen))

	const dir = "multi-payload-only-conflict/"
	for name, edit := range map[string]func(c map[string][]byte){
		"a verification report": func(c map[string][]byte) {
			c[dir+PackVerifyFile(1)] = bytes.Replace(c[dir+PackVerifyFile(1)], []byte(`"records": "3"`), []byte(`"records": "2"`), 1)
		},
		"an item": func(c map[string][]byte) {
			c[dir+FileReads] = bytes.Replace(c[dir+FileReads], []byte(`"conflict": true`), []byte(`"conflict": false`), 1)
		},
		"a read's filter": func(c map[string][]byte) {
			var rs Reads
			require.NoError(t, Unmarshal(c[dir+FileReads], &rs))
			rs[0].Filter = Filter{Kinds: []string{tracepack.KindAnnotation.String()}}
			c[dir+FileReads] = must(rs.Marshal())(t)
		},
		"a pack": func(c map[string][]byte) { c[dir+PackFile(1)] = must(FlipByte(c[dir+PackFile(1)], 100))(t) },
		"a pack missing": func(c map[string][]byte) {
			delete(c, dir+PackFile(1))
		},
	} {
		bad := edits(gen, edit)
		require.Error(t, readCommitted(t.Context(), v, bad), name)
	}
}

// multiRecipe returns a copy of the multi-pack recipe of id whose expectation can be edited without changing the recipe's.
func multiRecipe(t *testing.T, id string) Recipe {
	t.Helper()

	r := recipeByID(t, id)
	e := *r.Expect
	e.Packs = slices.Clone(e.Packs)
	e.Reads = slices.Clone(e.Reads)
	for i := range e.Reads {
		e.Reads[i].Items = slices.Clone(e.Reads[i].Items)
	}
	r.Expect = &e

	return r
}
