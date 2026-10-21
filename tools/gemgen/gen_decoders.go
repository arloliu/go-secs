package main

import (
	"bytes"
	_ "embed"
	"fmt"
	"go/format"
	"slices"
	"strconv"
	"strings"
	"text/template"
)

//go:embed templates/decoders.go.tmpl
var decodersTemplate string

//go:embed templates/decode_support.go.tmpl
var decodeSupportSource string

// decodeField is one field of a generated result struct.
//
// Name is the E5 data item (or repeated-group) name upper-cased, which is already the form the
// generated enum types in items.go use, so a result struct reads as the E5 body it decodes.
// A node that sets as: is named after that instead.
//
// Param is the index of the builder parameter the field mirrors, or -1 when it mirrors none.
// Presence marks the <FIELD>Present field an optional leaf adds right after its value field:
// it mirrors no parameter and reports whether the item arrived with a value.
// Elem is the element item's goType for the field of a typed repeat, and empty for any other field:
// such a field is a slice of decoded values, while its parameter takes secs2 items.
// Path is the list-index path from the body root to the item the field is read from;
// a presence field shares its value field's path.
type decodeField struct {
	Name     string
	Type     string
	Param    int
	Presence bool
	Elem     string
	Path     []int
}

// optionalLeaf is one optional leaf of a decoded body:
// what a generated test needs to send that leaf zero-length.
type optionalLeaf struct {
	Field string // the leaf's value field
	Path  []int  // list-index path from the body root to the leaf
	Empty string // expression building a zero-length item of the leaf's format, e.g. secs2.U2()
}

// decoderView is the per-decoder view model the decoders template renders.
type decoderView struct {
	TypeName string        // "S1F14Reply"
	FuncName string        // "DecodeS1F14"
	Fields   []decodeField // result-struct fields, in body order
	Lines    []string      // pre-rendered Go statement lines of the decoder body
	TypeDoc  []string      // godoc lines for the result struct (without leading "// ")
	FuncDoc  []string      // godoc lines for the decoder function (without leading "// ")
}

// renderDecodeSupport returns the shared decode helpers every generated decoder calls.
//
// The source is a fixed file rather than a rendered template: it carries no per-message data, and
// keeping it under templates/ means the generator remains the single writer of every gem file the
// decoders need, including the one the compile guards must plant alongside them.
func renderDecodeSupport() ([]byte, error) {
	out, err := format.Source([]byte(decodeSupportSource))
	if err != nil {
		return nil, fmt.Errorf("format decode support source: %w", err)
	}

	return out, nil
}

// renderDecoders renders one stream's body decoders into gofmt-normalized Go source.
//
// Every body the generator builds gets a decoder except a header-only one, which carries no body
// to decode, and a variant, which the main body's decoder for its actor decodes.
func renderDecoders(mf MessageFile, items map[string]Item) ([]byte, error) {
	tmpl, err := template.New("decoders.go.tmpl").Parse(decodersTemplate)
	if err != nil {
		return nil, fmt.Errorf("parse decoders template: %w", err)
	}

	var decoders []decoderView

	for _, m := range mf.Messages {
		for _, b := range m.Bodies {
			if b.Structure == nil || b.Variant != "" {
				continue // header-only: no body to decode; variant: decoded by its main body's decoder
			}

			dv, err := newDecoderView(mf.Stream, m, b, items)
			if err != nil {
				return nil, fmt.Errorf("S%dF%d: %w", mf.Stream, m.Function, err)
			}

			decoders = append(decoders, dv)
		}
	}

	data := struct{ Decoders []decoderView }{Decoders: decoders}

	var buf bytes.Buffer
	if err := tmpl.Execute(&buf, data); err != nil {
		return nil, fmt.Errorf("execute decoders template: %w", err)
	}

	out, err := format.Source(buf.Bytes())
	if err != nil {
		return nil, fmt.Errorf("format generated decoders source: %w", err)
	}

	return out, nil
}

// ResultTypeName returns the generated result-struct name for one (message, body) pair.
//
// A secondary (even) function carries a reply, a primary (odd) one carries a request body; the
// suffix keeps the type from colliding with the builder function of the same name.
func ResultTypeName(stream int, function int, actor string) string {
	name := callName(stream, function, actor)

	if function%2 == 0 {
		return name + "Reply"
	}

	return name + "Body"
}

