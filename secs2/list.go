package secs2

import (
	"errors"
	"fmt"
	"strings"

	"github.com/arloliu/go-secs/internal/util"
)

// ListItem is a immutable data type that represents a list data in a SECS-II message.
//
// It implements the Item interface, providing methods to interact with and manipulate items in the list.
//
// It contains other item, and the size of ListItem is equal to the number
// of items it contains, counted *non-recursively*.
//
// The size of the ListItem in it's string representation, will be only specified when the size is deterministic,
// which means there is no ellipsis and Item variable.
type ListItem struct {
	baseItem
	values []Item // Array of Items that this ListItem contains
}

// NewListItem creates a new ListItem representing an ordered set of elemments in a SECS-II message.
//
// This function implements the Item interface.
//
// values: One or more values to be stored in the ListItem. Each value can be:
//   - ASCIIItem
//   - BinaryItem
//   - BooleanItem
//   - ListItem, UListItem
//   - FloatItem
//
// If the `byteSize` is invalid or any of the input values cannot be converted to a valid signed int64
// (or are outside the representable range for the given byteSize), an error is set on the item.
//
// The newly created ListItem is returned, potentially with an error attached.
func NewListItem(values ...Item) Item {
	item := getListItem()

	dataBytes, _ := getDataByteLength(ListType, len(values))
	if dataBytes > MaxByteSize {
		item.setErrorMsg("item size limit exceeded")
		return item
	}

	if cap(item.values) < len(values) {
		item.values = make([]Item, 0, len(values))
	} else {
		item.values = item.values[:0]
	}

	for _, value := range values {
		if value == nil {
			continue
		}
		item.values = append(item.values, value)
	}

	return item
}

// Free releases the ListItem back to the pool for reuse.
//
// After calling Free, the ListItem should not be accessed or used again, as its underlying memory
// might be reused for other ListItem objects.
//
// This method is essential for efficient memory management when working with a large number of ListItem objects.
func (item *ListItem) Free() {
	for _, val := range item.values {
		if val != nil {
			val.Free()
		}
	}
	item.values = []Item{}
	putListItem(item)
}

// Get retrieves the current ListItem.
//
// This method implements the Item.Get() interface.
func (item *ListItem) Get(indices ...int) (Item, error) {
	if len(indices) == 0 {
		return item, nil
	}

	var dataItem Item = item
	for _, idx := range indices {
		if !dataItem.IsList() {
			return nil, errors.New("failed to get nested item")
		}

		listItem, _ := dataItem.(*ListItem)
		if idx < 0 || idx >= listItem.Size() {
			return nil, errors.New("failed to get nested item")
		}
		dataItem = listItem.values[idx]
	}

	return dataItem, nil
}

// ToList retrieves list of items stored within the item.
//
// This method implements a specialized version of Item.Get() for items retrieval.
// It returns the Item slice containing items in the list.
func (item *ListItem) ToList() ([]Item, error) {
	return item.values, nil
}

// Size implements Item.Size().
func (item *ListItem) Size() int {
	return len(item.values)
}

// Values retrieves the items stored in the item as a Item slice.
//
// This method implements the Item.Values() interface. It returns a direct
// reference to the underlying byte slice, allowing for potential modification.
//
// Caution: Modifying the returned slice will directly affect the data within the item.
//
// The returned value can be type-asserted to a `[]Item`.
func (item *ListItem) Values() any {
	return item.values
}

// SetValues sets the item values within the ListItem.
//
// This method implements the Item.SetValues() interface. It accepts one or more values, each of which can be:
//   - ASCIIItem
//   - BinaryItem
//   - BooleanItem
//   - ListItem, UListItem
//   - FloatItem
//
// If an error occurs during the combination process (e.g., due to incompatible types),
// the error is returned and also stored within the item for later retrieval.
func (item *ListItem) SetValues(values ...any) error {
	item.resetError()

	dataBytes, _ := getDataByteLength(ListType, len(values))
	if dataBytes > MaxByteSize {
		item.setErrorMsg("item size limit exceeded")
		return item.Error()
	}

	if cap(item.values) < len(values) {
		item.values = make([]Item, 0, len(values))
	} else {
		item.values = item.values[:0]
	}

	for _, value := range values {
		if v, ok := value.(Item); ok {
			item.values = append(item.values, v)
		} else {
			item.setErrorMsg("invalid item type")
			return item.Error()
		}
	}

	return nil
}

// ToBytes serializes the ListItem into a byte slice conforming to the SECS-II data format.
//
// This method implements the Item.ToBytes() interface.
//
// If an error occurs during header generation, an empty byte slice is returned,
// and the error is stored within the item for later retrieval.
func (item *ListItem) ToBytes() []byte {
	result, _ := getHeaderBytes(ListType, len(item.values), 0)

	for _, value := range item.values {
		if value == nil {
			continue
		}
		// invoke ToBytes() of child item recursively
		nestedResult := value.ToBytes()
		if len(nestedResult) == 0 {
			return []byte{}
		}

		result = append(result, nestedResult...)
	}

	return result
}

// ToSML converts the ListItem into its SML representation.
//
// This method implements the Item.ToSML() interface. It generates an SML string
// that represents items and its nested items stored in the list .
func (item *ListItem) ToSML() string {
	return item.formatSML(0)
}

// Clone creates a shadow copy of the ListItem.
//
// This method implements the Item.Clone() interface. It returns a new
// ListItem with a shadow copy of the items.
//
// Note: It's unsafe to use the new cloned data for immutable operations.
func (item *ListItem) Clone() Item {
	return &ListItem{values: util.CloneSlice(item.values, 0)}
}

func (item *ListItem) Error() error {
	var errs error
	if item.baseItem.itemErr != nil {
		errs = errors.Join(errs, item.baseItem.itemErr)
	}

	for _, v := range item.values {
		if v != nil {
			errs = errors.Join(errs, v.Error())
		}
	}

	return errs
}

// Type returns "list" string.
func (item *ListItem) Type() string { return ListType }

// IsList returns true, indicating that ListItem is a list data item.
func (item *ListItem) IsList() bool { return true }

// formatSML returns the indented string representation of this list node.
// Each indent level adds 2 spaces as prefix to each line.
// The indent level should be non-negative.
func (item *ListItem) formatSML(level int) string {
	indentStr := strings.Repeat("  ", level)
	if item.Size() == 0 {
		return indentStr + "<L[0]>"
	}

	var sb strings.Builder
	sb.Grow(len(item.values) * 20)

	for _, value := range item.values {
		if v, ok := value.(*ListItem); ok {
			// Nested ListItem
			sb.WriteString(v.formatSML(level + 1))
			sb.WriteByte('\n')
		} else {
			// Child Item - Format and append
			sb.WriteString(indentStr)
			sb.WriteString("  ")
			sb.WriteString(value.ToSML())
			sb.WriteByte('\n')
		}
	}

	return fmt.Sprintf("%v<L[%d]\n%s%s>", indentStr, item.Size(), sb.String(), indentStr)
}
