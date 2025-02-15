package sml

import (
	"errors"
	"fmt"

	"github.com/arloliu/go-secs/secs2"
)

// RawSMLItem represents a raw SML data item.
// It's used to store raw SML data in a SECS-II message for lazy parsing and evaluation.
type RawSMLItem struct {
	itemErr    error
	value      []byte
	strictMode bool
	item       secs2.Item
}

var _ secs2.Item = (*RawSMLItem)(nil)

// NewRawSMLItem creates a new RawSMLItem containing the given raw SML data.
func NewRawSMLItem(value []byte, strictMode bool) secs2.Item {
	return &RawSMLItem{value: value, strictMode: strictMode}
}

// Free does nothing for RawSMLItem
func (item *RawSMLItem) Free() {}

// Get implements Item.Get().
//
// It does not accept any index arguments as ASCIIItem represents a single item, not a list.
//
// If any indices are provided, an error is returned indicating the item is not a list.
func (item *RawSMLItem) Get(indices ...int) (secs2.Item, error) {
	if len(indices) != 0 {
		err := fmt.Errorf("item is not a list, it is a raw SML item, indices is %v", indices)
		item.setError(err)
		return nil, err
	}

	return item, nil
}

// Values retrieves the SML string value as the any data format stored in the item.
//
// This method implements the Item.Values() interface. It returns the
// underlying SML string value associated with the item.
//
// The returned value can be type-asserted to a `[]byte`.
func (item *RawSMLItem) Values() any {
	return item.value
}

// SetValues sets the SML string for the item.
//
// This method implements the Item.SetValues() interface.
// It accepts one or more values, which must be of type `string` or `[]byte`.
// All provided string values are concatenated and stored within the item.
//
// If any of the provided values are not of type `string`, an error is returned
// and also stored within the item for later retrieval.
func (item *RawSMLItem) SetValues(values ...any) error {
	item.resetError()

	var itemValue []byte

	switch len(values) {
	case 0:
		itemValue = []byte{}

	case 1:

		switch val := values[0].(type) {
		case string:
			itemValue = secs2.StringToBytes(val)
		case []byte:
			itemValue = val
		default:
			err := secs2.NewItemErrorWithMsg("the value is not a string or []byte")
			item.setError(err)
			return err
		}
	default:
		for _, value := range values {
			switch val := value.(type) {
			case string:
				itemValue = append(itemValue, secs2.StringToBytes(val)...)
			case []byte:
				itemValue = append(itemValue, val...)
			default:
				err := secs2.NewItemErrorWithMsg("the value is not a string or []byte")
				item.setError(err)
				return err
			}
		}
	}

	item.value = itemValue

	return nil
}

// Size implements Item.Size().
func (item *RawSMLItem) Size() int {
	return len(item.value)
}

// ToList retrieves the list of items if the item is a ListItem.
// It returns an error if the item is not a list.
func (item *RawSMLItem) ToList() ([]secs2.Item, error) {
	if err := item.parse(); err != nil {
		return nil, err
	}

	return item.item.ToList()
}

// ToBinary retrieves the byte slice if the item is a BinaryItem.
// It returns an error if the item is not a binary item.
func (item *RawSMLItem) ToBinary() ([]byte, error) {
	if err := item.parse(); err != nil {
		return nil, err
	}

	return item.item.ToBinary()
}

// ToBoolean retrieves the boolean values if the item is a BooleanItem.
// It returns an error if the item is not a boolean item.
func (item *RawSMLItem) ToBoolean() ([]bool, error) {
	if err := item.parse(); err != nil {
		return nil, err
	}

	return item.item.ToBoolean()
}

// ToASCII retrieves the ASCII string if the item is an ASCIIItem.
// It returns an error if the item is not an ASCII item.
func (item *RawSMLItem) ToASCII() (string, error) {
	if err := item.parse(); err != nil {
		return "", err
	}

	return item.item.ToASCII()
}

// ToJIS8 retrieves the JIS-8 string if the item is an JIS8Item.
// It returns an error if the item is not an JIS-8 item.
func (item *RawSMLItem) ToJIS8() (string, error) {
	if err := item.parse(); err != nil {
		return "", err
	}

	return item.item.ToJIS8()
}

// ToInt retrieves the signed integer values as int64 if the item is an IntItem.
// It returns an error if the item is not an integer item.
func (item *RawSMLItem) ToInt() ([]int64, error) {
	if err := item.parse(); err != nil {
		return nil, err
	}

	return item.item.ToInt()
}

// ToUint retrieves the unsigned integer values as uint64 if the item is a UintItem.
// It returns an error if the item is not an unsigned integer item.
func (item *RawSMLItem) ToUint() ([]uint64, error) {
	if err := item.parse(); err != nil {
		return nil, err
	}

	return item.item.ToUint()
}

