package main

import (
	"bytes"
	"errors"
	"fmt"
	"go/token"
	"go/types"
	"io"
	"maps"
	"math"
	"regexp"
	"slices"
	"strings"

	"gopkg.in/yaml.v3"
)

// singleFormatGoType maps a single secs2 constructor to its concrete Go type
// for fixed-binding items. `L` is intentionally absent (lists are always open).
var singleFormatGoType = map[string]string{
	"A": "string", "J": "string", "W": "string",
	"B": "byte", "BOOLEAN": "bool",
	"I1": "int8", "I2": "int16", "I4": "int32", "I8": "int64",
	"U1": "uint8", "U2": "uint16", "U4": "uint32", "U8": "uint64",
	"F4": "float32", "F8": "float64",
}

// asNameRE matches a valid as: name: a letter, then letters and digits.
var asNameRE = regexp.MustCompile(`^[A-Za-z][A-Za-z0-9]*$`)

// variantNameRE matches a valid variant: name: an upper-case letter, then letters and digits,
// so it reads as one more CamelCase word of the builder name it ends.
var variantNameRE = regexp.MustCompile(`^[A-Z][A-Za-z0-9]*$`)

// integerRanges maps every goType a values table may enumerate with integers
// to the range its constants must fall in.
// A float goType takes any integer:
// a typed float constant rounds rather than overflows.
var integerRanges = map[string]struct {
	lo int64
	hi uint64
}{
	"byte": {0, math.MaxUint8}, "uint8": {0, math.MaxUint8},
	"uint16": {0, math.MaxUint16}, "uint32": {0, math.MaxUint32}, "uint64": {0, math.MaxUint64},
	"int8": {math.MinInt8, math.MaxInt8}, "int16": {math.MinInt16, math.MaxInt16},
	"int32": {math.MinInt32, math.MaxInt32}, "int64": {math.MinInt64, math.MaxInt64},
	"float32": {math.MinInt64, math.MaxUint64}, "float64": {math.MinInt64, math.MaxUint64},
}

// byteArrayGoTypeRE matches a fixed-size byte-array Go type, e.g. "[10]byte".
var byteArrayGoTypeRE = regexp.MustCompile(`^\[[0-9]+\]byte$`)

// deriveBinding derives whether an item with the given SECS-II format-code
// shortcuts should be treated as fixed or open. An item is fixed only when it
// declares exactly one format and that format has a known Go type; every
// other case (zero, multiple, or unknown formats) is open.
func deriveBinding(formats []string) Binding {
	if len(formats) == 1 {
		if _, ok := singleFormatGoType[formats[0]]; ok {
			return BindingFixed
		}
	}

	return BindingOpen
}

// deriveGoType derives the concrete Go type for a fixed-binding item's format
// list. It reports false when the formats do not derive a fixed binding.
func deriveGoType(formats []string) (string, bool) {
	if deriveBinding(formats) != BindingFixed {
		return "", false
	}
	return singleFormatGoType[formats[0]], true
}

// isByteArrayGoType reports whether gt is a valid byte-array/byte-slice
// alternative to the scalar "byte" goType, allowed only on a format-B item:
// E5 format code 10 covers both a single accept/deny byte and a multi-byte
// binary string (e.g. MHEAD/SHEAD's 10-byte header, ABS's variable-length
// blob), and the DSL needs a way to say which.
func isByteArrayGoType(gt string) bool {
	return gt == "[]byte" || byteArrayGoTypeRE.MatchString(gt)
}

// LoadItems parses the items.yaml DSL content into a name-keyed map of Item.
// A key the DSL does not define is an error (see decodeStrict).
func LoadItems(data []byte) (map[string]Item, error) {
	items := map[string]Item{}
	if err := decodeStrict(data, &items); err != nil {
		return nil, fmt.Errorf("parse items: %w", err)
	}

	return items, nil
}

// decodeStrict decodes the YAML document in data into out,
// rejecting any mapping key that names no field of the struct it decodes into,
// so a misspelled key such as optinal fails loading instead of being ignored.
// Empty input decodes to nothing, as with yaml.Unmarshal.
func decodeStrict(data []byte, out any) error {
	dec := yaml.NewDecoder(bytes.NewReader(data))
	dec.KnownFields(true)

	if err := dec.Decode(out); err != nil && !errors.Is(err, io.EOF) {
		return err
	}

	return nil
}

