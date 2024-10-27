package secs2

import (
	"errors"
	"fmt"
)

// MaxByteSize defines the maximum allowed size (in bytes) for an Item's data.
const MaxByteSize = 1<<24 - 1

const (
	EmptyType   = "empty"
	ListType    = "list"
	BinaryType  = "binary"
	BooleanType = "boolean"
	ASCIIType   = "ascii"
	Int8Type    = "i1"
	Int16Type   = "i2"
	Int32Type   = "i4"
	Int64Type   = "i8"
	Uint8Type   = "u1"
	Uint16Type  = "u2"
	Uint32Type  = "u4"
	Uint64Type  = "u8"
	Float32Type = "f4"
	Float64Type = "f8"
)

type FormatCode = int

const (
	ListFormatCode    FormatCode = 0o00
	BinaryFormatCode  FormatCode = 0o10
	BooleanFormatCode FormatCode = 0o11
	ASCIIFormatCode   FormatCode = 0o20
	Int64FormatCode   FormatCode = 0o30
	Int8FormatCode    FormatCode = 0o31
	Int16FormatCode   FormatCode = 0o32
	Int32FormatCode   FormatCode = 0o34
	Float64FormatCode FormatCode = 0o40
	Float32FormatCode FormatCode = 0o44
	Uint64FormatCode  FormatCode = 0o50
	Uint8FormatCode   FormatCode = 0o51
	Uint16FormatCode  FormatCode = 0o52
	Uint32FormatCode  FormatCode = 0o54
)

type ItemType struct {
	FormatCode FormatCode
	Size       int
}

var itemTypeMap = map[string]*ItemType{
	ListType:    {FormatCode: ListFormatCode, Size: 1},
	BinaryType:  {FormatCode: BinaryFormatCode, Size: 1},
	BooleanType: {FormatCode: BooleanFormatCode, Size: 1},
	ASCIIType:   {FormatCode: ASCIIFormatCode, Size: 1},
	Int64Type:   {FormatCode: Int64FormatCode, Size: 8},
	Int8Type:    {FormatCode: Int8FormatCode, Size: 1},
	Int16Type:   {FormatCode: Int16FormatCode, Size: 2},
	Int32Type:   {FormatCode: Int32FormatCode, Size: 4},
	Float64Type: {FormatCode: Float64FormatCode, Size: 8},
	Float32Type: {FormatCode: Float32FormatCode, Size: 4},
	Uint64Type:  {FormatCode: Uint64FormatCode, Size: 8},
	Uint8Type:   {FormatCode: Uint8FormatCode, Size: 1},
	Uint16Type:  {FormatCode: Uint16FormatCode, Size: 2},
	Uint32Type:  {FormatCode: Uint32FormatCode, Size: 4},
}

// Item represents an immutable data item in a SECS-II message.
//
// Items can hold various data types (e.g., binary, boolean, ASCII, integers, floats) and can
// be nested to form complex structures.
//
// There's a limit on the total size of data an Item can contain, as defined by the SEMI standard:
//
//	n * b <= 16,777,215 (3 bytes)
//	- n: number of data values within the Item (and its nested children)
//	- b: byte size to represent each individual data value (varies by Item type)
type Item interface {
	// Get retrieves a nested Item at the specified indices.
	// An error is returned if the item doesn't represent a list or if the indices are invalid.
	Get(indices ...int) (Item, error)

	// ToList retrieves a list of items stored within the item.
	// Only available for ListItem.
	ToList() ([]Item, error)

	// ToBinary retrieves binary data as a byte slice data stored within the item.
	// Only available for BinaryItem.
	ToBinary() ([]byte, error)

	// ToBoolean retrieves a list of boolean data stored within the item.
	// Only available for BooleanItem.
	ToBoolean() ([]bool, error)

	// ToASCII retrieves a nested ASCII string data stored within the item.
	// Only available for ASCIIItem.
	ToASCII() (string, error)

	// ToInt retrieves a list of signed 64-bit integer data stored within the item.
	// Only available for IntItem.
	ToInt() ([]int64, error)

	// ToUint retrieves a list of unsigned 64-bit integer data stored within the item.
	// Only available for UintItem.
	ToUint() ([]uint64, error)

	// ToFloat retrieves a list of 64-bit float data stored within the item.
	// Only available for FloatItem
	ToFloat() ([]float64, error)

	// Values returns the value(s) held by the Item.
	// The return type is `any`, and the actual type depends on the specific Item implementation.
	// It can be a single value or a list, depending on the item type.
	// Please refer to the documentation of the specific item type for details on the returned value's type.
	//
	// The returned data is the same as Get() without index argument.
	Values() any

	// SetValues sets the value(s) within the Item.
	// The accepted value types and behavior depend on the specific Item implementation.
	// It may return an error if the provided values are invalid.
	SetValues(values ...any) error

	// Size returns the list size of the data item, aka., the number of data items.
	Size() int

	// ToBytes serializes the Item into its byte representation for SECS-II message transmission.
	ToBytes() []byte

	// ToSML converts the Item into its SML (SECS Message Language) representation.
	ToSML() string

	// Clone creates a deep copy of the Item, allowing for safe modification without affecting the original.
	Clone() Item

	// Error returns any error that occurred during the creation or manipulation of the Item.
	Error() error

	// Free releases the item resource and put back to the corresponding pool.
	//
	// After calling Free, the item should not be accessed or used again, as its underlying memory
	// might be reused for other item objects.
	Free()

	Type() string

	IsEmpty() bool
	IsList() bool
	IsBinary() bool
	IsBoolean() bool
	IsASCII() bool
	IsInt8() bool
	IsInt16() bool
	IsInt32() bool
	IsInt64() bool
	IsUint8() bool
	IsUint16() bool
	IsUint32() bool
	IsUint64() bool
	IsFloat32() bool
	IsFloat64() bool
}

