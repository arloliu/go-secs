package main

import (
	"errors"
	"fmt"
	"math"
	"regexp"
	"strconv"

	"gopkg.in/yaml.v3"
)

// Binding controls the generated parameter type for an item.
type Binding string

// Binding values.
const (
	BindingFixed Binding = "fixed"
	BindingOpen  Binding = "open"
)

// Item is one entry of the Data Item Dictionary (E5 Table 3).
//
// Note is one sentence for the godoc of the item's values table,
// such as a rule another standard adds for those values;
// it is valid only on an item with a values table.
// NamedType gives a fixed item without a values table a named type of its own,
// which its parameters and decoder fields use as they would an enumeration's,
// with no constants: a code E5 defines no values for.
type Item struct {
	Formats     []string    `yaml:"formats"`
	Binding     Binding     `yaml:"binding"`
	GoType      string      `yaml:"goType"`
	Description string      `yaml:"description"`
	Source      string      `yaml:"source"`
	Note        string      `yaml:"note"`
	NamedType   bool        `yaml:"namedType"`
	Values      []ItemValue `yaml:"values"`
}

// ItemValue is one named enumerated value for an item with a `values:` table.
//
// Source names the standard that defines the value, e.g. e87.1,
// when that differs from the item's own source.
// Note is one sentence for the godoc of the value's constant,
// such as another standard's spelling of the value.
type ItemValue struct {
	Name   string    `yaml:"name"`
	Value  EnumValue `yaml:"value"`
	Source string    `yaml:"source"`
	Note   string    `yaml:"note"`
}

// enumKind tells which representation an [EnumValue] holds.
type enumKind int

// enumKind values.
// The zero value means the YAML value was missing or null.
const (
	enumUnset enumKind = iota
	enumInt
	enumUint
	enumString
)

// EnumValue is the scalar value of one enumerated item value:
// an integer or a string.
//
// An integer that fits int64 is held signed;
// one above math.MaxInt64 is held unsigned.
// A quoted YAML scalar is a string even when it reads as digits, e.g. "10".
type EnumValue struct {
	kind enumKind
	i    int64
	u    uint64
	s    string
}

// MessageFile is one stream's YAML file (e.g. messages/s1.yaml).
type MessageFile struct {
	Stream   int       `yaml:"stream"`
	Messages []Message `yaml:"messages"`
}

// Message is one stream/function definition.
type Message struct {
	Function    int    `yaml:"function"`
	Name        string `yaml:"name"`
	Mnemonic    string `yaml:"mnemonic"`
	Direction   string `yaml:"direction"`
	Description string `yaml:"description"`
	Exception   string `yaml:"exception"`
	Source      string `yaml:"source"`
	Confidence  string `yaml:"confidence"`
	Bodies      []Body `yaml:"bodies"`
}

// Body is one wire shape of a message.
//
// A message has one main body per actor.
// A body that sets Variant is instead a named short form of the main body for its actor,
// for a form SEMI E5 allows the sender to omit part of:
// it gets its own builder, SxFy[Host]<Variant>, but no decoder,
// since the main body's decoder already accepts it.
type Body struct {
	Actor         string         `yaml:"actor"`
	ReplyExpected bool           `yaml:"replyExpected"`
	Variant       string         `yaml:"variant"`
	Structure     *StructureNode `yaml:"structure"`
}

// GroupFile is the groups file, groups.yaml:
// the named list shapes message bodies repeat.
type GroupFile struct {
	Groups []Group `yaml:"groups"`
}

// Group is one named list shape, generated as a Go struct of the same name.
//
// Structure is always a fixed list.
// A repeat's of names a group to repeat that shape,
// and a fixed list inside another group may hold one as an optional child.
type Group struct {
	Name        string         `yaml:"name"`
	Description string         `yaml:"description"`
	Structure   *StructureNode `yaml:"structure"`

	// users names what references the group, for its godoc:
	// the messages, as SxFy, and the other groups, as doc links.
	users []string
}

// StructureNode is one node of a message body tree.
//
// A leaf sets Item. A list sets Type "list" with either Items, Repeat+Of, or
// Packed+Of. An opaque body sets Type "opaque". A nil *StructureNode is a
// header-only body.
// A group reference sets Group, alone or with As and Optional.
type StructureNode struct {
	Type  string          `yaml:"type"`
	Item  string          `yaml:"item"`
	Items []StructureNode `yaml:"items"`
	// Repeat names a list of separate items, each of the shape Of declares.
	// When Of is a binding: fixed leaf with a scalar goType,
	// the builder takes the elements as values of that type and the decoder returns them so;
	// a group element is a value of the group's struct, a repeat element a slice,
	// and any other element is taken and returned as a secs2.Item.
	Repeat string `yaml:"repeat"`
	// Packed marks E5's packed multi-value single item (e.g. S1F10's
	// <tsip1,,tsipn>): all values share one item header and one format, unlike
	// Repeat's list of separate items. Of.Item must be a binding: fixed item.
	Packed   string         `yaml:"packed"`
	Of       *StructureNode `yaml:"of"`
	MinItems *int           `yaml:"minItems"`
	MaxItems *int           `yaml:"maxItems"`
	// As renames a leaf or opaque node:
	// its builder parameter becomes lower(As) and its decoder field upper(As).
	// It is how one body carries the same item twice, or two opaque nodes.
	As string `yaml:"as"`
	// Optional marks a leaf SEMI E5 lets the sender transmit as a zero-length item.
	// Its builder parameter, decoder field, and group field are then pointers:
	// nil sends and decodes as the zero-length item of the leaf's format.
	// It is valid only on a binding: fixed leaf with a scalar goType other than an ASCII string,
	// outside any repeat's or packed group's of; a group's leaf may be optional.
	// On a group reference it marks a list SEMI E5 lets be sent as L[0] instead:
	// the field or element is then a pointer, nil for L[0].
	Optional bool `yaml:"optional"`
	// AllowEmptyList marks a leaf where SEMI E5 gives the zero-length list L[0] a meaning,
	// although its item's formats exclude L, as S2F14 does for an equipment constant value.
	// Its decoder then accepts L[0] as well as any non-list item; a list with children is still an error.
	// It is valid only on a leaf of a binding: open item whose formats are declared and exclude L,
	// whose decoder otherwise rejects every list.
	AllowEmptyList bool `yaml:"allowEmptyList"`
	// Group names the group of groups.yaml this node references.
	Group string `yaml:"group"`

	// def is the group Group names, set by [ResolveGroups].
	def *Group
}

