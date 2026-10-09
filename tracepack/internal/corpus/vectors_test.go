package corpus

import (
	"bytes"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/arloliu/go-secs/tracepack"
)

// TestInvalidFooterVectorsBreakTheirClause checks that the tracepack package rejects the footer of each vector
// whose subject is one footer validation clause (the tracepack format specification §10) for that clause:
// the first check its validation fails names it.
// The package checks the clauses in its own order, so this shows that no clause it checks earlier fails.
// The recipes keep the other clauses intact where the clause allows it, verify-defect-seq-order's included,
// and three of them check that as they build;
// footer-invalid-epoch-range and footer-invalid-overflow cannot, and also break the epoch-entry clause and the exact count sums.
// The repair vectors whose footer states a seq-order defect need only a rejected footer:
// this pins the clause that rejects it first, and their footers break the seq-set and F-5 aggregate clauses too.
func TestInvalidFooterVectorsBreakTheirClause(t *testing.T) {
	t.Parallel()

	clauses := map[string]string{
		"footer-invalid-f2-offset":            "f2_offset 80, not 72",
		"footer-invalid-f2-entry-len":         "f2_entry_len 79 below 80",
		"footer-invalid-sections":             "F-5 at offset",
		"footer-invalid-f2-contiguity":        "block 1: F-2 offset",
		"footer-invalid-seq-order":            "block 1: F-2 first_seq 4 does not follow the previous last_seq 4",
		"footer-invalid-summary-bounds":       "block 1: F-2 summary at",
		"footer-invalid-seq-set":              "block 0: F-3 seq_range 0 starts at 3",
		"footer-invalid-count-sums":           "block 0: F-3 kind_counts sums past record_count 3",
		"footer-invalid-f5-aggregate":         "F-5 content_bytes",
		"footer-invalid-trailer-record-count": "trailer record_count 8",
		"footer-invalid-trailer-last-seq":     "trailer last_seq 8",
		"footer-invalid-header-len":           "record_header_len 43 below 44",
		"footer-invalid-ts-range":             "lie in different UTC hours",
		"footer-invalid-epoch-range":          "epoch_min 1 above epoch_max 0",
		"footer-invalid-epoch-entry":          "F-3 epoch 1 seqs 0 to 4 outside",
		"footer-invalid-boundary-seq":         "F-3 boundary seq 2 is not a seq of the block",
		"footer-invalid-overflow":             "block 1: F-3 epoch record counts sum past record_count 4",
		"footer-invalid-empty-stats":          "F-1 f3_len 16 in a pack without blocks",
		"footer-seq-range-hides-gap":          "block 0: F-3 without seq_range entries",
		"footer-f5-higher-close-seq":          "F-5 epoch entries differ",
		"verify-defect-seq-order":             "block 1: F-2 first_seq 2 does not follow the previous last_seq 5",
		"repair-refused-seq-order":            "block 2: F-2 first_seq 1 does not follow the previous last_seq 3",
		"repair-refused-lost-run":             "block 2: F-2 first_seq 1 does not follow the previous last_seq 3",
	}
	for id, clause := range clauses {
		t.Run(id, func(t *testing.T) {
			t.Parallel()

			r := recipeByID(t, id)
			b := must(r.Build(r.seed()))(t)
			rd, err := tracepack.Open(t.Context(), bytes.NewReader(b.Pack), int64(len(b.Pack)), tracepack.ReaderOptions{})
			require.NoError(t, err)
			require.ErrorIs(t, rd.Header().FooterErr, tracepack.ErrInvalidFooter)
			assert.Contains(t, rd.Header().FooterErr.Error(), clause)
		})
	}
}

// TestRepairRefusalVectorsMeetTheirCondition checks that the tracepack package refuses to repair each refusal vector
// for the condition it is built for (the tracepack format specification §13):
// the first condition its Repair finds names it.
// Repair checks the conditions in its own order, role, period, writer defects, block hours, commitments, then lost runs,
// so this shows that no condition it checks earlier holds;
// repair-refused-hour-span meets the block-hour condition too, which Repair checks after the writer defects.
func TestRepairRefusalVectorsMeetTheirCondition(t *testing.T) {
	t.Parallel()

	conditions := map[string][]string{
		"repair-refused-extract":      {"pack_role extract is not a stored pack's"},
		"repair-refused-seq-order":    {"would reject the seq-order writer defect of block 2"},
		"repair-refused-hour-span":    {"would reject the hour-span writer defect of block 1"},
		"repair-refused-lost-run":     {"the lost run from block 1 has no seq between its validated neighbours"},
		"repair-refused-scope-breach": {"block 2 at offset", "lies outside the UTC hour of the period"},
	}
	for id, condition := range conditions {
		t.Run(id, func(t *testing.T) {
			t.Parallel()

			r := recipeByID(t, id)
			b := must(r.Build(r.seed()))(t)
			opts := RepairOptionsFor(r.seed(), b.Repair)
			o := must(opts.ToOptions())(t)
			var patch bytes.Buffer
			_, err := tracepack.Repair(t.Context(), bytes.NewReader(b.Pack), int64(len(b.Pack)), &patch, o)
			require.ErrorIs(t, err, tracepack.ErrNotRepairable)
			for _, c := range condition {
				assert.Contains(t, err.Error(), c)
			}
			assert.Zero(t, patch.Len())
		})
	}
}

// TestCheckInjection checks that checkInjection accepts the I-2 injection and refuses a damage that fails no block.
func TestCheckInjection(t *testing.T) {
	t.Parallel()

	run := must(InjectionRun())(t)
	require.NoError(t, checkInjection(run, InjectI2))
	require.Error(t, checkInjection(run, func(enc []byte) []byte { return enc }), "an identity damage")
}

// TestNewLogDataCorrelation checks that a log record of epoch 0 is correlation-incomplete without a missing identity,
// and that one of epoch 1 with every identity is not.
func TestNewLogDataCorrelation(t *testing.T) {
	t.Parallel()

	frame := dataFrame(1, 1, true, 1, nil)
	epoch0 := newLogData(0, 0, tracepack.DirHostToEquipment, frame, 0)
	assert.NotZero(t, epoch0.Quality&tracepack.QualityCorrelationIncomplete, "epoch 0")
	epoch1 := newLogData(0, 1, tracepack.DirHostToEquipment, frame, 0)
	assert.Zero(t, epoch1.Quality&tracepack.QualityCorrelationIncomplete, "epoch 1")
}
