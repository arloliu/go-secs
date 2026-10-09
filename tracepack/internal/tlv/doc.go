// Package tlv implements the TLV entry encoding of the tracepack format specification §5
// and the tag registries of the structures that use it.
//
// The same encoding carries the pack metadata, the payloads of transport-event and annotation records,
// the block summaries and pack statistics of the footer, and every nested tlv value.
// Decode checks only the entry structure, and keeps unknown tags and unknown value types as they are;
// Entries walks the same entries in place, without building a slice;
// the typed accessors of Entry check value contents such as the u64 limit and UTF-8 validity;
// Validate applies a registry to the known tags,
// checking their contents by the same rules and descending into nested tlv values.
//
// UUIDs are plain [16]byte values in RFC 9562 byte order.
package tlv
