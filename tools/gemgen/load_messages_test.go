package main

import (
	"strings"
	"testing"

	"github.com/stretchr/testify/require"
)

const goodMsgs = `
stream: 1
messages:
  - function: 2
    name: On Line Data
    mnemonic: D
    direction: bidirectional
    description: Data signifying that the equipment is alive.
    exception: The host sends a zero-length list to the equipment.
    source: e5
    bodies:
      - actor: equipment
        replyExpected: false
        structure: {type: list, items: [{item: MDLN}, {item: SOFTREV}]}
      - actor: host
        replyExpected: false
        structure: {type: list, items: []}
`

func testItems() map[string]Item {
	return map[string]Item{
		"MDLN":    {Formats: []string{"A"}, Binding: BindingFixed, GoType: "string"},
		"SOFTREV": {Formats: []string{"A"}, Binding: BindingFixed, GoType: "string"},
		"SVID":    {Formats: []string{"U1", "U2", "U4", "U8"}, Binding: BindingOpen},
		"TSIP":    {Formats: []string{"B"}, Binding: BindingFixed, GoType: "byte"},
		"MHEAD":   {Formats: []string{"B"}, Binding: BindingFixed, GoType: "[10]byte"},
	}
}

func TestLoadMessageFileValid(t *testing.T) {
	mf, err := LoadMessageFile([]byte(goodMsgs))
	require.NoError(t, err)
	require.Equal(t, 1, mf.Stream)
	require.Len(t, mf.Messages, 1)
	require.Equal(t, 2, mf.Messages[0].Function)

	require.NoError(t, ValidateMessages([]MessageFile{mf}, testItems()))
}

func TestValidateMessagesRejectsUnknownItemRef(t *testing.T) {
	const yml = `
stream: 1
messages:
  - function: 2
    name: On Line Data
    bodies:
      - actor: equipment
        structure: {type: list, items: [{item: NOPE}]}
`
	mf, err := LoadMessageFile([]byte(yml))
	require.NoError(t, err)
	require.Error(t, ValidateMessages([]MessageFile{mf}, testItems()))
}

func TestValidateMessagesRejectsUnknownItemRefNested(t *testing.T) {
	const yml = `
stream: 1
messages:
  - function: 2
    name: On Line Data
    bodies:
      - actor: equipment
        structure: {type: list, repeat: svids, of: {item: NOPE}}
`
	mf, err := LoadMessageFile([]byte(yml))
	require.NoError(t, err)
	require.Error(t, ValidateMessages([]MessageFile{mf}, testItems()))
}

func TestValidateMessagesRejectsDuplicateStreamFunction(t *testing.T) {
	const a = `
stream: 1
messages:
  - function: 2
    name: On Line Data
    bodies:
      - actor: equipment
        structure: {type: list, items: [{item: MDLN}]}
`
	const b = `
stream: 1
messages:
  - function: 2
    name: On Line Data Again
    bodies:
      - actor: equipment
        structure: {type: list, items: [{item: MDLN}]}
`
	mfA, err := LoadMessageFile([]byte(a))
	require.NoError(t, err)
	mfB, err := LoadMessageFile([]byte(b))
	require.NoError(t, err)

	require.Error(t, ValidateMessages([]MessageFile{mfA, mfB}, testItems()))
}

func TestValidateMessagesRejectsBadActor(t *testing.T) {
	const yml = `
stream: 1
messages:
  - function: 2
    name: On Line Data
    bodies:
      - actor: sideways
        structure: {type: list, items: [{item: MDLN}]}
`
	mf, err := LoadMessageFile([]byte(yml))
	require.NoError(t, err)
	require.Error(t, ValidateMessages([]MessageFile{mf}, testItems()))
}

