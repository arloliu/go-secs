package main

// Binding controls the generated parameter type for an item.
type Binding string

// Binding values.
const (
	BindingFixed Binding = "fixed"
	BindingOpen  Binding = "open"
)

// Item is one entry of the Data Item Dictionary (E5 Table 3).
type Item struct {
	Formats     []string    `yaml:"formats"`
	Binding     Binding     `yaml:"binding"`
	GoType      string      `yaml:"goType"`
	Description string      `yaml:"description"`
	Source      string      `yaml:"source"`
	Values      []ItemValue `yaml:"values"`
}

// ItemValue is one named enumerated value for an item with a `values:` table.
type ItemValue struct {
	Name  string `yaml:"name"`
	Value int64  `yaml:"value"`
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

// Body is one wire shape of a message (one per distinct actor variant).
type Body struct {
	Actor         string         `yaml:"actor"`
	ReplyExpected bool           `yaml:"replyExpected"`
	Structure     *StructureNode `yaml:"structure"`
}

// StructureNode is one node of a message body tree.
//
// A leaf sets Item. A list sets Type "list" with either Items, Repeat+Of, or
// Packed+Of. An opaque body sets Type "opaque". A nil *StructureNode is a
// header-only body.
type StructureNode struct {
	Type   string          `yaml:"type"`
	Item   string          `yaml:"item"`
	Items  []StructureNode `yaml:"items"`
	Repeat string          `yaml:"repeat"`
	// Packed marks E5's packed multi-value single item (e.g. S1F10's
	// <tsip1,,tsipn>): all values share one item header and one format, unlike
	// Repeat's list of separate items. Of.Item must be a binding: fixed item.
	Packed   string         `yaml:"packed"`
	Of       *StructureNode `yaml:"of"`
	MinItems *int           `yaml:"minItems"`
	MaxItems *int           `yaml:"maxItems"`
}

// Kind returns "leaf", "opaque", or "list" for the node.
func (n *StructureNode) Kind() string {
	switch {
	case n.Item != "":
		return "leaf"
	case n.Type == "opaque":
		return "opaque"
	default:
		return "list"
	}
}
