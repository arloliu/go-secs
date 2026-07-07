package secs2

import (
	"errors"
	"fmt"
	"iter"
	"unsafe"
)

// MaxByteSize defines the maximum allowed size (in bytes) for a SECS-II item's data payload.
const MaxByteSize = 1<<24 - 1

// Type constants define SECS-II data item type strings.
const (
	EmptyType        = "empty"
	ListType         = "list"
	BinaryType       = "binary"
	BooleanType      = "boolean"
	ASCIIType        = "ascii"
	JIS8Type         = "jis8"
	LocalizedStrType = "localized_str"
	Int8Type         = "i1"
	Int16Type        = "i2"
	Int32Type        = "i4"
	Int64Type        = "i8"
	Uint8Type        = "u1"
	Uint16Type       = "u2"
	Uint32Type       = "u4"
	Uint64Type       = "u8"
	Float32Type      = "f4"
	Float64Type      = "f8"
)

// FormatCode is a SECS-II item format code (SEMI E5 §9.2). Format codes are 6-bit values in the
// range [0, 63]; there is no negative or out-of-band sentinel value.
type FormatCode uint8

// Format constants define the SECS-II item format codes.
const (
	ListFormatCode         FormatCode = 0o00
	BinaryFormatCode       FormatCode = 0o10
	BooleanFormatCode      FormatCode = 0o11
	ASCIIFormatCode        FormatCode = 0o20
	JIS8FormatCode         FormatCode = 0o21
	LocalizedStrFormatCode FormatCode = 0o22
	Int64FormatCode        FormatCode = 0o30
	Int8FormatCode         FormatCode = 0o31
	Int16FormatCode        FormatCode = 0o32
	Int32FormatCode        FormatCode = 0o34
	Float64FormatCode      FormatCode = 0o40
	Float32FormatCode      FormatCode = 0o44
	Uint64FormatCode       FormatCode = 0o50
	Uint8FormatCode        FormatCode = 0o51
	Uint16FormatCode       FormatCode = 0o52
	Uint32FormatCode       FormatCode = 0o54
)

// String returns the SECS-II item-type name for the format code (for example "list", "ascii",
// "i4"). Codes outside the SEMI E5 set render as "unknown(<value>)".
func (fc FormatCode) String() string {
	switch fc {
	case ListFormatCode:
		return ListType
	case BinaryFormatCode:
		return BinaryType
	case BooleanFormatCode:
		return BooleanType
	case ASCIIFormatCode:
		return ASCIIType
	case JIS8FormatCode:
		return JIS8Type
	case LocalizedStrFormatCode:
		return LocalizedStrType
	case Int8FormatCode:
		return Int8Type
	case Int16FormatCode:
		return Int16Type
	case Int32FormatCode:
		return Int32Type
	case Int64FormatCode:
		return Int64Type
	case Uint8FormatCode:
		return Uint8Type
	case Uint16FormatCode:
		return Uint16Type
	case Uint32FormatCode:
		return Uint32Type
	case Uint64FormatCode:
		return Uint64Type
	case Float32FormatCode:
		return Float32Type
	case Float64FormatCode:
		return Float64Type
	default:
		return fmt.Sprintf("unknown(%d)", uint8(fc))
	}
}

// itemType holds encoding metadata for one SECS-II data type.
type itemType struct {
	FormatCode FormatCode
	Size       int // bytes per element
}

// itemTypeMap maps type-string constants to their encoding metadata.
var itemTypeMap = map[string]*itemType{
	ListType:         {FormatCode: ListFormatCode, Size: 1},
	BinaryType:       {FormatCode: BinaryFormatCode, Size: 1},
	BooleanType:      {FormatCode: BooleanFormatCode, Size: 1},
	ASCIIType:        {FormatCode: ASCIIFormatCode, Size: 1},
	JIS8Type:         {FormatCode: JIS8FormatCode, Size: 1},
	LocalizedStrType: {FormatCode: LocalizedStrFormatCode, Size: 1},
	Int64Type:        {FormatCode: Int64FormatCode, Size: 8},
	Int8Type:         {FormatCode: Int8FormatCode, Size: 1},
	Int16Type:        {FormatCode: Int16FormatCode, Size: 2},
	Int32Type:        {FormatCode: Int32FormatCode, Size: 4},
	Float64Type:      {FormatCode: Float64FormatCode, Size: 8},
	Float32Type:      {FormatCode: Float32FormatCode, Size: 4},
	Uint64Type:       {FormatCode: Uint64FormatCode, Size: 8},
	Uint8Type:        {FormatCode: Uint8FormatCode, Size: 1},
	Uint16Type:       {FormatCode: Uint16FormatCode, Size: 2},
	Uint32Type:       {FormatCode: Uint32FormatCode, Size: 4},
}

