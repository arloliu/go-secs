package main

import (
	"maps"
	"strings"
	"testing"

	"github.com/stretchr/testify/require"
)

// catalogItems extends testItems with acknowledge codes, one with a values table and one without.
func catalogItems() map[string]Item {
	items := testItems()
	maps.Copy(items, map[string]Item{
		"ACK": {
			Formats: []string{"B"}, Binding: BindingFixed, GoType: "byte",
			Values: []ItemValue{{Name: "Accepted", Value: IntValue(0)}, {Name: "Denied", Value: IntValue(1)}},
		},
		"CODE":    {Formats: []string{"U1"}, Binding: BindingFixed, GoType: "uint8"},
		"ERRCODE": {Formats: []string{"U1", "U2", "U4"}, Binding: BindingOpen},
	})

	return items
}

// catalogMessages renders a stream-1 messages file whose S1F1 is a primary expecting a reply
// and whose S1F2 has the body structure, carrying annotation as its acceptReply value.
func catalogMessages(annotation string, structure string) string {
	return `
stream: 1
messages:
  - function: 1
    name: Request
    direction: host-to-equipment
    bodies:
      - actor: both
        replyExpected: true
        structure: null
  - function: 2
    name: Acknowledge
    direction: equipment-to-host
    bodies:
      - actor: both
        acceptReply: ` + annotation + `
        structure: ` + structure + "\n"
}

// TestValidateMessagesAcceptsAcceptReply proves every body shape acceptReply is meant for passes validation.
func TestValidateMessagesAcceptsAcceptReply(t *testing.T) {
	tests := []struct {
		name       string
		annotation string
		structure  string
	}{
		{"a code alone", `{value: Accepted}`, `{item: ACK}`},
		{"a code and an error list", `{value: Accepted}`, `{type: list, items: [{item: ACK}, {type: list, repeat: errors, of: {item: ERRCODE}}]}`},
		{"a list before the code", `{value: Accepted}`, `{type: list, items: [{type: list, repeat: errors, of: {item: ERRCODE}}, {item: ACK}]}`},
		{"no parameters", `{}`, `{type: list, items: []}`},
		{"header only", `{}`, `null`},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			mf, err := LoadMessageFile([]byte(catalogMessages(tt.annotation, tt.structure)))
			require.NoError(t, err)
			require.NotNil(t, mf.Messages[1].Bodies[0].AcceptReply)

			require.NoError(t, ValidateMessages([]MessageFile{mf}, catalogItems()))
		})
	}
}

// TestValidateMessagesRejectsBadAcceptReply proves acceptReply is rejected on a body whose builder it cannot call
// with one accepting value, and on a message that is not a reply to a primary expecting one.
func TestValidateMessagesRejectsBadAcceptReply(t *testing.T) {
	tests := []struct {
		name       string
		annotation string
		structure  string
		wantErr    string
	}{
		{"unknown value", `{value: Granted}`, `{item: ACK}`, `has no value named "Granted"`},
		{"value missing", `{}`, `{item: ACK}`, `has no value named ""`},
		{"value on no parameters", `{value: Accepted}`, `{type: list, items: []}`, "names no leaf"},
		{"code without values", `{value: Accepted}`, `{item: CODE}`, "with a values table"},
		{"open code", `{value: Accepted}`, `{item: ERRCODE}`, "with a values table"},
		{"optional code", `{value: Accepted}`, `{item: ACK, optional: true}`, "is optional"},
		{"two leaves", `{value: Accepted}`, `{type: list, items: [{item: ACK}, {item: MDLN}]}`, "one leaf and any number of repeats"},
		{"repeats alone", `{}`, `{type: list, repeat: errors, of: {item: ERRCODE}}`, "one leaf and any number of repeats"},
		{"opaque", `{value: Accepted}`, `{type: list, items: [{item: ACK}, {type: opaque}]}`, "one leaf and any number of repeats"},
		{"packed", `{value: Accepted}`, `{type: list, items: [{item: ACK}, {type: list, packed: tsips, of: {item: TSIP}}]}`, "one leaf and any number of repeats"},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			mf, err := LoadMessageFile([]byte(catalogMessages(tt.annotation, tt.structure)))
			require.NoError(t, err)

			err = ValidateMessages([]MessageFile{mf}, catalogItems())
			require.Error(t, err)
			require.Contains(t, err.Error(), tt.wantErr)
		})
	}
}

