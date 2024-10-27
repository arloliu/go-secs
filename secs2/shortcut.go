package secs2

var (
	L       = NewListItem
	A       = NewASCIIItem
	B       = NewBinaryItem
	BOOLEAN = NewBooleanItem
)

func I1(values ...any) Item {
	return NewIntItem(1, values...)
}

func I2(values ...any) Item {
	return NewIntItem(2, values...)
}

func I4(values ...any) Item {
	return NewIntItem(4, values...)
}

func I8(values ...any) Item {
	return NewIntItem(8, values...)
}

func U1(values ...any) Item {
	return NewUintItem(1, values...)
}

func U2(values ...any) Item {
	return NewUintItem(2, values...)
}

func U4(values ...any) Item {
	return NewUintItem(4, values...)
}

func U8(values ...any) Item {
	return NewUintItem(8, values...)
}

func F4(values ...any) Item {
	return NewFloatItem(4, values...)
}

func F8(values ...any) Item {
	return NewFloatItem(8, values...)
}