// ValidateItems checks every loaded item against the Data Item Dictionary
// rules: a fixed-binding item must declare exactly one format and a goType
// matching what deriveGoType derives for it; every item's stored Binding must
// match what deriveBinding derives from its Formats.
// A values: table must suit its item (see checkValues),
// namedType needs an item that can have a named type (see checkNamedType),
// and the constants all tables generate must be distinct identifiers (see checkValueNames).
func ValidateItems(items map[string]Item) error {
	for name, it := range items {
		want := deriveBinding(it.Formats)
		if it.Binding != want {
			return fmt.Errorf("item %s: binding %q, want %q for formats %v", name, it.Binding, want, it.Formats)
		}

		if it.Binding == BindingFixed {
			if len(it.Formats) != 1 {
				return fmt.Errorf("item %s: fixed binding needs exactly one format, got %v", name, it.Formats)
			}

			gt, _ := deriveGoType(it.Formats)
			if it.GoType == "" {
				return fmt.Errorf("item %s: fixed binding needs goType", name)
			}
			if it.GoType != gt && (it.Formats[0] != "B" || !isByteArrayGoType(it.GoType)) {
				return fmt.Errorf("item %s: goType %q, want %q (or a byte-array/byte-slice variant for format B)", name, it.GoType, gt)
			}
		}

		if err := checkValues(name, it); err != nil {
			return err
		}

		if err := checkNamedType(name, it); err != nil {
			return err
		}

		if err := checkCustomString(name, it); err != nil {
			return err
		}
	}

	return checkValueNames(items)
}

// checkValues checks an item's values: table against the item.
//
// Every value must be given.
// A fixed item's values become constants of its named type,
// so they must suit its goType:
// integers within the goType's range for an integer goType,
// any integer for a float goType,
// and strings for a string goType.
// No other goType can carry an enumeration.
// Only an ASCII (format A) item takes strings,
// since its generated code reads and writes them with the ASCII helpers.
// An open item's values become untyped constants,
// which may be integers or strings,
// but all of one kind, so the set reads as one enumeration,
// and a kind one of the item's formats can carry:
// strings need format A,
// and integers a binary, unsigned, or signed integer format.
// A value's source, when given, must name a SEMI standard in lower case, e.g. e87.1.
// Notes become godoc lines, so each must be a single line,
// and an item note needs a values table to document.
func checkValues(name string, it Item) error {
	if it.Note != "" && len(it.Values) == 0 {
		return fmt.Errorf("item %s: note needs a values table, whose godoc it is rendered into", name)
	}

	if strings.Contains(it.Note, "\n") {
		return fmt.Errorf("item %s: note must be a single line", name)
	}

	strs := 0

	for _, v := range it.Values {
		if !v.Value.IsSet() {
			return fmt.Errorf("item %s: value %s has a missing or null value", name, v.Name)
		}

		if strings.Contains(v.Note, "\n") {
			return fmt.Errorf("item %s: value %s: note must be a single line", name, v.Name)
		}

		if v.Source != "" && !semiSourceRE.MatchString(v.Source) {
			return fmt.Errorf("item %s: value %s has source %q, want a lower-case SEMI standard such as e87.1", name, v.Name, v.Source)
		}

		if v.Value.IsString() {
			strs++
		}
	}

	if len(it.Values) == 0 {
		return nil
	}

	if it.Binding != BindingFixed {
		return checkOpenValues(name, it, strs)
	}

	if it.GoType == "string" {
		if it.Formats[0] != "A" {
			return fmt.Errorf("item %s: format %s cannot carry a values table: string values need format A", name, it.Formats[0])
		}

		for _, v := range it.Values {
			if !v.Value.IsString() {
				return fmt.Errorf("item %s: value %s is %s, but goType string needs a string", name, v.Name, v.Value)
			}
		}

		return nil
	}

	r, ok := integerRanges[it.GoType]
	if !ok {
		return fmt.Errorf("item %s: goType %s cannot carry a values table", name, it.GoType)
	}

	for _, v := range it.Values {
		if !v.Value.fitsInteger(r.lo, r.hi) {
			return fmt.Errorf("item %s: value %s is %s, but goType %s needs an integer in [%d, %d]", name, v.Name, v.Value, it.GoType, r.lo, r.hi)
		}
	}

	return nil
}

// checkNamedType checks an item marked namedType:
// only a fixed item has a type of its own,
// an item with a values table has its named type already,
// and its goType must be one a values table could give a named type,
// so the type reads and writes as an enumeration's does:
// an integer or float, or a string on format A.
func checkNamedType(name string, it Item) error {
	if !it.NamedType {
		return nil
	}

	if it.Binding != BindingFixed {
		return fmt.Errorf("item %s: namedType needs a fixed item; an open item's fields stay secs2.Item", name)
	}

	if len(it.Values) > 0 {
		return fmt.Errorf("item %s: namedType is for an item without values; a values table already gives the item its named type", name)
	}

	if _, ok := integerRanges[it.GoType]; !ok && (it.GoType != "string" || it.Formats[0] != "A") {
		return fmt.Errorf("item %s: namedType needs an integer or float goType, or string on format A, got %s on %v", name, it.GoType, it.Formats)
	}

	return nil
}

