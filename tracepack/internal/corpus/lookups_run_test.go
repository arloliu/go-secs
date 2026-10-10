package corpus

import (
	"testing"

	"github.com/stretchr/testify/require"

	"github.com/arloliu/go-secs/tracepack"
)

// TestLookupRecipesAreChecked checks that Generate refuses a lookup recipe that does not fit its class
// and fails, before writing anything, when a lookup disagrees with the recipe's expectation:
// a changed flag, fact, outcome, role, key field or scope, a form changed, or a lookup missing.
func TestLookupRecipesAreChecked(t *testing.T) {
	t.Parallel()

	refused := map[string]func(r *Recipe){
		"no lookup expectations":   func(r *Recipe) { r.Expect.Lookups = nil },
		"lookups on a read vector": func(r *Recipe) { r.Class = ClassRead },
	}
	for name, edit := range refused {
		t.Run(name, func(t *testing.T) {
			t.Parallel()

			r := multiRecipe(t, "tx-repeated-key")
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
		{"tx-repeated-key", "a candidate's valid flag", func(e *Expectation) { e.Lookups[0].Records[1].Valid = new(false) }},
		{"tx-repeated-key", "a version's bound flag", func(e *Expectation) { e.Lookups[0].Records[3].Bound = true }},
		{"tx-repeated-key", "a version's in_window flag", func(e *Expectation) { e.Lookups[1].Records[1].InWindow = true }},
		{"tx-repeated-key", "an existence fact", func(e *Expectation) { e.Lookups[1].Gaps = []FactWant{{Fact: Fact{Reason: FactOpenWindow}}} }},
		{"tx-repeated-key", "a conflict fact", func(e *Expectation) {
			e.Lookups[0].Gaps = []FactWant{{Fact: Fact{Reason: FactConflict, Hours: []I64{I64(txHour)}, Seq: new(U64(2))}}}
		}},
		{"tx-repeated-key", "the window end", func(e *Expectation) { e.Lookups[0].WindowEnd = new(uint64(4)) }},
		{"tx-candidate-selection", "the outcome", func(e *Expectation) { e.Lookups[4].Outcome = "ambiguous" }},
		{"tx-candidate-selection", "a version's roles", func(e *Expectation) { e.Lookups[0].Records[1].Roles = []string{RoleOutcome} }},
		{"tx-candidate-selection", "a version kept", func(e *Expectation) {
			r := e.Lookups[0].Records
			e.Lookups[0].Records = append([]LookupRecord{r[0], txKept(10, true, false)}, r[1:]...)
		}},
		{"tx-candidate-selection", "a key field", func(e *Expectation) { e.Lookups[0].Key = txPrimaryKey(9, selectionSystemBytes['f']) }},
		{"tx-candidate-selection", "the scope searched", func(e *Expectation) { e.Lookups[1].Searched[0].Indexed = false }},
		{"tx-candidate-selection", "the error form as a result", func(e *Expectation) {
			e.Lookups[2] = txSelection("not-primary", 1, 'a', txCandidate(2, true))
		}},
		{"tx-candidate-selection", "a result as the error form", func(e *Expectation) { e.Lookups[4] = LookupWant{ID: "valid", Error: ErrorNotPrimary} }},
		{"tx-candidate-selection", "a lookup missing", func(e *Expectation) { e.Lookups = e.Lookups[1:] }},
		{"tx-index-mismatch", "a fact with both a position and an offset", func(e *Expectation) {
			e.Lookups[0].Gaps[0].Offset = new(U64(0))
		}},
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

// TestLookupViewOrder checks the view order of a lookup's source (the tracepack corpus specification §8):
// the generation's member first, unless a patch based on the generation replaced it,
// then the scope's other view packs in the order the source lists them,
// a patch based on the generation standing where it is listed and the pack it replaces left out.
// The view order decides searched's packs and the representative, and so the pack, of each version kept.
func TestLookupViewOrder(t *testing.T) {
	t.Parallel()

	// One scope of capture A in hour H: pack 0 the member of generation 1, holding the primary at 1;
	// segments 1 and 2, each one block of its reply at 2 and the socket-close at 3;
	// pack 3 a patch based on the generation that replaces segment 2, holding the same records;
	// pack 4 a patch based on the generation that replaces the member, holding the primary.
	// The copies of 2 and 3 lie in blocks of the same first seq, so the view order alone decides their representative
	// (the tracepack semantics specification §7.4, Conflicts).
	const seed = "lookup-view-order"
	x := uint32(0x78)
	primary, reply := txPrimary(1, 1, x), txReply(2, 1, 2, x)
	closed, err := newSocketEvent(3, 1, tracepack.EventSocketClose)
	require.NoError(t, err)
	set := IDFor(roleReplacementSetID, multiSeed(seed, multiSetRole, 0))
	patch := func(replaced int) func(m *tracepack.PackMeta) {
		return func(m *tracepack.PackMeta) {
			m.PackRole, m.PatchBase = tracepack.PackRoleRepair, &set
			m.Supersedes = []tracepack.UUID{IDFor(rolePackID, multiSeed(seed, multiPackRole, replaced))}
			m.QualityEvaluated = true
		}
	}
	built, err := writeMultiPacks(seed, []multiPack{
		{capture: 0, archive: true, blocks: [][]tracepack.Record{{primary}}, edit: qualityEvaluated},
		{capture: 0, minute: 1, blocks: [][]tracepack.Record{{reply, closed}}, edit: qualityEvaluated},
		{capture: 0, minute: 2, blocks: [][]tracepack.Record{{reply, closed}}, edit: qualityEvaluated},
		{capture: 0, minute: 3, blocks: [][]tracepack.Record{{reply, closed}}, edit: patch(2)},
		{capture: 0, minute: 4, blocks: [][]tracepack.Record{{primary}}, edit: patch(0)},
	})
	require.NoError(t, err)
	packs := make([][]byte, 0, len(built))
	stored := make([]map[storedKey]storedRecord, 0, len(built))
	for _, p := range built {
		packs = append(packs, p.Bytes)
		stored = append(stored, must(storedRecords(t.Context(), p.Bytes))(t))
	}
	require.Equal(t, tracepack.PackRoleRepair, openPack(t, packs[3]).Header().Meta.PackRole)

	tests := []struct {
		name     string
		view     []int
		searched []int
		// reps holds the pack of the version kept at seqs 1, 2 and 3.
		reps [3]int
	}{
		{"segments listed in order", []int{1, 0, 2}, []int{0, 1, 2}, [3]int{0, 1, 1}},
		{"segments listed in reverse", []int{2, 1, 0}, []int{0, 2, 1}, [3]int{0, 2, 2}},
		{"a patch listed after a segment", []int{1, 3, 0, 2}, []int{0, 1, 3}, [3]int{0, 1, 1}},
		{"a patch listed first", []int{3, 2, 1, 0}, []int{0, 3, 1}, [3]int{0, 3, 3}},
		{"a patch replacing the member", []int{1, 4, 0, 2}, []int{1, 4, 2}, [3]int{4, 1, 1}},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()

			spec := txLookup(seed, "view-order", 1, tt.view...)
			v, err := runLookup(t.Context(), packs, stored, &spec)
			require.NoError(t, err)
			e := v.Expect
			require.Equal(t, "matched", e.Outcome)
			require.Empty(t, *e.Gaps)
			require.Equal(t, []SearchedScope{{Hour: I64(txHour), Indexed: true, Packs: tt.searched}}, *e.Searched)
			require.Len(t, *e.Records, 3)
			for i, r := range *e.Records {
				require.Equal(t, U64(i+1), r.Seq)
				require.Equal(t, tt.reps[i], r.Pack, "the version kept at seq %d", r.Seq)
			}
		})
	}
}

// TestRunLookupRefusesSource checks that the harness refuses a source that is not a set of packs of the vector
// before the library sees it (the tracepack corpus specification §5.11):
// an evidence pack listed in view, a pack listed twice, a pack the vector does not have, an empty view, an absent evidence.
func TestRunLookupRefusesSource(t *testing.T) {
	t.Parallel()

	r := recipeByID(t, "tx-repeated-key")
	built := must(r.Build(r.seed()))(t)
	packs := [][]byte{built.Packs[0].Bytes}
	stored := []map[storedKey]storedRecord{must(storedRecords(t.Context(), packs[0]))(t)}
	spec := built.Lookups[0]
	_, err := runLookup(t.Context(), packs, stored, &spec)
	require.NoError(t, err)

	tests := map[string]func(s *LookupSource){
		"an evidence pack in view": func(s *LookupSource) { s.Evidence = []int{0} },
		"a view pack twice":        func(s *LookupSource) { s.View = []int{0, 0} },
		"a pack of none":           func(s *LookupSource) { s.View = []int{1} },
		"a negative pack":          func(s *LookupSource) { s.Evidence = []int{-1} },
		"an empty view":            func(s *LookupSource) { s.View = []int{} },
		"no evidence":              func(s *LookupSource) { s.Evidence = nil },
	}
	for name, edit := range tests {
		t.Run(name, func(t *testing.T) {
			t.Parallel()

			s := built.Lookups[0]
			edit(&s.Source)
			_, err := runLookup(t.Context(), packs, stored, &s)
			require.Error(t, err)
		})
	}
}

// TestCheckTxRecordContents checks that a version a lookup keeps must be the stored record its pack and block name,
// its payload always compared: a changed payload byte, a payload dropped, a changed record header field,
// another block, or a pack of no source fails the check.
func TestCheckTxRecordContents(t *testing.T) {
	t.Parallel()

	r := recipeByID(t, "tx-candidate-selection")
	built := must(r.Build(r.seed()))(t)
	pack := built.Packs[0].Bytes
	stored := []map[storedKey]storedRecord{must(storedRecords(t.Context(), pack))(t)}
	reader := openPack(t, pack)
	numbers := map[tracepack.UUID]int{reader.Header().PackID: 0}
	lookup := func() []tracepack.TxRecord {
		src, err := tracepack.NewReaderSource([]*tracepack.Reader{reader}, tracepack.ReaderSourceOptions{Complete: true})
		require.NoError(t, err)
		res, err := tracepack.FindTransaction(t.Context(), src, tracepack.TxKey{Capture: captureOf(r.seed(), 0), Seq: 11, Hour: txHour},
			tracepack.TxOptions{MaxScopes: 1})
		require.NoError(t, err)
		require.Len(t, res.Records, 4)

		return res.Records
	}
	require.NoError(t, checkTxRecordContents(lookup(), numbers, stored))

	tests := map[string]func(rs []tracepack.TxRecord){
		"a payload byte":         func(rs []tracepack.TxRecord) { rs[1].Record.Payload[13] ^= 1 },
		"a payload dropped":      func(rs []tracepack.TxRecord) { rs[1].Record.Payload = nil },
		"a record header field":  func(rs []tracepack.TxRecord) { rs[2].Record.TSUTCNs++ },
		"an extension added":     func(rs []tracepack.TxRecord) { rs[0].HeaderExtra = []byte{} },
		"another block":          func(rs []tracepack.TxRecord) { rs[3].Block++ },
		"a pack of no source":    func(rs []tracepack.TxRecord) { rs[0].Pack = tracepack.UUID{} },
		"another seq in a block": func(rs []tracepack.TxRecord) { rs[2].Record.Seq = 15 },
	}
	for name, edit := range tests {
		t.Run(name, func(t *testing.T) {
			t.Parallel()

			rs := lookup()
			edit(rs)
			require.Error(t, checkTxRecordContents(rs, numbers, stored))
		})
	}
}