// TestValidateMessagesRejectsAcceptReplyOutsideAReply proves acceptReply is only for the main body of a reply
// whose primary is defined and expects a reply.
func TestValidateMessagesRejectsAcceptReplyOutsideAReply(t *testing.T) {
	ack := &Acceptance{Value: "Accepted"}
	primary := Message{Function: 1, Name: "Request", Direction: "host-to-equipment", Bodies: []Body{{Actor: "both", ReplyExpected: true}}}
	reply := Message{Function: 2, Name: "Acknowledge", Direction: "equipment-to-host", Bodies: []Body{{Actor: "both", Structure: &StructureNode{Item: "ACK"}}}}

	tests := []struct {
		name     string
		messages func() []Message
		wantErr  string
	}{
		{"on a primary", func() []Message {
			p := primary
			p.Bodies = []Body{{Actor: "both", ReplyExpected: true, AcceptReply: ack, Structure: &StructureNode{Item: "ACK"}}}

			return []Message{p, reply}
		}, "only a reply"},
		{"on a variant", func() []Message {
			r := reply
			r.Bodies = []Body{
				{Actor: "both", Structure: &StructureNode{Type: "list", Items: []StructureNode{{Item: "ACK"}, {Item: "MDLN"}}}},
				{Actor: "both", Variant: "Short", AcceptReply: ack, Structure: &StructureNode{Type: "list", Items: []StructureNode{{Item: "ACK"}}}},
			}

			return []Message{primary, r}
		}, "variant Short"},
		{"without its primary", func() []Message {
			r := reply
			r.Bodies = []Body{{Actor: "both", AcceptReply: ack, Structure: &StructureNode{Item: "ACK"}}}

			return []Message{r}
		}, "the primary S1F1 is not defined"},
		{"to a primary expecting no reply", func() []Message {
			p := primary
			p.Bodies = []Body{{Actor: "both"}}
			r := reply
			r.Bodies = []Body{{Actor: "both", AcceptReply: ack, Structure: &StructureNode{Item: "ACK"}}}

			return []Message{p, r}
		}, "expects a reply"},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			mf := MessageFile{Stream: 1, Messages: tt.messages()}

			err := ValidateMessages([]MessageFile{mf}, catalogItems())
			require.Error(t, err)
			require.Contains(t, err.Error(), tt.wantErr)
		})
	}
}

// catalogFiles is a stream file with a message whose two roles send different bodies,
// a header-only primary, a message reconstructed from an external reference, and annotated replies,
// plus a stream file that sorts after it by name but before it by number.
func catalogFiles() []MessageFile {
	ack := StructureNode{Item: "ACK"}
	errorList := StructureNode{Type: "list", Repeat: "errors", Of: &StructureNode{Item: "ERRCODE"}}

	s10 := MessageFile{Stream: 10, Messages: []Message{
		{Function: 1, Name: "Split Request", Mnemonic: "SR", Direction: "bidirectional", Source: "e5", Bodies: []Body{
			{Actor: "equipment", ReplyExpected: true, Structure: &StructureNode{Type: "list", Items: []StructureNode{{Item: "MDLN"}}}},
			{Actor: "host", ReplyExpected: true, Structure: &StructureNode{Type: "list"}},
		}},
		{Function: 2, Name: "Split Acknowledge", Mnemonic: "SA", Direction: "bidirectional", Source: "e5", Bodies: []Body{
			{Actor: "equipment", Structure: &StructureNode{Type: "list", Items: []StructureNode{ack, {Item: "MDLN"}}}},
			{Actor: "host", AcceptReply: &Acceptance{Value: "Accepted"}, Structure: &StructureNode{Type: "list", Items: []StructureNode{ack}}},
		}},
		{Function: 3, Name: "Header Request", Direction: "host-to-equipment", Source: "external", Bodies: []Body{
			{Actor: "both", ReplyExpected: true},
		}},
		{Function: 4, Name: "Error Acknowledge", Direction: "equipment-to-host", Source: "e5", Bodies: []Body{
			{Actor: "both", AcceptReply: &Acceptance{Value: "Accepted"}, Structure: &StructureNode{Type: "list", Items: []StructureNode{errorList, ack, errorList}}},
		}},
		{Function: 5, Name: "Notice", Direction: "equipment-to-host", Source: "e5", Bodies: []Body{
			{Actor: "both", Structure: &StructureNode{Item: "MDLN"}},
		}},
	}}

	s2 := MessageFile{Stream: 2, Messages: []Message{
		{Function: 17, Name: "Date Request", Direction: "bidirectional", Source: "e5", Bodies: []Body{{Actor: "both", ReplyExpected: true}}},
		{Function: 18, Name: "Date Acknowledge", Direction: "bidirectional", Source: "e5", Bodies: []Body{
			{Actor: "both", AcceptReply: &Acceptance{}, Structure: &StructureNode{Type: "list"}},
		}},
	}}

	return []MessageFile{s10, s2}
}

