package main

import (
	"bytes"
	"cmp"
	_ "embed"
	"errors"
	"fmt"
	"go/format"
	"slices"
	"strconv"
	"strings"
	"text/template"
)

//go:embed templates/catalog.go.tmpl
var catalogTemplate string

//go:embed templates/catalog_lookup_test.go.tmpl
var catalogLookupTestTemplate string

// directionConsts maps each E5 direction the DSL spells to the catalog's Direction constant.
var directionConsts = map[string]string{
	"host-to-equipment": "HostToEquipment",
	"equipment-to-host": "EquipmentToHost",
	"bidirectional":     "Bidirectional",
}

// catalogRoles are the actors of the catalog's two roles, in the order its tables index them:
// the host's form first, then the equipment's.
var catalogRoles = [2]string{"host", "equipment"}

// catalogRoleConsts are the catalog's Role constants, in the order of catalogRoles.
var catalogRoleConsts = [2]string{"RoleHost", "RoleEquipment"}

// catalogEntryView is one message's line of the catalog table.
type catalogEntryView struct {
	Stream   int
	Function int
	Forms    string // the [2]MessageInfo expression: the host's form, then the equipment's
}

// acceptView is one reply's line of the accept-reply table.
type acceptView struct {
	Stream   int
	Function int
	Builds   [2]string // the function that builds each role's accepting reply, or "" when that role has none
}

// lookupCaseView is one form the generated Lookup test expects.
type lookupCaseView struct {
	Name   string // "S1F14 host"
	Role   string // "gem.RoleHost"
	Want   string // the expected gem.MessageInfo literal, without Decode
	Body   string // the body built from the samples, or "" for a header-only form
	Decode string // the form's own decoder, or "" for a header-only form
}

// renderCatalog renders gem/catalog.go:
// the Role, Direction, and MessageInfo types,
// Lookup over every message the stream files define, one form per role,
// and AcceptReply over every reply body that carries an acceptReply annotation.
func renderCatalog(files []MessageFile, items map[string]Item) ([]byte, error) {
	tmpl, err := template.New("catalog.go.tmpl").Parse(catalogTemplate)
	if err != nil {
		return nil, fmt.Errorf("parse catalog template: %w", err)
	}

	var (
		entries []catalogEntryView
		accepts []acceptView
	)

	for _, f := range byStream(files) {
		for _, m := range f.Messages {
			forms, err := messageForms(f, m)
			if err != nil {
				return nil, err
			}

			entries = append(entries, catalogEntryView{Stream: f.Stream, Function: m.Function, Forms: formsExpr(forms)})

			builds, ok, err := acceptBuilds(f.Stream, m, items)
			if err != nil {
				return nil, err
			}

			if ok {
				accepts = append(accepts, acceptView{Stream: f.Stream, Function: m.Function, Builds: builds})
			}
		}
	}

	data := struct {
		Entries []catalogEntryView
		Accepts []acceptView
	}{Entries: entries, Accepts: accepts}

	var buf bytes.Buffer
	if err := tmpl.Execute(&buf, data); err != nil {
		return nil, fmt.Errorf("execute catalog template: %w", err)
	}

	out, err := format.Source(buf.Bytes())
	if err != nil {
		return nil, fmt.Errorf("format generated catalog source: %w", err)
	}

	return out, nil
}

// renderCatalogLookupTests renders gem/catalog_lookup_test.go:
// a test that Lookup returns every form of every message as the stream files define it,
// and that each form's Decode returns what the form's own decoder returns for the body its builder sends,
// and a test that Lookup finds nothing else.
func renderCatalogLookupTests(files []MessageFile, items map[string]Item) ([]byte, error) {
	tmpl, err := template.New("catalog_lookup_test.go.tmpl").Parse(catalogLookupTestTemplate)
	if err != nil {
		return nil, fmt.Errorf("parse catalog lookup test template: %w", err)
	}

	var cases []lookupCaseView

	for _, f := range byStream(files) {
		for _, m := range f.Messages {
			forms, err := messageForms(f, m)
			if err != nil {
				return nil, err
			}

			for i, form := range forms {
				c, err := lookupCase(f.Stream, m, i, form, items)
				if err != nil {
					return nil, err
				}

				cases = append(cases, c)
			}
		}
	}

	data := struct{ Cases []lookupCaseView }{Cases: cases}

	var buf bytes.Buffer
	if err := tmpl.Execute(&buf, data); err != nil {
		return nil, fmt.Errorf("execute catalog lookup test template: %w", err)
	}

	out, err := format.Source(buf.Bytes())
	if err != nil {
		return nil, fmt.Errorf("format generated catalog lookup test source: %w", err)
	}

	return out, nil
}

