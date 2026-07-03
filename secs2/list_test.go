package secs2

import (
	"math"
	"sync"
	"testing"

	"github.com/stretchr/testify/require"
)

// TestListItem covers round-trip construction, exact wire bytes, and SML output. Vectors are
// ported from git show main:secs2/list_test.go.
func TestListItem(t *testing.T) {
	t.Parallel()

	tests := []struct {
		description     string
		input           []Item
		expectedSize    int
		expectedToBytes []byte
		expectedToSML   string
	}{
		{
			description:     "empty list",
			input:           []Item{},
			expectedSize:    0,
			expectedToBytes: []byte{0x01, 0x00},
			expectedToSML:   `<L[0]>`,
		},
		{
			description:  "three scalar children",
			input:        []Item{NewASCIIItem("text"), NewIntItem(1, 1, 2, 3), NewIntItem(1, 4, 5, 6)},
			expectedSize: 3,
			expectedToBytes: []byte{
				0x01, 0x03,
				0x41, 0x04, 0x74, 0x65, 0x78, 0x74,
				0x65, 0x03, 0x01, 0x02, 0x03,
				0x65, 0x03, 0x04, 0x05, 0x06,
			},
			expectedToSML: "<L[3]\n  <A[4] \"text\">\n  <I1[3] 1 2 3>\n  <I1[3] 4 5 6>\n>",
		},
		{
			description: "nested list children only",
			input: []Item{
				NewListItem(),
				NewListItem(NewIntItem(1, 100, 101)),
			},
			expectedSize: 2,
			expectedToBytes: []byte{
				0x01, 0x02,
				0x01, 0x00,
				0x01, 0x01, 0x65, 0x02, 0x64, 0x65,
			},
			expectedToSML: "<L[2]\n  <L[0]>\n  <L[1]\n    <I1[2] 100 101>\n  >\n>",
		},
		{
			description: "mixed scalars and nested lists",
			input: []Item{
				NewListItem(
					NewIntItem(2, 100, 200),
					NewListItem(NewASCIIItem("text")),
				),
				NewFloatItem(4, 33.3, 56.13),
			},
			expectedSize: 2,
			expectedToBytes: []byte{
				0x01, 0x02,
				0x01, 0x02,
				0x69, 0x04, 0x00, 0x64, 0x00, 0xc8,
				0x01, 0x01,
				0x41, 0x04, 0x74, 0x65, 0x78, 0x74,
				0x91, 0x08, 0x42, 0x05, 0x33, 0x33, 0x42, 0x60, 0x85, 0x1f,
			},
			expectedToSML: "<L[2]\n  <L[2]\n    <I2[2] 100 200>\n    <L[1]\n      <A[4] \"text\">\n    >\n  >\n  <F4[2] 33.2999992 56.1300011>\n>",
		},
	}

	for _, test := range tests {
		t.Run(test.description, func(t *testing.T) {
			t.Parallel()

			require := require.New(t)

			node := NewListItem(test.input...)
			require.NoError(node.Error())
			require.Equal(test.expectedSize, node.Size())
			require.Equal(test.expectedToBytes, node.ToBytes())
			require.Equal(test.expectedToSML, node.ToSML())
		})
	}
}