// ItemError records a failed item construction or type mismatch.
type ItemError struct {
	err error
}

// NewItemErrorWithMsg creates a new ItemError with the given message.
func NewItemErrorWithMsg(errMsg string) *ItemError {
	return &ItemError{err: errors.New(errMsg)}
}

// NewItemError wraps err as an ItemError.
// If err is already an ItemError, the inner error is unwrapped to avoid double-wrapping.
func NewItemError(err error) *ItemError {
	itemErr := &ItemError{}
	if errors.As(err, &itemErr) {
		return &ItemError{err: errors.Unwrap(err)}
	}

	return &ItemError{err: err}
}

// Error implements the error interface.
func (e *ItemError) Error() string { return e.err.Error() }

// Unwrap returns the underlying error.
func (e *ItemError) Unwrap() error { return e.err }

// Item is a deeply immutable SECS-II data item (SEMI E5 §9).
//
// All methods are safe for concurrent use, and no method exposes mutable internal storage.
type Item interface {
	// Type returns the type-string constant for this item (e.g., "ascii", "i4", "list").
	Type() string

	// Size returns the number of elements in this item.
	//
	// It returns bytes for binary/string, values for numeric, and sub-items for lists.
	Size() int

	// Error returns any deferred construction error stored on the item.
	Error() error

	// IsEmpty returns true if the item carries no data and no type (EmptyItem).
	IsEmpty() bool

	// IsList returns true if the item is a list (ListItem).
	IsList() bool

	// IsBinary returns true if the item is a binary item (BinaryItem).
	IsBinary() bool

	// IsBoolean returns true if the item is a boolean item (BooleanItem).
	IsBoolean() bool

	// IsASCII returns true if the item is an ASCII string item (ASCIIItem).
	IsASCII() bool

	// IsJIS8 returns true if the item is a JIS-8 string item (JIS8Item).
	IsJIS8() bool

	// IsLocalizedStr returns true if the item is a localized-string item (LocalizedStrItem).
	IsLocalizedStr() bool

	// IsInt8 returns true if the item is a signed 8-bit integer item (IntItem with byteSize 1).
	IsInt8() bool

	// IsInt16 returns true if the item is a signed 16-bit integer item (IntItem with byteSize 2).
	IsInt16() bool

	// IsInt32 returns true if the item is a signed 32-bit integer item (IntItem with byteSize 4).
	IsInt32() bool

	// IsInt64 returns true if the item is a signed 64-bit integer item (IntItem with byteSize 8).
	IsInt64() bool

	// IsUint8 returns true if the item is an unsigned 8-bit integer item (UintItem with byteSize 1).
	IsUint8() bool

	// IsUint16 returns true if the item is an unsigned 16-bit integer item (UintItem with byteSize 2).
	IsUint16() bool

	// IsUint32 returns true if the item is an unsigned 32-bit integer item (UintItem with byteSize 4).
	IsUint32() bool

	// IsUint64 returns true if the item is an unsigned 64-bit integer item (UintItem with byteSize 8).
	IsUint64() bool

	// IsFloat32 returns true if the item is a 32-bit floating-point item (FloatItem with byteSize 4).
	IsFloat32() bool

	// IsFloat64 returns true if the item is a 64-bit floating-point item (FloatItem with byteSize 8).
	IsFloat64() bool

	// Get navigates a path of list indices and returns the item at that position.
	//
	// With no indices it returns the receiver itself.
	Get(indices ...int) (Item, error)

	// ToList returns the sub-items of a list item.
	ToList() ([]Item, error)

	// ToBinary returns the bytes of a binary item.
	ToBinary() ([]byte, error)

	// ToBoolean returns the boolean values of a boolean item.
	ToBoolean() ([]bool, error)

	// ToASCII returns the string value of an ASCII item.
	ToASCII() (string, error)

	// ToJIS8 returns the string value of a JIS-8 item.
	ToJIS8() (string, error)

	// ToLocalizedStr returns the string value of a localized-string item.
	ToLocalizedStr() (string, error)

	// ToLocalizedStrHeader returns the language/character-set header of a localized-string item.
	ToLocalizedStrHeader() (uint16, error)

	// ToInt returns the int64-widened values of a signed integer item.
	ToInt() ([]int64, error)

	// ToUint returns the uint64-widened values of an unsigned integer item.
	ToUint() ([]uint64, error)

	// ToFloat returns the float64-widened values of a floating-point item.
	ToFloat() ([]float64, error)

	// ItemAt returns the sub-item at index i of a list item.
	ItemAt(i int) (Item, error)

	// ByteAt returns the byte at index i of a binary item.
	ByteAt(i int) (byte, error)

	// BoolAt returns the boolean at index i of a boolean item.
	BoolAt(i int) (bool, error)

	// IntAt returns the int64-widened value at index i of a signed integer item.
	IntAt(i int) (int64, error)

	// UintAt returns the uint64-widened value at index i of an unsigned integer item.
	UintAt(i int) (uint64, error)

	// FloatAt returns the float64-widened value at index i of a floating-point item.
	FloatAt(i int) (float64, error)

	// Items returns an iterator over the sub-items of a list item.
	//
	// It yields nothing on any non-list item, whether the mismatch is a plain
	// type mismatch or a deferred error.
	Items() iter.Seq[Item]

	// Bools returns an iterator over the boolean values of a boolean item.
	//
	// It yields nothing on any non-boolean item, whether the mismatch is a plain
	// type mismatch or a deferred error.
	Bools() iter.Seq[bool]

	// Ints returns an iterator over the int64-widened values of a signed integer item.
	//
	// It yields nothing on any non-signed-integer item, whether the mismatch is a plain
	// type mismatch or a deferred error.
	Ints() iter.Seq[int64]

	// Uints returns an iterator over the uint64-widened values of an unsigned integer item.
	//
	// It yields nothing on any non-unsigned-integer item, whether the mismatch is a plain
	// type mismatch or a deferred error.
	Uints() iter.Seq[uint64]

	// Floats returns an iterator over the float64-widened values of a floating-point item.
	//
	// It yields nothing on any non-floating-point item, whether the mismatch is a plain
	// type mismatch or a deferred error.
	Floats() iter.Seq[float64]

	// AppendBinaryTo appends the raw bytes of a binary item into dst without allocating.
	//
	// On any non-binary item it returns dst unchanged.
	AppendBinaryTo(dst []byte) []byte

	// EncodedLen returns the total SECS-II wire byte length (header + payload, recursive for lists).
	EncodedLen() int

	// AppendTo appends the SECS-II wire encoding of this item into dst and returns the result.
	//
	// No internal storage is exposed.
	AppendTo(dst []byte) []byte

	// ToBytes allocates exactly one buffer and returns the SECS-II wire encoding of this item.
	//
	// Equivalent to AppendTo(make([]byte, 0, EncodedLen())).
	ToBytes() []byte

	// ToSML returns the SML (SECS Message Language) text representation of this item.
	//
	// When Error() is non-nil the returned text is unspecified; such items are rejected at
	// message construction and are never serialized.
	ToSML() string
}