// A ItemError records a failed item creation.
type ItemError struct {
	err error
}

func newItemErrorWithMsg(errMsg string) *ItemError {
	return &ItemError{err: errors.New(errMsg)}
}

func newItemError(err error) *ItemError {
	itemErr := &ItemError{}

	if errors.As(err, &itemErr) {
		return &ItemError{err: errors.Unwrap(err)}
	}

	return &ItemError{err: err}
}

func (e *ItemError) Error() string {
	return e.err.Error()
}

func (e *ItemError) Unwrap() error {
	return e.err
}

// EmptyItem is a immutable data type that represents a empty data item item.
// It will be used mostly on error cases.
type EmptyItem struct {
	baseItem
}

type EmptyItemPtr = *EmptyItem

// NewEmptyItem creates a new empty data item item.
func NewEmptyItem() Item {
	return &EmptyItem{}
}

func (item *EmptyItem) Get(indices ...int) (Item, error) {
	if len(indices) != 0 {
		err := newItemError(fmt.Errorf("item is not a list, item is %s, indices is %v", item.ToSML(), indices))
		item.setError(err)
		return nil, err
	}

	return item, nil
}

func (item *EmptyItem) Size() int {
	return 0
}

func (item *EmptyItem) Values() any {
	return []string{}
}

func (item *EmptyItem) SetValues(_ ...any) error {
	return nil
}

func (item *EmptyItem) ToBytes() []byte {
	return []byte{}
}

// ToSML returns the SML representation of the item.
func (item *EmptyItem) ToSML() string {
	return ""
}

func (item *EmptyItem) Clone() Item {
	return &EmptyItem{}
}

func (item *EmptyItem) Type() string {
	return EmptyType
}

func (item *EmptyItem) IsEmpty() bool { return true }

// baseItem provides a partial implementation of the Item interface,
// focusing on optional methods and error handling.
//
// This struct serves as a convenient base for other Item implementations,
// allowing them to inherit default behavior for certain methods and centralizing
// error management.
//
// Note that baseItem does not implement all required Item methods.
// Concrete implementations must provide their own logic for the remaining methods.
type baseItem struct {
	itemErr error // Stores any error that occurred during item creation or manipulation
}

func (item *baseItem) ToList() ([]Item, error) {
	err := newItemErrorWithMsg("method GetList not implemented")
	item.setError(err)

	return nil, err
}

func (item *baseItem) ToBinary() ([]byte, error) {
	err := newItemErrorWithMsg("method GetBinary not implemented")
	item.setError(err)

	return nil, err
}

func (item *baseItem) ToBoolean() ([]bool, error) {
	err := newItemErrorWithMsg("method GetBoolean not implemented")
	item.setError(err)

	return nil, err
}

func (item *baseItem) ToASCII() (string, error) {
	err := newItemErrorWithMsg("method GetASCII not implemented")
	item.setError(err)

	return "", err
}

