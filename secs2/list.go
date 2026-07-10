package secs2

import (
	"errors"
	"fmt"
	"iter"
	"slices"
	"strings"
)

// ListItem represents an immutable recursive list container in a SECS-II message (SEMI E5 §9.2).
//
// It implements the Item interface. All methods are safe for concurrent use; no method
// exposes mutable internal storage. Children are shared by reference — this is safe because
// every Item implementation in this package is deeply immutable after construction.
type ListItem struct {
	baseItem
	values []Item
	// clean caches whether every child (recursively) is a known-clean built-in item with no
	// deferred error, so Error() can return nil in O(1) instead of re-walking the tree. It is
	// computed once in NewListItem (and set directly by the decoder, which never produces an
	// errored tree) and is never recomputed, matching the immutable-after-construction contract
	// of all built-in items.
	clean bool
}

var _ Item = (*ListItem)(nil)

// NewListItem creates a new ListItem containing the given child items.
//
// nil children are silently skipped. If the total
// child count exceeds MaxByteSize, a deferred error is stored on the returned
// item; call Error() to inspect it.
//
// Parameters:
//   - values: the child items to include in the list.
//
// Returns:
//   - Item: the created ListItem.
func NewListItem(values ...Item) Item {
	item := &ListItem{}

	dataBytes, _ := getDataByteLength(ListType, len(values))
	if dataBytes > MaxByteSize {
		item.setErrorMsg("item size limit exceeded")

		return item
	}

	item.values = make([]Item, 0, len(values))

	clean := true

	for _, v := range values {
		if v == nil {
			continue
		}

		item.values = append(item.values, v)

		if !childClean(v) {
			clean = false
		}
	}

	item.clean = clean

	return item
}

// childClean reports whether v is a known-clean built-in item: a non-nil built-in pointer
// carrying no deferred error, and — for a *ListItem child — itself already known-clean.
//
// It never calls the public Error() method, so it cannot trigger an external implementation's
// side effects or observe a typed-nil built-in pointer's panic. A typed-nil built-in pointer or
// any dynamic type outside this package is reported not clean, which routes ListItem.Error()
// through its full recursive walk instead of the O(1) fast path.
func childClean(v Item) bool { //nolint:cyclop // one type-switch arm per built-in concrete type; irreducible enumeration, not accidental complexity
	switch t := v.(type) {
	case *IntItem:
		return t != nil && t.itemErr == nil
	case *UintItem:
		return t != nil && t.itemErr == nil
	case *FloatItem:
		return t != nil && t.itemErr == nil
	case *ASCIIItem:
		return t != nil && t.itemErr == nil
	case *JIS8Item:
		return t != nil && t.itemErr == nil
	case *BinaryItem:
		return t != nil && t.itemErr == nil
	case *BooleanItem:
		return t != nil && t.itemErr == nil
	case *LocalizedStrItem:
		return t != nil && t.itemErr == nil
	case *EmptyItem:
		return t != nil && t.itemErr == nil
	case *ListItem:
		return t != nil && t.itemErr == nil && t.clean
	default:
		return false
	}
}

// Get navigates a path of list indices and returns the item at that position.
// With no indices it returns the list itself. Returns an error if any intermediate
// item is not a list or if an index is out of range.
func (item *ListItem) Get(indices ...int) (Item, error) {
	if len(indices) == 0 {
		return item, nil
	}

	var cur Item = item

	for _, idx := range indices {
		if !cur.IsList() {
			return nil, errors.New("failed to get nested item")
		}

		li, _ := cur.(*ListItem)

		if idx < 0 || idx >= li.Size() {
			return nil, errors.New("failed to get nested item")
		}

		cur = li.values[idx]
	}

	return cur, nil
}

// ToList returns a fresh shallow clone of the child item slice. Children are immutable
// so shallow cloning is concurrency-safe. Returns an error if the item carries a
// deferred construction error.
//
// It returns the item's own deferred error (see Error) when the item was constructed with
// one — a passing Is* predicate does NOT imply a nil error here. Always check the returned
// error; do not discard it via `v, _ := item.ToList()`. Note that a nil error here does not
// mean every child is error-free: call Error() to check the item's own error joined with
// every child's deferred error.
func (item *ListItem) ToList() ([]Item, error) {
	if item.itemErr != nil {
		return nil, item.itemErr
	}

	return slices.Clone(item.values), nil
}