// newDecoderView builds the view model for one (message, body) pair's decoder.
func newDecoderView(stream int, m Message, b Body, items map[string]Item) (decoderView, error) {
	code := fmt.Sprintf("S%dF%d", stream, m.Function)
	name := callName(stream, m.Function, b.Actor)
	typeName := ResultTypeName(stream, m.Function, b.Actor)

	e, err := walkDecoder(code, b.Structure, items)
	if err != nil {
		return decoderView{}, err
	}

	return decoderView{
		TypeName: typeName,
		FuncName: "Decode" + name,
		Fields:   e.fields,
		Lines:    e.lines,
		TypeDoc:  resultTypeDoc(typeName, code, name, m, b, items),
		FuncDoc:  decoderDoc(typeName, code, name, m, b),
	}, nil
}

// BuildFields returns the ordered result-struct fields a generated decoder fills.
//
// It walks the structure with the same ordering and the same stopping rules as [BuildParams],
// so every field except a presence field describes the builder parameter its Param names,
// and those parameters appear in order.
// gen_decoders_test.go asserts that agreement name-by-name over every message in data/messages.
func BuildFields(n *StructureNode, items map[string]Item) ([]decodeField, error) {
	e, err := walkDecoder("S0F0", n, items)
	if err != nil {
		return nil, err
	}

	return e.fields, nil
}

// optionalLeaves returns the optional leaves of a body, in body order.
func optionalLeaves(n *StructureNode, items map[string]Item) ([]optionalLeaf, error) {
	e, err := walkDecoder("S0F0", n, items)
	if err != nil {
		return nil, err
	}

	return e.optionals, nil
}

// walkDecoder walks a validated body structure with a fresh emitter.
// code labels the shape errors the emitted statements report.
func walkDecoder(code string, n *StructureNode, items map[string]Item) (*decodeEmitter, error) {
	e := &decodeEmitter{code: code, items: items}
	if err := e.emitNode(n, nil); err != nil {
		return nil, err
	}

	return e, nil
}

// decodeEmitter accumulates the fields and the Go statement lines of one generated decoder as it
// walks a validated structure node.
type decodeEmitter struct {
	code      string
	items     map[string]Item
	fields    []decodeField
	optionals []optionalLeaf
	lines     []string
	counter   int
	params    int // builder parameters mirrored so far: the next value field's Param
}

// nextID returns a fresh, monotonically increasing suffix for a generated variable name.
func (e *decodeEmitter) nextID() int {
	e.counter++

	return e.counter
}

// emit appends one or more pre-rendered Go statement lines.
// Indentation is left to gofmt.
func (e *decodeEmitter) emit(lines ...string) {
	e.lines = append(e.lines, lines...)
}

// field records one result-struct field that mirrors the next builder parameter and is read from path,
// and returns its Go name.
func (e *decodeEmitter) field(name string, goType string, path []int) string {
	e.fields = append(e.fields, decodeField{Name: name, Type: goType, Param: e.params, Path: slices.Clone(path)})
	e.params++

	return name
}

// presenceField records the presence field of the optional leaf at path whose value field is name,
// and returns its Go name.
func (e *decodeEmitter) presenceField(name string, path []int) string {
	presence := name + "Present"
	e.fields = append(e.fields, decodeField{Name: presence, Type: "bool", Param: -1, Presence: true, Path: slices.Clone(path)})

	return presence
}

// assign emits the guarded "decode into the field, or return the error" statement pair every
// scalar and item-valued field shares.
func (e *decodeEmitter) assign(name string, call string) {
	e.emit(
		fmt.Sprintf("if out.%s, err = %s; err != nil {", name, call),
		"return out, err",
		"}",
		"",
	)
}

// pathExpr renders the cursor expression addressing a body position.
func pathExpr(path []int) string {
	if len(path) == 0 {
		return "root"
	}

	parts := make([]string, len(path))
	for i, idx := range path {
		parts[i] = strconv.Itoa(idx)
	}

	return "root.At(" + strings.Join(parts, ", ") + ")"
}

// pathLabel renders the human-readable label a shape error names for a body position, e.g.
// "S1F14 body[1]".
func (e *decodeEmitter) pathLabel(path []int) string {
	var b strings.Builder

	b.WriteString(e.code)
	b.WriteString(" body")

	for _, idx := range path {
		b.WriteString("[")
		b.WriteString(strconv.Itoa(idx))
		b.WriteString("]")
	}

	return b.String()
}