// checkCustomString checks an item marked customString:
// only an item the generator would otherwise write a String method for may have one by hand,
// so the mark never stands on an item where it changes nothing.
func checkCustomString(name string, it Item) error {
	if !it.CustomString {
		return nil
	}

	if !stringerEligible(it) {
		return fmt.Errorf("item %s: customString needs a fixed item with a byte or uint8 goType and a values table, which would otherwise get a generated String method", name)
	}

	return nil
}

// checkOpenValues checks an open item's values table,
// of which strs values are strings:
// all of one kind, and a kind one of the item's formats can carry.
func checkOpenValues(name string, it Item, strs int) error {
	switch {
	case strs != 0 && strs != len(it.Values):
		return fmt.Errorf("item %s: values mix integers and strings", name)
	case strs != 0 && !slices.Contains(it.Formats, "A"):
		return fmt.Errorf("item %s: string values need format A, but the item may be sent only as %v", name, it.Formats)
	case strs == 0 && !slices.ContainsFunc(it.Formats, isIntegerFormat):
		return fmt.Errorf("item %s: integer values need a B, U, or I format, but the item may be sent only as %v", name, it.Formats)
	default:
		return nil
	}
}

// isIntegerFormat reports whether f is a binary, unsigned, or signed integer format,
// whose secs2 constructor takes an integer constant.
func isIntegerFormat(f string) bool {
	switch f {
	case "B", "U1", "U2", "U4", "U8", "I1", "I2", "I4", "I8":
		return true
	default:
		return false
	}
}

// checkValueNames rejects values whose constants would not compile in one package.
//
// Each value is generated as the constant <ITEM><Name>,
// which must be a Go identifier,
// and must not repeat another value's constant
// or the named type of an item with a fixed values table.
func checkValueNames(items map[string]Item) error {
	names := slices.Sorted(maps.Keys(items))

	seen := map[string]string{}
	for _, name := range names {
		it := items[name]
		if hasNamedType(it) {
			seen[name] = "the type of item " + name
		}
	}

	for _, name := range names {
		for _, v := range items[name].Values {
			c := name + v.Name
			if v.Name == "" || !token.IsIdentifier(c) {
				return fmt.Errorf("item %s: value name %q does not make a Go identifier", name, v.Name)
			}

			if prev, ok := seen[c]; ok {
				return fmt.Errorf("item %s: constant %s would also name %s", name, c, prev)
			}

			seen[c] = "a value of item " + name
		}
	}

	return nil
}

// LoadMessageFile parses one stream's messages YAML (e.g. messages/s1.yaml)
// into a MessageFile.
// A key the DSL does not define is an error (see decodeStrict).
func LoadMessageFile(data []byte) (MessageFile, error) {
	var mf MessageFile
	if err := decodeStrict(data, &mf); err != nil {
		return mf, fmt.Errorf("parse messages: %w", err)
	}

	return mf, nil
}

// ValidateMessages checks every message across all loaded message files
// against the DSL rules: every {item: X} reference at any structure depth
// must exist in items; no (stream, function) pair may repeat across files;
// every body's actor must be one of both/equipment/host; a list node may not
// combine more than one of repeat, packed, or minItems/maxItems, since they are
// mutually exclusive node shapes; and a packed node's of.item must resolve to a
// binding: fixed item, since packing needs one shared format for every value.
// optional: is valid only on the nodes checkOptional accepts.
// as: is valid only on a leaf or opaque node and must be a plain identifier,
// and every body's emitted names must be usable Go identifiers that do not collide
// (see checkNames).
// An acceptReply annotation must fit its body (see checkAcceptReply).
// A message's variant bodies must each shorten a main body (see checkVariants),
// and the package-level names a stream file emits must be distinct (see checkFileNames).
func ValidateMessages(files []MessageFile, items map[string]Item) error {
	seen := map[[2]int]bool{}
	for _, f := range files {
		for _, m := range f.Messages {
			key := [2]int{f.Stream, m.Function}
			if seen[key] {
				return fmt.Errorf("duplicate S%dF%d", f.Stream, m.Function)
			}
			seen[key] = true

			for _, b := range m.Bodies {
				switch b.Actor {
				case "both", "equipment", "host":
				default:
					return fmt.Errorf("S%dF%d: bad actor %q", f.Stream, m.Function, b.Actor)
				}

				if b.AcceptReply != nil {
					if err := checkAcceptReply(f, m, b, items); err != nil {
						return fmt.Errorf("S%dF%d (actor %s): %w", f.Stream, m.Function, b.Actor, err)
					}
				}

				ctx := nodeCtx{where: fmt.Sprintf("S%dF%d", f.Stream, m.Function)}
				if err := walkNode(b.Structure, items, ctx, roleRoot); err != nil {
					return err
				}

				if err := checkNames(b.Structure, items); err != nil {
					return fmt.Errorf("S%dF%d (actor %s): %w", f.Stream, m.Function, b.Actor, err)
				}
			}

			if err := checkVariants(m, items); err != nil {
				return fmt.Errorf("S%dF%d: %w", f.Stream, m.Function, err)
			}
		}

		if err := checkFileNames(f); err != nil {
			return err
		}
	}

	return nil
}