// baseItem is the shared, immutable base for all concrete items: the deferred-error field, the
// decoder-owned raw bytes (nil for constructed items), and default accessors that report a type
// mismatch (or yield nothing) for accessors a concrete type does not support. All defaults are
// read-only — they never mutate the item.
type baseItem struct {
	itemErr error
	rawPtr  *byte
	rawLen  int
}

func (b *baseItem) raw() []byte {
	if b.rawPtr == nil {
		return nil
	}

	return unsafe.Slice(b.rawPtr, b.rawLen)
}

func (b *baseItem) setRaw(raw []byte) {
	if len(raw) == 0 {
		b.rawPtr = nil
		b.rawLen = 0

		return
	}

	b.rawPtr = unsafe.SliceData(raw)
	b.rawLen = len(raw)
}

func (b *baseItem) Error() error { return b.itemErr }

func (b *baseItem) setError(err error) { //nolint:unused // called by concrete-type constructors added in later tasks
	b.itemErr = errors.Join(b.itemErr, NewItemError(err))
}

func (b *baseItem) setErrorMsg(msg string) { //nolint:unused // called by concrete-type constructors added in later tasks
	b.itemErr = errors.Join(b.itemErr, NewItemErrorWithMsg(msg))
}

func (b *baseItem) typeErr(method string) error {
	if b.itemErr != nil {
		return b.itemErr
	}

	return NewItemErrorWithMsg("method " + method + " not supported by this item type")
}