func TestValidateMessagesRejectsRepeatWithMinMaxItems(t *testing.T) {
	const yml = `
stream: 1
messages:
  - function: 2
    name: On Line Data
    bodies:
      - actor: equipment
        structure: {type: list, repeat: x, minItems: 0, of: {item: MDLN}}
`
	mf, err := LoadMessageFile([]byte(yml))
	require.NoError(t, err)
	require.Error(t, ValidateMessages([]MessageFile{mf}, testItems()))
}

// TestValidateMessagesRejectsPackedOnOpenItem proves a packed node whose
// of.item resolves to a binding: open item is rejected: packing needs one
// shared format for every value, which an open item does not have.
func TestValidateMessagesRejectsPackedOnOpenItem(t *testing.T) {
	const yml = `
stream: 1
messages:
  - function: 10
    name: Port Transfer Status Data
    bodies:
      - actor: both
        structure: {type: list, packed: svids, of: {item: SVID}}
`
	mf, err := LoadMessageFile([]byte(yml))
	require.NoError(t, err)
	require.Error(t, ValidateMessages([]MessageFile{mf}, testItems()))
}

// TestValidateMessagesRejectsPackedByteArrayItem proves a packed node whose
// of.item carries a byte-array/byte-slice goType is rejected: packing multiple
// binary blobs under one header is not a shape this generator supports (nor
// does any known SEMI E5 message need it -- packed groups hold arrays of
// scalar values, not arrays of binary blobs).
func TestValidateMessagesRejectsPackedByteArrayItem(t *testing.T) {
	const yml = `
stream: 1
messages:
  - function: 10
    name: Port Transfer Status Data
    bodies:
      - actor: both
        structure: {type: list, packed: mheads, of: {item: MHEAD}}
`
	mf, err := LoadMessageFile([]byte(yml))
	require.NoError(t, err)
	require.Error(t, ValidateMessages([]MessageFile{mf}, testItems()))
}

// TestValidateMessagesRejectsPackedWithRepeat proves packed and repeat are
// mutually exclusive on one structure node.
func TestValidateMessagesRejectsPackedWithRepeat(t *testing.T) {
	const yml = `
stream: 1
messages:
  - function: 10
    name: Port Transfer Status Data
    bodies:
      - actor: both
        structure: {type: list, packed: tsips, repeat: tsips, of: {item: TSIP}}
`
	mf, err := LoadMessageFile([]byte(yml))
	require.NoError(t, err)
	require.Error(t, ValidateMessages([]MessageFile{mf}, testItems()))
}

// TestLoadMessageFileRejectsUnknownKeys proves a key the messages DSL does not define fails loading
// at every level of a stream file:
// the file, a message, a body, a structure node, and a node nested in a list and in a repeat's of.
// A misspelled key such as optinal would otherwise be dropped silently and change the generated code.
func TestLoadMessageFileRejectsUnknownKeys(t *testing.T) {
	const message = `
stream: 1
messages:
  - function: 2
    name: On Line Data
    bodies:
      - actor: equipment
        structure: {type: list, items: [{item: MDLN}]}
`

	tests := []struct {
		name string
		old  string
		new  string
		key  string
	}{
		{"file", "stream: 1\n", "stream: 1\nstreem: 1\n", "streem"},
		{"message", "    name: On Line Data\n", "    name: On Line Data\n    replyExpected: true\n", "replyExpected"},
		{"body", "      - actor: equipment\n", "      - actor: equipment\n        shortForm: Short\n", "shortForm"},
		{"structure node", "{type: list, items:", "{type: list, repeats: x, items:", "repeats"},
		{"node in a list", "{item: MDLN}", "{item: MDLN, optinal: true}", "optinal"},
		{"node in a repeat's of", "items: [{item: MDLN}]", "repeat: x, of: {item: MDLN, typd: true}", "typd"},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			yml := strings.Replace(message, tt.old, tt.new, 1)
			require.NotEqual(t, message, yml)

			_, err := LoadMessageFile([]byte(yml))
			require.ErrorContains(t, err, "field "+tt.key+" not found")
		})
	}

	_, err := LoadMessageFile([]byte(message))
	require.NoError(t, err)
}