// ToFloat retrieves the floating-point values as float64 if the item is a FloatItem.
// It returns an error if the item is not a float item.
func (item *RawSMLItem) ToFloat() ([]float64, error) {
	if err := item.parse(); err != nil {
		return nil, err
	}

	return item.item.ToFloat()
}

// ToBytes returns the raw SML data stored within the item.
//
// This method implements the Item.ToBytes() interface.
func (item *RawSMLItem) ToBytes() []byte {
	if err := item.parse(); err != nil {
		return nil
	}

	return item.item.ToBytes()
}

// ToSML retrieves the SML data stored within the item.
//
// This method implements the Item.ToSML() interface.
// It returns the underlying SML data associated with the item.
func (item *RawSMLItem) ToSML() string {
	if err := item.parse(); err != nil {
		return ""
	}

	return item.item.ToSML()
}

// Clone returns a deep copy of the item.
//
// This method implements the Item.Clone() interface.
func (item *RawSMLItem) Clone() secs2.Item {
	newVal := make([]byte, len(item.value))
	copy(newVal, item.value)
	return NewRawSMLItem(newVal, item.strictMode)
}

// Type returns the actual type of the item.
func (item *RawSMLItem) Type() string {
	if err := item.parse(); err != nil {
		return "sml"
	}

	return item.item.Type()
}

func (item *RawSMLItem) IsEmpty() bool {
	if err := item.parse(); err != nil {
		return false
	}

	return item.item.IsEmpty()
}

func (item *RawSMLItem) IsList() bool {
	if err := item.parse(); err != nil {
		return false
	}

	return item.item.IsList()
}

func (item *RawSMLItem) IsBinary() bool {
	if err := item.parse(); err != nil {
		return false
	}

	return item.item.IsBinary()
}

func (item *RawSMLItem) IsBoolean() bool {
	if err := item.parse(); err != nil {
		return false
	}

	return item.item.IsBoolean()
}

func (item *RawSMLItem) IsASCII() bool {
	if err := item.parse(); err != nil {
		return false
	}

	return item.item.IsASCII()
}

func (item *RawSMLItem) IsJIS8() bool {
	if err := item.parse(); err != nil {
		return false
	}

	return item.item.IsJIS8()
}

func (item *RawSMLItem) IsInt8() bool {
	if err := item.parse(); err != nil {
		return false
	}

	return item.item.IsInt8()
}

func (item *RawSMLItem) IsInt16() bool {
	if err := item.parse(); err != nil {
		return false
	}

	return item.item.IsInt16()
}

func (item *RawSMLItem) IsInt32() bool {
	if err := item.parse(); err != nil {
		return false
	}

	return item.item.IsInt32()
}

func (item *RawSMLItem) IsInt64() bool {
	if err := item.parse(); err != nil {
		return false
	}

	return item.item.IsInt64()
}

func (item *RawSMLItem) IsUint8() bool {
	if err := item.parse(); err != nil {
		return false
	}

	return item.item.IsUint8()
}

func (item *RawSMLItem) IsUint16() bool {
	if err := item.parse(); err != nil {
		return false
	}

	return item.item.IsUint16()
}

func (item *RawSMLItem) IsUint32() bool {
	if err := item.parse(); err != nil {
		return false
	}

	return item.item.IsUint32()
}

func (item *RawSMLItem) IsUint64() bool {
	if err := item.parse(); err != nil {
		return false
	}

	return item.item.IsUint64()
}

func (item *RawSMLItem) IsFloat32() bool {
	if err := item.parse(); err != nil {
		return false
	}

	return item.item.IsFloat32()
}

func (item *RawSMLItem) IsFloat64() bool {
	if err := item.parse(); err != nil {
		return false
	}

	return item.item.IsFloat64()
}

// Error returns any error that occurred during the creation or manipulation of the Item.
func (item *RawSMLItem) Error() error {
	return item.itemErr
}

func (item *RawSMLItem) parse() error {
	if item.item != nil {
		return nil
	}

	if len(item.value) == 0 {
		item.item = secs2.NewEmptyItem()
		return nil
	}

	p := NewHSMSParser()
	p.WithStrictMode(item.strictMode)

	input := secs2.BytesToString(item.value)
	p.input = input
	p.data = input
	p.len = len(input)
	p.pos = 0

	parsedItem, err := p.parseText()
	if err != nil {
		return err
	}

	item.item = parsedItem

	return nil
}

func (item *RawSMLItem) resetError() {
	item.itemErr = nil
}

func (item *RawSMLItem) setError(err error) {
	item.itemErr = errors.Join(item.itemErr, secs2.NewItemError(err))
}
