// Package secsitem walks the SECS-II message text of a captured data frame (SEMI E5 §9) for masking:
// it decides whether the text is one valid item per the item predicate of the tracepack semantics specification §3,
// resolves item path patterns over it, and locates the content bytes of its leaf items.
//
// It decodes no value and allocates no item tree: Screen validates the text once,
// and the other methods navigate the validated bytes.
// Like the classifier, it enforces a list nesting depth of MaxDepth;
// a deeper text cannot be screened, so a masking writer defers its record instead of guessing a mask.
package secsitem

import (
	"errors"
	"fmt"
	"strconv"
	"strings"
)

// MaxDepth is the deepest list nesting Screen walks, the limit the classifier of the module inherits from go-secs:
// a text whose lists nest deeper cannot be screened.
const MaxDepth = 64

// maxLength is the largest length three length bytes can encode: no list holds more elements.
const maxLength = 1<<24 - 1

// listCode is the format code of a list.
const listCode = 0o00

// localizedCode is the format code of a localized string, whose non-empty body starts with a 2-byte header.
const localizedCode = 0o22

// localizedHeaderLen is the length of a localized string's header, which names its encoding.
const localizedHeaderLen = 2

var (
	// ErrDepth reports a text whose lists nest deeper than MaxDepth before it is known not to be one valid item:
	// the text cannot be screened.
	ErrDepth = errors.New("secsitem: list nesting deeper than 64 cannot be screened")
	// ErrPattern reports a malformed item path pattern.
	ErrPattern = errors.New("secsitem: malformed item path pattern")
)

// errInvalid stops a walk at the first byte that keeps the text from being one valid item.
var errInvalid = errors.New("secsitem: not one valid item")

// elementSize holds the element size of every format code SEMI E5 §9.2 defines but the list.
var elementSize = map[byte]int{
	0o10: 1, 0o11: 1, 0o20: 1, 0o21: 1, 0o22: 1,
	0o30: 8, 0o31: 1, 0o32: 2, 0o34: 4,
	0o40: 8, 0o44: 4,
	0o50: 8, 0o51: 1, 0o52: 2, 0o54: 4,
}

// Text is a screened message text.
type Text struct {
	b       []byte
	one     bool
	content bool
}

// Range is the half-open byte range [Start, End) of a message text.
type Range struct {
	Start, End int
}

// Selection is one item a pattern selects.
type Selection struct {
	// Path is the item's concrete path: 1-based element indexes separated by "/", empty for the root item.
	Path string
	// Item is the encoded item, its header included.
	Item Range
}

// Step is one step of an item path pattern.
type Step struct {
	// Index is the 1-based element index of a numbered step; 0 for a * step.
	Index int
}

// Pattern is an item path pattern: steps applied from the root item, empty for the root item itself.
type Pattern []Step

// header is the decoded header of the item at an offset of a text.
type header struct {
	code   byte
	length int
	body   int
}

// Screen decides whether text is one valid item:
// exactly one item or list, valid per the item predicate of the tracepack semantics specification §3
// (a defined format code, 1 to 3 length bytes, a body length that is a whole number of elements, every list element present,
// a localized string of length 0 or of 2 or more), ending at the last byte of text.
// An empty text is not one valid item.
//
// Parameters:
//   - text: the message text, the captured bytes after the 4-byte length and the 10-byte HSMS header;
//     Screen keeps it, so the caller must not change it while it uses the result.
//
// Returns:
//   - Text: the screened text.
//   - error: an error wrapping ErrDepth when the walk meets a list nested deeper than MaxDepth
//     before it finds a byte that keeps the text from being one valid item.
func Screen(text []byte) (Text, error) {
	t := Text{b: text}
	if len(text) == 0 {
		return t, nil
	}

	end, err := t.validate(0, 0)
	switch {
	case errors.Is(err, ErrDepth):
		return Text{}, err
	case err != nil:
		return Text{b: text}, nil
	}
	t.one = end == len(text)

	return t, nil
}

// ParsePattern parses an item path pattern:
// steps separated by "/", each "*" or a 1-based element index written in decimal without a leading zero;
// the empty string is the root item.
//
// Returns:
//   - Pattern: the parsed pattern.
//   - error: an error wrapping ErrPattern for an empty step, an index of 0, above 2^24 - 1 or with a leading zero,
//     or any other character.
func ParsePattern(s string) (Pattern, error) {
	if s == "" {
		return Pattern{}, nil
	}

	parts := strings.Split(s, "/")
	p := make(Pattern, 0, len(parts))
	for _, part := range parts {
		if part == "*" {
			p = append(p, Step{})
			continue
		}
		if part == "" || part[0] == '0' || strings.TrimLeft(part, "0123456789") != "" {
			return nil, fmt.Errorf("%w: step %q of %q", ErrPattern, part, s)
		}
		n, err := strconv.Atoi(part)
		if err != nil || n > maxLength {
			return nil, fmt.Errorf("%w: step %q of %q", ErrPattern, part, s)
		}
		p = append(p, Step{Index: n})
	}

	return p, nil
}

// OneItem reports whether the text is one valid item.
func (t *Text) OneItem() bool {
	return t.one
}

// HasContent reports whether a leaf item of the text holds a content byte; meaningful only when OneItem.
// A localized string's 2-byte header is no content byte.
func (t *Text) HasContent() bool {
	return t.content
}

