// Package secs2 implements deeply immutable SECS-II (SEMI E5 §9) data items.
// All Item values are safe for concurrent use; no method exposes mutable internal storage.
//
// # Immutability contract
//
// Every Item is fully initialised at construction time and never mutated afterward.
// Accessors that return slices always return a freshly allocated copy of the internal data.
// Iterators yield values directly from internal storage without copying.
// Individual-element accessors return values by value, never by pointer or reference to
// internal state. AppendBinaryTo appends raw bytes into a caller-supplied buffer and never
// returns an internal slice.
//
// # Accessor families
//
// Three families of accessors cover different performance and convenience trade-offs:
//
//   - Copy accessors (ToList, ToBinary, ToBoolean, ToInt, ToUint, ToFloat, ToASCII, ToJIS8,
//     ToLocalizedStr, ToLocalizedStrHeader) return a fresh, caller-owned copy of the data.
//
//   - Zero-copy iterators (Items, Bools, Ints, Uints, Floats) return an iter.Seq that yields
//     values directly from internal storage; the sequence is safe to call concurrently.
//     Iterating allocates nothing when called on the concrete item type; calling through the
//     Item interface costs a few allocations from interface dispatch (still cheaper than
//     copying a large slice via the copy accessors).
//
//   - Indexed accessors (ItemAt, ByteAt, BoolAt, IntAt, UintAt, FloatAt) return a single
//     value by value at the given index.
//
// # Wire encoding
//
// AppendTo(dst []byte) appends the complete SECS-II wire encoding (header + payload) into dst
// and returns the extended slice. No allocation occurs when dst already has sufficient capacity.
//
// ToBytes() is a convenience wrapper equivalent to AppendTo(make([]byte, 0, EncodedLen())).
//
// EncodedLen() returns the exact total byte count, enabling callers to pre-size a buffer before
// calling AppendTo to avoid any allocation during encoding.
//
// # Construction
//
// Short-form constructors are provided for each type:
//
//	L(children...)         // ListItem
//	A(s)                   // ASCIIItem
//	J(s)                   // JIS8Item
//	W(s)                   // LocalizedStrItem (UTF-8 encoding)
//	B(values...)           // BinaryItem
//	BOOLEAN(values...)     // BooleanItem
//	I1(values...)          // IntItem — 1-byte signed integers
//	I2(values...)          // IntItem — 2-byte signed integers
//	I4(values...)          // IntItem — 4-byte signed integers
//	I8(values...)          // IntItem — 8-byte signed integers
//	U1(values...)          // UintItem — 1-byte unsigned integers
//	U2(values...)          // UintItem — 2-byte unsigned integers
//	U4(values...)          // UintItem — 4-byte unsigned integers
//	U8(values...)          // UintItem — 8-byte unsigned integers
//	F4(values...)          // FloatItem — 4-byte (single-precision) floats
//	F8(values...)          // FloatItem — 8-byte (double-precision) floats
//
// Full constructors (NewListItem, NewASCIIItem, NewBinaryItem, NewBooleanItem, NewIntItem,
// NewUintItem, NewFloatItem, NewJIS8Item, NewLocalizedStrItem, NewUTF8StrItem, NewEmptyItem)
// are also available.
//
// Constructors never panic. Out-of-range or invalid arguments store a deferred error on the
// returned item; callers should check Error() before use:
//
//	item := I4(1, 2, 3)
//	if err := item.Error(); err != nil {
//	    // handle construction error
//	}
//
// Out-of-range numeric values are clamped to the representable range for the target type;
// unsupported argument types store a deferred error instead.
//
// # Decoding
//
// Decode parses a single SECS-II item from its wire bytes:
//
//	item, err := secs2.Decode(wireBytes)
//
// Decode copies the input bytes (the caller may reuse or free the buffer immediately after the
// call returns). Empty input yields NewEmptyItem(). List nesting is capped at MaxListDepth to
// prevent stack exhaustion on adversarial input. An error is returned for any malformed input
// (unknown format code, truncated header or payload, etc.).
package secs2
