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

// TestDataMessagesS5YAML loads the real data/items.yaml catalog together with
// the authored data/messages/s5.yaml and asserts the Stream 5 Exception
// Handling schema parses, validates, and renders cleanly. It guards the content
// deliverable: exactly 18 functions (S5F1-S5F18, no F0 gap), with F15
// (Exception Recovery Complete Notify) genuinely present in E5 (not a dropped
// "Not Used"), every {item: X} reference resolving against the real Data Item
// Dictionary, and the four sender-optional notifies (F1, F3, F9, F11) plus the
// mandatory-reply requests (F5, F13, F17) and F15 carrying replyExpected: true.
func TestDataMessagesS5YAML(t *testing.T) {
	itemsData, err := os.ReadFile("data/items.yaml")
	require.NoError(t, err)

	items, err := LoadItems(itemsData)
	require.NoError(t, err)
	require.NoError(t, ValidateItems(items))

	msgData, err := os.ReadFile("data/messages/s5.yaml")
	require.NoError(t, err)

	mf, err := LoadMessageFile(msgData)
	require.NoError(t, err)
	require.Equal(t, 5, mf.Stream)

	// Exactly 18 functions, cross-referenced against the real items.yaml so
	// every item reference at any structure depth actually resolves.
	require.Len(t, mf.Messages, 18)
	require.NoError(t, ValidateMessages([]MessageFile{mf}, items))

	// Function numbers are a contiguous F1-F18 with no F0 and no gap. In
	// particular F15 (Exception Recovery Complete Notify) is present: E5 gives it
	// a full Description and Structure, so it is authored, not skipped.
	byFunc := map[int]Message{}
	for _, m := range mf.Messages {
		byFunc[m.Function] = m
	}
	for f := 1; f <= 18; f++ {
		_, ok := byFunc[f]
		require.Truef(t, ok, "expected S5F%d to be present", f)
	}
	_, hasF0 := byFunc[0]
	require.False(t, hasF0, "S5F0 must not be authored")

	// Every message carries exactly one body (no sender-dependent structure in
	// Stream 5), and reply expectation matches E5's W-bit markers: the primary
	// notifies/requests expect a reply; every acknowledge/confirm/data reply does
	// not.
	replyExpected := map[int]bool{1: true, 3: true, 5: true, 7: true, 9: true, 11: true, 13: true, 15: true, 17: true}
	for _, m := range mf.Messages {
		require.Lenf(t, m.Bodies, 1, "S5F%d must have a single body", m.Function)
		require.Equalf(t, replyExpected[m.Function], m.Bodies[0].ReplyExpected,
			"S5F%d replyExpected mismatch", m.Function)
	}

	// The whole file renders without error (exercises the template end-to-end).
	_, err = renderMessages(mf, items)
	require.NoError(t, err)
}

// TestDataMessagesS9YAML loads the real data/items.yaml catalog together with
// the authored data/messages/s9.yaml and asserts the Stream 9 System Errors
// schema parses, validates, and renders cleanly. It guards the content
// deliverable: exactly 7 functions (S9F1, F3, F5, F7, F9, F11, F13), with the
// "Not Used" functions (F0, F2, F4, F6, F8, F10, F12) absent, and every
// {item: X} reference resolving against the real Data Item Dictionary.
func TestDataMessagesS9YAML(t *testing.T) {
	itemsData, err := os.ReadFile("data/items.yaml")
	require.NoError(t, err)

	items, err := LoadItems(itemsData)
	require.NoError(t, err)
	require.NoError(t, ValidateItems(items))

	msgData, err := os.ReadFile("data/messages/s9.yaml")
	require.NoError(t, err)

	mf, err := LoadMessageFile(msgData)
	require.NoError(t, err)
	require.Equal(t, 9, mf.Stream)

	// Exactly 7 functions, cross-referenced against the real items.yaml so
	// every item reference at any structure depth actually resolves.
	require.Len(t, mf.Messages, 7)
	require.NoError(t, ValidateMessages([]MessageFile{mf}, items))

	// The present functions are exactly the odd primary-message numbers; the
	// even functions plus F0 are all "Not Used" in E5 §10.13 and must be absent.
	byFunc := map[int]Message{}
	for _, m := range mf.Messages {
		byFunc[m.Function] = m
	}
	for _, f := range []int{1, 3, 5, 7, 9, 11, 13} {
		_, ok := byFunc[f]
		require.Truef(t, ok, "expected S9F%d to be present", f)
	}
	for _, f := range []int{0, 2, 4, 6, 8, 10, 12} {
		_, ok := byFunc[f]
		require.Falsef(t, ok, "S9F%d is Not Used and must not be authored", f)
	}

	// Every S9 message is a one-way equipment notification: no reply expected.
	for _, m := range mf.Messages {
		require.Lenf(t, m.Bodies, 1, "S9F%d must have a single body", m.Function)
		require.Falsef(t, m.Bodies[0].ReplyExpected,
			"S9F%d must not expect a reply", m.Function)
	}

	// The whole file renders without error (exercises the template end-to-end).
	_, err = renderMessages(mf, items)
	require.NoError(t, err)
}