func (b *baseItem) Get(...int) (Item, error)        { return nil, b.typeErr("Get") }
func (b *baseItem) ToList() ([]Item, error)         { return nil, b.typeErr("ToList") }
func (b *baseItem) ToBinary() ([]byte, error)       { return nil, b.typeErr("ToBinary") }
func (b *baseItem) ToBoolean() ([]bool, error)      { return nil, b.typeErr("ToBoolean") }
func (b *baseItem) ToASCII() (string, error)        { return "", b.typeErr("ToASCII") }
func (b *baseItem) ToJIS8() (string, error)         { return "", b.typeErr("ToJIS8") }
func (b *baseItem) ToLocalizedStr() (string, error) { return "", b.typeErr("ToLocalizedStr") }
func (b *baseItem) ToLocalizedStrHeader() (uint16, error) {
	return 0, b.typeErr("ToLocalizedStrHeader")
}
func (b *baseItem) ToInt() ([]int64, error)     { return nil, b.typeErr("ToInt") }
func (b *baseItem) ToUint() ([]uint64, error)   { return nil, b.typeErr("ToUint") }
func (b *baseItem) ToFloat() ([]float64, error) { return nil, b.typeErr("ToFloat") }

func (b *baseItem) ItemAt(int) (Item, error)     { return nil, b.typeErr("ItemAt") }
func (b *baseItem) ByteAt(int) (byte, error)     { return 0, b.typeErr("ByteAt") }
func (b *baseItem) BoolAt(int) (bool, error)     { return false, b.typeErr("BoolAt") }
func (b *baseItem) IntAt(int) (int64, error)     { return 0, b.typeErr("IntAt") }
func (b *baseItem) UintAt(int) (uint64, error)   { return 0, b.typeErr("UintAt") }
func (b *baseItem) FloatAt(int) (float64, error) { return 0, b.typeErr("FloatAt") }

func (b *baseItem) Items() iter.Seq[Item]            { return func(func(Item) bool) {} }
func (b *baseItem) Bools() iter.Seq[bool]            { return func(func(bool) bool) {} }
func (b *baseItem) Ints() iter.Seq[int64]            { return func(func(int64) bool) {} }
func (b *baseItem) Uints() iter.Seq[uint64]          { return func(func(uint64) bool) {} }
func (b *baseItem) Floats() iter.Seq[float64]        { return func(func(float64) bool) {} }
func (b *baseItem) AppendBinaryTo(dst []byte) []byte { return dst }

func (b *baseItem) IsEmpty() bool        { return false }
func (b *baseItem) IsList() bool         { return false }
func (b *baseItem) IsBinary() bool       { return false }
func (b *baseItem) IsBoolean() bool      { return false }
func (b *baseItem) IsASCII() bool        { return false }
func (b *baseItem) IsJIS8() bool         { return false }
func (b *baseItem) IsLocalizedStr() bool { return false }
func (b *baseItem) IsInt8() bool         { return false }
func (b *baseItem) IsInt16() bool        { return false }
func (b *baseItem) IsInt32() bool        { return false }
func (b *baseItem) IsInt64() bool        { return false }
func (b *baseItem) IsUint8() bool        { return false }
func (b *baseItem) IsUint16() bool       { return false }
func (b *baseItem) IsUint32() bool       { return false }
func (b *baseItem) IsUint64() bool       { return false }
func (b *baseItem) IsFloat32() bool      { return false }
func (b *baseItem) IsFloat64() bool      { return false }