// leadingZeroRE matches a decimal number written with a leading zero, e.g. 010 or -08.
// YAML reads 010 as octal 8 and 08 as a float,
// so neither means what a reader of the table expects.
var leadingZeroRE = regexp.MustCompile(`^[-+]?0[0-9]`)

// IntValue returns the integer enumerated value v.
func IntValue(v int64) EnumValue {
	return EnumValue{kind: enumInt, i: v}
}

// UintValue returns the integer enumerated value v,
// held signed when it fits int64, as [EnumValue.UnmarshalYAML] would.
func UintValue(v uint64) EnumValue {
	if v <= math.MaxInt64 {
		return IntValue(int64(v))
	}

	return EnumValue{kind: enumUint, u: v}
}

// StringValue returns the string enumerated value v.
func StringValue(v string) EnumValue {
	return EnumValue{kind: enumString, s: v}
}

// Kind returns "leaf", "opaque", "group", or "list" for the node.
func (n *StructureNode) Kind() string {
	switch {
	case n.Item != "":
		return "leaf"
	case n.Type == "opaque":
		return "opaque"
	case n.Group != "":
		return "group"
	default:
		return "list"
	}
}

// UnmarshalYAML reads a value: scalar.
// A !!int scalar becomes an integer and a !!str scalar a string;
// any other tag, a mapping, or a sequence is an error.
// An unquoted number with a leading zero is an error too,
// whichever tag YAML gives it;
// 0x, 0o, and 0b prefixes still select hexadecimal, octal, and binary.
//
// yaml.v3 does not call it for a null node,
// so a null or missing value leaves the EnumValue unset,
// which ValidateItems then rejects.
func (v *EnumValue) UnmarshalYAML(n *yaml.Node) error {
	if n.Kind != yaml.ScalarNode {
		return fmt.Errorf("line %d: value must be an integer or a string scalar", n.Line)
	}

	if n.ShortTag() != "!!str" && leadingZeroRE.MatchString(n.Value) {
		return fmt.Errorf("line %d: value %s has a leading zero; write a decimal without it, or use a 0o prefix for octal", n.Line, n.Value)
	}

	switch n.ShortTag() {
	case "!!int":
		i, err := strconv.ParseInt(n.Value, 0, 64)
		if err == nil {
			*v = IntValue(i)
			return nil
		}

		if !errors.Is(err, strconv.ErrRange) {
			// YAML drops every underscore before it reads an integer, Go only single ones between digits,
			// so YAML may tag as !!int a literal such as 1__0 that Go rejects.
			return fmt.Errorf("line %d: value %s is not a valid integer literal", n.Line, n.Value)
		}

		u, err := strconv.ParseUint(n.Value, 0, 64)
		if err != nil {
			return fmt.Errorf("line %d: value %s does not fit a 64-bit integer", n.Line, n.Value)
		}

		*v = UintValue(u)

		return nil
	case "!!str":
		*v = StringValue(n.Value)

		return nil
	default:
		return fmt.Errorf("line %d: value %s is %s, not an integer or a string", n.Line, n.Value, n.ShortTag())
	}
}

// IsSet reports whether the value was given:
// false for a missing or null YAML value.
func (v EnumValue) IsSet() bool {
	return v.kind != enumUnset
}

// IsString reports whether the value is a string.
func (v EnumValue) IsString() bool {
	return v.kind == enumString
}

// Literal renders the value as a Go constant literal:
// a decimal integer, or a quoted string.
func (v EnumValue) Literal() string {
	switch v.kind {
	case enumInt:
		return strconv.FormatInt(v.i, 10)
	case enumUint:
		return strconv.FormatUint(v.u, 10)
	case enumString:
		return strconv.Quote(v.s)
	case enumUnset:
		return "<unset>"
	default:
		return "<unknown>"
	}
}

// IsNegative reports whether the value is an integer below zero.
func (v EnumValue) IsNegative() bool {
	return v.kind == enumInt && v.i < 0
}

// String renders the value for error messages.
func (v EnumValue) String() string {
	return v.Literal()
}

// fitsInteger reports whether the value is an integer within [lo, hi].
// hi is unsigned so the range of uint64 can be expressed.
func (v EnumValue) fitsInteger(lo int64, hi uint64) bool {
	if v.kind == enumUint {
		return v.u <= hi
	}

	return v.kind == enumInt && v.i >= lo && (v.i < 0 || uint64(v.i) <= hi)
}