// ItemAt returns the child item at index i. Returns an error if the item carries a
// deferred error or i is out of range.
func (item *ListItem) ItemAt(i int) (Item, error) {
	if item.itemErr != nil {
		return nil, item.itemErr
	}

	if i < 0 || i >= len(item.values) {
		return nil, NewItemErrorWithMsg("index out of range")
	}

	return item.values[i], nil
}

// Items returns an iterator over the child items in order. Yields nothing if the
// item carries a deferred construction error.
func (item *ListItem) Items() iter.Seq[Item] {
	return func(yield func(Item) bool) {
		if item.itemErr != nil {
			return
		}

		for _, v := range item.values {
			if !yield(v) {
				return
			}
		}
	}
}

// Size returns the number of direct children (non-recursive).
func (item *ListItem) Size() int { return len(item.values) }

// Type returns "list".
func (item *ListItem) Type() string { return ListType }

// IsList returns true.
//
// It reflects the DECLARED type only and does not consult the item's deferred error or any
// child's deferred error: a true result does not imply the item (or its children) is usable.
// Gate value extraction on Error(), which aggregates child errors for lists, not on Is* alone.
func (item *ListItem) IsList() bool { return true }

// Error aggregates the item's own deferred error with each child's Error() via errors.Join.
// A list that contains an invalid child reports a non-nil Error.
func (item *ListItem) Error() error {
	if item.clean {
		return nil
	}

	var errs error

	if item.itemErr != nil {
		errs = errors.Join(errs, item.itemErr)
	}

	for _, v := range item.values {
		if v != nil {
			errs = errors.Join(errs, v.Error())
		}
	}

	return errs
}

// EncodedLen returns the total SECS-II wire byte length: the list header (whose data-length
// field encodes the child count) plus the sum of each child's EncodedLen. Returns 0 for
// items with a deferred construction error.
func (item *ListItem) EncodedLen() int {
	if item.itemErr != nil {
		return 0
	}

	if item.rawPtr != nil {
		return item.rawLen
	}

	n := headerLen(len(item.values))

	for _, child := range item.values {
		n += child.EncodedLen()
	}

	return n
}

// AppendTo appends the SECS-II wire encoding of this list (header + recursively encoded
// children) into dst and returns the extended slice. Returns dst unchanged for items with
// a deferred construction error.
func (item *ListItem) AppendTo(dst []byte) []byte {
	if item.itemErr != nil {
		return dst
	}

	if item.rawPtr != nil {
		return append(dst, item.raw()...)
	}

	dst, _ = appendHeaderBytes(dst, ListType, len(item.values)) //nolint:errcheck

	for _, child := range item.values {
		dst = child.AppendTo(dst)
	}

	return dst
}

// ToBytes allocates a single buffer and returns the SECS-II wire encoding.
// Equivalent to AppendTo(make([]byte, 0, EncodedLen())).
func (item *ListItem) ToBytes() []byte {
	return item.AppendTo(make([]byte, 0, item.EncodedLen()))
}

// ToSML returns the SML (SECS Message Language) text representation of this list,
// with nested items indented by two spaces per level.
func (item *ListItem) ToSML() string {
	return item.formatSML(0)
}

// formatSML returns the indented SML representation of this list at the given indent level.
// Each level adds 2 spaces of prefix to child lines. itemErr is intentionally not guarded
// here (template pattern): an error list with no children renders as <L[0]>.
func (item *ListItem) formatSML(level int) string {
	indentStr := strings.Repeat("  ", level)

	if item.Size() == 0 {
		return indentStr + "<L[0]>"
	}

	var sb strings.Builder

	sb.Grow(len(item.values) * 20) //nolint:mnd

	for _, value := range item.values {
		if v, ok := value.(*ListItem); ok {
			sb.WriteString(v.formatSML(level + 1))
			sb.WriteByte('\n')
		} else {
			sb.WriteString(indentStr)
			sb.WriteString("  ")
			sb.WriteString(value.ToSML())
			sb.WriteByte('\n')
		}
	}

	return fmt.Sprintf("%v<L[%d]\n%s%s>", indentStr, item.Size(), sb.String(), indentStr)
}