// emitNode walks one structure node, recording its fields and emitting the statements that fill
// them. path is the list-index path from the body root to n.
func (e *decodeEmitter) emitNode(n *StructureNode, path []int) error {
	switch n.Kind() {
	case "leaf":
		return e.emitLeaf(n, path)
	case "opaque":
		name := nodeNames(n)
		e.assign(e.field(name.Field, "secs2.Item", path), fmt.Sprintf("decodeItem(%s, %q)", pathExpr(path), name.Label))

		return nil
	default: // list
		return e.emitList(n, path)
	}
}

// emitList walks a list node: a repeated group, a packed group, or a fixed group of children.
func (e *decodeEmitter) emitList(n *StructureNode, path []int) error {
	switch {
	case n.Repeat != "" && n.Typed:
		return e.emitTypedRepeat(n, path)
	case n.Repeat != "":
		name := nodeNames(n)
		e.assign(e.field(name.Field, "[]secs2.Item", path), fmt.Sprintf("decodeItems(%s, %q)", pathExpr(path), name.Label))

		return nil
	case n.Packed != "":
		return e.emitPacked(n, path)
	default:
		return e.emitFixedList(n, path)
	}
}

// emitTypedRepeat walks a typed repeat:
// a list whose every element is one item of the of leaf's format,
// decoded element by element with the helper a leaf of that item uses.
// An element's decode error names the field and the element's index.
func (e *decodeEmitter) emitTypedRepeat(n *StructureNode, path []int) error {
	it := e.items[n.Of.Item]
	if it.Binding != BindingFixed || isByteArrayGoType(it.GoType) {
		return fmt.Errorf("typed repeat %s: item %s needs a binding: fixed item with a scalar goType", n.Repeat, n.Of.Item)
	}

	goType := it.GoType
	if hasNamedType(it) {
		goType = n.Of.Item // enum item: the named defined type generated into items.go
	}

	sf, err := scalarDecodeFunc(firstFormat(it), goType)
	if err != nil {
		return fmt.Errorf("typed repeat %s: item %s: %w", n.Repeat, n.Of.Item, err)
	}

	fn := sf.fn
	if sf.result != goType {
		// decodeEach cannot convert a helper's plain result to a named type.
		// decodeASCIIAs reads a named string enum directly;
		// no other format has such a helper.
		if firstFormat(it) != "A" {
			return fmt.Errorf("typed repeat %s: item %s: no decoder for a named %s type of format %s", n.Repeat, n.Of.Item, sf.result, firstFormat(it))
		}

		fn = fmt.Sprintf("decodeASCIIAs[%s]", goType)
	}

	name := nodeNames(n)
	field := decodeField{Name: name.Field, Type: "[]" + goType, Param: e.params, Elem: it.GoType, Path: slices.Clone(path)}
	e.fields = append(e.fields, field)
	e.params++

	e.assign(field.Name, fmt.Sprintf("decodeEach(%s, %q, %s)", pathExpr(path), name.Label, fn))

	return nil
}

// emitPacked walks a packed multi-value group, which is a single primitive item on the wire, not
// a list of separate items.
func (e *decodeEmitter) emitPacked(n *StructureNode, path []int) error {
	it := e.items[n.Of.Item]
	if itemFormat := firstFormat(it); itemFormat != "B" || it.GoType != "byte" {
		return fmt.Errorf("packed %s: no decoder for item %s (format %q, goType %q)", n.Packed, n.Of.Item, firstFormat(it), it.GoType)
	}

	name := nodeNames(n)
	e.assign(e.field(name.Field, "[]byte", path), fmt.Sprintf("decodeBinary(%s, %q)", pathExpr(path), name.Label))

	return nil
}

