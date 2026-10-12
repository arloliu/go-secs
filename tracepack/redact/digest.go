package redact

import (
	"crypto/hmac"
	"crypto/sha256"
	"encoding/binary"
	"errors"
	"fmt"
	"hash"
	"unicode/utf8"
)

// DigestSize is the length of a digest, the HMAC-SHA-256 output.
const DigestSize = sha256.Size

// DomainMessageText is the domain of a whole-text mask (the tracepack semantics specification §8 Masking).
const DomainMessageText = "message-text"

// maxDomainLen is the longest domain, in bytes.
const maxDomainLen = 65535

// ErrDomain reports a domain that is not 1 to 65535 bytes of UTF-8.
var ErrDomain = errors.New("redact: a domain is not 1 to 65535 bytes of UTF-8")

// Digest computes the digest of an entry (the tracepack semantics specification §8 Digest):
// HMAC-SHA-256 under key over the domain's byte length as a little-endian u16, the domain's bytes, then original.
// Digests are comparable only when their key id and domain are equal.
//
// Parameters:
//   - key: the digest key.
//   - domain: the digest domain, 1 to 65535 bytes of UTF-8, compared byte for byte.
//   - original: the original bytes: the encoded item, its header included, for a resolved path,
//     the whole message text for a whole-text mask, the value bytes for an annotation.
//
// Returns:
//   - []byte: the DigestSize-byte digest.
//   - error: an error wrapping ErrDomain for a domain the specification does not allow.
func Digest(key []byte, domain string, original []byte) ([]byte, error) {
	if err := checkDomain(domain); err != nil {
		return nil, err
	}

	return digestWith(hmac.New(sha256.New, key), domain, original), nil
}

// checkDomain checks that domain is 1 to 65535 bytes of UTF-8.
func checkDomain(domain string) error {
	if len(domain) == 0 || len(domain) > maxDomainLen || !utf8.ValidString(domain) {
		return fmt.Errorf("%w: %d bytes", ErrDomain, len(domain))
	}

	return nil
}

// digestWith computes a digest with mac, an HMAC-SHA-256 keyed with the digest key, for a valid domain.
func digestWith(mac hash.Hash, domain string, original []byte) []byte {
	mac.Reset()
	var n [2]byte
	binary.LittleEndian.PutUint16(n[:], uint16(len(domain)))
	mac.Write(n[:])
	mac.Write([]byte(domain))
	mac.Write(original)

	return mac.Sum(nil)
}