// byStream returns the stream files ordered by stream number, so the catalog lists stream 2 before stream 10.
func byStream(files []MessageFile) []MessageFile {
	return slices.SortedFunc(slices.Values(files), func(a, b MessageFile) int { return cmp.Compare(a.Stream, b.Stream) })
}

// catalogForm is what the catalog records for one role's form of a message.
type catalogForm struct {
	stream        int
	function      int
	name          string
	mnemonic      string
	direction     string // the Direction constant
	replyExpected bool
	replyFunction int
	verified      bool
	body          Body
}

// decoder returns the name of the form's own decoder, or "" for a header-only form, which has none.
func (c catalogForm) decoder() string {
	if c.body.Structure == nil {
		return ""
	}

	return "Decode" + callName(c.stream, c.function, c.body.Actor)
}

// messageForms returns the two forms of the message m of the stream file f, indexed as [catalogRoles]:
// the main body for the role's own actor, else the one for both,
// else the message's only main body, which the other side sends.
func messageForms(f MessageFile, m Message) ([2]catalogForm, error) {
	var forms [2]catalogForm

	dir, ok := directionConsts[m.Direction]
	if !ok {
		return forms, fmt.Errorf("S%dF%d: bad direction %q", f.Stream, m.Function, m.Direction)
	}

	replyFunction := 0
	if m.Function%2 == 1 && hasFunction(f, m.Function+1) {
		replyFunction = m.Function + 1
	}

	for i, actor := range catalogRoles {
		b, ok := formBody(m, actor)
		if !ok {
			return forms, fmt.Errorf("S%dF%d: no main body", f.Stream, m.Function)
		}

		forms[i] = catalogForm{
			stream:        f.Stream,
			function:      m.Function,
			name:          m.Name,
			mnemonic:      m.Mnemonic,
			direction:     dir,
			replyExpected: b.ReplyExpected,
			replyFunction: replyFunction,
			verified:      m.Source != "external",
			body:          b,
		}
	}

	return forms, nil
}

// formBody returns the main body of m that the actor sends:
// its own, else the one for both, else the only main body m has.
func formBody(m Message, actor string) (Body, bool) {
	if b, ok := mainBody(m, actor); ok {
		return b, true
	}

	if b, ok := mainBody(m, "both"); ok {
		return b, true
	}

	var only []Body

	for _, b := range m.Bodies {
		if b.Variant == "" {
			only = append(only, b)
		}
	}

	if len(only) != 1 {
		return Body{}, false
	}

	return only[0], true
}

// hasFunction reports whether the stream file f defines the function fn.
func hasFunction(f MessageFile, fn int) bool {
	_, ok := findMessage(f, fn)

	return ok
}

// findMessage returns the message of the stream file f with the function fn.
func findMessage(f MessageFile, fn int) (Message, bool) {
	for _, m := range f.Messages {
		if m.Function == fn {
			return m, true
		}
	}

	return Message{}, false
}

// formsExpr renders the catalog table's value for a message's two forms:
// sameForm(info) when both roles send the same body, and a two-element array otherwise.
func formsExpr(forms [2]catalogForm) string {
	if forms[0].body.Actor == forms[1].body.Actor {
		return "sameForm(" + infoLiteral(forms[0], "", "decodeAny("+forms[0].decoder()+")") + ")"
	}

	parts := make([]string, len(forms))
	for i, form := range forms {
		parts[i] = strings.TrimPrefix(infoLiteral(form, "", "decodeAny("+form.decoder()+")"), "MessageInfo")
	}

	return "[2]MessageInfo{" + strings.Join(parts, ", ") + "}"
}

// infoLiteral renders the MessageInfo literal of the form, its type qualified by qualifier,
// such as "gem." for the generated tests, with the Decode expression decode,
// which is left out for a header-only form, or when decode is empty.
func infoLiteral(form catalogForm, qualifier string, decode string) string {
	fields := []string{
		fmt.Sprintf("Stream: %d", form.stream),
		fmt.Sprintf("Function: %d", form.function),
		"Name: " + strconv.Quote(form.name),
		"Mnemonic: " + strconv.Quote(form.mnemonic),
		"Direction: " + qualifier + form.direction,
		fmt.Sprintf("ReplyExpected: %t", form.replyExpected),
		fmt.Sprintf("ReplyFunction: %d", form.replyFunction),
		fmt.Sprintf("HasBody: %t", form.body.Structure != nil),
		fmt.Sprintf("Verified: %t", form.verified),
	}

	if form.body.Structure != nil && decode != "" {
		fields = append(fields, "Decode: "+decode)
	}

	return qualifier + "MessageInfo{" + strings.Join(fields, ", ") + "}"
}

