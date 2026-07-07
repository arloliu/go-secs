package main

import (
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
