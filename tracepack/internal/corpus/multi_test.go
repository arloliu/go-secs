package corpus

import (
	"bytes"
	"errors"
	"fmt"
	"maps"
	"slices"
	"strconv"
	"strings"
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
		{"multi-time-backward", "the time order", func(e *Expectation) {
			items := e.Reads[0].Items
			items[1], items[2] = items[2], items[1]
		}},
		{"multi-equal-timestamps", "the version order", func(e *Expectation) {
			e.Reads[4].Conflicts = []ConflictWant{{Capture: 2, Seq: 7, Versions: [][]int{{3}, {4}}}}
		}},
		{"multi-equal-timestamps", "the clusters tied by the order of packs", func(e *Expectation) {
			c := e.Reads[2].Conflicts
			e.Reads[2].Conflicts = []ConflictWant{c[1], c[0]}
		}},
		{"multi-header-conflicts", "a conflict missing", func(e *Expectation) { e.Reads[0].Conflicts = e.Reads[0].Conflicts[1:] }},
		{"multi-conflict-limit", "the error form at the bound", func(e *Expectation) {
			e.Reads[0] = ReadWant{ID: "limit-exact", Error: ErrorConflictLimit, Conflicts: limitConflicts}
		}},
		{"multi-conflict-limit", "success past the bound", func(e *Expectation) {
			e.Reads[1] = ReadWant{ID: "limit-exceeded", Items: e.Reads[0].Items, Conflicts: limitConflicts}
		}},
		{"multi-conflict-limit", "the conflicts in the order of capture_id and seq", func(e *Expectation) {
			e.Reads[1].Conflicts = limitConflicts[1:]
		}},
		{"multi-conflict-limit", "the bound's conflicts out of discovery order", func(e *Expectation) {
			e.Reads[1].Conflicts = []ConflictWant{limitConflicts[1], limitConflicts[0]}
		}},
		{"multi-walked-excluded", "no footer error", func(e *Expectation) { e.Reads[0].FooterErrors = nil }},
		{"multi-unfinalized-beside-archive", "no footer error", func(e *Expectation) { e.Reads[0].FooterErrors = nil }},
		{"multi-unfinalized-beside-archive", "no incomplete reason", func(e *Expectation) { e.Reads[1].Incomplete = nil }},
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
// and that a group or root file over its ceiling, a vector of no group and a root file of no ceiling each fail the check;
// and that the size-budget table of the corpus README states exactly those ceilings and their sum,
// a changed, missing or extra row, a changed total or a missing table each failing the comparison.
func TestGroupBudgets(t *testing.T) {
	t.Parallel()

	committed, err := readTree(corpusDir)
	require.NoError(t, err)
	assert.Empty(t, checkBudgets(committed))
	readme := committed["README.md"]
	require.NoError(t, checkBudgetTable(readme))
	for name, edit := range map[string]func(md string) string{
		"a ceiling changed": func(md string) string { return strings.Replace(md, "| 330,000 |", "| 331,000 |", 1) },
		"a row missing": func(md string) string {
			i := strings.Index(md, "| `tx-` |")
			j := i + strings.Index(md[i:], "\n") + 1

			return md[:i] + md[j:]
		},
		"an extra row": func(md string) string {
			return strings.Replace(md, "| **Total**", "| `store-` | | | | 1 |\n| **Total**", 1)
		},
		"the total changed": func(md string) string { return strings.Replace(md, "| 1,489,000 |", "| 1,490,000 |", 1) },
		"no table":          func(md string) string { return strings.Replace(md, "| Budget |", "| Ceiling |", 1) },
	} {
		edited := edit(string(readme))
		require.NotEqual(t, string(readme), edited, name)
		require.Error(t, checkBudgetTable([]byte(edited)), name)
	}

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

// checkBudgetTable compares the size-budget table of the corpus README readme with the ceilings the corpus is checked against
// (vectorGroupCeilings, rootFileCeilings):
// the table's Budget column holds one row per vector group, by its prefix, and per set of root files, by its names,
// each that group's or set's ceiling, and a Total row holding their sum.
//
// Returns:
//   - error: a missing table, a row it cannot read, a row of no ceiling, a ceiling of no row, a budget other than its ceiling,
//     or a total other than the sum.
func checkBudgetTable(readme []byte) error {
	const header = "| Group | Vectors | Files | Bytes | Budget |"
	lines := strings.Split(string(readme), "\n")
	start := slices.Index(lines, header)
	if start < 0 || start+2 > len(lines) {
		return errors.New("the README has no size-budget table")
	}
	ceilings := make(map[string]int, len(vectorGroupCeilings)+len(rootFileCeilings))
	maps.Copy(ceilings, vectorGroupCeilings)
	sum := 0
	for _, c := range rootFileCeilings {
		ceilings[strings.Join(c.files, ", ")] = c.ceiling
	}
	for _, c := range ceilings {
		sum += c
	}

	var errs []error
	rows := make(map[string]int)
	total := -1
	for _, line := range lines[start+2:] {
		if !strings.HasPrefix(line, "|") {
			break
		}
		cells := strings.Split(strings.Trim(line, "|"), "|")
		if len(cells) != 5 {
			errs = append(errs, fmt.Errorf("budget row %q: %d cells", line, len(cells)))

			continue
		}
		name := strings.ReplaceAll(strings.TrimSpace(cells[0]), "`", "")
		budget, err := strconv.Atoi(strings.ReplaceAll(strings.TrimSpace(cells[4]), ",", ""))
		if err != nil {
			errs = append(errs, fmt.Errorf("budget row %q: %w", line, err))

			continue
		}
		if name == "**Total**" {
			total = budget

			continue
		}
		rows[name] = budget
	}
	for name, budget := range rows {
		ceiling, ok := ceilings[name]
		if !ok {
			errs = append(errs, fmt.Errorf("budget row %s: no ceiling", name))
		} else if budget != ceiling {
			errs = append(errs, fmt.Errorf("budget row %s: %d, its ceiling %d", name, budget, ceiling))
		}
	}
	for name := range ceilings {
		if _, ok := rows[name]; !ok {
			errs = append(errs, fmt.Errorf("ceiling %s: no budget row", name))
		}
	}
	if total != sum {
		errs = append(errs, fmt.Errorf("budget total %d, the ceilings sum to %d", total, sum))
	}

	return errors.Join(errs...)
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

	// A lookup's result and its inputs bind the read path too.
	k = slices.IndexFunc(m.Vectors, func(v Vector) bool { return v.ID == "tx-candidate-selection" })
	require.GreaterOrEqual(t, k, 0)
	v = &m.Vectors[k]
	require.NoError(t, readCommitted(t.Context(), v, gen))
	const txDir = "tx-candidate-selection/"
	for name, edit := range map[string]func(c map[string][]byte){
		"a version's flag": func(c map[string][]byte) {
			c[txDir+FileLookups] = bytes.Replace(c[txDir+FileLookups], []byte(`"valid": true`), []byte(`"valid": false`), 1)
		},
		"a lookup's source": func(c map[string][]byte) {
			c[txDir+FileLookups] = bytes.Replace(c[txDir+FileLookups], []byte(`"complete": true`), []byte(`"complete": false`), 1)
		},
		"lookups.json missing": func(c map[string][]byte) { delete(c, txDir+FileLookups) },
	} {
		bad := edits(gen, edit)
		require.Error(t, readCommitted(t.Context(), v, bad), name)
	}
}

// multiRecipe returns a copy of the multi-pack recipe of id whose expectation can be edited without changing the recipe's:
// its arrays of packs, reads and lookups, and the items of each read and the records, gaps and scopes of each lookup, are copies.
func multiRecipe(t *testing.T, id string) Recipe {
	t.Helper()

	r := recipeByID(t, id)
	e := *r.Expect
	e.Packs = slices.Clone(e.Packs)
	e.Reads = slices.Clone(e.Reads)
	for i := range e.Reads {
		e.Reads[i].Items = slices.Clone(e.Reads[i].Items)
	}
	e.Lookups = slices.Clone(e.Lookups)
	for i := range e.Lookups {
		l := &e.Lookups[i]
		l.Records, l.Gaps, l.Searched = slices.Clone(l.Records), slices.Clone(l.Gaps), slices.Clone(l.Searched)
	}
	r.Expect = &e

	return r
}

// TestReversedPacksKeepConflicts checks, for every read over several packs of the multi group,
// that the same read given its packs in the reverse order keeps which records conflict and which versions they have,
// and so which versions it yields, which footers it does not use and which incomplete reasons it reports,
// while the representatives, and with them the packs and blocks the items name, may change
// (the tracepack semantics specification §7.4, Conflicts).
// A read that fails on its conflict bound fails in both orders, the number of conflicts being the same,
// and its conflicts are not compared: the bound ends it on a conflict that depends on the order of discovery.
func TestReversedPacksKeepConflicts(t *testing.T) {
	t.Parallel()

	changed := 0
	for _, r := range mustRecipes(t) {
		if r.Class != ClassMultiPack || !strings.HasPrefix(r.ID, "multi-") {
			continue
		}
		built, err := r.Build(r.seed())
		require.NoError(t, err, r.ID)
		packs := make([][]byte, 0, len(built.Packs))
		captures := make([]string, 0, len(built.Packs))
		for _, p := range built.Packs {
			packs = append(packs, p.Bytes)
			captures = append(captures, openPack(t, p.Bytes).Header().CaptureID.String())
		}

		var reads []ReadSpec
		for _, rd := range built.Reads {
			if len(rd.Packs) < 2 {
				continue
			}
			rev := rd
			rev.ID, rev.Packs = rd.ID+"-reversed", slices.Clone(rd.Packs)
			slices.Reverse(rev.Packs)
			reads = append(reads, rd, rev)
		}
		if len(reads) == 0 {
			continue
		}
		out, err := readMultiVector(t.Context(), packs, reads, nil)
		require.NoError(t, err, r.ID)
		byID := make(map[string]*ReadExpect, len(out.reads))
		for i := range out.reads {
			byID[out.reads[i].ID] = &out.reads[i].Expect
		}

		for k := 0; k < len(reads); k += 2 {
			id := r.ID + "/" + reads[k].ID
			given, reversed := byID[reads[k].ID], byID[reads[k+1].ID]
			require.Equal(t, given.Error, reversed.Error, id)
			if given.Error != "" {
				require.Len(t, reversed.Conflicts, len(given.Conflicts), id)
				continue
			}
			require.Equal(t, conflictSet(given.Conflicts), conflictSet(reversed.Conflicts), "%s: the conflicts", id)
			require.Equal(t, yieldedVersions(*given.Items, given.Conflicts, captures), yieldedVersions(*reversed.Items, reversed.Conflicts, captures),
				"%s: the versions yielded", id)
			require.ElementsMatch(t, *given.FooterErrors, *reversed.FooterErrors, "%s: the footer errors", id)
			require.ElementsMatch(t, *given.Incomplete, *reversed.Incomplete, "%s: the incomplete reasons", id)
			if !slices.Equal(*given.Items, *reversed.Items) {
				changed++
			}
		}
	}
	require.Positive(t, changed, "no read changed its representatives in the reverse order")

	// The representatives change: in the reverse order, the version of pack 1 comes first, and pack 1 represents every record.
	r := recipeByID(t, "multi-conflict-selection")
	built, err := r.Build(r.seed())
	require.NoError(t, err)
	out, err := readMultiVector(t.Context(), [][]byte{built.Packs[0].Bytes, built.Packs[1].Bytes},
		[]ReadSpec{{ID: "reversed", Cites: readCites, Packs: []int{1, 0}, Order: ReadOrderCapture}}, nil)
	require.NoError(t, err)
	require.Equal(t, []ReadItem{{Seq: 1, Pack: 1}, {Seq: 2, Pack: 1, Conflict: true}, {Seq: 2, Pack: 0, Conflict: true}, {Seq: 3, Pack: 1}},
		*out.reads[0].Expect.Items)
	require.Equal(t, [][]int{{1}, {0}}, out.reads[0].Expect.Conflicts[0].Versions)

	// An item of a conflict names its version: pack 0's copy of seq 2 in place of pack 1's is another version.
	conflicts := out.reads[0].Expect.Conflicts
	items := slices.Clone(*out.reads[0].Expect.Items)
	other := slices.Clone(items)
	other[1].Pack, other[2].Pack = 0, 0
	captures := []string{conflicts[0].CaptureID, conflicts[0].CaptureID}
	require.NotEqual(t, yieldedVersions(items, conflicts, captures), yieldedVersions(other, conflicts, captures))
}

// conflictSet returns the conflicts cs as a set: each conflict's capture_id, seq and versions, each version's packs ascending,
// the versions and the conflicts sorted.
func conflictSet(cs []ConflictEntry) []string {
	out := make([]string, 0, len(cs))
	for _, c := range cs {
		vs := make([]string, 0, len(c.Versions))
		for _, v := range c.Versions {
			v = slices.Sorted(slices.Values(v))
			vs = append(vs, fmt.Sprint(v))
		}
		slices.Sort(vs)
		out = append(out, fmt.Sprintf("%s/%d %v", c.CaptureID, c.Seq, vs))
	}
	slices.Sort(out)

	return out
}

// yieldedVersions returns the items as a multiset of the versions they yield, without their representatives or order:
// each item's capture, from captures by pack number, and its seq,
// and for an item of a conflict, of conflicts, the packs holding its version, the version whose packs hold the item's pack, ascending;
// sorted.
func yieldedVersions(items []ReadItem, conflicts []ConflictEntry, captures []string) []string {
	out := make([]string, 0, len(items))
	for _, it := range items {
		key := fmt.Sprintf("%s/%d", captures[it.Pack], it.Seq)
		if it.Conflict {
			version := "no version"
			for _, c := range conflicts {
				if c.CaptureID != captures[it.Pack] || c.Seq != it.Seq {
					continue
				}
				for _, v := range c.Versions {
					if slices.Contains(v, it.Pack) {
						version = fmt.Sprint(slices.Sorted(slices.Values(v)))
					}
				}
			}
			key += " " + version
		}
		out = append(out, key)
	}
	slices.Sort(out)

	return out
}
