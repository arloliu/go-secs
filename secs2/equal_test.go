package secs2

import (
	"math"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// fakeItem is a minimal external Item implementation used to verify that Equal never panics
// when Type()/Size() match a built-in type but the concrete Go type does not. It delegates every
// method except Type()/Size() to an embedded Item, so it satisfies the interface while never
// being a *IntItem/*ListItem/etc. — exactly the "external implementation" hazard Equal must
// guard against.
type fakeItem struct {
	Item
	typ  string
	size int
}

func (f *fakeItem) Type() string { return f.typ }
func (f *fakeItem) Size() int    { return f.size }

var _ Item = (*fakeItem)(nil)

func TestEqual_NilHandling(t *testing.T) {
	t.Parallel()

	assert.True(t, Equal(nil, nil))
	assert.False(t, Equal(nil, I1(5)))
	assert.False(t, Equal(I1(5), nil))
}

func TestEqual_ErroredItemNeverEqual(t *testing.T) {
	t.Parallel()

	errored := NewIntItem(3) // invalid byteSize -> deferred error
	require.Error(t, errored.Error())

	assert.False(t, Equal(errored, errored), "an errored item is never equal, even to itself")
	assert.False(t, Equal(errored, I1(5)))
	assert.False(t, Equal(I1(5), errored))
}

func TestEqual_DifferentWidthsNotEqual(t *testing.T) {
	t.Parallel()

	assert.False(t, Equal(I1(5), I2(5)), "I1 and I2 differ by Type() even at the same value")
	assert.False(t, Equal(U1(5), U2(5)))
	assert.False(t, Equal(NewFloatItem(4, 1.0), NewFloatItem(8, 1.0)))
}

func TestEqual_DifferentValuesNotEqual(t *testing.T) {
	t.Parallel()

	assert.False(t, Equal(I1(5), I1(6)))
	assert.False(t, Equal(U1(5), U1(6)))
	assert.False(t, Equal(A("foo"), A("bar")))
}

func TestEqual_DifferentTypesNotEqual(t *testing.T) {
	t.Parallel()

	assert.False(t, Equal(I1(5), U1(5)))
	assert.False(t, Equal(A("5"), I1(5)))
}

func TestEqual_SameTypeSameValue(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name string
		a, b Item
	}{
		{"I1", I1(1, 2, 3), I1(1, 2, 3)},
		{"I2", I2(-100, 200), I2(-100, 200)},
		{"I4", I4(1000), I4(1000)},
		{"I8", I8(int64(1) << 40), I8(int64(1) << 40)},
		{"U1", U1(1, 2, 3), U1(1, 2, 3)},
		{"U2", NewUintItem(2, uint(1000)), NewUintItem(2, uint(1000))},
		{"U4", NewUintItem(4, uint(100000)), NewUintItem(4, uint(100000))},
		{"U8", NewUintItem(8, uint64(1)<<40), NewUintItem(8, uint64(1)<<40)},
		{"F8", NewFloatItem(8, 3.14159265358979), NewFloatItem(8, 3.14159265358979)},
		{"Boolean", NewBooleanItem(true, false), NewBooleanItem(true, false)},
		{"Binary", NewBinaryItem([]byte{0x01, 0x02}), NewBinaryItem([]byte{0x01, 0x02})},
		{"ASCII", A("hello"), A("hello")},
		{"JIS8", J("hello"), J("hello")},
		{"Empty", NewEmptyItem(), NewEmptyItem()},
		{"LocalizedStr", NewLocalizedStrItem(1, "hello"), NewLocalizedStrItem(1, "hello")},
	}

	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			t.Parallel()
			assert.True(t, Equal(test.a, test.b))
			assert.True(t, Equal(test.b, test.a), "Equal must be symmetric")
		})
	}
}

// TestEqual_NonCanonicalWireEncoding proves the core reason Equal exists: a decoded item whose
// wire encoding used a non-canonical (larger-than-necessary) length field must still compare
// Equal to a canonically-constructed item of the same logical value, even though ToBytes()
// differs between them.
func TestEqual_NonCanonicalWireEncoding(t *testing.T) {
	t.Parallel()

	// U1[1] with a 2-byte length field (format byte 0xA6 = u1 formatCode<<2 | 2) instead of the
	// canonical 1-byte length field (0xA5) that NewUintItem always produces.
	nonCanonical := []byte{0xA6, 0x00, 0x01, 0x05}
	decoded, err := Decode(nonCanonical)
	require.NoError(t, err)

	canonical := U1(5)

	require.NotEqual(t, canonical.ToBytes(), decoded.ToBytes(),
		"test setup must actually exercise different wire encodings")
	assert.True(t, Equal(canonical, decoded), "logically-identical items must compare Equal despite differing wire bytes")
	assert.True(t, Equal(decoded, canonical))
}