// emitFixedList walks a fixed group of children, emitting its arity check first.
//
// A group the DSL bounds with minItems below its declared child count is optional: SEMI E5 lets
// the sender omit it entirely, so both the short and the full form decode, and the fields the
// short form omits keep their zero value.
func (e *decodeEmitter) emitFixedList(n *StructureNode, path []int) error {
	label := e.pathLabel(path)
	full := len(n.Items)

	if n.MaxItems != nil && *n.MaxItems != full {
		return fmt.Errorf("%s: maxItems %d does not match the %d declared children", label, *n.MaxItems, full)
	}

	short := full
	if n.MinItems != nil {
		short = *n.MinItems
	}

	if short > full {
		return fmt.Errorf("%s: minItems %d exceeds the %d declared children", label, short, full)
	}

	if short == full {
		e.emit(
			fmt.Sprintf("if err = requireArity(%s, %q, %d); err != nil {", pathExpr(path), label, full),
			"return out, err",
			"}",
			"",
		)

		return e.emitChildren(n, path, 0, full)
	}

	count := fmt.Sprintf("n%d", e.nextID())
	e.emit(
		fmt.Sprintf("%s, err := optionalArity(%s, %q, %d, %d)", count, pathExpr(path), label, short, full),
		"if err != nil {",
		"return out, err",
		"}",
		"",
	)

	if err := e.emitChildren(n, path, 0, short); err != nil {
		return err
	}

	// Fence the optional tail: the short form stops at `short` children, so the fields beyond it
	// are read only when the full form arrived.
	tail := &decodeEmitter{code: e.code, items: e.items, counter: e.counter, params: e.params}
	if err := tail.emitChildren(n, path, short, full); err != nil {
		return err
	}

	e.counter = tail.counter
	e.params = tail.params
	e.fields = append(e.fields, tail.fields...)
	e.optionals = append(e.optionals, tail.optionals...)

	e.emit(fmt.Sprintf("if %s == %d {", count, full))
	e.emit(trimTrailingBlank(tail.lines)...)
	e.emit("}", "")

	return nil
}

// emitChildren walks children [from, to) of a fixed list node.
func (e *decodeEmitter) emitChildren(n *StructureNode, path []int, from int, to int) error {
	for i := from; i < to; i++ {
		if err := e.emitNode(&n.Items[i], append(append([]int{}, path...), i)); err != nil {
			return err
		}
	}

	return nil
}

// emitLeaf walks a leaf node, dispatching on the E5 data item's declared SECS-II format.
//
// The format, not the Go type, picks the accessor: a byte-typed item is B (binary) while a
// uint8-typed one is U1, and Go cannot tell byte from uint8.
func (e *decodeEmitter) emitLeaf(n *StructureNode, path []int) error {
	it := e.items[n.Item]
	name := nodeNames(n)
	expr := pathExpr(path)

	// Validation rejects this case first;
	// the check keeps an unvalidated node from being decoded as if it were not optional.
	if n.Optional && (it.Binding != BindingFixed || isByteArrayGoType(it.GoType)) {
		return fmt.Errorf("item %s: optional needs a binding: fixed item with a scalar goType", n.Item)
	}

	if it.Binding != BindingFixed {
		e.assign(e.field(name.Field, "secs2.Item", path), fmt.Sprintf("decodeItem(%s, %q)", expr, name.Label))

		return nil
	}

	goType := it.GoType
	if hasNamedType(it) {
		goType = n.Item // enum item: the named defined type generated into items.go
	}

	itemFormat := firstFormat(it)
	if itemFormat == "B" && it.GoType != "byte" {
		return e.emitBinaryLeaf(n, name, goType, path)
	}

	sc, err := scalarDecodeCall(itemFormat, goType, expr, name.Label)
	if err != nil {
		return fmt.Errorf("item %s: %w", n.Item, err)
	}

	if !n.Optional {
		e.emit(e.decodeInto(e.field(name.Field, goType, path), goType, sc)...)

		return nil
	}

	e.emitOptionalLeaf(name, goType, itemFormat, path, sc)

	return nil
}

// scalarCall is a decode helper call that reads one single-value leaf.
type scalarCall struct {
	call   string // the call, e.g. decodeUintAs[uint16](root.At(0), "SVID")
	family string // the secs2 item type the call reads, e.g. *secs2.UintItem
	result string // the Go type the call returns
}

// scalarFunc is a decode helper that reads one single-value item, named as a function value.
type scalarFunc struct {
	fn     string // the helper, e.g. decodeUintAs[uint16]
	family string // the secs2 item type the helper reads, e.g. *secs2.UintItem
	result string // the Go type the helper returns
}