// checkAcceptReply checks the acceptReply annotation of the body b of the message m in the stream file f.
//
// Only a reply's main body may carry one: an even function's, whose primary f defines and sends with a reply expected in some form.
// The body's builder parameters must be one leaf plus any number of repeats, or none at all (see acceptReplyLeaves).
// With none, the annotation names no value.
// With a leaf, the leaf must be of a binding: fixed item with a values table, and not optional,
// and the annotation must name one of its values.
func checkAcceptReply(f MessageFile, m Message, b Body, items map[string]Item) error {
	switch {
	case m.Function%2 != 0:
		return errors.New("acceptReply: only a reply, an even function, may carry it")
	case b.Variant != "":
		return fmt.Errorf("acceptReply: variant %s may not carry it; annotate the main body", b.Variant)
	}

	primary, ok := findMessage(f, m.Function-1)
	if !ok {
		return fmt.Errorf("acceptReply: the primary S%dF%d is not defined", f.Stream, m.Function-1)
	}

	if !slices.ContainsFunc(primary.Bodies, func(p Body) bool { return p.Variant == "" && p.ReplyExpected }) {
		return fmt.Errorf("acceptReply: no form of the primary S%dF%d expects a reply", f.Stream, m.Function-1)
	}

	leaves, err := acceptReplyLeaves(b.Structure)
	if err != nil {
		return err
	}

	value := b.AcceptReply.Value

	if len(leaves) == 0 {
		if value != "" {
			return fmt.Errorf("acceptReply: value %s names no leaf: the body has no parameters", value)
		}

		return nil
	}

	leaf := leaves[0]
	it := items[leaf.Item]

	switch {
	case it.Binding != BindingFixed || len(it.Values) == 0:
		return fmt.Errorf("acceptReply: the leaf %s must be of a binding: fixed item with a values table", leaf.Item)
	case leaf.Optional:
		return fmt.Errorf("acceptReply: the leaf %s is optional; an acceptance sends a value", leaf.Item)
	case !slices.ContainsFunc(it.Values, func(v ItemValue) bool { return v.Name == value }):
		return fmt.Errorf("acceptReply: %s has no value named %q", leaf.Item, value)
	}

	return nil
}

// checkVariants checks a message's main and variant bodies against each other.
//
// A message has exactly one main (non-variant) body per actor,
// and every variant names itself with a CamelCase word and shortens the main body for its actor.
// A variant's generated test decodes it with that main body's decoder,
// so the main body must carry a structure,
// and each decoder field the variant emits must also be a field of the main body, of the same type:
// otherwise the test would read a field the main decoder's result does not have.
// A header-only variant is rejected:
// a message sent without a body carries no item at all, not an empty list,
// which a decoder of a list or a fixed item rejects,
// and which a decoder of a whole-body opaque or open item would not report as omitted fields.
// The main body's root must be a fixed list, bounded or not:
// a leaf, an opaque node, a repeat, or a packed group has no children a short form could omit.
// A variant is the same message in a shorter form,
// so it must set the same reply bit as its main body.
// Whether the main decoder accepts the variant's list lengths is left to the variant's generated test.
func checkVariants(m Message, items map[string]Item) error {
	mains := map[string]Body{}

	for _, b := range m.Bodies {
		if b.Variant != "" {
			continue
		}

		if _, ok := mains[b.Actor]; ok {
			return fmt.Errorf("actor %s has more than one body without a variant name", b.Actor)
		}

		mains[b.Actor] = b
	}

	for _, b := range m.Bodies {
		if b.Variant == "" {
			continue
		}

		if !variantNameRE.MatchString(b.Variant) {
			return fmt.Errorf("variant %q must be an upper-case letter followed by letters and digits", b.Variant)
		}

		main, ok := mains[b.Actor]
		if !ok {
			return fmt.Errorf("variant %s has no main body for actor %s", b.Variant, b.Actor)
		}

		if b.Structure == nil {
			return fmt.Errorf("variant %s is header-only, but the main body's decoder cannot read a message without a body as a short form", b.Variant)
		}

		if main.Structure == nil {
			return fmt.Errorf("variant %s shortens a header-only main body, which has no decoder to check it with", b.Variant)
		}

		if !isFixedList(main.Structure) {
			return fmt.Errorf("variant %s shortens a main body whose root is not a fixed list, which has no part to omit", b.Variant)
		}

		if b.ReplyExpected != main.ReplyExpected {
			return fmt.Errorf("variant %s sets replyExpected %t, but its main body sets %t", b.Variant, b.ReplyExpected, main.ReplyExpected)
		}

		if err := checkVariantFields(b, main, items); err != nil {
			return err
		}
	}

	return nil
}

