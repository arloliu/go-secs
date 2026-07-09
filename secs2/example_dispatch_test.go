package secs2_test

import (
	"fmt"

	"github.com/arloliu/go-secs/v2/secs2"
)

// ExampleItem_typeDispatch is the canonical, EXHAUSTIVE Is*/To* dispatch over an item's
// declared type. Copy it as the reference when you need to turn an arbitrary secs2.Item into a
// concrete Go value.
//
// Two properties are load-bearing and easy to get wrong in a hand-rolled version:
//
//  1. Every branch reads through the typed To* accessor and PROPAGATES that accessor's error
//     rather than discarding it (see [ExampleItem_deferredError]). Is* reflects only the declared
//     type; it does not imply the item is usable.
//  2. Every string family is covered — ASCII, JIS8, AND LocalizedStr. Omitting one (JIS8 is the
//     usual casualty) silently mishandles valid items: a JIS8 CEID that falls through to the
//     default branch is a real, shipped bug class, not a hypothetical.
func ExampleItem_typeDispatch() {
	describe := func(item secs2.Item) string {
		switch {
		case item.IsList():
			v, err := item.ToList()
			return fmt.Sprintf("list len=%d err=%v", len(v), err)
		case item.IsBinary():
			v, err := item.ToBinary()
			return fmt.Sprintf("binary %v err=%v", v, err)
		case item.IsBoolean():
			v, err := item.ToBoolean()
			return fmt.Sprintf("boolean %v err=%v", v, err)
		case item.IsASCII():
			v, err := item.ToASCII()
			return fmt.Sprintf("ascii %q err=%v", v, err)
		case item.IsJIS8():
			v, err := item.ToJIS8()
			return fmt.Sprintf("jis8 %q err=%v", v, err)
		case item.IsLocalizedStr():
			v, err := item.ToLocalizedStr()
			return fmt.Sprintf("localized %q err=%v", v, err)
		case item.IsInt8(), item.IsInt16(), item.IsInt32(), item.IsInt64():
			v, err := item.ToInt()
			return fmt.Sprintf("int %v err=%v", v, err)
		case item.IsUint8(), item.IsUint16(), item.IsUint32(), item.IsUint64():
			v, err := item.ToUint()
			return fmt.Sprintf("uint %v err=%v", v, err)
		case item.IsFloat32(), item.IsFloat64():
			v, err := item.ToFloat()
			return fmt.Sprintf("float %v err=%v", v, err)
		case item.IsEmpty():
			return "empty"
		default:
			return "unhandled"
		}
	}

	items := []secs2.Item{
		secs2.NewASCIIItem("EQ"),
		secs2.NewJIS8Item("J"),
		secs2.NewIntItem(2, 7),
		secs2.NewUintItem(1, 255),
		secs2.NewFloatItem(4, 1.5),
		secs2.NewBooleanItem(true, false),
		secs2.NewListItem(secs2.NewASCIIItem("a")),
		secs2.NewEmptyItem(),
	}
	for _, item := range items {
		fmt.Println(describe(item))
	}
	// Output:
	// ascii "EQ" err=<nil>
	// jis8 "J" err=<nil>
	// int [7] err=<nil>
	// uint [255] err=<nil>
	// float [1.5] err=<nil>
	// boolean [true false] err=<nil>
	// list len=1 err=<nil>
	// empty
}