// EmptyItem represents an empty SECS-II data item that carries no type or payload.
// It is used as a zero value and as a sentinel for absent items.
type EmptyItem struct{ baseItem }

// NewEmptyItem returns a new EmptyItem as an Item interface value.
func NewEmptyItem() Item { return &EmptyItem{} }

// Get returns the item itself when called with no indices, or an error if indices are provided
// (an empty item is not a list).
func (item *EmptyItem) Get(indices ...int) (Item, error) {
	if len(indices) != 0 {
		return nil, NewItemError(fmt.Errorf("item is not a list, item is empty, indices is %v", indices))
	}

	return item, nil
}

// Size returns 0; an empty item has no elements.
func (item *EmptyItem) Size() int { return 0 }

// EncodedLen returns 0; an empty item produces no wire bytes.
func (item *EmptyItem) EncodedLen() int { return 0 }

// AppendTo returns dst unchanged; an empty item has no wire encoding.
func (item *EmptyItem) AppendTo(dst []byte) []byte { return dst }

// ToBytes returns an empty byte slice.
func (item *EmptyItem) ToBytes() []byte { return []byte{} }

// ToSML returns an empty string; an empty item has no SML representation.
func (item *EmptyItem) ToSML() string { return "" }

// Type returns EmptyType ("empty").
func (item *EmptyItem) Type() string { return EmptyType }

// IsEmpty returns true.
//
// It reflects the DECLARED type only and does not consult the item's deferred error:
// a true result does not imply the item is usable. Gate value extraction on Error()
// (or the To* accessor's returned error), not on Is* alone.
func (item *EmptyItem) IsEmpty() bool { return true }

var _ Item = (*EmptyItem)(nil)

// getDataByteLength returns the total payload byte length for the given type string and element count.
func getDataByteLength(dataType string, size int) (int, error) {
	it, ok := itemTypeMap[dataType]
	if !ok {
		return 0, fmt.Errorf("invalid item type %s", dataType)
	}

	return size * it.Size, nil
}

// appendHeaderBytes appends the SECS-II item header (format byte + 1..3 length bytes) for the given
// data type and element count into dst, returning the extended slice. It appends into dst rather
// than allocating a fresh buffer.
func appendHeaderBytes(dst []byte, dataType string, size int) ([]byte, error) {
	it, ok := itemTypeMap[dataType]
	if !ok {
		return dst, fmt.Errorf("invalid item type: %s", dataType)
	}

	dataByteLength, err := getDataByteLength(dataType, size)
	if err != nil {
		return dst, err
	}

	if dataByteLength > MaxByteSize {
		return dst, errors.New("size limit exceeded")
	}

	lenBytes := [3]byte{byte(dataByteLength >> 16), byte(dataByteLength >> 8), byte(dataByteLength)}
	lenByteCount := 3

	if lenBytes[0] == 0 {
		lenByteCount--
		if lenBytes[1] == 0 {
			lenByteCount--
		}
	}

	dst = append(dst, byte(it.FormatCode)<<2+byte(lenByteCount))
	dst = append(dst, lenBytes[3-lenByteCount:]...)

	return dst, nil
}

// getHeaderBytes returns a standalone header slice backed by a fresh buffer, for callers and tests
// that need a slice without an existing buffer to append into.
func getHeaderBytes(dataType string, size int, preAlloc int) ([]byte, error) {
	return appendHeaderBytes(make([]byte, 0, 4+preAlloc), dataType, size)
}

// headerLen returns the byte count of the SECS-II item header (format byte + 1..3 length bytes)
// for a payload of dataByteLength bytes. Used by concrete types' EncodedLen so ToBytes can
// pre-allocate exactly.
//
//nolint:unused // called by concrete types added in later tasks
func headerLen(dataByteLength int) int {
	switch {
	case dataByteLength > 0xFFFF:
		return 4 // format byte + 3 length bytes
	case dataByteLength > 0xFF:
		return 3 // format byte + 2 length bytes
	default:
		return 2 // format byte + 1 length byte (covers the 0-length case too)
	}
}