// isFixedList reports whether n is a list of declared children, bounded or not,
// rather than a leaf, an opaque node, a repeat, or a packed group.
func isFixedList(n *StructureNode) bool {
	return n.Kind() == "list" && n.Repeat == "" && n.Packed == ""
}

// checkVariantFields rejects a variant that emits a decoder field its main body lacks,
// emits it with a different type (other than a plain value where the main body's leaf is optional),
// or sends it at a different position in the body.
// The main body's decoder reads each field from its own position,
// so a variant field at another position would decode into a different field;
// two fields of the same type swapped in the variant would still decode without error.
// It expects structures walkNode has already accepted.
func checkVariantFields(variant Body, main Body, items map[string]Item) error {
	mainFields, err := BuildFields(main.Structure, items)
	if err != nil {
		// A main body the decoder generator cannot handle is reported by renderDecoders.
		return nil //nolint:nilerr // reported by renderDecoders, not by variant checking
	}

	fields, err := BuildFields(variant.Structure, items)
	if err != nil {
		return fmt.Errorf("variant %s: %w", variant.Variant, err)
	}

	mainByName := map[string]decodeField{}
	for _, f := range mainFields {
		mainByName[f.Name] = f
	}

	for _, f := range fields {
		mf, ok := mainByName[f.Name]
		switch {
		case !ok:
			return fmt.Errorf("variant %s: field %s is not a field of the main body for actor %s", variant.Variant, f.Name, variant.Actor)
		case mf.Type != f.Type && mf.Type != "*"+f.Type: // a variant may send a value the main body lets be omitted
			return fmt.Errorf("variant %s: field %s has type %s, but the main body's field has type %s", variant.Variant, f.Name, f.Type, mf.Type)
		case !slices.Equal(mf.Path, f.Path):
			return fmt.Errorf("variant %s: field %s is at body%s, but the main body reads it from body%s", variant.Variant, f.Name, pathIndex(f.Path), pathIndex(mf.Path))
		default:
		}
	}

	return nil
}

// pathIndex renders a list-index path as the index suffix a body position reads as, e.g. "[1][0]",
// or "" for the body root.
func pathIndex(path []int) string {
	var b strings.Builder
	for _, idx := range path {
		fmt.Fprintf(&b, "[%d]", idx)
	}

	return b.String()
}

// checkFileNames rejects a stream file that would declare one package-level name twice.
//
// Every body emits a builder,
// and every main body with a structure also emits a decoder and its result type.
// Variant names make collisions possible that the fixed naming scheme alone never produced:
// variant Reply on an even function's equipment body spells that function's result type,
// and an equipment variant Host spells the host body's builder.
func checkFileNames(f MessageFile) error {
	seen := map[string]string{}

	add := func(name string, what string) error {
		if prev, ok := seen[name]; ok {
			return fmt.Errorf("%s would name both %s and %s", name, prev, what)
		}

		seen[name] = what

		return nil
	}

	for _, m := range f.Messages {
		for _, b := range m.Bodies {
			what := fmt.Sprintf("the S%dF%d builder for actor %s", f.Stream, m.Function, b.Actor)
			if b.Variant != "" {
				what = fmt.Sprintf("the S%dF%d variant %s builder for actor %s", f.Stream, m.Function, b.Variant, b.Actor)
			}

			if err := add(builderName(f.Stream, m.Function, b), what); err != nil {
				return err
			}

			if b.Variant != "" || b.Structure == nil {
				continue
			}

			name := callName(f.Stream, m.Function, b.Actor)

			if err := add("Decode"+name, fmt.Sprintf("the S%dF%d decoder for actor %s", f.Stream, m.Function, b.Actor)); err != nil {
				return err
			}

			if err := add(ResultTypeName(f.Stream, m.Function, b.Actor), fmt.Sprintf("the S%dF%d result type for actor %s", f.Stream, m.Function, b.Actor)); err != nil {
				return err
			}
		}
	}

	return nil
}

