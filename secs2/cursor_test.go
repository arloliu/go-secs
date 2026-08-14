package secs2

import (
	"testing"

	"github.com/stretchr/testify/require"
)

// cursorTree builds a small nested tree used across the happy-path tests:
//
//	L{
//	  L{U1[7], U2[7], U4[7], U8[7]},
//	  L{I1[-7], I2[-7], I4[-7], I8[-7]},
//	  L{F4[1.5], F8[1.5]},
//	  BOOLEAN[true],
//	  A["hi"],
//	  B[0x01, 0x02],
//	}
func cursorTree() Item {
	return L(
		L(U1(uint(7)), U2(uint(7)), U4(uint(7)), U8(uint(7))),
		L(I1(-7), I2(-7), I4(-7), I8(-7)),
		L(F4(1.5), F8(1.5)),
		BOOLEAN(true),
		A("hi"),
		B(byte(0x01), byte(0x02)),
	)
}

type externalNilItem struct {
	Item
}

var _ Item = (*externalNilItem)(nil)

func requireCursorAccessorsError(t *testing.T, c Cursor) {
	t.Helper()

	require.Error(t, c.Err())

	_, err := c.Uint()
	require.Error(t, err)

	_, err = c.Int()
	require.Error(t, err)

	_, err = c.Float()
	require.Error(t, err)

	_, err = c.Bool()
	require.Error(t, err)

	_, err = c.ASCII()
	require.Error(t, err)

	_, err = c.Binary()
	require.Error(t, err)

	_, err = c.Size()
	require.Error(t, err)

	_, err = c.Item()
	require.Error(t, err)
}

func TestNewCursor_NilItem(t *testing.T) {
	t.Parallel()

	c := NewCursor(nil)

	require.Error(t, c.Err())
	require.ErrorContains(t, c.Err(), "nil")

	_, err := c.Uint()
	require.ErrorIs(t, err, c.Err())

	_, err = c.Int()
	require.ErrorIs(t, err, c.Err())

	_, err = c.Float()
	require.ErrorIs(t, err, c.Err())

	_, err = c.Bool()
	require.ErrorIs(t, err, c.Err())

	_, err = c.ASCII()
	require.ErrorIs(t, err, c.Err())

	_, err = c.Binary()
	require.ErrorIs(t, err, c.Err())

	_, err = c.Size()
	require.ErrorIs(t, err, c.Err())

	_, err = c.Item()
	require.ErrorIs(t, err, c.Err())

	// At on an errored cursor is a no-op.
	c2 := c.At(0)
	require.ErrorIs(t, c2.Err(), c.Err())
}

func TestNewCursor_TypedNilBuiltins(t *testing.T) {
	t.Parallel()

	for _, tt := range builtinTypeChildren() {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()

			requireCursorAccessorsError(t, NewCursor(tt.nilv))
		})
	}
}

func TestNewCursor_ExternalTypedNil(t *testing.T) {
	t.Parallel()

	var item *externalNilItem
	requireCursorAccessorsError(t, NewCursor(item))
}

func TestCursor_HappyPathAllocs(t *testing.T) {
	root := L(L(U1(uint(7))))
	var value uint64
	var err error

	allocs := testing.AllocsPerRun(100, func() {
		value, err = NewCursor(root).At(0, 0).Uint()
	})

	require.NoError(t, err)
	require.Equal(t, uint64(7), value)
	require.Zero(t, allocs)
}

func TestCursor_Uint_Widths(t *testing.T) {
	t.Parallel()

	root := cursorTree()

	tests := []struct {
		name  string
		index int
	}{
		{"U1", 0},
		{"U2", 1},
		{"U4", 2},
		{"U8", 3},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()

			v, err := NewCursor(root).At(0, tt.index).Uint()
			require.NoError(t, err)
			require.Equal(t, uint64(7), v)
		})
	}
}

func TestCursor_Int_Widths(t *testing.T) {
	t.Parallel()

	root := cursorTree()

	tests := []struct {
		name  string
		index int
	}{
		{"I1", 0},
		{"I2", 1},
		{"I4", 2},
		{"I8", 3},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()

			v, err := NewCursor(root).At(1, tt.index).Int()
			require.NoError(t, err)
			require.Equal(t, int64(-7), v)
		})
	}
}

func TestCursor_Float_Widths(t *testing.T) {
	t.Parallel()

	root := cursorTree()

	tests := []struct {
		name  string
		index int
	}{
		{"F4", 0},
		{"F8", 1},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()

			v, err := NewCursor(root).At(2, tt.index).Float()
			require.NoError(t, err)
			require.InDelta(t, 1.5, v, 0)
		})
	}
}

func TestCursor_Bool(t *testing.T) {
	t.Parallel()

	root := cursorTree()

	v, err := NewCursor(root).At(3).Bool()
	require.NoError(t, err)
	require.True(t, v)
}

func TestCursor_ASCII(t *testing.T) {
	t.Parallel()

	root := cursorTree()

	v, err := NewCursor(root).At(4).ASCII()
	require.NoError(t, err)
	require.Equal(t, "hi", v)
}

func TestCursor_Binary(t *testing.T) {
	t.Parallel()

	root := cursorTree()

	v, err := NewCursor(root).At(5).Binary()
	require.NoError(t, err)
	require.Equal(t, []byte{0x01, 0x02}, v)
}

// TestCursor_Binary_ReturnsClone asserts that mutating the returned slice does not affect the
// item — the Binary accessor must clone, not alias.
func TestCursor_Binary_ReturnsClone(t *testing.T) {
	t.Parallel()

	item := B(byte(0x01), byte(0x02))

	v1, err := NewCursor(item).Binary()
	require.NoError(t, err)

	v1[0] = 0xff

	v2, err := NewCursor(item).Binary()
	require.NoError(t, err)
	require.Equal(t, []byte{0x01, 0x02}, v2, "mutating the first result must not affect the item")
}

