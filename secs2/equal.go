package secs2

import (
	"bytes"
	"math"
	"slices"
)

// Equal reports whether a and b are the same SECS-II item: same declared type/width (per Type())
// and the same decoded logical value — NOT the same wire encoding. Two items built by different
// paths that carry the same logical value compare equal even when their wire bytes differ, e.g.
// a value decoded with a non-canonical (larger-than-necessary) SECS-II length field versus the
// same value freshly constructed (which always canonicalizes to the shortest length field):
// decoding a non-canonical U1[1] body and secs2.U1(5) compare Equal even though ToBytes() differs.
//
// Items of different declared widths are not equal even at the same numeric value
// (I2[1]{5} != I4[1]{5}), because Type() differs.
//
// An item carrying a deferred construction error (Error() != nil) is never equal to any item,
// including another errored item, because its logical value is unspecified.
//
// Two nil items are equal; a nil item is not equal to a non-nil item.
//
// Parameters:
//   - a: the first item to compare.
//   - b: the second item to compare.
//
// Returns:
//   - bool: true if a and b are the same SECS-II item.
func Equal(a, b Item) bool {
	if a == nil || b == nil {
		return a == nil && b == nil
	}
	if a.Error() != nil || b.Error() != nil {
		return false
	}
	if a.Type() != b.Type() || a.Size() != b.Size() {
		return false
	}

	// Type()/Size() equality does NOT guarantee b is the same concrete Go type as a: Item has no
	// sealing method, so an external type can implement Item and report a built-in Type() string.
	// Every helper below therefore uses the two-result assertion form and reports not-equal on a
	// failed assertion rather than assuming success.
	switch av := a.(type) {
	case *IntItem:
		return equalInt(av, b)
	case *UintItem:
		return equalUint(av, b)
	case *FloatItem:
		return equalFloat(av, b)
	case *BooleanItem:
		return equalBoolean(av, b)
	case *BinaryItem:
		return equalBinary(av, b)
	case *ASCIIItem:
		return equalASCII(av, b)
	case *JIS8Item:
		return equalJIS8(av, b)
	case *LocalizedStrItem:
		return equalLocalizedStr(av, b)
	case *ListItem:
		return equalList(av, b)
	case *EmptyItem:
		_, ok := b.(*EmptyItem)

		return ok // Type()/Size() already matched (both "empty", size 0); no payload to compare.
	default:
		// Unreachable for the current closed set of concrete item types. If a new item type is
		// added to secs2 without a case here, this is a deliberate fail-safe: report not-equal
		// rather than silently falling back to the wire-byte comparison this fix just removed.
		return false
	}
}

func equalInt(av *IntItem, b Item) bool {
	bv, ok := b.(*IntItem)
	if !ok {
		return false
	}
	x, _ := av.ToInt()
	y, _ := bv.ToInt()

	return slices.Equal(x, y)
}

func equalUint(av *UintItem, b Item) bool {
	bv, ok := b.(*UintItem)
	if !ok {
		return false
	}
	x, _ := av.ToUint()
	y, _ := bv.ToUint()

	return slices.Equal(x, y)
}

// equalFloat compares at the declared wire precision, not raw ToFloat() equality: FloatItem
// always stores float64 and applies F4 narrowing only at encode time, so two F4 items holding
// the same transmitted value can have different ToFloat() results if one was constructed from a
// float64 that isn't exactly float32-representable and the other was decoded from wire bytes. F8
// compares the full float64 bit pattern; F4 compares the float32-narrowed bit pattern both sides
// would actually transmit. Using bit patterns (not ==) makes NaN compare equal to NaN and +0/-0
// compare unequal, which is the correct notion of "same wire value" for this API.
func equalFloat(av *FloatItem, b Item) bool {
	bv, ok := b.(*FloatItem)
	if !ok {
		return false
	}

	x, _ := av.ToFloat()
	y, _ := bv.ToFloat()
	if len(x) != len(y) {
		return false
	}

	for i := range x {
		if av.byteSize == 4 {
			if math.Float32bits(float32(x[i])) != math.Float32bits(float32(y[i])) {
				return false
			}
		} else if math.Float64bits(x[i]) != math.Float64bits(y[i]) {
			return false
		}
	}

	return true
}

func equalBoolean(av *BooleanItem, b Item) bool {
	bv, ok := b.(*BooleanItem)
	if !ok {
		return false
	}
	x, _ := av.ToBoolean()
	y, _ := bv.ToBoolean()

	return slices.Equal(x, y)
}

func equalBinary(av *BinaryItem, b Item) bool {
	bv, ok := b.(*BinaryItem)
	if !ok {
		return false
	}
	x, _ := av.ToBinary()
	y, _ := bv.ToBinary()

	return bytes.Equal(x, y)
}

func equalASCII(av *ASCIIItem, b Item) bool {
	bv, ok := b.(*ASCIIItem)
	if !ok {
		return false
	}
	x, _ := av.ToASCII()
	y, _ := bv.ToASCII()

	return x == y
}

func equalJIS8(av *JIS8Item, b Item) bool {
	bv, ok := b.(*JIS8Item)
	if !ok {
		return false
	}
	x, _ := av.ToJIS8()
	y, _ := bv.ToJIS8()

	return x == y
}

// equalLocalizedStr requires both the text and the language-set header (LSH) to match: two
// localized-string items with the same text but a different LSH are NOT the same wire value.
func equalLocalizedStr(av *LocalizedStrItem, b Item) bool {
	bv, ok := b.(*LocalizedStrItem)
	if !ok {
		return false
	}
	xs, _ := av.ToLocalizedStr()
	ys, _ := bv.ToLocalizedStr()
	xh, _ := av.ToLocalizedStrHeader()
	yh, _ := bv.ToLocalizedStrHeader()

	return xs == ys && xh == yh
}

func equalList(av *ListItem, b Item) bool {
	bv, ok := b.(*ListItem)
	if !ok {
		return false
	}
	xc, _ := av.ToList()
	yc, _ := bv.ToList()
	if len(xc) != len(yc) {
		return false
	}

	for i := range xc {
		if !Equal(xc[i], yc[i]) {
			return false
		}
	}

	return true
}
