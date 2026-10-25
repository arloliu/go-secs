package corpus

import (
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/arloliu/go-secs/tracepack"
)

// TestVersionVectorsKeepTheirStatus checks the version-aware vectors through the reader
// (the tracepack format specification §5 and §14):
// in a pack of format_minor 0 tag 0x0033 stays unknown, quality bit 7 has no meaning,
// and format 1.1's nested tags of a redaction_policy and a redaction entry are no encryption fields;
// a pack of minor 2 is read with the newest registry, its unassigned tag kept.
func TestVersionVectorsKeepTheirStatus(t *testing.T) {
	t.Parallel()

	rd, items := builtItems(t, "basic-new-tags-segment-1-0")
	h := rd.Header()
	require.Equal(t, uint16(0), h.FormatMinor)
	assert.Empty(t, h.Meta.Thinning)
	require.Len(t, h.Meta.Unknown, 2)
	for _, u := range h.Meta.Unknown {
		assert.Equal(t, metaThinningTag, u.Tag)
	}
	require.Len(t, items, 3)
	assert.Equal(t, tracepack.Quality(qualityBit7), items[1].Record.Quality, "the stored bit is kept")
	assert.False(t, rd.Unobserved(&items[1].Record), "a reserved bit in a pack of minor 0")

	rd, items = builtItems(t, "basic-new-tags-extract-1-0")
	h = rd.Header()
	require.Equal(t, uint16(0), h.FormatMinor)
	require.NotNil(t, h.Meta.RedactionPolicy)
	assert.Equal(t, extractPolicyID, h.Meta.RedactionPolicy.PolicyID)
	assert.Nil(t, h.Meta.RedactionPolicy.EncryptionAlgorithm)
	assert.Nil(t, h.Meta.RedactionPolicy.EncryptionKeyID)
	require.Len(t, h.Meta.Redaction, 1)
	assert.False(t, h.Meta.Redaction[0].HasSIV)
	assert.Equal(t, extractDomain, h.Meta.Redaction[0].Domain)
	assert.Empty(t, h.RedactionDefects)
	require.NotNil(t, items[0].Redaction)
	assert.False(t, items[0].Redaction.WhollyMasked, "%v", items[0].Redaction.Defects)

	for id, classifiers := range map[string][]string{
		"basic-extract-policy-no-classifier-1-0":    nil,
		"basic-extract-policy-multi-classifier-1-0": {corpusClassifier, secondClassifier},
	} {
		rd, _ = builtItems(t, id)
		h = rd.Header()
		assert.Equal(t, uint16(0), h.FormatMinor, id)
		assert.Equal(t, tracepack.PackRoleExtract, h.Meta.PackRole, id)
		assert.NotNil(t, h.Meta.RedactionPolicy, id)
		assert.Empty(t, h.Meta.Redaction, id)
		assert.Equal(t, classifiers, h.Meta.Classifiers, id)
	}

	rd, _ = builtItems(t, "basic-future-minor")
	h = rd.Header()
	assert.Equal(t, uint16(3), h.FormatMinor)
	require.Len(t, h.Meta.Unknown, 1)
	assert.Equal(t, unassignedMetaTag, h.Meta.Unknown[0].Tag)
}