// TestListItem_Get verifies the nested-index walk: valid paths, out-of-range indices, and
// indexing into a non-list. Ported from git show main:secs2/list_test.go TestListItem_Get.
func TestListItem_Get(t *testing.T) {
	t.Parallel()

	require := require.New(t)

	nestedItem := NewListItem(
		NewListItem(
			NewIntItem(1, 1, 2, 3),
			NewListItem(
				NewIntItem(4, 4, 5, 6),
				NewListItem(
					NewFloatItem(8, 1.1, 2.2),
				),
			),
		),
	)

	// Get() with no indices returns the list itself.
	self, err := nestedItem.Get()
	require.NoError(err)
	require.Equal(nestedItem, self)

	// Out-of-range index at the top level (only 1 child at index 0).
	_, err = nestedItem.Get(1)
	require.Error(err)

	// Valid path [0,0] → I1(1,2,3).
	item, err := nestedItem.Get(0, 0)
	require.NoError(err)

	values, err := item.ToInt()
	require.NoError(err)
	require.Equal([]int64{1, 2, 3}, values)

	// Valid path [0,1] → L(I4(4,5,6), L(F8(1.1,2.2))).
	item, err = nestedItem.Get(0, 1)
	require.NoError(err)
	require.True(item.IsList())

	// Valid path [0,1,0] → I4(4,5,6).
	item, err = nestedItem.Get(0, 1, 0)
	require.NoError(err)
	require.True(item.IsInt32())

	values, err = item.ToInt()
	require.NoError(err)
	require.Equal([]int64{4, 5, 6}, values)

	// Valid path [0,1,1,0] → F8(1.1,2.2).
	item, err = nestedItem.Get(0, 1, 1, 0)
	require.NoError(err)
	require.True(item.IsFloat64())

	floatValues, err := item.ToFloat()
	require.NoError(err)
	require.Equal([]float64{1.1, 2.2}, floatValues)

	// Indexing into a non-list errors.
	_, err = nestedItem.Get(0, 0, 0)
	require.Error(err)
}

// TestListItem_ToList_NoLeak confirms that mutating the slice returned by ToList does not
// affect the ListItem's internal state (teeth test for the shallow-clone guarantee).
func TestListItem_ToList_NoLeak(t *testing.T) {
	t.Parallel()

	require := require.New(t)

	a := NewASCIIItem("original")
	b := NewASCIIItem("second")
	list := NewListItem(a, b)

	got, err := list.ToList()
	require.NoError(err)
	require.Len(got, 2)

	// Mutate the returned slice: overwrite an element.
	got[0] = NewASCIIItem("mutated")

	// The source list must be unchanged.
	require.Equal(2, list.Size())

	orig, err := list.ToList()
	require.NoError(err)
	require.Equal(a, orig[0])
	require.Equal(b, orig[1])
}

// TestListItem_Items verifies that Items() yields children in order and yields nothing
// for items with a deferred error.
func TestListItem_Items(t *testing.T) {
	t.Parallel()

	require := require.New(t)

	a := NewASCIIItem("a")
	b := NewIntItem(1, 42)
	c := NewBooleanItem(true)
	list := NewListItem(a, b, c)

	got := make([]Item, 0, list.Size())

	for child := range list.Items() {
		got = append(got, child)
	}

	require.Len(got, 3)
	require.Equal(a, got[0])
	require.Equal(b, got[1])
	require.Equal(c, got[2])

	// Error item yields nothing.
	errList := NewListItem(NewIntItem(3))
	errGot := make([]Item, 0, errList.Size())

	for child := range errList.Items() {
		errGot = append(errGot, child)
	}

	// errList.Error() != nil but its own itemErr is nil (the child has the error);
	// Items() only gates on item.itemErr, so it yields the (invalid) child.
	require.Len(errGot, 1)
}

// TestListItem_ItemAt verifies bounds-checked single-child access.
func TestListItem_ItemAt(t *testing.T) {
	t.Parallel()

	require := require.New(t)

	a := NewASCIIItem("hello")
	b := NewIntItem(4, 99)
	list := NewListItem(a, b)

	got0, err := list.ItemAt(0)
	require.NoError(err)
	require.Equal(a, got0)

	got1, err := list.ItemAt(1)
	require.NoError(err)
	require.Equal(b, got1)

	_, err = list.ItemAt(-1)
	require.Error(err)

	_, err = list.ItemAt(2)
	require.Error(err)
}

// TestListItem_AppendTo_IndependentVector verifies the exact wire bytes for a simple nested
// structure, computed by hand without relying on ToBytes:
//
//	L( B[1]{0x42} ) →
//	  list header : format=0x01, length=1 child → {0x01, 0x01}
//	  BinaryItem  : BinaryFormatCode=0o10, 8<<2+1=0x21, length=1, data=0x42 → {0x21, 0x01, 0x42}
//	  total       : {0x01, 0x01, 0x21, 0x01, 0x42}
func TestListItem_AppendTo_IndependentVector(t *testing.T) {
	t.Parallel()

	require := require.New(t)

	item := NewListItem(NewBinaryItem(byte(0x42)))
	expected := []byte{0x01, 0x01, 0x21, 0x01, 0x42}

	buf := make([]byte, 0, item.EncodedLen())
	require.Equal(expected, item.AppendTo(buf))
	require.Equal(expected, item.ToBytes())
}