// scalarDecodeFunc returns the decode helper that reads a single-value item of format itemFormat as goType.
//
// The numeric and byte helpers are generic and return goType itself;
// the ASCII and boolean helpers return a plain string or bool,
// which a named enum type must be converted from.
func scalarDecodeFunc(itemFormat string, goType string) (scalarFunc, error) {
	switch itemFormat {
	case "A":
		return scalarFunc{"decodeASCII", "*secs2.ASCIIItem", "string"}, nil
	case "BOOLEAN":
		return scalarFunc{"decodeBool", "*secs2.BooleanItem", "bool"}, nil
	case "B":
		return scalarFunc{fmt.Sprintf("decodeByteAs[%s]", goType), "*secs2.BinaryItem", goType}, nil
	case "U1", "U2", "U4", "U8":
		return scalarFunc{fmt.Sprintf("decodeUintAs[%s]", goType), "*secs2.UintItem", goType}, nil
	case "I1", "I2", "I4", "I8":
		return scalarFunc{fmt.Sprintf("decodeIntAs[%s]", goType), "*secs2.IntItem", goType}, nil
	case "F4", "F8":
		return scalarFunc{fmt.Sprintf("decodeFloatAs[%s]", goType), "*secs2.FloatItem", goType}, nil
	default:
		return scalarFunc{}, fmt.Errorf("no decoder for format %q", itemFormat)
	}
}

// scalarDecodeCall returns the decode helper call
// that reads a single-value leaf of format itemFormat at expr as goType.
// label is the name the call's errors report.
func scalarDecodeCall(itemFormat string, goType string, expr string, label string) (scalarCall, error) {
	sf, err := scalarDecodeFunc(itemFormat, goType)
	if err != nil {
		return scalarCall{}, err
	}

	return scalarCall{fmt.Sprintf("%s(%s, %q)", sf.fn, expr, label), sf.family, sf.result}, nil
}

// decodeInto renders the statements that decode sc into the field name of type goType, or return the error.
//
// When sc returns another type than goType,
// as decodeASCII does for a named string enum,
// the value goes through a local s<k> and is converted on assignment.
func (e *decodeEmitter) decodeInto(name string, goType string, sc scalarCall) []string {
	if sc.result == goType {
		return []string{
			fmt.Sprintf("if out.%s, err = %s; err != nil {", name, sc.call),
			"return out, err",
			"}",
			"",
		}
	}

	v := fmt.Sprintf("s%d", e.nextID())

	return []string{
		fmt.Sprintf("var %s %s", v, sc.result),
		fmt.Sprintf("if %s, err = %s; err != nil {", v, sc.call),
		"return out, err",
		"}",
		"",
		fmt.Sprintf("out.%s = %s(%s)", name, goType, v),
		"",
	}
}

// emitOptionalLeaf emits the statements that decode an optional leaf into its value and presence fields.
//
// A zero-length item of the type sc reads leaves the value at zero and the presence field false.
// Any other item goes to sc, exactly as for a leaf that is not optional,
// so a wrong-format item, empty or not, is still an error.
func (e *decodeEmitter) emitOptionalLeaf(name emittedName, goType string, itemFormat string, path []int, sc scalarCall) {
	value := e.field(name.Field, goType, path)
	presence := e.presenceField(value, path)

	empty := fmt.Sprintf("secs2.%s()", itemFormat)
	if itemFormat == "A" {
		empty = `secs2.A("")`
	}

	e.optionals = append(e.optionals, optionalLeaf{Field: value, Path: slices.Clone(path), Empty: empty})

	e.emit(
		fmt.Sprintf("if out.%s, err = presentAt[%s](%s, %q); err != nil {", presence, sc.family, pathExpr(path), name.Label),
		"return out, err",
		"}",
		"",
		fmt.Sprintf("if out.%s {", presence),
	)
	e.emit(trimTrailingBlank(e.decodeInto(value, goType, sc))...)
	e.emit("}", "")
}

// emitBinaryLeaf walks a B-format leaf whose Go type is a byte slice or a byte array,
// and so reads the whole item or a fixed-width part of it rather than one byte.
func (e *decodeEmitter) emitBinaryLeaf(n *StructureNode, name emittedName, goType string, path []int) error {
	rawType := e.items[n.Item].GoType
	expr := pathExpr(path)

	switch {
	case rawType == "[]byte":
		e.assign(e.field(name.Field, goType, path), fmt.Sprintf("decodeBinary(%s, %q)", expr, name.Label))
	case byteArrayGoTypeRE.MatchString(rawType):
		width, err := strconv.Atoi(strings.TrimSuffix(strings.TrimPrefix(rawType, "["), "]byte"))
		if err != nil {
			return fmt.Errorf("item %s: cannot read the array width out of goType %q", n.Item, rawType)
		}

		blob := fmt.Sprintf("b%d", e.nextID())
		e.emit(
			fmt.Sprintf("var %s []byte", blob),
			fmt.Sprintf("if %s, err = decodeBinary(%s, %q); err != nil {", blob, expr, name.Label),
			"return out, err",
			"}",
			"",
			fmt.Sprintf("if len(%s) != %d {", blob, width),
			fmt.Sprintf("return out, errSize(%q, len(%s), %d)", name.Label, blob, width),
			"}",
			"",
			fmt.Sprintf("out.%s = %s(%s)", e.field(name.Field, goType, path), goType, blob),
			"",
		)
	default:
		return fmt.Errorf("item %s: no decoder for B-format goType %q", n.Item, goType)
	}

	return nil
}