// nodeRole is where a structure node sits in its parent.
type nodeRole int

// nodeRole values.
const (
	roleRoot  nodeRole = iota // a body's or a group's whole structure
	roleChild                 // a child of a fixed list
	roleOf                    // a repeat's or packed group's of
)

// nodeCtx is what walkNode knows about the structure a node belongs to.
type nodeCtx struct {
	where   string // the prefix of every error: "S1F2", or "group Report"
	inOf    bool   // the node sits below a repeat's or packed group's of
	inGroup bool   // the node belongs to a group's structure
}

// walkNode recursively validates a structure node and its descendants
// (Items list children and the Of repeat/packed/bounded element shape) against
// the items dictionary, the repeat/packed/minItems/maxItems mutual-exclusion
// rule, and the packed-item binding: fixed requirement.
// A repeat's of must be a leaf, an opaque node, a group reference, or another repeat:
// a list element is declared once in groups.yaml and referenced by name (see checkGroupRef).
// Inside a group, a list is never bounded and never packed.
// role says where n sits in its parent.
func walkNode(n *StructureNode, items map[string]Item, ctx nodeCtx, role nodeRole) error {
	if n == nil {
		return nil
	}

	if err := checkNodeOptions(n, items, ctx); err != nil {
		return err
	}

	switch n.Kind() {
	case "leaf":
		if _, ok := items[n.Item]; !ok {
			return fmt.Errorf("%s: unknown item %q", ctx.where, n.Item)
		}
	case "group":
		return checkGroupRef(n, ctx, role)
	case "list":
		return walkList(n, items, ctx)
	default:
	}

	return nil
}

// walkList validates a list node and its descendants for [walkNode].
func walkList(n *StructureNode, items map[string]Item, ctx nodeCtx) error {
	s := ctx.where

	exclusive := 0
	if n.Repeat != "" {
		exclusive++
	}
	if n.Packed != "" {
		exclusive++
	}
	if n.MinItems != nil || n.MaxItems != nil {
		exclusive++
	}
	if exclusive > 1 {
		return fmt.Errorf("%s: repeat, packed, and minItems/maxItems are mutually exclusive", s)
	}

	if ctx.inGroup && (n.MinItems != nil || n.MaxItems != nil) {
		return fmt.Errorf("%s: a list inside a group cannot be bounded with minItems or maxItems (declare an omittable list as a group and reference it with optional: true)", s)
	}

	if ctx.inGroup && n.Packed != "" {
		return fmt.Errorf("%s: packed %q cannot sit inside a group", s, n.Packed)
	}

	if n.Packed != "" {
		if n.Of == nil || n.Of.Item == "" {
			return fmt.Errorf("%s: packed %q needs an of.item leaf", s, n.Packed)
		}
		it, ok := items[n.Of.Item]
		if !ok {
			return fmt.Errorf("%s: unknown item %q", s, n.Of.Item)
		}
		if it.Binding != BindingFixed {
			return fmt.Errorf("%s: packed %q's item %q must be binding: fixed (packing needs one shared format)", s, n.Packed, n.Of.Item)
		}
		if isByteArrayGoType(it.GoType) {
			return fmt.Errorf("%s: packed %q's item %q must not be a byte-array/byte-slice goType (packing multiple binary blobs under one header is not supported)", s, n.Packed, n.Of.Item)
		}
	}

	if n.Repeat != "" {
		if err := checkRepeatOf(n, s); err != nil {
			return err
		}
	}

	for i := range n.Items {
		if err := walkNode(&n.Items[i], items, ctx, roleChild); err != nil {
			return err
		}
	}

	of := ctx
	of.inOf = true

	return walkNode(n.Of, items, of, roleOf)
}

// checkRepeatOf rejects a repeat whose of is missing, a fixed or bounded list, or a packed group.
// A list element is declared once in groups.yaml, under a name its Go struct takes,
// and referenced as of: {group: Name}.
func checkRepeatOf(n *StructureNode, where string) error {
	switch {
	case n.Of == nil:
		return fmt.Errorf("%s: repeat %s needs an of", where, n.Repeat)
	case n.Of.Kind() != "list" || n.Of.Repeat != "":
		return nil
	case n.Of.Packed != "":
		return fmt.Errorf("%s: repeat %s: a packed group cannot be a repeat's element", where, n.Repeat)
	default:
		return fmt.Errorf("%s: repeat %s has an inline list element; declare it in groups.yaml and reference it as of: {group: Name}", where, n.Repeat)
	}
}