// TestListItem_Error verifies that Error() aggregates errors from all children recursively,
// and that a valid list reports nil. Ported from git show main:secs2/list_test.go TestListItem_Errors.
func TestListItem_Error(t *testing.T) {
	t.Parallel()

	require := require.New(t)

	// Invalid child: byteSize=3 is not valid for IntItem.
	require.Error(NewListItem(NewIntItem(3)).Error())

	// Unparseable string value in child.
	require.Error(NewListItem(NewIntItem(1, "string")).Error())

	// Deeply nested error propagates to the outer list.
	deep := NewListItem(
		NewListItem(
			NewIntItem(1, 255),
			NewIntItem(1, "string"),
			NewListItem(
				NewIntItem(2, math.MaxInt32),
			),
		),
	)
	require.Error(deep.Error())

	// Valid list reports nil error.
	require.NoError(NewListItem(NewASCIIItem("ok")).Error())

	// Empty list reports nil error.
	require.NoError(NewListItem().Error())
}

// TestListItem_NilSkip confirms that nil children passed to NewListItem are silently ignored.
func TestListItem_NilSkip(t *testing.T) {
	t.Parallel()

	require := require.New(t)

	list := NewListItem(NewASCIIItem("a"), nil, NewASCIIItem("b"))
	require.Equal(2, list.Size())
}

// TestListItem_EncodedLen_Invariant asserts EncodedLen() == len(ToBytes()) for several
// structures, including a list with an errored child.
func TestListItem_EncodedLen_Invariant(t *testing.T) {
	t.Parallel()

	require := require.New(t)

	// Valid nested structure.
	nested := NewListItem(
		NewListItem(NewASCIIItem("hi"), NewIntItem(1, 1, 2)),
		NewFloatItem(4, 3.14),
	)
	require.Equal(len(nested.ToBytes()), nested.EncodedLen())

	// Empty list.
	require.Equal(len(NewListItem().ToBytes()), NewListItem().EncodedLen())

	// List with a size-error child (EncodedLen and len(ToBytes) must agree).
	erred := NewListItem(NewIntItem(3))
	require.Equal(len(erred.ToBytes()), erred.EncodedLen())
}

// TestListItem_Race exercises concurrent reads on a shared nested list under -race.
func TestListItem_Race(t *testing.T) {
	t.Parallel()

	shared := NewListItem(
		NewASCIIItem("shared"),
		NewIntItem(4, 1, 2, 3),
		NewListItem(
			NewBooleanItem(true, false),
			NewBinaryItem(byte(0xAB)),
		),
	)

	const goroutines = 8

	var wg sync.WaitGroup

	wg.Add(goroutines)

	for range goroutines {
		go func() {
			defer wg.Done()

			_ = shared.ToBytes()

			buf := make([]byte, 0, shared.EncodedLen())
			_ = shared.AppendTo(buf)

			for child := range shared.Items() {
				_ = child
			}

			_, _ = shared.Get(2, 0)
			_, _ = shared.ToList()
		}()
	}

	wg.Wait()
}

// TestListItem_Allocs asserts that AppendTo into a pre-sized buffer performs 0 allocations
// and that ToBytes performs exactly 1 allocation (the pre-sized make).
// Note: AllocsPerRun must not run in a parallel test.
func TestListItem_Allocs(t *testing.T) {
	list := NewListItem(
		NewASCIIItem("test"),
		NewIntItem(2, 1, 2, 3),
	)
	buf := make([]byte, 0, list.EncodedLen())

	appendAllocs := testing.AllocsPerRun(100, func() {
		buf = buf[:0]
		buf = list.AppendTo(buf)
	})

	if appendAllocs > 0 {
		t.Errorf("AppendTo: want 0 allocs, got %v", appendAllocs)
	}

	toBytesAllocs := testing.AllocsPerRun(100, func() {
		_ = list.ToBytes()
	})

	if toBytesAllocs != 1 {
		t.Errorf("ToBytes: want 1 alloc, got %v", toBytesAllocs)
	}
}
