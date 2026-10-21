package main

import (
	"fmt"
	"maps"
	"os"
	"path/filepath"
	"slices"
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

// TestDataMessagesS2YAML loads the real data/items.yaml catalog together with
// the authored data/messages/s2.yaml and asserts the Stream 2 Equipment Control
// and Diagnostics schema parses, validates, and renders cleanly. It guards the
// largest content deliverable in Phase 2: exactly 50 functions (S2F1-S2F50, no
// F0 gap), every {item: X} reference at any nesting depth resolving against the
// real Data Item Dictionary, and the two value-dependent Enhanced Remote Command
// messages (F49/F50) being modelled as opaque bodies since no static structure
// can express CEPVAL/CEPACK's recursive, runtime-chosen shape.
func TestDataMessagesS2YAML(t *testing.T) {
	itemsData, err := os.ReadFile("data/items.yaml")
	require.NoError(t, err)

	items, err := LoadItems(itemsData)
	require.NoError(t, err)
	require.NoError(t, ValidateItems(items))

	msgData, err := os.ReadFile("data/messages/s2.yaml")
	require.NoError(t, err)

	mf, err := LoadMessageFile(msgData)
	require.NoError(t, err)
	require.Equal(t, 2, mf.Stream)

	// Exactly 50 functions, cross-referenced against the real items.yaml so
	// every item reference at any structure depth actually resolves.
	require.Len(t, mf.Messages, 50)
	require.NoError(t, ValidateMessages([]MessageFile{mf}, items))

	// Function numbers are a contiguous F1-F50 with no F0 and no gap.
	byFunc := map[int]Message{}
	for _, m := range mf.Messages {
		byFunc[m.Function] = m
	}
	for f := 1; f <= 50; f++ {
		_, ok := byFunc[f]
		require.Truef(t, ok, "expected S2F%d to be present", f)
	}
	_, hasF0 := byFunc[0]
	require.False(t, hasF0, "S2F0 must not be authored")

	// F49/F50 (Enhanced Remote Command / Acknowledge) are opaque: their
	// CEPVAL/CEPACK shape is value-dependent and recursive, so no static
	// structure tree can express it and the body is a single secs2.Item.
	for _, f := range []int{49, 50} {
		m := byFunc[f]
		require.Lenf(t, m.Bodies, 1, "S2F%d must have a single body", f)
		require.NotNilf(t, m.Bodies[0].Structure, "S2F%d body must be opaque, not header-only", f)
		require.Equalf(t, "opaque", m.Bodies[0].Structure.Kind(),
			"S2F%d must be modelled as an opaque body", f)
	}

	// No other function is opaque: every F1-F48 body is either a leaf, a list,
	// or a header-only (nil) structure.
	for _, m := range mf.Messages {
		if m.Function == 49 || m.Function == 50 {
			continue
		}
		for _, b := range m.Bodies {
			if b.Structure != nil {
				require.NotEqualf(t, "opaque", b.Structure.Kind(),
					"S2F%d must not be opaque", m.Function)
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

	// Every message carries exactly one main body (no sender-dependent structure in Stream 5).
	// Only S5F14, S5F15, and S5F18 add a variant,
	// the short form E5 names whose error list is zero length.
	// Every body's reply expectation matches E5's W-bit markers:
	// the primary notifies/requests expect a reply;
	// every acknowledge/confirm/data reply does not.
	replyExpected := map[int]bool{1: true, 3: true, 5: true, 7: true, 9: true, 11: true, 13: true, 15: true, 17: true}
	variants := map[int]string{14: "Accepted", 15: "Success", 18: "Success"}
	for _, m := range mf.Messages {
		want := 1
		if variants[m.Function] != "" {
			want = 2
		}

		require.Lenf(t, m.Bodies, want, "S5F%d body count", m.Function)
		require.Emptyf(t, m.Bodies[0].Variant, "S5F%d's first body must be its main body", m.Function)

		if want == 2 {
			require.Equalf(t, variants[m.Function], m.Bodies[1].Variant, "S5F%d variant name", m.Function)
		}

		for _, b := range m.Bodies {
			require.Equalf(t, replyExpected[m.Function], b.ReplyExpected,
				"S5F%d replyExpected mismatch", m.Function)
		}
	}

	// The whole file renders without error (exercises the template end-to-end).
	_, err = renderMessages(mf, items)
	require.NoError(t, err)
}

// TestDataMessagesS6YAML loads the real data/items.yaml catalog together with
// the authored data/messages/s6.yaml and asserts the Stream 6 Data Collection
// schema parses, validates, and renders cleanly. It guards the largest content
// deliverable: exactly 30 functions (S6F1-S6F30, no F0 gap), with F13
// (Annotated Event Report Send) reconstructed from cross-references and carrying
// source: external/confidence: low, every {item: X} reference resolving against
// the real Data Item Dictionary, and the reply expectation of each primary
// matching E5's W-bit markers.
func TestDataMessagesS6YAML(t *testing.T) {
	itemsData, err := os.ReadFile("data/items.yaml")
	require.NoError(t, err)

	items, err := LoadItems(itemsData)
	require.NoError(t, err)
	require.NoError(t, ValidateItems(items))

	msgData, err := os.ReadFile("data/messages/s6.yaml")
	require.NoError(t, err)

	mf, err := LoadMessageFile(msgData)
	require.NoError(t, err)
	require.Equal(t, 6, mf.Stream)

	// Exactly 30 functions, cross-referenced against the real items.yaml so
	// every item reference at any structure depth actually resolves.
	require.Len(t, mf.Messages, 30)
	require.NoError(t, ValidateMessages([]MessageFile{mf}, items))

	// Function numbers are a contiguous F1-F30 with no F0 and no gap. In
	// particular F13 (Annotated Event Report Send) is present: its body text was
	// dropped from the local E5 copy, so it is reconstructed, not skipped.
	byFunc := map[int]Message{}
	for _, m := range mf.Messages {
		byFunc[m.Function] = m
	}
	for f := 1; f <= 30; f++ {
		_, ok := byFunc[f]
		require.Truef(t, ok, "expected S6F%d to be present", f)
	}
	_, hasF0 := byFunc[0]
	require.False(t, hasF0, "S6F0 must not be authored")

	// S6F13 carries its external-provenance metadata; no e5-sourced function does.
	f13 := byFunc[13]
	require.Equal(t, "external", f13.Source, "S6F13 must be source: external")
	require.Equal(t, "low", f13.Confidence, "S6F13 must be confidence: low")
	for _, m := range mf.Messages {
		if m.Function != 13 {
			require.NotEqualf(t, "external", m.Source,
				"S6F%d must not be source: external", m.Function)
		}
	}

	// Reply expectation matches E5's W-bit markers: every primary marked reply or
	// [reply] expects a reply; every acknowledge/data reply (and F29, which E5
	// leaves unmarked) does not.
	replyExpected := map[int]bool{
		1: true, 3: true, 5: true, 7: true, 9: true, 11: true, 13: true,
		15: true, 17: true, 19: true, 21: true, 23: true, 25: true, 27: true,
	}
	for _, m := range mf.Messages {
		require.Lenf(t, m.Bodies, 1, "S6F%d must have a single body", m.Function)
		require.Equalf(t, replyExpected[m.Function], m.Bodies[0].ReplyExpected,
			"S6F%d replyExpected mismatch", m.Function)
	}

	// The external-source disclaimer must survive into the generated S6F13 godoc,
	// and no other (e5-sourced) function's godoc may contain it.
	for _, m := range mf.Messages {
		for _, b := range m.Bodies {
			fv := newFuncView(mf.Stream, m, b, items)
			doc := strings.Join(fv.Doc, "\n")
			if m.Function == 13 {
				require.Containsf(t, doc, externalSourceDisclaimer,
					"S6F13 godoc must contain the external-source disclaimer")
			} else {
				require.NotContainsf(t, doc, externalSourceDisclaimer,
					"S6F%d (source %q) godoc must not contain the external-source disclaimer",
					m.Function, m.Source)
			}
		}
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

// functionSpan returns the function numbers lo through hi.
func functionSpan(lo, hi int) []int {
	fns := make([]int, 0, hi-lo+1)
	for f := lo; f <= hi; f++ {
		fns = append(fns, f)
	}

	return fns
}

// oddFunctions returns the odd function numbers lo through hi.
func oddFunctions(lo, hi int) []int {
	var fns []int

	for f := lo; f <= hi; f++ {
		if f%2 == 1 {
			fns = append(fns, f)
		}
	}

	return fns
}

// expectedStreamFunctions returns, for every stream the generator covers,
// the sorted function numbers its stream file must define.
// The sets come from SEMI E5-0813 §10.
// F0 (Abort Transaction) is never generated, and the functions E5 marks Not Used are left out.
func expectedStreamFunctions() map[int][]int {
	return map[int][]int{
		1: functionSpan(1, 24),
		2: functionSpan(1, 50),
		3: functionSpan(1, 36),
		// The original material-control protocol defines F1-F18 with gaps.
		// The E32 macro and micro messages follow.
		4:  slices.Concat(functionSpan(1, 3), oddFunctions(5, 17), functionSpan(18, 27), oddFunctions(29, 41)),
		5:  functionSpan(1, 18),
		6:  functionSpan(1, 30),
		7:  functionSpan(1, 44),
		8:  functionSpan(1, 4),
		9:  oddFunctions(1, 13),
		10: slices.Concat(functionSpan(1, 7), functionSpan(9, 10)),
		12: functionSpan(1, 19),
		13: functionSpan(1, 16),
		14: functionSpan(1, 28),
		15: functionSpan(1, 54),
		16: slices.Concat(functionSpan(1, 12), functionSpan(15, 30)),
		17: functionSpan(1, 14),
		18: functionSpan(1, 16),
		19: functionSpan(1, 20),
	}
}

// TestExpectedStreamFunctionsTable guards the expected-function table against typos.
// The streams S3, S4, S7, S8, S10 and S12 to S19 define 315 functions between them.
func TestExpectedStreamFunctionsTable(t *testing.T) {
	table := expectedStreamFunctions()
	require.Len(t, table, 18)

	total := 0

	for stream, fns := range table {
		require.Truef(t, slices.IsSorted(fns), "S%d: functions not sorted", stream)
		require.Equalf(t, len(fns), len(slices.Compact(slices.Clone(fns))), "S%d: duplicate function", stream)
		require.NotContainsf(t, fns, 0, "S%d: F0 must not be listed", stream)

		if !slices.Contains([]int{1, 2, 5, 6, 9}, stream) {
			total += len(fns)
		}
	}

	require.Equal(t, 315, total)
}

// TestStreamFilesDefineExpectedFunctions checks every stream file against the expected-function table.
// A stream file must define exactly its stream's functions, each once;
// a stream file for a stream outside the table fails, and a table stream without a file yet is skipped.
func TestStreamFilesDefineExpectedFunctions(t *testing.T) {
	table := expectedStreamFunctions()

	files, err := loadMessageFiles(filepath.Join("data", "messages"))
	require.NoError(t, err)
	require.NotEmpty(t, files)

	present := map[int]MessageFile{}

	for _, mf := range files {
		_, ok := table[mf.Stream]
		require.Truef(t, ok, "S%d: stream file for a stream outside the expected-function table", mf.Stream)
		present[mf.Stream] = mf
	}

	for _, stream := range slices.Sorted(maps.Keys(table)) {
		t.Run(fmt.Sprintf("S%d", stream), func(t *testing.T) {
			mf, ok := present[stream]
			if !ok {
				t.Skipf("no stream file for S%d yet", stream)
			}

			want := table[stream]

			got := make([]int, 0, len(mf.Messages))
			for _, m := range mf.Messages {
				got = append(got, m.Function)
			}

			slices.Sort(got)

			var missing, extra []int

			for _, f := range want {
				if !slices.Contains(got, f) {
					missing = append(missing, f)
				}
			}

			for _, f := range got {
				if !slices.Contains(want, f) {
					extra = append(extra, f)
				}
			}

			require.Truef(t, len(missing) == 0 && len(extra) == 0,
				"S%d: missing functions %v, functions outside the expected set %v", stream, missing, extra)
			require.Equalf(t, want, got, "S%d: every function must be defined exactly once", stream)
		})
	}
}