func TestEqual_NestedLists(t *testing.T) {
	t.Parallel()

	a := L(I1(1), A("x"), L(U1(2), U1(3)))
	b := L(I1(1), A("x"), L(U1(2), U1(3)))
	assert.True(t, Equal(a, b))

	// Differ in a nested child value.
	c := L(I1(1), A("x"), L(U1(2), U1(4)))
	assert.False(t, Equal(a, c))

	// Different lengths.
	d := L(I1(1), A("x"))
	assert.False(t, Equal(a, d))

	// A non-canonical-vs-canonical child inside a list must still propagate the fix recursively.
	nonCanonicalChild, err := Decode([]byte{0xA6, 0x00, 0x01, 0x05})
	require.NoError(t, err)
	withCanonicalChild := L(I1(1), U1(5))
	withNonCanonicalChild := L(I1(1), nonCanonicalChild)
	assert.True(t, Equal(withCanonicalChild, withNonCanonicalChild))
}

func TestEqual_LocalizedStr_SameTextDifferentHeader(t *testing.T) {
	t.Parallel()

	a := NewLocalizedStrItem(1, "hello")
	b := NewLocalizedStrItem(2, "hello")
	assert.False(t, Equal(a, b), "same text but different language-set header must be unequal")
}

func TestEqual_Float32RoundTrip(t *testing.T) {
	t.Parallel()

	// 1.234 is not exactly representable in float32, so the constructed item's raw float64 and
	// the wire-decoded (float32-widened) value differ at the float64 level, but represent the
	// exact same F4 wire value.
	constructed := NewFloatItem(4, 1.234)
	decoded, err := DecodeOwned(constructed.ToBytes())
	require.NoError(t, err)

	constructedFloat, ok := constructed.(*FloatItem)
	require.True(t, ok)
	decodedFloat, ok := decoded.(*FloatItem)
	require.True(t, ok)

	cVals, err := constructedFloat.ToFloat()
	require.NoError(t, err)
	dVals, err := decodedFloat.ToFloat()
	require.NoError(t, err)
	require.NotEqual(t, cVals, dVals,
		"test setup must actually exercise the float32-narrowing precision gap")

	assert.True(t, Equal(constructed, decoded),
		"F4 items representing the same wire value must compare Equal despite differing raw float64 storage")
}

func TestEqual_Float64ExactRoundTrip(t *testing.T) {
	t.Parallel()

	constructed := NewFloatItem(8, 2.5) // exactly representable at both float32 and float64
	decoded, err := DecodeOwned(constructed.ToBytes())
	require.NoError(t, err)

	assert.True(t, Equal(constructed, decoded))
}

func TestEqual_FloatNaN(t *testing.T) {
	t.Parallel()

	a4 := NewFloatItem(4, math.NaN())
	b4 := NewFloatItem(4, math.NaN())
	assert.True(t, Equal(a4, b4), "NaN F4 must compare equal to NaN F4 (bit-pattern comparison)")

	a8 := NewFloatItem(8, math.NaN())
	b8 := NewFloatItem(8, math.NaN())
	assert.True(t, Equal(a8, b8), "NaN F8 must compare equal to NaN F8 (bit-pattern comparison)")
}

// TestEqual_PanicSafety verifies Equal never panics when Type()/Size() report a built-in type but
// the concrete Go type is not the corresponding built-in struct.
func TestEqual_PanicSafety(t *testing.T) {
	t.Parallel()

	fakeInt := &fakeItem{Item: NewEmptyItem(), typ: I1(5).Type(), size: I1(5).Size()}

	assert.NotPanics(t, func() {
		assert.False(t, Equal(I1(5), fakeInt))
	})
	assert.NotPanics(t, func() {
		assert.False(t, Equal(fakeInt, I1(5)))
	})

	realList := L(I1(1))
	fakeList := &fakeItem{Item: NewEmptyItem(), typ: realList.Type(), size: realList.Size()}

	assert.NotPanics(t, func() {
		assert.False(t, Equal(realList, fakeList))
	})
	assert.NotPanics(t, func() {
		assert.False(t, Equal(fakeList, realList))
	})
}