// Select resolves p over the text, which must be one valid item.
// Each step applies to a list and fails on any other item; a numbered step also fails when the list has fewer elements;
// a * step expands to every element of the list, and to none when it is empty.
//
// Returns:
//   - []Selection: the items the branches of p reach, in text order, possibly none.
//   - bool: false when a step of any branch fails, so p does not resolve.
func (t *Text) Select(p Pattern) ([]Selection, bool) {
	if !t.one {
		return nil, false
	}

	var out []Selection
	ok := t.selectAt(0, p, "", &out)

	return out, ok
}

// Content appends to dst the content bytes of every leaf item inside r, the item of a Selection, in text order,
// adjacent ranges joined: the body of each leaf, but a localized string's 2-byte header; an empty body adds nothing.
//
// Returns:
//   - []Range: dst with the ranges appended.
func (t *Text) Content(r Range, dst []Range) []Range {
	t.appendContent(r.Start, &dst)
	return dst
}

// String returns the pattern as ParsePattern reads it.
func (p Pattern) String() string {
	var sb strings.Builder
	for i, s := range p {
		if i > 0 {
			sb.WriteByte('/')
		}
		if s.Index == 0 {
			sb.WriteByte('*')
		} else {
			sb.WriteString(strconv.Itoa(s.Index))
		}
	}

	return sb.String()
}

// Overlaps reports whether p and q can select overlapping items of one text:
// every step they share is equal or a * on either side,
// so one selects an item that the other selects, or an ancestor or descendant of it.
// The empty pattern, the root item, overlaps every pattern.
func (p Pattern) Overlaps(q Pattern) bool {
	for i := range min(len(p), len(q)) {
		if p[i].Index != 0 && q[i].Index != 0 && p[i].Index != q[i].Index {
			return false
		}
	}

	return true
}

// readHeader decodes the item header at off of a text already known to hold a whole header there.
func (t *Text) readHeader(off int) header {
	n := int(t.b[off] & 0x03)
	length := 0
	for _, c := range t.b[off+1 : off+1+n] {
		length = length<<8 | int(c)
	}

	return header{code: t.b[off] >> 2, length: length, body: off + 1 + n}
}

// validate checks the item at off, inside depth enclosing lists, and returns its end offset;
// it marks the text as holding content when a leaf holds a content byte.
//
// Returns:
//   - int: the end offset of the item.
//   - error: errInvalid when the item is not valid, or an error wrapping ErrDepth.
func (t *Text) validate(off, depth int) (int, error) {
	if off >= len(t.b) {
		return 0, errInvalid
	}
	n := int(t.b[off] & 0x03)
	if n == 0 || off+1+n > len(t.b) {
		return 0, errInvalid
	}
	h := t.readHeader(off)
	if h.code == listCode {
		return t.validateList(h, depth)
	}

	size, known := elementSize[h.code]
	switch {
	case !known, h.length%size != 0, h.code == localizedCode && h.length == 1, h.length > len(t.b)-h.body:
		return 0, errInvalid
	case h.code == localizedCode && h.length > localizedHeaderLen, h.code != localizedCode && h.length > 0:
		t.content = true
	default:
		// A valid leaf without a content byte: an empty body, or a localized string's header alone.
	}

	return h.body + h.length, nil
}

// validateList checks the elements of the list h, inside depth enclosing lists, and returns the list's end offset.
func (t *Text) validateList(h header, depth int) (int, error) {
	if depth+1 > MaxDepth {
		return 0, fmt.Errorf("%w: a list at depth %d", ErrDepth, depth+1)
	}
	// Every element takes at least two bytes, its format byte and one length byte.
	if h.length > (len(t.b)-h.body)/2 {
		return 0, errInvalid
	}

	off := h.body
	for range h.length {
		end, err := t.validate(off, depth+1)
		if err != nil {
			return 0, err
		}
		off = end
	}

	return off, nil
}

// end returns the end offset of the valid item at off.
func (t *Text) end(off int) int {
	h := t.readHeader(off)
	if h.code != listCode {
		return h.body + h.length
	}

	off = h.body
	for range h.length {
		off = t.end(off)
	}

	return off
}

// selectAt resolves p over the valid item at off, whose concrete path is path, and appends its selections to out.
func (t *Text) selectAt(off int, p Pattern, path string, out *[]Selection) bool {
	if len(p) == 0 {
		*out = append(*out, Selection{Path: path, Item: Range{Start: off, End: t.end(off)}})
		return true
	}

	h := t.readHeader(off)
	if h.code != listCode {
		return false
	}
	step := p[0]
	if step.Index > h.length {
		return false
	}

	child := h.body
	for k := 1; k <= h.length; k++ {
		if step.Index == 0 || step.Index == k {
			if !t.selectAt(child, p[1:], childPath(path, k), out) {
				return false
			}
			if step.Index == k {
				return true
			}
		}
		child = t.end(child)
	}

	return true
}

// appendContent appends the content ranges of the leaves of the valid item at off to dst, joining adjacent ranges.
func (t *Text) appendContent(off int, dst *[]Range) {
	h := t.readHeader(off)
	if h.code == listCode {
		child := h.body
		for range h.length {
			t.appendContent(child, dst)
			child = t.end(child)
		}

		return
	}

	start := h.body
	if h.code == localizedCode && h.length > 0 {
		start += localizedHeaderLen
	}
	end := h.body + h.length
	if start >= end {
		return
	}
	if n := len(*dst); n > 0 && (*dst)[n-1].End == start {
		(*dst)[n-1].End = end
		return
	}
	*dst = append(*dst, Range{Start: start, End: end})
}

// childPath returns the concrete path of element k of the list at path.
func childPath(path string, k int) string {
	if path == "" {
		return strconv.Itoa(k)
	}

	return path + "/" + strconv.Itoa(k)
}
