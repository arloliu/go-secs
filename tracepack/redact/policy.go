package redact

import (
	"context"
	"errors"
	"fmt"
	"slices"

	"github.com/arloliu/go-secs/tracepack"
	"github.com/arloliu/go-secs/tracepack/internal/secsitem"
)

// maxStream is the largest stream a data record can carry: bits 0-6 of payload byte 6.
const maxStream = 127

var (
	// ErrInvalidPolicy reports a policy that Compile rejects before use.
	ErrInvalidPolicy = errors.New("redact: invalid policy")
	// ErrOverlappingRules reports, in a policy with encryption, two item rules of one stream and function
	// whose path patterns can select overlapping items (the tracepack semantics specification §8 Policy).
	// It is returned wrapped with ErrInvalidPolicy.
	ErrOverlappingRules = errors.New("redact: item rules of one stream and function can select overlapping items")
)

// Policy is a redaction policy (the tracepack semantics specification §8 Policy):
// its identity, the key id of its digests, its encryption, and its rules.
// How policies are written, stored and distributed is the caller's.
type Policy struct {
	// ID identifies the policy (policy_id).
	ID string
	// Version is the policy's version (policy_version).
	Version uint64
	// DigestKeyID identifies the HMAC-SHA-256 key of the digests (key_id); Mask asks Keys.DigestKey for it.
	DigestKeyID string
	// Encryption, when set, makes the policy one with encryption:
	// the masked bytes of data records are encrypted in place under the capture's encryption key.
	// Nil makes them zeros.
	Encryption *Encryption
	// ItemRules mask items of data records; several may share a stream and function.
	ItemRules []ItemRule
	// AnnotationRules mask the text or raw value of annotation records; several may share a kind.
	AnnotationRules []AnnotationRule
}

// Encryption is the encryption of a policy: aes-siv-cmac-512, the one algorithm format 1.1 defines,
// under the key the key id names in each capture.
type Encryption struct {
	// KeyID identifies the encryption key (encryption_key_id); Mask asks Keys.EncryptionKey for it with the record's capture.
	KeyID string
}

// ItemRule masks the items one path pattern selects in the data records of one stream and function.
type ItemRule struct {
	// Stream and Function select the data records whose payload bytes 6 (bits 0-6) and 7 equal them,
	// whatever field_validity says; Stream is at most 127.
	Stream, Function uint8
	// Path is the item path pattern: 1-based element indexes into nested lists separated by "/",
	// "*" for every element of a list, and the empty string for the root item.
	Path string
	// Domain is the digest domain of the rule's entries: 1 to 65535 bytes of UTF-8.
	Domain string
}

// AnnotationRule masks the text or raw value of the annotation records of one kind.
type AnnotationRule struct {
	// Kind selects the annotation records of that annotation_kind.
	Kind tracepack.AnnotationKind
	// Domain is the digest domain of the rule's entries: 1 to 65535 bytes of UTF-8.
	Domain string
}

// Keys gives Mask the keys a policy names; the caller keeps their custody.
// Mask asks for a key only when a record has an entry that needs it,
// and passes each method the context of its own call, which a key service can use to bound its request.
type Keys interface {
	// DigestKey returns the HMAC-SHA-256 key of keyID.
	DigestKey(ctx context.Context, keyID string) ([]byte, error)
	// EncryptionKey returns the 64-byte aes-siv-cmac-512 key of keyID for the capture.
	EncryptionKey(ctx context.Context, capture tracepack.UUID, keyID string) ([]byte, error)
}

// Compiled is a policy Compile validated, ready to mask records; it is safe for concurrent use.
type Compiled struct {
	policy      Policy
	items       map[sf][]itemRule
	annotations map[tracepack.AnnotationKind][]string
}

// sf is a stream and function.
type sf struct {
	stream, function uint8
}

// itemRule is an item rule with its parsed pattern.
type itemRule struct {
	pattern secsitem.Pattern
	domain  string
}

// Compile validates p and returns it ready to mask records.
// It keeps a copy of p, so changing p afterwards changes nothing compiled.
//
// Parameters:
//   - p: the policy.
//
// Returns:
//   - *Compiled: the compiled policy; nil on error.
//   - error: an error wrapping ErrInvalidPolicy
//     for a domain that is not 1 to 65535 bytes of UTF-8 (also wrapping ErrDomain), a path that is not an item path pattern, or a stream above 127;
//     and, for a policy with encryption, for two item rules of one stream and function
//     that can select overlapping items, also wrapping ErrOverlappingRules:
//     identical patterns, a pattern that is a prefix of another, or patterns whose * and numbered steps can select one item.
func Compile(p Policy) (*Compiled, error) {
	c := &Compiled{
		policy:      clonePolicy(&p),
		items:       make(map[sf][]itemRule),
		annotations: make(map[tracepack.AnnotationKind][]string),
	}
	for i, r := range p.ItemRules {
		if err := checkDomain(r.Domain); err != nil {
			return nil, fmt.Errorf("%w: item rule %d: %w", ErrInvalidPolicy, i, err)
		}
		if r.Stream > maxStream {
			return nil, fmt.Errorf("%w: item rule %d: stream %d is above %d", ErrInvalidPolicy, i, r.Stream, maxStream)
		}
		pattern, err := secsitem.ParsePattern(r.Path)
		if err != nil {
			return nil, fmt.Errorf("%w: item rule %d: %w", ErrInvalidPolicy, i, err)
		}
		key := sf{r.Stream, r.Function}
		if p.Encryption != nil {
			for _, other := range c.items[key] {
				if pattern.Overlaps(other.pattern) {
					return nil, fmt.Errorf("%w: item rule %d (%q) and the rule of path %q for S%dF%d: %w",
						ErrInvalidPolicy, i, r.Path, other.pattern, r.Stream, r.Function, ErrOverlappingRules)
				}
			}
		}
		c.items[key] = append(c.items[key], itemRule{pattern: pattern, domain: r.Domain})
	}
	for i, r := range p.AnnotationRules {
		if err := checkDomain(r.Domain); err != nil {
			return nil, fmt.Errorf("%w: annotation rule %d: %w", ErrInvalidPolicy, i, err)
		}
		c.annotations[r.Kind] = append(c.annotations[r.Kind], r.Domain)
	}

	return c, nil
}

// PackPolicy returns the pack metadata redaction_policy of the policy:
// its id, version and digest key id, the digest algorithm hmac-sha256,
// and, for a policy with encryption, both encryption tags, encryption_algorithm aes-siv-cmac-512 and encryption_key_id;
// neither without encryption.
// Each call returns a value of its own.
func (c *Compiled) PackPolicy() tracepack.RedactionPolicy {
	rp := tracepack.RedactionPolicy{
		PolicyID:        c.policy.ID,
		PolicyVersion:   c.policy.Version,
		KeyID:           c.policy.DigestKeyID,
		DigestAlgorithm: tracepack.DigestAlgorithmHMACSHA256,
	}
	if c.policy.Encryption != nil {
		alg := tracepack.EncryptionAlgorithmAESSIVCMAC512
		keyID := c.policy.Encryption.KeyID
		rp.EncryptionAlgorithm, rp.EncryptionKeyID = &alg, &keyID
	}

	return rp
}

// clonePolicy returns a copy of p that shares no memory with it.
func clonePolicy(p *Policy) Policy {
	c := *p
	if p.Encryption != nil {
		e := *p.Encryption
		c.Encryption = &e
	}
	c.ItemRules = slices.Clone(p.ItemRules)
	c.AnnotationRules = slices.Clone(p.AnnotationRules)

	return c
}
