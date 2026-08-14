package secs2

import (
	"errors"
	"fmt"
	"slices"
)

// Cursor is a small value type that provides typed, path-based access into a nested [Item] tree,
// replacing the Get / type-assert / ToXxx / index dance with a single chained call:
//
//	v, err := secs2.NewCursor(item).At(1, 0).ASCII()
//
// A Cursor is created with [NewCursor].
// Copying it is free because it is a plain value with no pointer to mutable state.
// Navigation and non-copying terminal accessors allocate nothing on the happy path;
// [Cursor.Binary] allocates its documented caller-owned copy.
//
// Navigation with [Cursor.At] accumulates errors instead of failing immediately: once a hop
// fails, every later [Cursor.At] call on the resulting Cursor is a no-op, and the terminal
// accessor ([Cursor.Uint], [Cursor.ASCII], and so on) returns that first error.
// This turns a multi-hop extraction into one error check instead of one per hop.
type Cursor struct {
	item  Item
	err   error
	index int // index of the last successful At hop; meaningless while depth == 0
	depth int // number of successful At hops applied so far
}

// NewCursor returns a Cursor positioned at item.
//
// A nil item yields a Cursor whose accessors all return a descriptive error; it never panics.
func NewCursor(item Item) Cursor {
	if isNilItem(item) {
		return Cursor{err: errors.New("secs2: cursor: item is nil")}
	}

	return Cursor{item: item}
}

// fault reports why the cursor cannot be read, or nil when it can.
//
// It folds the two conditions every terminal accessor must reject:
// an error already accumulated by [NewCursor] or by a failed [Cursor.At] hop,
// and the zero-value Cursor.
//
// Cursor is an exported struct, so a consumer reaches its zero value without calling NewCursor —
// through a struct field left unset, a var assigned on only one branch, or a map lookup that misses.
// That value carries a nil item and a nil error,
// and without this guard the accessors dereference the nil item while formatting a mismatch error.
func (c Cursor) fault() error {
	if c.err != nil {
		return c.err
	}

	if isNilItem(c.item) {
		return errors.New("secs2: cursor: zero-value Cursor; construct one with NewCursor")
	}

	return nil
}

// hopTypeErr reports that [Cursor.At] tried to navigate a list index into an item that is not a
// list. idx and depth describe the hop that failed: idx is the index being navigated, depth is
// the number of hops that succeeded before it.
func hopTypeErr(idx, depth int, got string) error {
	return fmt.Errorf("secs2: cursor at index %d (depth %d): got %s, want %s", idx, depth, safeTypeName(got), ListType)
}

// safeTypeName returns t, or "invalid" when t is empty.
//
// [FormatCode.String] and the numeric Type methods return "" for an item whose declared byte
// width has no valid format code (for example a UintItem built with an invalid byteSize) — a
// blank got type in a cursor error would read as a bug rather than as a description of the item.
func safeTypeName(t string) string {
	if t == "" {
		return "invalid"
	}

	return t
}

// At navigates indices as a path of nested list hops and returns the resulting Cursor.
//
// Called with no indices it returns c unchanged.
// If c already carries an error, At is a no-op that returns c unchanged — the error from the
// earliest failing hop is preserved, not overwritten by a later one.
// A hop fails when the current item is not a list, or when the index is out of range for it, or
// when the list itself carries a deferred construction error; any of these is recorded on the
// returned Cursor and surfaces from the next accessor call.
func (c Cursor) At(indices ...int) Cursor {
	if c.err != nil {
		return c
	}

	cur := c.item
	depth := c.depth
	index := c.index

	for _, idx := range indices {
		// Not redundant: NewCursor validates its item and the hop below validates each next,
		// but Cursor is an exported struct, so a consumer's zero value arrives here with a nil item and no error.
		// Removing this guard makes Cursor{}.At(0) panic.
		if isNilItem(cur) {
			return Cursor{err: fmt.Errorf("secs2: cursor at index %d (depth %d): item is nil", idx, depth)}
		}

		if !cur.IsList() {
			return Cursor{err: hopTypeErr(idx, depth, cur.Type())}
		}

		next, err := cur.ItemAt(idx)
		if err != nil {
			return Cursor{err: fmt.Errorf("secs2: cursor at index %d (depth %d): %w", idx, depth, err)}
		}
		if isNilItem(next) {
			return Cursor{err: fmt.Errorf("secs2: cursor at index %d (depth %d): item is nil", idx, depth)}
		}

		cur = next
		index = idx
		depth++
	}

	return Cursor{item: cur, index: index, depth: depth}
}

// Uint returns the item at the cursor as a uint64, widened from its declared byte width.
//
// The item must be a UintItem of any width (U1, U2, U4, or U8) and must hold exactly one value:
// it reads the item's scalar storage directly, so unlike [UintItem.ToUint] it never clones a
// slice.
// Returns an error if the cursor already carries one, the item is not a UintItem, the item
// carries a deferred construction error, or its size is not 1.
func (c Cursor) Uint() (uint64, error) {
	if err := c.fault(); err != nil {
		return 0, err
	}

	item, ok := c.item.(*UintItem)
	if !ok {
		return 0, c.mismatchErr(c.item.Type(), "uint")
	}

	if item.itemErr != nil {
		return 0, item.itemErr
	}

	if size := item.Size(); size != 1 {
		return 0, c.sizeErr(size)
	}

	return item.scalar, nil
}