func TestCursor_Size(t *testing.T) {
	t.Parallel()

	root := cursorTree()

	size, err := NewCursor(root).Size()
	require.NoError(t, err)
	require.Equal(t, 6, size)

	size, err = NewCursor(root).At(0).Size()
	require.NoError(t, err)
	require.Equal(t, 4, size)
}

func TestCursor_Item(t *testing.T) {
	t.Parallel()

	root := cursorTree()

	item, err := NewCursor(root).At(4).Item()
	require.NoError(t, err)
	require.True(t, item.IsASCII())
}

func TestCursor_Err(t *testing.T) {
	t.Parallel()

	// A clean cursor's Err is nil.
	require.NoError(t, NewCursor(A("x")).Err())

	// A cursor that failed to navigate carries the first error.
	c := NewCursor(A("x")).At(0)
	require.Error(t, c.Err())
}

// TestCursor_At_NoIndices asserts that At() with no arguments is a no-op.
func TestCursor_At_NoIndices(t *testing.T) {
	t.Parallel()

	root := cursorTree()

	item, err := NewCursor(root).At().Item()
	require.NoError(t, err)
	require.Same(t, root, item)
}

// --- Failure classes ---

func TestCursor_At_IndexOutOfRange(t *testing.T) {
	t.Parallel()

	root := cursorTree()

	_, err := NewCursor(root).At(0, 99).Uint()
	require.Error(t, err)
	require.ErrorContains(t, err, "index out of range")
	require.ErrorContains(t, err, "index 99")
	require.ErrorContains(t, err, "depth 1")
}

func TestCursor_At_NonListIntermediate(t *testing.T) {
	t.Parallel()

	root := cursorTree()

	// index 4 is an ASCIIItem, not a list — the second hop must fail.
	_, err := NewCursor(root).At(4, 0).ASCII()
	require.Error(t, err)
	require.ErrorContains(t, err, "index 0")
	require.ErrorContains(t, err, "depth 1")
	require.ErrorContains(t, err, "got ascii")
	require.ErrorContains(t, err, "want list")
}

func TestCursor_At_TypedNilChild(t *testing.T) {
	t.Parallel()

	var child *ASCIIItem
	builtin := NewListItem(child)
	external := &externalListItem{
		Item:     NewListItem(),
		children: []Item{builtin},
	}
	root := NewListItem(external)

	requireCursorAccessorsError(t, NewCursor(root).At(0, 0, 0))
}

func TestCursor_WrongTypeFamily(t *testing.T) {
	t.Parallel()

	root := cursorTree()

	_, err := NewCursor(root).At(4).Uint()
	require.Error(t, err)
	require.ErrorContains(t, err, "got ascii")
	require.ErrorContains(t, err, "want uint")
	require.ErrorContains(t, err, "index 4")
	require.ErrorContains(t, err, "depth 1")
}

func TestCursor_WrongTypeFamily_AtRoot(t *testing.T) {
	t.Parallel()

	// No At() hop happened yet: the message must not fabricate a hop index.
	_, err := NewCursor(A("x")).Uint()
	require.Error(t, err)
	require.ErrorContains(t, err, "at root")
	require.ErrorContains(t, err, "got ascii")
	require.ErrorContains(t, err, "want uint")
	require.NotContains(t, err.Error(), "index")
}

func TestCursor_SizeNotOne(t *testing.T) {
	t.Parallel()

	item := U4(uint(1), uint(2), uint(3))

	_, err := NewCursor(item).Uint()
	require.Error(t, err)
	require.ErrorContains(t, err, "size 3")
	require.ErrorContains(t, err, "want size 1")
	require.ErrorContains(t, err, "at root")
}

func TestCursor_SizeNotOne_AfterHop(t *testing.T) {
	t.Parallel()

	root := L(U4(uint(1), uint(2)))

	_, err := NewCursor(root).At(0).Uint()
	require.Error(t, err)
	require.ErrorContains(t, err, "size 2")
	require.ErrorContains(t, err, "index 0")
	require.ErrorContains(t, err, "depth 1")
}

func TestCursor_DeferredConstructionError(t *testing.T) {
	t.Parallel()

	// Negative value for an unsigned item: byteSize stays valid (family check passes) but
	// construction records a deferred error.
	item := U1(-5)
	require.Error(t, item.Error())

	_, err := NewCursor(item).Uint()
	require.Error(t, err)
	require.ErrorIs(t, err, item.Error())
}

func TestCursor_DeferredConstructionError_DuringHop(t *testing.T) {
	t.Parallel()

	// A list whose own construction failed carries a non-nil itemErr; navigating into it must
	// surface that error, wrapped with hop position context, instead of a plain
	// index-out-of-range.
	badList := &ListItem{values: []Item{A("x")}}
	badList.itemErr = NewItemErrorWithMsg("boom")

	root := L(badList)

	_, err := NewCursor(root).At(0, 0).Item()
	require.Error(t, err)
	require.ErrorContains(t, err, "boom")
	require.ErrorContains(t, err, "index 0")
	require.ErrorContains(t, err, "depth 1")
}

// TestCursor_ErrorAccumulation asserts that once a hop fails, later At calls are a no-op and the
// terminal error still names the first failing hop, not a later one.
func TestCursor_ErrorAccumulation(t *testing.T) {
	t.Parallel()

	root := cursorTree()

	c := NewCursor(root).At(99).At(0)

	_, err := c.Uint()
	require.Error(t, err)
	require.ErrorContains(t, err, "index 99")
	require.NotContains(t, err.Error(), "index 0")
}
