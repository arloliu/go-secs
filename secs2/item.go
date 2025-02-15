package secs2

import (
	"errors"
	"fmt"
)

// MaxByteSize defines the maximum allowed size (in bytes) for an Item's data.
const MaxByteSize = 1<<24 - 1

// Type contants defines SECS-II data item type strings.
const (
	EmptyType   = "empty"
	ListType    = "list"
	BinaryType  = "binary"
	BooleanType = "boolean"
	ASCIIType   = "ascii"
	JIS8Type    = "jis8"
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

// FormatCode defines constants representing data item format codes.
type FormatCode = int

// Format constants defines format codes of SECS-II data items
const (
	ListFormatCode    FormatCode = 0o00
	BinaryFormatCode  FormatCode = 0o10
	BooleanFormatCode FormatCode = 0o11
	ASCIIFormatCode   FormatCode = 0o20
	JIS8FormatCode    FormatCode = 0o21
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

type itemType struct {
	FormatCode FormatCode
	Size       int
}

var itemTypeMap = map[string]*itemType{
	ListType:    {FormatCode: ListFormatCode, Size: 1},
	BinaryType:  {FormatCode: BinaryFormatCode, Size: 1},
	BooleanType: {FormatCode: BooleanFormatCode, Size: 1},
	ASCIIType:   {FormatCode: ASCIIFormatCode, Size: 1},
	JIS8Type:    {FormatCode: JIS8FormatCode, Size: 1},
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
// It defines a common interface for various SECS-II data item types, including:
//   - List: A collection of other Items.
//   - Binary: A sequence of bytes.
//   - Boolean: A boolean value (true or false).
//   - ASCII: An ASCII string.
//   - Integer: Signed integers (I1, I2, I4, I8) with different byte sizes.
//   - Unsigned Integer: Unsigned integers (U1, U2, U4, U8) with different byte sizes.
//   - Float: Floating-point numbers (F4, F8) with different byte sizes.
//
// Immutability:
// Items are designed to be immutable. For operations that might modify the data, use the `Clone()` method
// to create a new, independent copy of the item before performing the operation.
//
// Size Limit:
// There's a limit on the total size of data an Item can contain, as defined by the SEMI standard:
//
//	n * b <= 16,777,215 (3 bytes)
//	where:
//	  - n: number of data values within the Item (and its nested children)
//	  - b: byte size to represent each individual data value (varies by Item type)
//
// This interface provides methods for:
//   - Accessing nested items: `Get(indices ...int)`
//   - Converting to specific data types: `ToList()`, `ToBinary()`, `ToBoolean()`, etc.
//   - Getting and setting values: `Values()`, `SetValues(...)`
//   - Serializing to bytes and SML: `ToBytes()`, `ToSML()`
//   - Cloning: `Clone()`
//   - Error handling: `Error()`
//   - Resource management: `Free()`
//   - Type checking: `Type()`, `IsEmpty()`, `IsList()`, `IsBinary()`, etc.
type Item interface {
	// Get retrieves a nested Item at the specified indices.
	// It returns an error if the item is not a list or if the indices are invalid.
	Get(indices ...int) (Item, error)

	// ToList retrieves the list of items if the item is a ListItem.
	// It returns an error if the item is not a list.
	ToList() ([]Item, error)

	// ToBinary retrieves the byte slice if the item is a BinaryItem.
	// It returns an error if the item is not a binary item.
	ToBinary() ([]byte, error)

	// ToBoolean retrieves the boolean values if the item is a BooleanItem.
	// It returns an error if the item is not a boolean item.
	ToBoolean() ([]bool, error)

	// ToASCII retrieves the ASCII string if the item is an ASCIIItem.
	// It returns an error if the item is not an ASCII item.
	ToASCII() (string, error)

	// ToJIS8 retrieves the JIS-8 string if the item is an JIS8Item.
	// It returns an error if the item is not an JIS-8 item.
	ToJIS8() (string, error)

	// ToInt retrieves the signed integer values as int64 if the item is an IntItem.
	// It returns an error if the item is not an integer item.
	ToInt() ([]int64, error)

	// ToUint retrieves the unsigned integer values as uint64 if the item is a UintItem.
	// It returns an error if the item is not an unsigned integer item.
	ToUint() ([]uint64, error)

	// ToFloat retrieves the floating-point values as float64 if the item is a FloatItem.
	// It returns an error if the item is not a float item.
	ToFloat() ([]float64, error)

	// Values returns the value(s) held by the Item.
	// The return type is any, and the actual type depends on the specific Item implementation.
	// Refer to the documentation of the specific item type for details on the returned value's type.
	Values() any

	// SetValues sets the value(s) within the Item.
	// The accepted value types and behavior depend on the specific Item implementation.
	// It may return an error if the provided values are invalid.
	SetValues(values ...any) error

	// Size returns the number of data values within the Item.
	Size() int

	// ToBytes serializes the Item into its byte representation for SECS-II message transmission.
	ToBytes() []byte

	// ToSML converts the Item into its SML (SECS Message Language) representation.
	ToSML() string

	// Clone creates a deep copy of the Item.
	Clone() Item

	// Error returns any error that occurred during the creation or manipulation of the Item.
	Error() error

	// Free releases the Item back to the pool for reuse.
	// After calling Free, the item should not be accessed or used again.
	Free()

	// Type returns the item type name.
	Type() string

	// IsEmpty returns true if the item is empty, false otherwise.
	IsEmpty() bool

	// IsList returns true if the item is a ListItem, false otherwise.
	IsList() bool

	// IsBinary returns true if the item is a BinaryItem, false otherwise.
	IsBinary() bool

	// IsBoolean returns true if the item is a BooleanItem, false otherwise.
	IsBoolean() bool

	// IsASCII returns true if the item is an ASCIIItem, false otherwise.
	IsASCII() bool

	// IsJIS8 returns true if the item is an JIS8Item, false otherwise.
	IsJIS8() bool

	// IsInt8 returns true if the item is an Int8Item, false otherwise.
	IsInt8() bool

	// IsInt16 returns true if the item is an Int16Item, false otherwise.
	IsInt16() bool

	// IsInt32 returns true if the item is an Int32Item, false otherwise.
	IsInt32() bool

	// IsInt64 returns true if the item is an Int64Item, false otherwise.
	IsInt64() bool

	// IsUint8 returns true if the item is a Uint8Item, false otherwise.
	IsUint8() bool

	// IsUint16 returns true if the item is a Uint16Item, false otherwise.
	IsUint16() bool

	// IsUint32 returns true if the item is a Uint32Item, false otherwise.
	IsUint32() bool

	// IsUint64 returns true if the item is a Uint64Item, false otherwise.
	IsUint64() bool

	// IsFloat32 returns true if the item is a Float32Item, false otherwise.
	IsFloat32() bool

	// IsFloat64 returns true if the item is a Float64Item, false otherwise.
	IsFloat64() bool
}

// A ItemError records a failed item creation.
type ItemError struct {
	err error
}

// newItemErrorWithMsg creates a new ItemError with the given error message.
func NewItemErrorWithMsg(errMsg string) *ItemError {
	return &ItemError{err: errors.New(errMsg)}
}

// newItemError creates a new ItemError from the given error.
// If the provided error is already an ItemError, it unwraps the underlying error to avoid nested ItemErrors.
func NewItemError(err error) *ItemError {
	itemErr := &ItemError{}

	if errors.As(err, &itemErr) {
		return &ItemError{err: errors.Unwrap(err)}
	}

	return &ItemError{err: err}
}

// Error returns the string representation of the underlying error.
func (e *ItemError) Error() string {
	return e.err.Error()
}

// Unwrap returns the underlying error.
func (e *ItemError) Unwrap() error {
	return e.err
}

// EmptyItem represents an empty data item in a SECS-II message.
// It is often used as a placeholder or in error cases where a valid data item cannot be provided.
//
// EmptyItem implements the Item interface, providing default implementations for the interface methods
// that are appropriate for an empty item.
type EmptyItem struct {
	baseItem
}

// NewEmptyItem creates a new empty data item item.
func NewEmptyItem() Item {
	return &EmptyItem{}
}

// Get retrieves a nested Item at the specified indices.
// Since EmptyItem represents an empty item, it returns itself if no indices are provided.
// If indices are provided, it returns an error indicating that the item is not a list.
func (item *EmptyItem) Get(indices ...int) (Item, error) {
	if len(indices) != 0 {
		err := NewItemError(fmt.Errorf("item is not a list, item is %s, indices is %v", item.ToSML(), indices))
		item.setError(err)
		return nil, err
	}

	return item, nil
}

// Size returns 0, as an EmptyItem has no size.
func (item *EmptyItem) Size() int {
	return 0
}

// Values returns an empty string slice, as an EmptyItem holds no values.
func (item *EmptyItem) Values() any {
	return []string{}
}

// SetValues does nothing and returns nil, as an EmptyItem cannot hold any values.
func (item *EmptyItem) SetValues(_ ...any) error {
	return nil
}

// ToBytes returns an empty byte slice, as an EmptyItem has no byte representation.
func (item *EmptyItem) ToBytes() []byte {
	return []byte{}
}

// ToSML returns an empty string, as an EmptyItem has no SML representation.
func (item *EmptyItem) ToSML() string {
	return ""
}

// Clone creates a new EmptyItem, effectively returning a copy of itself.
func (item *EmptyItem) Clone() Item {
	return &EmptyItem{}
}

// Type returns "empty" string, indicating the type of a empty data item.
func (item *EmptyItem) Type() string {
	return EmptyType
}

// IsEmpty returns true, as an EmptyItem is always empty.
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
	err := NewItemErrorWithMsg("method ToList not implemented")
	item.setError(err)

	return nil, err
}

func (item *baseItem) ToBinary() ([]byte, error) {
	err := NewItemErrorWithMsg("method ToBinary not implemented")
	item.setError(err)

	return nil, err
}

func (item *baseItem) ToBoolean() ([]bool, error) {
	err := NewItemErrorWithMsg("method ToBoolean not implemented")
	item.setError(err)

	return nil, err
}

func (item *baseItem) ToASCII() (string, error) {
	err := NewItemErrorWithMsg("method ToASCII not implemented")
	item.setError(err)

	return "", err
}

func (item *baseItem) ToJIS8() (string, error) {
	err := NewItemErrorWithMsg("method ToJIS8 not implemented")
	item.setError(err)

	return "", err
}

func (item *baseItem) ToInt() ([]int64, error) {
	err := NewItemErrorWithMsg("method ToInt not implemented")
	item.setError(err)

	return nil, err
}

func (item *baseItem) ToUint() ([]uint64, error) {
	err := NewItemErrorWithMsg("method ToUint not implemented")
	item.setError(err)

	return nil, err
}

func (item *baseItem) ToFloat() ([]float64, error) {
	err := NewItemErrorWithMsg("method ToFloat not implemented")
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
func (item *baseItem) IsJIS8() bool    { return false }
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
	item.itemErr = errors.Join(item.itemErr, NewItemErrorWithMsg(errMsg))
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
		return []byte{}, errors.New("size limit exceeded")
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