// checkGroupRef checks a group reference against where it sits.
//
// The reference must name a group [ResolveGroups] resolved, and set nothing but as and optional.
// It may be a repeat's of, without as,
// or a child of a fixed list inside a group, with both optional and as:
// the child is an omittable list, a field named by as and nil when sent as L[0].
// A message body holds a group only as a repeat's element,
// so the result struct's fields stay the body's own data items.
func checkGroupRef(n *StructureNode, ctx nodeCtx, role nodeRole) error {
	s := ctx.where

	switch {
	case n.def == nil:
		return fmt.Errorf("%s: unknown group %q", s, n.Group)
	case n.Type != "" || len(n.Items) > 0 || n.Repeat != "" || n.Packed != "" || n.Of != nil || n.MinItems != nil || n.MaxItems != nil:
		return fmt.Errorf("%s: group reference %s may set only as and optional", s, n.Group)
	}

	switch role {
	case roleOf:
		if n.As != "" {
			return fmt.Errorf("%s: group reference %s is a repeat's element, which the repeat names: drop as %q", s, n.Group, n.As)
		}
	case roleChild:
		if !ctx.inGroup {
			return fmt.Errorf("%s: group %s is referenced as a list child; a message body references a group only as a repeat's of", s, n.Group)
		}

		if !n.Optional || n.As == "" {
			return fmt.Errorf("%s: group %s is a child of a list, which needs optional: true and an as: name", s, n.Group)
		}
	case roleRoot:
		return fmt.Errorf("%s: group %s cannot be a whole body or group structure", s, n.Group)
	default:
		return fmt.Errorf("%s: group %s sits in an unknown position", s, n.Group)
	}

	return nil
}

// checkNodeOptions checks the as:, optional:, and allowEmptyList: settings of one node,
// leaving its children to walkNode.
func checkNodeOptions(n *StructureNode, items map[string]Item, ctx nodeCtx) error {
	if err := checkAs(n, ctx.where); err != nil {
		return err
	}

	if err := checkOptional(n, items, ctx); err != nil {
		return err
	}

	return checkAllowEmptyList(n, items, ctx.where)
}

// checkAllowEmptyList rejects allowEmptyList: true anywhere but on a leaf whose decoder rejects lists:
// one of a binding: open item whose formats are declared and exclude L.
// A fixed item's decoder reads one format, never a list;
// an item whose formats include L, or that declares none, already decodes any list.
// The leaf may sit anywhere, a repeat's of included, since the setting belongs to its position.
func checkAllowEmptyList(n *StructureNode, items map[string]Item, where string) error {
	if !n.AllowEmptyList {
		return nil
	}

	if n.Kind() != "leaf" {
		return fmt.Errorf("%s: allowEmptyList is valid only on a leaf, not a list, repeat, packed group, opaque node, or group reference", where)
	}

	it, ok := items[n.Item]
	if !ok {
		return fmt.Errorf("%s: unknown item %q", where, n.Item)
	}

	if !rejectsList(it) {
		return fmt.Errorf("%s: allowEmptyList item %s must be binding: open with declared formats that exclude L, whose decoder rejects a list", where, n.Item)
	}

	return nil
}

// rejectsList reports whether a leaf of it decodes whatever item arrives except a list:
// it is binding: open, and its declared formats exclude L.
// An open item that declares no formats, such as SPR, accepts any item.
func rejectsList(it Item) bool {
	return it.Binding == BindingOpen && len(it.Formats) > 0 && !slices.Contains(it.Formats, "L")
}

// checkAs rejects an as: name on a list, repeat, or packed node,
// and one that is not a letter followed by letters and digits.
func checkAs(n *StructureNode, where string) error {
	if n.As == "" {
		return nil
	}

	if n.Kind() == "list" {
		return fmt.Errorf("%s: as %q is valid only on a leaf, opaque node, or group reference, not a list, repeat, or packed group", where, n.As)
	}

	if !asNameRE.MatchString(n.As) {
		return fmt.Errorf("%s: as %q must be a letter followed by letters and digits", where, n.As)
	}

	return nil
}