// trimTrailingBlank drops the blank separator line a statement block ends with, so a block nested
// inside a generated guard does not leave a stray empty line before its closing brace.
func trimTrailingBlank(lines []string) []string {
	if len(lines) > 0 && lines[len(lines)-1] == "" {
		return lines[:len(lines)-1]
	}

	return lines
}

// firstFormat returns the item's declared SECS-II format, or "" when it declares none.
func firstFormat(it Item) string {
	if len(it.Formats) == 0 {
		return ""
	}

	return it.Formats[0]
}

// resultTypeDoc renders the godoc lines for a result struct: a name-first summary naming the
// message it decodes, then the same Body: shorthand the builder's godoc carries, so the struct
// and the shape it mirrors read together.
func resultTypeDoc(typeName string, code string, mainName string, m Message, b Body, items map[string]Item) []string {
	doc := []string{
		fmt.Sprintf("%s is the decoded body of the %s (%s) message%s.", typeName, code, m.Name, actorClause(b.Actor)),
		"",
		ensurePeriod("Body: " + BodyDoc(b.Structure, items)),
		"",
	}

	names := optionalFieldNames(b.Structure)
	doc = append(doc, mirrorsDoc(mainName, names, typedRepeats(b.Structure))...)

	if len(names) > 0 {
		pairs := make([]string, len(names))
		for i, name := range names {
			pairs[i] = fmt.Sprintf("%sPresent for %s", name, name)
		}

		doc = append(doc,
			"",
			optionalSummary(names)+":",
		)

		if len(names) == 1 {
			doc = append(doc, fmt.Sprintf("[Decode%s] decodes that form as a zero %s with %sPresent false.", mainName, names[0], names[0]))
		} else {
			doc = append(doc,
				fmt.Sprintf("[Decode%s] decodes that form as a zero field with its presence field false,", mainName),
				joinAnd(pairs)+".",
			)
		}

		doc = append(doc,
			fmt.Sprintf("[%s] always sends a value,", mainName),
			"so callers that need the zero-length form build the body with [secs2] directly.",
		)
	}

	if m.Source == "external" {
		doc = append(doc, "", externalSourceDisclaimer)
	}

	return doc
}

// decoderDoc renders the godoc lines for a decoder function.
func decoderDoc(typeName string, code string, mainName string, m Message, b Body) []string {
	doc := []string{
		fmt.Sprintf("Decode%s decodes the body of an %s (%s) message%s into a [%s].", mainName, code, m.Name, actorClause(b.Actor), typeName),
		"",
		"item is the message body, as returned by SECS2Message.Item.",
		"The body must match the shape [" + typeName + "] documents:",
		"a wrong list length, a wrong SECS-II type, or a missing position returns an error naming the E5 field that failed.",
	}

	if m.Source == "external" {
		doc = append(doc, "", externalSourceDisclaimer)
	}

	return doc
}

