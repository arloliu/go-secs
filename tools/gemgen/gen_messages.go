package main

import (
	"bytes"
	_ "embed"
	"fmt"
	"go/format"
	"strings"
	"text/template"
)

//go:embed templates/messages.go.tmpl
var messagesTemplate string

// externalSourceDisclaimer is the trailing godoc paragraph appended to a
// source: external message, so the provenance travels into the generated
// output (design doc "Godoc format").
const externalSourceDisclaimer = "Source: reconstructed from an external reference, not verified against the purchased SEMI standard."

// funcView is the per-function view model the messages template renders.
type funcView struct {
	Name    string // "S1F1" or "S1F2Host"
	Params  []Param
	Body    string // BodyExpr output
	Stream  int
	Func    int
	WaitBit bool
	Doc     []string // godoc lines (without leading "// ")
}

// renderMessages renders one stream's message builders into gofmt-normalized
// Go source. Scoped to header-only bodies; later tasks extend the template
// (and the funcView it feeds) to cover fixed lists, repeats, and opaque
// bodies.
func renderMessages(mf MessageFile, items map[string]Item) ([]byte, error) {
	tmpl, err := template.New("messages.go.tmpl").Funcs(template.FuncMap{
		"params": renderParamList,
	}).Parse(messagesTemplate)
	if err != nil {
		return nil, fmt.Errorf("parse messages template: %w", err)
	}

	var funcs []funcView
	for _, m := range mf.Messages {
		for _, b := range m.Bodies {
			funcs = append(funcs, newFuncView(mf.Stream, m, b, items))
		}
	}

	data := struct{ Funcs []funcView }{Funcs: funcs}

	var buf bytes.Buffer
	if err := tmpl.Execute(&buf, data); err != nil {
		return nil, fmt.Errorf("execute messages template: %w", err)
	}

	out, err := format.Source(buf.Bytes())
	if err != nil {
		return nil, fmt.Errorf("format generated messages source: %w", err)
	}

	return out, nil
}

// callName returns the name of the main builder for one actor of a message:
// SxFy, or SxFyHost for the host's body.
// The main body's decoder is Decode<callName>,
// and a variant's builder is callName followed by the variant name.
func callName(stream int, function int, actor string) string {
	name := fmt.Sprintf("S%dF%d", stream, function)
	if actor == "host" {
		name += "Host"
	}

	return name
}

// builderName returns the name of the builder generated for one body:
// its actor's [callName], followed by the variant name for a variant body.
func builderName(stream int, function int, b Body) string {
	return callName(stream, function, b.Actor) + b.Variant
}

// newFuncView builds the view model for one (message, body) pair.
func newFuncView(stream int, m Message, b Body, items map[string]Item) funcView {
	code := fmt.Sprintf("S%dF%d", stream, m.Function)
	name := builderName(stream, m.Function, b)

	doc := messageDoc(name, code, m, b, items)
	if b.Variant != "" {
		doc = variantDoc(name, code, callName(stream, m.Function, b.Actor), m, b, items)
	}

	return funcView{
		Name:    name,
		Params:  BuildParams(b.Structure, items),
		Body:    BodyExpr(b.Structure, items),
		Stream:  stream,
		Func:    m.Function,
		WaitBit: b.ReplyExpected,
		Doc:     doc,
	}
}

// variantDoc renders the godoc comment lines for a variant body's builder:
// a name-first summary calling it the short form of its message,
// the builder and decoder of the main body it shortens,
// and its own Body: shorthand.
// The description and exception stay on the main builder, which the summary links to.
func variantDoc(funcName string, code string, mainName string, m Message, b Body, items map[string]Item) []string {
	doc := []string{
		fmt.Sprintf("%s creates the short form of an %s (%s) message%s, direction: %s.", funcName, code, m.Name, actorClause(b.Actor), m.Direction),
		"",
		fmt.Sprintf("[%s] creates the full form,", mainName),
		fmt.Sprintf("and [Decode%s] decodes both.", mainName),
		"",
	}
	doc = append(doc, wrapParagraph(ensurePeriod("Body: "+BodyDoc(b.Structure, items)))...)
	doc = append(doc, optionalBuilderDoc(b.Structure)...)
	doc = append(doc, typedBuilderDoc(b.Structure, items)...)

	if m.Source == "external" {
		doc = append(doc, "")
		doc = append(doc, wrapParagraph(externalSourceDisclaimer)...)
	}

	return doc
}

// messageDoc renders the godoc comment lines (each without the leading "// ")
// for one generated function, following the design's §4 shape: a name-first
// summary line with NO section citation, a blank line, the description, a
// blank line, the Body: shorthand, a blank line, and the Exception: text.
// Each blank string entry becomes a genuine blank "//" comment line when the
// template renders {{range .Doc}}// {{.}} — this is what keeps description,
// Body, and Exception as three distinct godoc paragraphs instead of merging
// into one run-on paragraph. When m.Source == "external", an additional
// blank entry and a trailing provenance disclaimer paragraph are appended.
func messageDoc(funcName, code string, m Message, b Body, items map[string]Item) []string {
	summary := fmt.Sprintf("%s creates the %s (%s) message%s, direction: %s.", funcName, code, m.Name, actorClause(b.Actor), m.Direction)

	// summary is a generated fixed-format synopsis, not authored prose,
	// so it is never wrapped: wrapping it would treat the "direction:" colon as a clause boundary and break a line that was never meant to span more than one.
	doc := []string{summary, ""}
	doc = append(doc, wrapParagraph(ensurePeriod(m.Description))...)
	doc = append(doc, "")
	doc = append(doc, wrapParagraph(ensurePeriod("Body: "+BodyDoc(b.Structure, items)))...)
	doc = append(doc, optionalBuilderDoc(b.Structure)...)
	doc = append(doc, typedBuilderDoc(b.Structure, items)...)
	doc = append(doc, "")
	doc = append(doc, wrapParagraph(ensurePeriod("Exception: "+m.Exception))...)

	if m.Source == "external" {
		doc = append(doc, "")
		doc = append(doc, wrapParagraph(externalSourceDisclaimer)...)
	}

	return doc
}

// ensurePeriod appends a trailing "." when s does not already end with one,
// so callers can compose sentence fragments (e.g. "Body: " + BodyDoc(...))
// without producing a doubled or missing terminal period.
func ensurePeriod(s string) string {
	if strings.HasSuffix(s, ".") {
		return s
	}

	return s + "."
}

// renderParamList is the "params" template FuncMap helper: it renders a
// function's parameter list joined by ", ", using each param's own Type field
// throughout. A variadic (final repeat/packed) param renders as name ...<Type>;
// a non-final repeat/packed renders as name []<Type>; every other param
// (fixed/open leaf or opaque) renders as name <Type>. Type is secs2.Item for a
// repeat-origin param but a concrete goType (e.g. byte) for a packed-origin one;
// the branch structure is identical, only the wrapped type differs.
func renderParamList(params []Param) string {
	parts := make([]string, len(params))
	for i, p := range params {
		switch {
		case p.Variadic:
			parts[i] = fmt.Sprintf("%s ...%s", p.Name, p.Type)
		case p.Repeat:
			parts[i] = fmt.Sprintf("%s []%s", p.Name, p.Type)
		default:
			parts[i] = fmt.Sprintf("%s %s", p.Name, p.Type)
		}
	}

	return strings.Join(parts, ", ")
}
