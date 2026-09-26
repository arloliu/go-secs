package secs2

import "fmt"

// LSH constants define the Localized String Header encoding scheme identifiers as specified in SEMI E5 Table 2.
//
// LSHNone (code 0) is reserved by SEMI E5 and must not be sent; see IsReservedLSH.
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

// IsReservedLSH reports whether lsh is an encoding code SEMI E5 reserves:
// 0 ("none") and 15 through 32767, which are reserved for future expansion.
// Codes 1 through 14 are the defined encodings, and 32768 through 65535 are available for custom purposes.
//
// A reserved code must not be sent.
// NewLocalizedStrItem does not check it, and decoding accepts any code a peer sends,
// so call this to detect one on either side.
func IsReservedLSH(lsh uint16) bool {
	return lsh == LSHNone || (lsh >= 15 && lsh <= 32767)
}

// LocalizedStrItem represents an immutable Localized Character String data item in a SECS-II message.
//
// It includes a 16-bit Localized String Header (LSH) indicating the encoding scheme,
// except in its zero-length form (see NewEmptyLocalizedStrItem), which carries no LSH and no text.
//
// It implements the Item interface.
// All methods are safe for concurrent use. The backing fields are an immutable uint16 LSH and an immutable Go string,
// so reads require no copy and never expose mutable internal state.
//
// Wire layout: [format_byte][len_byte(s)][lsh_hi][lsh_lo][string_bytes...] The length field encodes len(value)+2 —
// the +2 accounts for the two LSH header bytes.
// The zero-length form is just [format_byte][0x00].
type LocalizedStrItem struct {
	baseItem
	lsh   uint16
	value string
	// noLSH marks the zero-length form: no LSH and no text on the wire.
	noLSH bool
}

var _ Item = (*LocalizedStrItem)(nil)

// NewLocalizedStrItem creates a new LocalizedStrItem with the given LSH and string value.
//
// The LSH is not validated.
// SEMI E5 reserves codes 0 (LSHNone) and 15 through 32767, and they must not be sent;
// check a code with IsReservedLSH before passing it here.
//
// If len(value)+2 exceeds MaxByteSize, a deferred error is stored on the returned item; call Error() to inspect it.
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

// NewEmptyLocalizedStrItem creates a zero-length LocalizedStrItem,
// encoded as the two bytes 0x49 0x00: a format-22 item header with length 0, and no LSH or text.
//
// SEMI E5 gives a zero-length item whatever meaning the message definition assigns,
// such as "not supplied".
// It differs from NewLocalizedStrItem(lsh, ""), which still sends the 2-byte LSH,
// and from NewEmptyItem, which is a sentinel that encodes to no bytes at all.
func NewEmptyLocalizedStrItem() Item {
	return &LocalizedStrItem{noLSH: true}
}

// NewUTF8StrItem is a convenience constructor that creates a LocalizedStrItem using the UTF-8 encoding scheme (LSH = LSHUTF8 = 2).
//
// Parameters:
//   - value: the input Go string.
//
// Returns:
//   - Item: the created LocalizedStrItem using UTF-8 encoding.
func NewUTF8StrItem(value string) Item {
	return NewLocalizedStrItem(LSHUTF8, value)
}

// ToLocalizedStr returns the string value stored in this item.
//
// Returns an error if the item carries a deferred construction error.
//
// It returns the item's deferred error (see Error) when the item was constructed with one —
// a passing Is* predicate does NOT imply a nil error here.
// Always check the returned error; do not discard it via `v, _ := item.ToLocalizedStr()`.
func (item *LocalizedStrItem) ToLocalizedStr() (string, error) {
	if item.itemErr != nil {
		return "", item.itemErr
	}

	return item.value, nil
}

// ToLocalizedStrHeader returns the LSH (Localized String Header) value.
//
// Returns an error if the item carries a deferred construction error.
//
// A zero-length item has no LSH and reports 0,
// the same value as an item that carries LSH 0.
// Tell them apart with HasLocalizedStrHeader, or through the Item interface with Size:
// it is 0 for the zero-length item and at least 2 for any item with an LSH.
//
// It returns the item's deferred error (see Error) when the item was constructed with one —
// a passing Is* predicate does NOT imply a nil error here.
// Always check the returned error; do not discard it via `v, _ := item.ToLocalizedStrHeader()`.
func (item *LocalizedStrItem) ToLocalizedStrHeader() (uint16, error) {
	if item.itemErr != nil {
		return 0, item.itemErr
	}

	return item.lsh, nil
}

// HasLocalizedStrHeader reports whether the item carries an LSH.
// It is false only for the zero-length form (see NewEmptyLocalizedStrItem).
func (item *LocalizedStrItem) HasLocalizedStrHeader() bool { return !item.noLSH }

// Size returns the total byte count of the wire payload: len(value)+2 (the +2 for the LSH),
// or 0 for the zero-length form.
func (item *LocalizedStrItem) Size() int {
	if item.noLSH {
		return 0
	}

	return len(item.value) + 2
}

// Type returns "localized_str".
func (item *LocalizedStrItem) Type() string { return LocalizedStrType }

// IsLocalizedStr returns true.
//
// It reflects the DECLARED type only and does not consult the item's deferred error:
// a true result does not imply the item is usable.
// Gate value extraction on Error() (or the To* accessor's returned error), not on Is* alone.
func (item *LocalizedStrItem) IsLocalizedStr() bool { return true }

// EncodedLen returns the total SECS-II wire byte length (header + payload).
//
// Returns 0 for items with deferred errors.
func (item *LocalizedStrItem) EncodedLen() int {
	if item.itemErr != nil {
		return 0
	}

	if item.rawPtr != nil {
		return item.rawLen
	}

	n := item.Size()

	return headerLen(n) + n
}

// AppendTo appends the SECS-II wire encoding of this item into dst and returns the result.
//
// Wire layout: [format_byte][len_byte(s)][lsh_hi][lsh_lo][string_bytes...] Returns dst unchanged for items with deferred errors.
func (item *LocalizedStrItem) AppendTo(dst []byte) []byte {
	if item.itemErr != nil {
		return dst
	}

	if item.rawPtr != nil {
		return append(dst, item.raw()...)
	}

	if item.noLSH {
		dst, _ = appendHeaderBytesFC(dst, LocalizedStrFormatCode, 0) //nolint:errcheck

		return dst
	}

	// The length field covers both the 2-byte LSH and the string bytes.
	n := len(item.value) + 2
	dst, _ = appendHeaderBytesFC(dst, LocalizedStrFormatCode, n) //nolint:errcheck
	dst = append(dst, byte(item.lsh>>8), byte(item.lsh))

	return append(dst, item.value...)
}

// ToBytes allocates a single buffer and returns the SECS-II wire encoding.
//
// Equivalent to AppendTo(make([]byte, 0, EncodedLen())).
func (item *LocalizedStrItem) ToBytes() []byte {
	return item.AppendTo(make([]byte, 0, item.EncodedLen()))
}

// ToSML returns the SML (SECS Message Language) text representation of this item.
//
// Format: <W "value">, or <W[0]> for the zero-length form.
// The LSH is not reflected in the SML representation.
// Special characters in value are escaped using Go's %q quoting.
func (item *LocalizedStrItem) ToSML() string {
	if item.noLSH {
		return "<W[0]>"
	}

	return fmt.Sprintf("<W %q>", item.value)
}