// mirrorsDoc renders the godoc sentence saying a result struct mirrors the parameters of its builder mainName,
// naming the exceptions:
// the presence fields of the optional leaves names,
// and the fields of the typed repeats, which hold decoded values where the builder takes secs2 items.
func mirrorsDoc(mainName string, names []string, typed []*StructureNode) []string {
	head := fmt.Sprintf("It is filled by [Decode%s] and mirrors the parameters of [%s], in order", mainName, mainName)

	var tails []string

	if len(names) > 0 {
		presence := make([]string, len(names))
		for i, name := range names {
			presence[i] = name + "Present"
		}

		fields := "field"
		if len(names) > 1 {
			fields = "fields"
		}

		tails = append(tails, fmt.Sprintf("apart from its %s %s", joinAnd(presence), fields))
	}

	if len(typed) > 0 {
		fields := make([]string, len(typed))
		for i, r := range typed {
			fields[i] = nodeNames(r).Field
		}

		verb := "holds"
		if len(typed) > 1 {
			verb = "hold"
		}

		clause := fmt.Sprintf("except that %s %s decoded values where [%s] takes [secs2.Item] elements", joinAnd(fields), verb, mainName)
		if len(tails) > 0 {
			clause = "and " + clause
		}

		tails = append(tails, clause)
	}

	if len(tails) == 0 {
		return []string{head + "."}
	}

	doc := []string{head + ","}
	for i, tail := range tails {
		if i == len(tails)-1 {
			doc = append(doc, tail+".")
		} else {
			doc = append(doc, tail+",")
		}
	}

	return doc
}

// optionalFieldNames returns the value field names of a body's optional leaves, in body order.
//
// It walks the same nodes as [BuildParams]:
// nothing below a repeat's or packed group's of may be optional.
func optionalFieldNames(n *StructureNode) []string {
	if n == nil {
		return nil
	}

	if n.Kind() == "leaf" && n.Optional {
		return []string{nodeNames(n).Field}
	}

	var names []string
	for i := range n.Items {
		names = append(names, optionalFieldNames(&n.Items[i])...)
	}

	return names
}

// optionalSummary renders the godoc clause naming the optional items of a body, e.g.
// "SEMI E5 lets FNLOC and FFROT each be sent as a zero-length item".
// names must not be empty.
func optionalSummary(names []string) string {
	if len(names) == 1 {
		return fmt.Sprintf("SEMI E5 lets %s be sent as a zero-length item", names[0])
	}

	return fmt.Sprintf("SEMI E5 lets %s each be sent as a zero-length item", joinAnd(names))
}

// joinAnd joins words as an English list:
// "A", "A and B", or "A, B, and C".
func joinAnd(words []string) string {
	switch len(words) {
	case 0:
		return ""
	case 1:
		return words[0]
	case 2:
		return words[0] + " and " + words[1]
	default:
		return strings.Join(words[:len(words)-1], ", ") + ", and " + words[len(words)-1]
	}
}

// optionalBuilderDoc renders the godoc paragraph a builder of a body with optional leaves carries,
// or nil when the body has none.
func optionalBuilderDoc(n *StructureNode) []string {
	names := optionalFieldNames(n)
	if len(names) == 0 {
		return nil
	}

	return []string{
		"",
		optionalSummary(names) + ",",
		"but this builder always sends a value:",
		"callers that need the zero-length form build the body with [secs2] directly.",
	}
}

// typedBuilderDoc renders the godoc paragraph a builder of a body with typed repeats carries,
// or nil when the body has none:
// the builder takes each element as a secs2 item,
// while the decoder returns the elements as values.
func typedBuilderDoc(n *StructureNode, items map[string]Item) []string {
	repeats := typedRepeats(n)
	doc := make([]string, 0, 4*len(repeats))

	for _, r := range repeats {
		it := items[r.Of.Item]
		goType := it.GoType
		if hasNamedType(it) {
			goType = r.Of.Item
		}

		doc = append(doc,
			"",
			fmt.Sprintf("Each element of %s is a [secs2.Item] holding one %s of format %s,", nodeNames(r).Param, r.Of.Item, firstFormat(it)),
			fmt.Sprintf("which the caller builds with [secs2.%s];", firstFormat(it)),
			fmt.Sprintf("the decoder returns the elements as values, in a []%s.", goType),
		)
	}

	return doc
}

// typedRepeats returns the typed repeats of a body, in body order.
//
// It walks the same nodes as [BuildParams]:
// nothing below a repeat's or packed group's of may be typed.
func typedRepeats(n *StructureNode) []*StructureNode {
	if n == nil {
		return nil
	}

	if n.Repeat != "" && n.Typed {
		return []*StructureNode{n}
	}

	var out []*StructureNode
	for i := range n.Items {
		out = append(out, typedRepeats(&n.Items[i])...)
	}

	return out
}

// actorClause renders the " for equipment" / " for host" godoc fragment for a body's actor.
func actorClause(actor string) string {
	switch actor {
	case "equipment":
		return " for equipment"
	case "host":
		return " for host"
	default: // "both": no actor clause
		return ""
	}
}
