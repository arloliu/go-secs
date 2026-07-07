package main

import (
	"os"
	"testing"

	"github.com/stretchr/testify/require"
)

// TestDataItemsYAML loads the real, extraction-generated data/items.yaml (the
// full E5 Table 3 Data Item Dictionary catalog) and asserts it parses and
// validates cleanly with exactly 335 entries. This guards against silent
// corruption of the generated catalog: any drift in count, binding, or goType
// derivation fails this test rather than passing unnoticed into Task 18
// message authoring.
func TestDataItemsYAML(t *testing.T) {
	data, err := os.ReadFile("data/items.yaml")
	require.NoError(t, err)

	items, err := LoadItems(data)
	require.NoError(t, err)
	require.NoError(t, ValidateItems(items))
	require.Len(t, items, 335)
}
