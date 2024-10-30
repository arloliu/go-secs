package secs2

// L is a shorthand function for creating a new ListItem.
// It's equivalent to calling NewListItem.
var L = NewListItem

// A is a shorthand function for creating a new ASCIIItem.
// It's equivalent to calling NewASCIIItem.
var A = NewASCIIItem

// B is a shorthand function for creating a new BinaryItem.
// It's equivalent to calling NewBinaryItem.
var B = NewBinaryItem

// BOOLEAN is a shorthand function for creating a new BooleanItem.
// It's equivalent to calling NewBooleanItem.
var BOOLEAN = NewBooleanItem

// I1 is a shorthand function for creating a new IntItem (1-byte signed integer).
// It's equivalent to calling NewIntItem(1, values...)
func I1(values ...any) Item {
	return NewIntItem(1, values...)
}

// I2 is a shorthand function for creating a new IntItem (2-byte signed integer).
// It's equivalent to calling NewIntItem(2, values...).
func I2(values ...any) Item {
	return NewIntItem(2, values...)
}

// I4 is a shorthand function for creating a new IntItem (4-byte signed integer).
// It's equivalent to calling NewIntItem(4, values...).
func I4(values ...any) Item {
	return NewIntItem(4, values...)
}

// I8 is a shorthand function for creating a new IntItem (8-byte signed integer).
// It's equivalent to calling NewIntItem(8, values...).
func I8(values ...any) Item {
	return NewIntItem(8, values...)
}

// U1 is a shorthand function for creating a new UintItem (1-byte unsigned integer).
// It's equivalent to calling NewUintItem(1, values...).
func U1(values ...any) Item {
	return NewUintItem(1, values...)
}

// U2 is a shorthand function for creating a new UintItem (2-byte unsigned integer).
// It's equivalent to calling NewUintItem(2, values...).
func U2(values ...any) Item {
	return NewUintItem(2, values...)
}

// U4 is a shorthand function for creating a new UintItem (4-byte unsigned integer).
// It's equivalent to calling NewUintItem(4, values...).
func U4(values ...any) Item {
	return NewUintItem(4, values...)
}

// U8 is a shorthand function for creating a new UintItem (8-byte unsigned integer).
// It's equivalent to calling NewUintItem(8, values...).
func U8(values ...any) Item {
	return NewUintItem(8, values...)
}

// F4 is a shorthand function for creating a new FloatItem (4-byte float).
// It's equivalent to calling NewFloatItem(4, values...).
func F4(values ...any) Item {
	return NewFloatItem(4, values...)
}

// F8 is a shorthand function for creating a new FloatItem (8-byte float).
// It's equivalent to calling NewFloatItem(8, values...).
func F8(values ...any) Item {
	return NewFloatItem(8, values...)
}
