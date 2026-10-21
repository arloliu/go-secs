package main

import (
	"os"
	"testing"

	"github.com/stretchr/testify/require"
)

// TestDataItemsYAML loads the real data/items.yaml
// (the full E5 Table 3 Data Item Dictionary catalog, plus PODID, which E5 uses but Table 3 omits)
// and asserts it parses and validates cleanly with exactly 336 entries.
// This guards against silent corruption of the catalog:
// any drift in count, binding, or goType derivation fails this test
// rather than passing unnoticed into message authoring.
func TestDataItemsYAML(t *testing.T) {
	data, err := os.ReadFile("data/items.yaml")
	require.NoError(t, err)

	items, err := LoadItems(data)
	require.NoError(t, err)
	require.NoError(t, ValidateItems(items))
	require.Len(t, items, 336)
}

// TestDataItemsRegenerateShippedItems proves the real data/items.yaml renders exactly the shipped gem/items.go,
// so a change to how values: tables load or render cannot silently change the generated enumerations.
func TestDataItemsRegenerateShippedItems(t *testing.T) {
	data, err := os.ReadFile("data/items.yaml")
	require.NoError(t, err)

	items, err := LoadItems(data)
	require.NoError(t, err)
	require.NoError(t, ValidateItems(items))

	got, err := renderItems(items)
	require.NoError(t, err)

	want, err := os.ReadFile("../../gem/items.go")
	require.NoError(t, err)
	require.Equal(t, string(want), string(got))
}
