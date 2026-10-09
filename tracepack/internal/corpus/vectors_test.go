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
// The recipes keep the other clauses intact where the clause allows it, and three of them check that as they build;
// footer-invalid-epoch-range and footer-invalid-overflow cannot, and also break the epoch-entry clause and the exact count sums.
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