// Int returns the item at the cursor as an int64, widened from its declared byte width.
//
// The item must be an IntItem of any width (I1, I2, I4, or I8) and must hold exactly one value:
// it reads the item's scalar storage directly, so unlike [IntItem.ToInt] it never clones a
// slice.
// Returns an error if the cursor already carries one, the item is not an IntItem, the item
// carries a deferred construction error, or its size is not 1.
func (c Cursor) Int() (int64, error) {
	if err := c.fault(); err != nil {
		return 0, err
	}

	item, ok := c.item.(*IntItem)
	if !ok {
		return 0, c.mismatchErr(c.item.Type(), "int")
	}

	if item.itemErr != nil {
		return 0, item.itemErr
	}

	if size := item.Size(); size != 1 {
		return 0, c.sizeErr(size)
	}

	return item.scalar, nil
}

// Float returns the item at the cursor as a float64, widened from its declared byte width.
//
// The item must be a FloatItem, F4 or F8, and must hold exactly one value: it reads the item's
// scalar storage directly, so unlike [FloatItem.ToFloat] it never clones a slice.
// Returns an error if the cursor already carries one, the item is not a FloatItem, the item
// carries a deferred construction error, or its size is not 1.
func (c Cursor) Float() (float64, error) {
	if err := c.fault(); err != nil {
		return 0, err
	}

	item, ok := c.item.(*FloatItem)
	if !ok {
		return 0, c.mismatchErr(c.item.Type(), "float")
	}

	if item.itemErr != nil {
		return 0, item.itemErr
	}

	if size := item.Size(); size != 1 {
		return 0, c.sizeErr(size)
	}

	return item.scalar, nil
}

// Bool returns the item at the cursor as a bool.
//
// The item must be a BooleanItem and must hold exactly one value: it reads the item's scalar
// storage directly, so unlike [BooleanItem.ToBoolean] it never clones a slice.
// Returns an error if the cursor already carries one, the item is not a BooleanItem, the item
// carries a deferred construction error, or its size is not 1.
func (c Cursor) Bool() (bool, error) {
	if err := c.fault(); err != nil {
		return false, err
	}

	item, ok := c.item.(*BooleanItem)
	if !ok {
		return false, c.mismatchErr(c.item.Type(), "boolean")
	}

	if item.itemErr != nil {
		return false, item.itemErr
	}

	if size := item.Size(); size != 1 {
		return false, c.sizeErr(size)
	}

	return item.scalar, nil
}

// ASCII returns the item at the cursor as a string.
//
// The item must be an ASCIIItem.
// There is no size-1 requirement: [ASCIIItem.Size] reports the string's byte length, not an
// element count, and the whole string is returned regardless of its length.
// Returns an error if the cursor already carries one, the item is not an ASCIIItem, or the item
// carries a deferred construction error.
func (c Cursor) ASCII() (string, error) {
	if err := c.fault(); err != nil {
		return "", err
	}

	item, ok := c.item.(*ASCIIItem)
	if !ok {
		return "", c.mismatchErr(c.item.Type(), "ascii")
	}

	if item.itemErr != nil {
		return "", item.itemErr
	}

	return item.value, nil
}

// Binary returns the item at the cursor as a fresh copy of its bytes.
//
// The item must be a BinaryItem.
// There is no size-1 requirement, matching [BinaryItem.ToBinary]: the result is always a clone,
// so mutating it never affects the item.
// Returns an error if the cursor already carries one, the item is not a BinaryItem, or the item
// carries a deferred construction error.
func (c Cursor) Binary() ([]byte, error) {
	if err := c.fault(); err != nil {
		return nil, err
	}

	item, ok := c.item.(*BinaryItem)
	if !ok {
		return nil, c.mismatchErr(c.item.Type(), "binary")
	}

	if item.itemErr != nil {
		return nil, item.itemErr
	}

	return slices.Clone(item.values), nil
}

// Size returns the [Item.Size] of the item at the cursor.
//
// It carries forward any error already on the cursor, and otherwise fails only on a zero-value
// Cursor: like Item.Size, it does not consult the item's deferred construction error.
func (c Cursor) Size() (int, error) {
	if err := c.fault(); err != nil {
		return 0, err
	}

	return c.item.Size(), nil
}

// Item unwraps the cursor and returns the [Item] at its current position.
//
// It carries forward any error already on the cursor, including the zero-value Cursor's.
// It does not itself consult the returned item's deferred construction error —
// call [Item.Error] on the result to check that.
func (c Cursor) Item() (Item, error) {
	if err := c.fault(); err != nil {
		return nil, err
	}

	return c.item, nil
}

// Err returns the first error encountered while navigating or reading through the cursor, or nil
// if none occurred.
func (c Cursor) Err() error {
	return c.err
}

// mismatchErr reports that the item at the cursor is not the type a terminal accessor required.
//
// At depth 0 the cursor has never completed an At hop, so no hop index is meaningful and the
// message names the root; otherwise it names the last successful hop's index and depth.
func (c Cursor) mismatchErr(got, want string) error {
	got = safeTypeName(got)

	if c.depth == 0 {
		return fmt.Errorf("secs2: cursor at root: got %s, want %s", got, want)
	}

	return fmt.Errorf("secs2: cursor at index %d (depth %d): got %s, want %s", c.index, c.depth, got, want)
}

// sizeErr reports that the item at the cursor does not hold exactly one value, using the same
// root-vs-hop framing as mismatchErr.
func (c Cursor) sizeErr(size int) error {
	if c.depth == 0 {
		return fmt.Errorf("secs2: cursor at root: size %d, want size 1", size)
	}

	return fmt.Errorf("secs2: cursor at index %d (depth %d): size %d, want size 1", c.index, c.depth, size)
}
