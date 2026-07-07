package main

import (
	"testing"

	"github.com/stretchr/testify/require"
)

// Task 15 closes Milestone E: every DSL structure-node kind is now proven
// against a message, EXCEPT bounded-arity list ({type: list, minItems: N,
// maxItems: M, items: [...]}) — no real S1 message needs it, so this file
// proves it via a synthetic S99F1 message instead of a golden real one.
//
// Task 6's own params_test.go ("bounded-arity list" cases in
// TestBuildParams/TestBodyExpr/TestBodyDoc) already proves BuildParams,
// BodyExpr, and BodyDoc handle a bounded node correctly in isolation; this
// file does not repeat those cases. What was still unproven is the FULL
// render pipeline: that a bounded node survives YAML parsing, passes
// ValidateMessages, and comes out the other end of renderMessages/renderTests
// as valid, behaviorally-correct generated Go — which is what
// TestRenderMessagesBoundedArityList and TestRenderTestsBoundedArityList
// below cover.
//
// The ValidateMessages "repeat + minItems/maxItems is rejected" teeth check
// already exists as TestValidateMessagesRejectsRepeatWithMinMaxItems in
// load_messages_test.go (added by Task 5); it is not duplicated here.

const boundedArityYAML = `
stream: 99
messages:
  - function: 1
    name: Bounded Arity Test
    mnemonic: BAT
    direction: bidirectional
    description: Synthetic message proving the bounded-arity list node type.
    exception: None
    source: e5
    bodies:
      - actor: both
        replyExpected: false
        structure: {type: list, minItems: 0, maxItems: 4, items: [{item: MDLN}]}
`

// TestLoadBoundedArityList proves the schema correctly parses minItems/
// maxItems from YAML (not just that they end up nil, which would also let
// ValidateMessages pass since a bounded list with no repeat is valid either
// way) and that ValidateMessages accepts the resulting node.
func TestLoadBoundedArityList(t *testing.T) {
	mf, err := LoadMessageFile([]byte(boundedArityYAML))
	require.NoError(t, err)
	require.Len(t, mf.Messages, 1)

	structure := mf.Messages[0].Bodies[0].Structure
	require.NotNil(t, structure)
	require.NotNil(t, structure.MinItems, "minItems must parse into a non-nil pointer")
	require.NotNil(t, structure.MaxItems, "maxItems must parse into a non-nil pointer")
	require.Equal(t, 0, *structure.MinItems)
	require.Equal(t, 4, *structure.MaxItems)

	require.NoError(t, ValidateMessages([]MessageFile{mf}, testItems()))
}

// TestRenderMessagesBoundedArityList proves the full renderMessages pipeline
// for a bounded-arity list: since the node carries explicit Items, it renders
// exactly like a plain fixed list (one param per item, secs2.L(...) body) —
// this test is the render-level proof of that, plus the "L[0,4]{ ... }"
// bounded-arity godoc shorthand actually reaching the generated comment.
func TestRenderMessagesBoundedArityList(t *testing.T) {
	items := testItems()
	mf := MessageFile{Stream: 99, Messages: []Message{{
		Function: 1, Name: "Bounded Arity Test", Mnemonic: "BAT",
		Direction: "bidirectional", Description: "Synthetic message proving the bounded-arity list node type.",
		Exception: "None", Source: "e5",
		Bodies: []Body{
			{
				Actor: "both", ReplyExpected: false,
				Structure: &StructureNode{Type: "list", MinItems: intp(0), MaxItems: intp(4), Items: []StructureNode{{Item: "MDLN"}}},
			},
		},
	}}}

	out, err := renderMessages(mf, items)
	require.NoError(t, err)
	src := string(out)

	require.Contains(t, src, "func S99F1(mdln string) secs2.SECS2Message {\n\treturn secs2.NewMessage(99, 1, false, secs2.L(secs2.A(mdln)))\n}")
	require.Contains(t, src, "// Body: L[0,4]{ A[mdln] }.")
}

// TestRenderTestsBoundedArityList proves the full renderTests pipeline for a
// bounded-arity list: the generated test builds the message with the same
// deterministic MDLN sample as a plain fixed list would, decodes it, and
// asserts a single-element list holding that sample — i.e. minItems/maxItems
// affect only the godoc shorthand, never the generated call or assertions.
func TestRenderTestsBoundedArityList(t *testing.T) {
	items := testItems()
	mf := MessageFile{Stream: 99, Messages: []Message{{
		Function: 1, Name: "Bounded Arity Test", Mnemonic: "BAT",
		Bodies: []Body{
			{
				Actor: "both", ReplyExpected: false,
				Structure: &StructureNode{Type: "list", MinItems: intp(0), MaxItems: intp(4), Items: []StructureNode{{Item: "MDLN"}}},
			},
		},
	}}}

	out, err := renderTests(mf, items)
	require.NoError(t, err)
	parseGoSource(t, out)

	src := string(out)
	require.Contains(t, src, "func TestS99F1(t *testing.T) {")
	require.Contains(t, src, `gem.S99F1("mdln")`)
	require.Contains(t, src, "assertS99Header(t, msg, 1, false)")
	require.Contains(t, src, "decoded.ToList()")
	require.Contains(t, src, "!= 1", "the bounded list must still be asserted to hold exactly its one declared item")
	require.Contains(t, src, ".ToASCII()")
	require.Contains(t, src, `!= "mdln"`)
}
