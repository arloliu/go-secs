package main

import (
	"os"
	"strings"
	"testing"

	"github.com/stretchr/testify/require"
)

// TestDataMessagesS1YAML loads the real data/items.yaml catalog together with
// the authored data/messages/s1.yaml and asserts the Stream 1 message schema
// parses, validates, and renders cleanly. It guards the actual content
// deliverable: exactly 24 functions (S1F1-S1F11, S1F13-S1F24 — no F12 gap, no
// F0), every {item: X} reference resolving against the real Data Item
// Dictionary, and the S1F12 external-provenance disclaimer surviving into the
// generated godoc while no e5-sourced function carries it.
func TestDataMessagesS1YAML(t *testing.T) {
	itemsData, err := os.ReadFile("data/items.yaml")
	require.NoError(t, err)

	items, err := LoadItems(itemsData)
	require.NoError(t, err)
	require.NoError(t, ValidateItems(items))

	msgData, err := os.ReadFile("data/messages/s1.yaml")
	require.NoError(t, err)

	mf, err := LoadMessageFile(msgData)
	require.NoError(t, err)
	require.Equal(t, 1, mf.Stream)

	// Exactly 24 functions, cross-referenced against the real items.yaml so
	// every item reference at any structure depth actually resolves.
	require.Len(t, mf.Messages, 24)
	require.NoError(t, ValidateMessages([]MessageFile{mf}, items))

	// Function numbers are F1-F11 and F13-F24: F12 exists (it is not skipped)
	// but there is no F0 and no gap other than the intentional non-emission of
	// any Abort/Function-0 entry.
	byFunc := map[int]Message{}
	for _, m := range mf.Messages {
		byFunc[m.Function] = m
	}
	for f := 1; f <= 24; f++ {
		_, ok := byFunc[f]
		require.Truef(t, ok, "expected S1F%d to be present", f)
	}
	_, hasF0 := byFunc[0]
	require.False(t, hasF0, "S1F0 must not be authored")

	// S1F12 carries its external-provenance metadata.
	f12 := byFunc[12]
	require.Equal(t, "external", f12.Source, "S1F12 must be source: external")
	require.Equal(t, "low", f12.Confidence, "S1F12 must be confidence: low")

	// The external disclaimer must survive into the generated S1F12 godoc, and
	// no other (e5-sourced) function's godoc may contain it.
	for _, m := range mf.Messages {
		for _, b := range m.Bodies {
			fv := newFuncView(mf.Stream, m, b, items)
			doc := strings.Join(fv.Doc, "\n")
			if m.Function == 12 {
				require.Containsf(t, doc, externalSourceDisclaimer,
					"S1F12 godoc must contain the external-source disclaimer")
			} else {
				require.NotContainsf(t, doc, externalSourceDisclaimer,
					"S1F%d (source %q) godoc must not contain the external-source disclaimer",
					m.Function, m.Source)
			}
		}
	}

	// The whole file renders without error (exercises the template end-to-end).
	_, err = renderMessages(mf, items)
	require.NoError(t, err)
}