// TestRenderCatalog proves the catalog records each role's form,
// the reply function only for a primary whose reply is defined, the verified flag, and decoders only for bodies,
// lists streams by number,
// and builds an acceptance only for an annotated form, with the accepting value and its repeats empty,
// or with the builder itself for a body without parameters.
func TestRenderCatalog(t *testing.T) {
	src, err := renderCatalog(catalogFiles(), catalogItems())
	require.NoError(t, err)

	out := string(src)

	for _, want := range []string{
		`{10, 1}: [2]MessageInfo{{Stream: 10, Function: 1, Name: "Split Request", Mnemonic: "SR", Direction: Bidirectional, ReplyExpected: true, ReplyFunction: 2, HasBody: true, Verified: true, Decode: decodeAny(DecodeS10F1Host)}, ` +
			`{Stream: 10, Function: 1, Name: "Split Request", Mnemonic: "SR", Direction: Bidirectional, ReplyExpected: true, ReplyFunction: 2, HasBody: true, Verified: true, Decode: decodeAny(DecodeS10F1)}},`,
		`{10, 3}: sameForm(MessageInfo{Stream: 10, Function: 3, Name: "Header Request", Mnemonic: "", Direction: HostToEquipment, ReplyExpected: true, ReplyFunction: 4, HasBody: false, Verified: false}),`,
		`{10, 5}: sameForm(MessageInfo{Stream: 10, Function: 5, Name: "Notice", Mnemonic: "", Direction: EquipmentToHost, ReplyExpected: false, ReplyFunction: 0, HasBody: true, Verified: true, Decode: decodeAny(DecodeS10F5)}),`,
		"func() secs2.SECS2Message { return S10F2Host(ACKAccepted) },\n\t\tnil,",
		"func() secs2.SECS2Message { return S10F4(nil, ACKAccepted) },",
		"{2, 18}: {\n\t\tS2F18,\n\t\tS2F18,\n\t},",
	} {
		require.Contains(t, out, want)
	}

	require.Less(t, strings.Index(out, "{2, 17}:"), strings.Index(out, "{10, 1}:"), "stream 2 must come before stream 10")
	require.NotContains(t, out, "{10, 6}:", "an undefined function must not be listed")
	require.NotContains(t, out, "S10F2(", "the equipment's unannotated form must not be an acceptance")
	require.NotContains(t, out, "return S10F5", "an unannotated message must not be an acceptance")
}

// TestRenderCatalogRejectsBadDirection proves a message whose direction the catalog cannot represent fails generation.
func TestRenderCatalogRejectsBadDirection(t *testing.T) {
	files := []MessageFile{{Stream: 1, Messages: []Message{{Function: 1, Name: "X", Direction: "sideways", Bodies: []Body{{Actor: "both"}}}}}}

	_, err := renderCatalog(files, catalogItems())
	require.ErrorContains(t, err, `S1F1: bad direction "sideways"`)
}

// TestRenderCatalogLookupTests proves the generated Lookup test lists every role's form,
// with a body and a decoder for a form that has a body and neither for a header-only one.
func TestRenderCatalogLookupTests(t *testing.T) {
	src, err := renderCatalogLookupTests(catalogFiles(), catalogItems())
	require.NoError(t, err)

	out := string(src)

	for _, want := range []string{
		`name:   "S10F1 host",`,
		`body:   gem.S10F1Host().Item(),`,
		`decode: lookupDecoder(gem.DecodeS10F1Host),`,
		`name:   "S10F1 equipment",`,
		`body:   gem.S10F1("mdln").Item(),`,
		`decode: lookupDecoder(gem.DecodeS10F1),`,
		`want:   gem.MessageInfo{Stream: 10, Function: 3, Name: "Header Request", Mnemonic: "", Direction: gem.HostToEquipment, ReplyExpected: true, ReplyFunction: 4, HasBody: false, Verified: false},`,
	} {
		require.Contains(t, out, want)
	}

	require.Equal(t, 2*7, strings.Count(out, "\t\t\tname: "), "every message's two forms")
}

// TestCatalogNamesAreReserved proves a group cannot take a name the catalog declares,
// nor one whose decode function would collide with a catalog helper.
func TestCatalogNamesAreReserved(t *testing.T) {
	names, err := catalogNames()
	require.NoError(t, err)

	for _, name := range []string{"Role", "RoleHost", "RoleEquipment", "Direction", "HostToEquipment", "EquipmentToHost", "Bidirectional", "MessageInfo", "Lookup", "AcceptReply", "decodeAny"} {
		require.Truef(t, names[name], "catalog name %s", name)
	}

	for _, tt := range []struct{ group, wantErr string }{
		{"MessageInfo", "declared by the catalog"},
		{"Any", "collide with a catalog helper"},
	} {
		groups := []Group{{Name: tt.group}}
		require.ErrorContains(t, checkGroupNames(groups, nil, catalogItems()), tt.wantErr)
	}
}
