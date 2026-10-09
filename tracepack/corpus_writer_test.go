package tracepack_test

import (
	"bytes"
	"os"
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/require"

	"github.com/arloliu/go-secs/tracepack"
	"github.com/arloliu/go-secs/tracepack/internal/corpus"
)

// TestCorpusValidatingWriter runs the validating Writer of the conformance corpus's I-2 injection vector
// with the defect injected into its last block's encoded body:
// the last Flush fails with ErrValidation, and the output, the file header, the pack metadata and every earlier block,
// equals the vector's committed pack.tpk byte for byte (the tracepack corpus specification §1, the tracepack format specification §12).
func TestCorpusValidatingWriter(t *testing.T) {
	t.Parallel()

	want, err := os.ReadFile(filepath.Join("testdata", "corpus", corpus.InjectionID, corpus.FilePack))
	require.NoError(t, err)
	run, err := corpus.InjectionRun()
	require.NoError(t, err)

	var buf bytes.Buffer
	w, err := tracepack.NewWriter(&buf, run.Options)
	require.NoError(t, err)
	calls := 0
	tracepack.SetEncodedHook(w, func(enc []byte) []byte {
		calls++
		if calls == len(run.Blocks) {
			return corpus.InjectI2(enc)
		}

		return enc
	})
	for i, block := range run.Blocks {
		for j := range block {
			require.NoError(t, w.Append(&block[j]), "block %d, record %d", i, j)
		}
		if i < len(run.Blocks)-1 {
			require.NoError(t, w.Flush(), "block %d", i)
			continue
		}
		require.ErrorIs(t, w.Flush(), tracepack.ErrValidation, "the damaged block")
	}

	require.Equal(t, want, buf.Bytes())
}