// checkOptional rejects optional: true anywhere but on a group reference
// and on a leaf whose zero-length item a nil pointer can stand for.
//
// The leaf's item must be binding: fixed with a scalar goType other than an ASCII string:
// an open item already decodes as whatever item arrives, zero-length included,
// a byte-slice or byte-array goType has its own length rules,
// and an empty string already sends and decodes as the zero-length ASCII item.
// It must also sit outside any repeat's or packed group's of,
// whose elements are values of the item;
// inside a group it is a pointer field like any other optional leaf.
func checkOptional(n *StructureNode, items map[string]Item, ctx nodeCtx) error {
	if !n.Optional || n.Kind() == "group" {
		return nil
	}

	s := ctx.where

	if n.Kind() != "leaf" {
		return fmt.Errorf("%s: optional is valid only on a leaf or a group reference, not a list, repeat, packed group, or opaque node", s)
	}

	if ctx.inOf {
		return fmt.Errorf("%s: optional item %s is inside a repeat's or packed group's of, whose elements are values", s, n.Item)
	}

	it, ok := items[n.Item]
	if !ok {
		return fmt.Errorf("%s: unknown item %q", s, n.Item)
	}

	if err := optionalLeafError(n.Item, it); err != nil {
		return fmt.Errorf("%s: %w", s, err)
	}

	return nil
}

// optionalLeafError reports why a leaf of the item it, named name, cannot be optional, or nil when it can.
// Validation and the decoder walk share it, so the walk refuses what validation would.
func optionalLeafError(name string, it Item) error {
	switch {
	case it.Binding != BindingFixed:
		return fmt.Errorf("optional item %s must be binding: fixed (an open item already decodes when sent zero-length)", name)
	case isByteArrayGoType(it.GoType):
		return fmt.Errorf("optional item %s must have a scalar goType, not %s", name, it.GoType)
	case firstFormat(it) == "A":
		return fmt.Errorf("optional item %s is ASCII: an empty string already sends the zero-length item", name)
	default:
		return nil
	}
}

// checkNames rejects a body whose emitted names would not compile.
//
// The builder parameters (the [BuildParams] walk, which stops at a repeat's or packed group's of)
// must be pairwise distinct ignoring case,
// since each one also names an upper-cased decoder field.
// Each must be a Go identifier,
// which a repeat:/packed: name, used verbatim, need not be,
// and none may be the blank identifier, a Go keyword, a predeclared identifier,
// or secs2, the package the builder body references.
// The decoder fields (the [BuildFields] walk) must be pairwise distinct ignoring case,
// and each must be exported, which a name starting with an underscore is not even upper-cased,
// so the caller's package can read it.
// Neither set can collide with a generated local:
// a builder declares none, and a decoder reaches its fields only through out.
// Items inside separate repeat of subtrees are never emitted,
// so they are not checked.
// It expects a structure walkNode has already accepted.
func checkNames(n *StructureNode, items map[string]Item) error {
	if n == nil {
		return nil
	}

	seen := map[string]string{}
	for _, p := range BuildParams(n, items) {
		switch {
		case p.Name == "_":
			return fmt.Errorf("parameter %q is the blank identifier", p.Name)
		case token.IsKeyword(p.Name):
			return fmt.Errorf("parameter %q is a Go keyword (rename it with as)", p.Name)
		case types.Universe.Lookup(p.Name) != nil:
			return fmt.Errorf("parameter %q is a predeclared Go identifier (rename it with as)", p.Name)
		case p.Name == "secs2":
			return fmt.Errorf("parameter %q shadows the secs2 package (rename it with as)", p.Name)
		case !token.IsIdentifier(p.Name):
			return fmt.Errorf("parameter %q is not a Go identifier", p.Name)
		}

		key := strings.ToLower(p.Name)
		if prev, ok := seen[key]; ok {
			return fmt.Errorf("parameters %q and %q collide (rename one with as)", prev, p.Name)
		}
		seen[key] = p.Name
	}

	fields, err := BuildFields(n, items)
	if err != nil {
		// A body the decoder generator cannot handle has no fields to check;
		// rendering its decoder reports that error.
		return nil //nolint:nilerr // reported by renderDecoders, not by name checking
	}

	// Distinct parameters already give distinct fields,
	// so this check guards against the decoder walk drifting from the builder walk.
	// It ignores case, like the parameter check.
	seenField := map[string]string{}
	for _, f := range fields {
		if !token.IsExported(f.Name) {
			return fmt.Errorf("decoder field %q is not exported (start the name with a letter)", f.Name)
		}

		key := strings.ToUpper(f.Name)
		if prev, ok := seenField[key]; ok {
			return fmt.Errorf("decoder fields %q and %q collide (rename one with as)", prev, f.Name)
		}
		seenField[key] = f.Name
	}

	return nil
}
