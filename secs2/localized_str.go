package secs2

import "fmt"

// LSH constants define the Localized String Header encoding scheme identifiers
// as specified in SEMI E5 Table 2.
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

// LocalizedStrItem represents an immutable Localized Character String data item in a SECS-II
// message. It includes a 16-bit Localized String Header (LSH) indicating the encoding scheme.
//
// It implements the Item interface. All methods are safe for concurrent use. The backing
// fields are an immutable uint16 LSH and an immutable Go string, so reads require no copy
// and never expose mutable internal state.
//
// Wire layout: [format_byte][len_byte(s)][lsh_hi][lsh_lo][string_bytes...]
// The length field encodes len(value)+2 — the +2 accounts for the two LSH header bytes.
type LocalizedStrItem struct {
	baseItem
	lsh   uint16
	value string
}

var _ Item = (*LocalizedStrItem)(nil)

// NewLocalizedStrItem creates a new LocalizedStrItem with the given LSH and string value.
//
// If len(value)+2 exceeds MaxByteSize, a deferred error is stored on the returned item;
// call Error() to inspect it.
//
// Parameters:
//   - lsh: the language/character-set header value.
//   - value: the input Go string.
//
// Returns:
//   - Item: the created LocalizedStrItem.
func NewLocalizedStrItem(lsh uint16, value string) Item {
	item := &LocalizedStrItem{}

	if len(value)+2 > MaxByteSize {
		item.setErrorMsg(fmt.Sprintf("item string is too long: %d > %d", len(value)+2, MaxByteSize))

		return item
	}

	item.lsh = lsh
	item.value = value

	return item
}

// NewUTF8StrItem is a convenience constructor that creates a LocalizedStrItem using the UTF-8
// encoding scheme (LSH = LSHUTF8 = 2).
//
// Parameters:
//   - value: the input Go string.
//
// Returns:
//   - Item: the created LocalizedStrItem using UTF-8 encoding.
func NewUTF8StrItem(value string) Item {
	return NewLocalizedStrItem(LSHUTF8, value)
}

// ToLocalizedStr returns the string value stored in this item. Returns an error if the item
// carries a deferred construction error.
func (item *LocalizedStrItem) ToLocalizedStr() (string, error) {
	if item.itemErr != nil {
		return "", item.itemErr
	}

	return item.value, nil
}

// ToLocalizedStrHeader returns the LSH (Localized String Header) value. Returns an error if
// the item carries a deferred construction error.
func (item *LocalizedStrItem) ToLocalizedStrHeader() (uint16, error) {
	if item.itemErr != nil {
		return 0, item.itemErr
	}

	return item.lsh, nil
}

// Size returns the total byte count of the wire payload: len(value)+2 (the +2 for the LSH).
func (item *LocalizedStrItem) Size() int { return len(item.value) + 2 }

// Type returns "localized_str".
func (item *LocalizedStrItem) Type() string { return LocalizedStrType }

// IsLocalizedStr returns true.
func (item *LocalizedStrItem) IsLocalizedStr() bool { return true }

// EncodedLen returns the total SECS-II wire byte length (header + payload).
// Returns 0 for items with deferred errors.
func (item *LocalizedStrItem) EncodedLen() int {
	if item.itemErr != nil {
		return 0
	}

	if item.raw != nil {
		return len(item.raw)
	}

	n := len(item.value) + 2

	return headerLen(n) + n
}

// AppendTo appends the SECS-II wire encoding of this item into dst and returns the result.
// Wire layout: [format_byte][len_byte(s)][lsh_hi][lsh_lo][string_bytes...]
// Returns dst unchanged for items with deferred errors.
func (item *LocalizedStrItem) AppendTo(dst []byte) []byte {
	if item.itemErr != nil {
		return dst
	}

	if item.raw != nil {
		return append(dst, item.raw...)
	}

	// The length field covers both the 2-byte LSH and the string bytes.
	n := len(item.value) + 2
	dst, _ = appendHeaderBytes(dst, LocalizedStrType, n) //nolint:errcheck
	dst = append(dst, byte(item.lsh>>8), byte(item.lsh))

	return append(dst, item.value...)
}

// ToBytes allocates a single buffer and returns the SECS-II wire encoding.
// Equivalent to AppendTo(make([]byte, 0, EncodedLen())).
func (item *LocalizedStrItem) ToBytes() []byte {
	return item.AppendTo(make([]byte, 0, item.EncodedLen()))
}

// ToSML returns the SML (SECS Message Language) text representation of this item.
//
// Format: <W "value">. The LSH is not reflected in the SML representation.
// Special characters in value are escaped using Go's %q quoting.
func (item *LocalizedStrItem) ToSML() string {
	return fmt.Sprintf("<W %q>", item.value)
}