// lookupCase renders the generated Lookup test's case for the form of m that the role at index i sends.
func lookupCase(stream int, m Message, i int, form catalogForm, items map[string]Item) (lookupCaseView, error) {
	c := lookupCaseView{
		Name: fmt.Sprintf("S%dF%d %s", stream, m.Function, catalogRoles[i]),
		Role: "gem." + catalogRoleConsts[i],
		Want: infoLiteral(form, "gem.", ""),
	}

	if form.body.Structure == nil {
		return c, nil
	}

	params := BuildParams(form.body.Structure, items)

	exprs, err := SampleExprs(form.body.Structure, items)
	if err != nil {
		return lookupCaseView{}, fmt.Errorf("S%dF%d: %w", stream, m.Function, err)
	}

	c.Body = fmt.Sprintf("gem.%s(%s).Item()", builderName(stream, m.Function, form.body), callArgs(params, exprs))
	c.Decode = "lookupDecoder(gem." + form.decoder() + ")"

	return c, nil
}

// acceptBuilds returns, for each of [catalogRoles], the function that builds the reply m accepting its primary,
// and reports whether any role's form of m carries an acceptReply annotation.
// It is the builder itself for a body with no parameters,
// and otherwise a function literal calling it with the annotated value's constant for the body's one values-table leaf,
// nil for every repeat that is not the final parameter, and nothing for a final, variadic one.
func acceptBuilds(stream int, m Message, items map[string]Item) ([2]string, bool, error) {
	var builds [2]string

	annotated := false

	for i, actor := range catalogRoles {
		b, ok := formBody(m, actor)
		if !ok || b.AcceptReply == nil {
			continue
		}

		args, err := acceptArgs(b, items)
		if err != nil {
			return builds, false, fmt.Errorf("S%dF%d: %w", stream, m.Function, err)
		}

		builds[i] = builderName(stream, m.Function, b)
		if len(args) > 0 {
			builds[i] = fmt.Sprintf("func() secs2.SECS2Message { return %s(%s) }", builds[i], strings.Join(args, ", "))
		}

		annotated = true
	}

	return builds, annotated, nil
}

// acceptArgs returns the arguments of the builder call that sends the annotated body b accepting its primary.
func acceptArgs(b Body, items map[string]Item) ([]string, error) {
	leaves, err := acceptReplyLeaves(b.Structure)
	if err != nil {
		return nil, err
	}

	var args []string

	for _, p := range BuildParams(b.Structure, items) {
		switch {
		case p.Variadic:
			// A final repeat: sending no element leaves it empty.
		case p.Repeat:
			args = append(args, "nil")
		default:
			args = append(args, leaves[0].Item+b.AcceptReply.Value)
		}
	}

	return args, nil
}

// errAcceptReplyShape is the error for a body whose parameters an acceptReply annotation cannot fill.
var errAcceptReplyShape = errors.New("acceptReply: the body's parameters must be one leaf and any number of repeats, or none at all")

// acceptReplyLeaves returns the leaf an acceptReply annotation's value fills in the body n:
// its one builder parameter that is not a repeat,
// or no leaf when the body has no parameter at all.
// Any other parameter, more than one leaf, or repeats without a leaf, is an error.
func acceptReplyLeaves(n *StructureNode) ([]*StructureNode, error) {
	var (
		leaves  []*StructureNode
		repeats int
		other   bool
	)

	var walk func(*StructureNode)
	walk = func(n *StructureNode) {
		if n == nil {
			return
		}

		switch n.Kind() {
		case "leaf":
			leaves = append(leaves, n)
		case "list":
			switch {
			case n.Repeat != "":
				repeats++
			case n.Packed != "":
				other = true
			default:
				for i := range n.Items {
					walk(&n.Items[i])
				}
			}
		default: // an opaque node or a group reference
			other = true
		}
	}
	walk(n)

	if other || len(leaves) > 1 || (len(leaves) == 0 && repeats > 0) {
		return nil, errAcceptReplyShape
	}

	return leaves, nil
}

// catalogNames returns the top-level names catalog.go declares outside its tables,
// whether exported or not.
func catalogNames() (map[string]bool, error) {
	src, err := renderCatalog(nil, nil)
	if err != nil {
		return nil, err
	}

	return declaredNames("catalog.go", src)
}