func (item *baseItem) ToInt() ([]int64, error) {
	err := newItemErrorWithMsg("method GetInt not implemented")
	item.setError(err)

	return nil, err
}

func (item *baseItem) ToUint() ([]uint64, error) {
	err := newItemErrorWithMsg("method GetUint not implemented")
	item.setError(err)

	return nil, err
}

func (item *baseItem) ToFloat() ([]float64, error) {
	err := newItemErrorWithMsg("method GetFloat not implemented")
	item.setError(err)

	return nil, err
}

func (item *baseItem) Error() error {
	return item.itemErr
}

func (item *baseItem) Free() {
}

func (item *baseItem) IsEmpty() bool   { return false }
func (item *baseItem) IsList() bool    { return false }
func (item *baseItem) IsBinary() bool  { return false }
func (item *baseItem) IsBoolean() bool { return false }
func (item *baseItem) IsASCII() bool   { return false }
func (item *baseItem) IsInt8() bool    { return false }
func (item *baseItem) IsInt16() bool   { return false }
func (item *baseItem) IsInt32() bool   { return false }
func (item *baseItem) IsInt64() bool   { return false }
func (item *baseItem) IsUint8() bool   { return false }
func (item *baseItem) IsUint16() bool  { return false }
func (item *baseItem) IsUint32() bool  { return false }
func (item *baseItem) IsUint64() bool  { return false }
func (item *baseItem) IsFloat32() bool { return false }
func (item *baseItem) IsFloat64() bool { return false }

func (item *baseItem) resetError() {
	item.itemErr = nil
}

func (item *baseItem) setError(err error) {
	item.itemErr = errors.Join(item.itemErr, &ItemError{err: err})
}

func (item *baseItem) setErrorMsg(errMsg string) {
	item.itemErr = errors.Join(item.itemErr, newItemErrorWithMsg(errMsg))
}

// getDataByteLength calculates the total number of bytes needed to represent data of a given type and size.
//
// Parameters:
//   - dataType: The type of data. Valid values include:
//   - "list": Represents a list.
//   - "binary": Represents binary data.
//   - "boolean": Represents boolean data.
//   - "ascii": Represents ASCII string data.
//   - "i1", "i2", "i4", "i8": Represents signed integers of various byte sizes.
//   - "u1", "u2", "u4", "u8": Represents unsigned integers of various byte sizes.
//   - "f4", "f8": Represents floating-point numbers of various byte sizes.
//   - size: The number of data values (or elements) in the item item.
//
// Returns:
//   - The total number of bytes required to represent the data.
//   - An error if the `dataType` is invalid.
func getDataByteLength(dataType string, size int) (int, error) {
	itemType, ok := itemTypeMap[dataType]
	if !ok {
		return 0, fmt.Errorf("invalid item type %s", dataType)
	}

	return size * itemType.Size, nil
}

// getHeaderBytes returns the header bytes, which consist of the format byte
// and the length bytes, of a SECS-II data item.
//
// The input argument dataType should be one of "list", "binary", "boolean", "ascii",
// "i8", "i1", "i2", "i4", "f8", "f4", "u8", "u1", "u2", or "u4".
// The input argument size means the number of values in a item item.
// An error is returned when the header bytes cannot be created.
func getHeaderBytes(dataType string, size int, preAlloc int) ([]byte, error) {
	itemType, ok := itemTypeMap[dataType]
	if !ok {
		return []byte{}, fmt.Errorf("invalid item type: %s", dataType)
	}
	formatCode := itemType.FormatCode

	dataByteLength, err := getDataByteLength(dataType, size)
	if err != nil {
		return []byte{}, err
	}

	if dataByteLength > MaxByteSize {
		return []byte{}, fmt.Errorf("size limit exceeded")
	}

	lenBytes := []byte{
		byte(dataByteLength >> 16),
		byte(dataByteLength >> 8),
		byte(dataByteLength),
	}

	// determine the number of length bytes needed
	lenByteCount := 3
	if lenBytes[0] == 0 {
		lenByteCount--
		if lenBytes[1] == 0 {
			lenByteCount--
		}
	}

	result := make([]byte, 0, 1+lenByteCount+preAlloc)
	result = append(result, byte(formatCode<<2+lenByteCount))
	result = append(result, lenBytes[3-lenByteCount:]...)

	return result, nil
}
