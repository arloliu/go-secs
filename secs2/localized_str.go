package secs2

import "fmt"

// LSH Constants corresponding to Table 2 encoding schemes
const (
	LSHNone                    uint16 = 0
	LSHUCS2                    uint16 = 1
	LSHUTF8                    uint16 = 2
	LSHASCII                   uint16 = 3
	LSHISO88591                uint16 = 4 // ISO Latin-1
	LSHISO885911               uint16 = 5 // Thai (proposed)
	LSHTIS620                  uint16 = 6 // Thai
	LSHISCII                   uint16 = 7
	LSHShiftJIS                uint16 = 8
	LSHJapaneseEUC             uint16 = 9
	LSHKoreanEUC               uint16 = 10
	LSHSimplifiedChineseGB     uint16 = 11
	LSHSimplifiedChineseEUCCN  uint16 = 12 // Simplified Chinese EUC-CN
	LSHTraditionalChineseBig5  uint16 = 13 // Traditional Chinese Big5
	LSHTraditionalChineseEUCTW uint16 = 14 // Traditional Chinese EUC-TW
)

// LocalizedStrItem represents a Localized Character String data item in a SECS-II message.
// It includes a 16-bit Localized String Header (LSH) indicating the encoding scheme.
type LocalizedStrItem struct {
	baseItem
	lsh   uint16
	value string
}

// NewLocalizedStrItem creates a new LocalizedStrItem with the given LSH and string value.
func NewLocalizedStrItem(lsh uint16, value string) Item {
	size := len(value) + 2
	if size > MaxByteSize {
		return &LocalizedStrItem{
			baseItem: baseItem{itemErr: NewItemErrorWithMsg(fmt.Sprintf("Item string is too long: %d > %d", size, MaxByteSize))},
		}
	}

	return &LocalizedStrItem{lsh: lsh, value: value}
}

// NewUTF8StrItem is a convenience function that creates a new LocalizedStrItem using UTF-8 encoding (LSH = 2).
func NewUTF8StrItem(value string) Item {
	return NewLocalizedStrItem(LSHUTF8, value)
}

func (item *LocalizedStrItem) Get(indices ...int) (Item, error) {
	if len(indices) != 0 {
		err := NewItemError(fmt.Errorf("item is not a list, item is %s, indices is %v", item.ToSML(), indices))
		item.setError(err)
		return nil, err
	}

	return item, nil
}

func (item *LocalizedStrItem) ToLocalizedStr() (string, error) {
	return item.value, nil
}

func (item *LocalizedStrItem) ToLocalizedStrHeader() (uint16, error) {
	return item.lsh, nil
}

func (item *LocalizedStrItem) Values() any {
	return []string{item.value}
}

func (item *LocalizedStrItem) SetValues(values ...any) error {
	if len(values) == 0 {
		return nil
	}
	if len(values) > 1 {
		return fmt.Errorf("LocalizedStrItem can only hold one string, got %d", len(values))
	}

	val, ok := values[0].(string)
	if !ok {
		return fmt.Errorf("invalid type for LocalizedStrItem value: expected string, got %T", values[0])
	}
	item.value = val

	return nil
}

// SetLSH sets the Localized String Header value for the item.
func (item *LocalizedStrItem) SetLSH(lsh uint16) {
	item.lsh = lsh
}

func (item *LocalizedStrItem) Size() int {
	return len(item.value) + 2 // include 2 bytes for the LSH
}

func (item *LocalizedStrItem) ToBytes() []byte {
	size := item.Size()

	headerBytes, err := getHeaderBytes(LocalizedStrType, size, size)
	if err != nil {
		item.setError(fmt.Errorf("failed to get header bytes: %w", err))
		return nil
	}

	headerBytes = append(headerBytes, byte(item.lsh>>8), byte(item.lsh))
	headerBytes = append(headerBytes, []byte(item.value)...)

	return headerBytes
}

func (item *LocalizedStrItem) ToSML() string {
	// Generating <W "value">. The LSH is implicitly defaulted (typically UTF-8).
	// When parsing SML it will default back to LSH=2 unless a specific format requires it.
	return fmt.Sprintf("<W %q>", item.value)
}

func (item *LocalizedStrItem) Clone() Item {
	return &LocalizedStrItem{
		baseItem: baseItem{itemErr: item.itemErr},
		lsh:      item.lsh,
		value:    item.value,
	}
}

func (item *LocalizedStrItem) Type() string {
	return LocalizedStrType
}

func (item *LocalizedStrItem) IsLocalizedStr() bool { return true }
