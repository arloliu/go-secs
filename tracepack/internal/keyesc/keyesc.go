// Package keyesc holds the spelling rules a storage key and a bus subject share
// (the tracepack storage specification §3, "Keys", and §4, "Bus message"):
// the tool escape, the tools no key can hold, and the one spelling of a UUID.
//
// The bus subject's tool token is defined as the key's tool escape with '.' also escaped,
// so both the root package and the bus subpackage take these rules from here rather than keeping copies that could drift.
package keyesc

import (
	"fmt"

	"github.com/arloliu/go-secs/tracepack/internal/format"
)

// UpperHex holds the hex digits an escape uses, %XX with uppercase digits.
const UpperHex = "0123456789ABCDEF"

// IsUnreserved reports whether c is in the RFC 3986 unreserved set, which a key keeps unescaped.
func IsUnreserved(c byte) bool {
	return 'A' <= c && c <= 'Z' || 'a' <= c && c <= 'z' || '0' <= c && c <= '9' || c == '-' || c == '.' || c == '_' || c == '~'
}

// CheckTool reports an error for a tool no key can hold: "", and "." and "..",
// which would be path components of their own since '.' is not escaped.
// The error states only the reason; its caller wraps it with its own sentinel.
func CheckTool(tool string) error {
	if tool == "" || tool == "." || tool == ".." {
		return fmt.Errorf("tool %q cannot be stored", tool)
	}

	return nil
}

// ParseUUID parses a UUID of a key or a bus message id, which only the canonical lowercase form spells.
func ParseUUID(s string) (format.UUID, error) {
	u, err := format.ParseUUID(s)
	if err != nil {
		return format.UUID{}, err
	}
	if u.String() != s {
		return format.UUID{}, fmt.Errorf("UUID %q not in lowercase", s)
	}

	return u, nil
}
