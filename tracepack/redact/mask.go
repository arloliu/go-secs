package redact

import (
	"cmp"
	"context"
	"crypto/hmac"
	"crypto/sha256"
	"errors"
	"fmt"
	"slices"

	"github.com/arloliu/go-secs/tracepack"
	"github.com/arloliu/go-secs/tracepack/internal/secsitem"
	"github.com/arloliu/go-secs/tracepack/internal/siv"
	"github.com/arloliu/go-secs/tracepack/internal/tlv"
)

// KeySize is the length of an aes-siv-cmac-512 encryption key.
const KeySize = siv.KeySize

// Layout of the payloads Mask reads.
const (
	// framePrefixLen is the 4-byte length and the 10-byte HSMS message header, which no mask covers.
	framePrefixLen = 14
	// streamOff and functionOff are the payload bytes of the stream (bits 0-6) and the function.
	streamOff, functionOff = 6, 7
	// streamMask keeps bits 0-6 of payload byte 6; bit 7 is the W bit.
	streamMask = 0x7F
	// Annotation payload tags whose values an annotation rule masks (the tracepack format specification §8).
	tagText, tagRaw uint16 = 0x0005, 0x0006
)

var (
	// ErrCannotScreen reports a record Mask cannot finish screening:
	// a data record whose message text nests lists deeper than the walker's limit of 64,
	// or an annotation whose payload does not decode while the policy has annotation rules.
	// A consumer defers such a record; it never writes it unscreened (the tracepack semantics specification §8 Masking).
	ErrCannotScreen = errors.New("redact: the record cannot be screened")
	// ErrNotUnobserved reports a record Mask was told is unobserved by design that does not carry quality.unobserved.
	ErrNotUnobserved = errors.New("redact: the record does not carry quality.unobserved")
	// ErrRedacted reports a record that already carries quality.redacted, so it is not in full.
	ErrRedacted = errors.New("redact: the record already carries quality.redacted")
	// ErrKeySize reports an encryption key that is not KeySize bytes long.
	ErrKeySize = errors.New("redact: encryption key is not 64 bytes")
)

// match is one mask of a record: its entry's item path, ranges and domain,
// the payload range whose original bytes its digest covers, and whether its target bytes are encrypted.
type match struct {
	path    *string
	ranges  []tracepack.MaskedRange
	domain  string
	digest  secsitem.Range
	encrypt bool
}

// Mask masks r, a record of the capture, under the policy (the tracepack semantics specification §8 Masking)
// and returns the masked record and its redaction entries, ordered by first masked offset.
//
// An item rule applies to a data record whose payload bytes 6 (bits 0-6) and 7 equal its stream and function,
// whatever field_validity says; its message text is every byte after the first 14, whatever the declared length says.
// A text that is one valid item with no content byte in any leaf is not masked.
// Otherwise, when the text is one valid item and the path of every rule of its stream and function resolves,
// each item a rule selects gives one entry with its concrete item path, masking the content bytes of the item's leaves;
// every item header and a localized string's 2-byte header are kept.
// When the text is not one valid item, or a rule's path does not resolve,
// the record gets one entry masking the whole message text, in DomainMessageText, without item path.
// An annotation rule applies to an annotation of its kind and masks the value of its text or raw entry.
// A match with no target bytes gives no entry.
// Control records, transport events and records of an unknown kind are never masked.
// A record already carrying quality.redacted is refused, since it is not in full.
//
// Under a policy with encryption the target bytes of a data record's entry are replaced by their AES-SIV ciphertext,
// sealed with the associated data of the capture, the seq and the entry's first offset, and the entry carries the siv;
// every other target byte becomes zero.
// Each digest covers the original bytes, computed before any mask changes them.
// Masking is deterministic: equal inputs give equal records and entries.
//
// Mask checks ctx before each key request, and passes ctx to keys;
// nothing else it does blocks, so a record that needs no key is returned whatever the state of ctx.
//
// Parameters:
//   - ctx: the context of the key requests.
//   - capture: the capture_id of the pack r will be written to.
//   - r: the record as received, classified from its payload as received; Mask does not change it.
//   - unobserved: true when r is unobserved by design, as the bus record's quality bit 7 says,
//     or Reader.Unobserved for a record read back: r is then returned untouched with no entry,
//     since an unobserved record is never masked.
//     False when r is not, whatever its bit 7 holds, as in a pack of format minor 0, where the bit is reserved.
//   - keys: the source of the keys, asked only when an entry needs one.
//
// Returns:
//   - tracepack.Record: r masked, with quality.redacted added and a payload of its own, when there is an entry;
//     else r itself, its payload shared with r.
//   - []tracepack.RedactionEntry: the record's entries; nil when nothing matched.
//   - error: an error wrapping ErrCannotScreen, ErrNotUnobserved, ErrRedacted or ErrKeySize, or the error of keys;
//     ctx.Err() itself when ctx is done before a key request;
//     no record or entry is returned with it.
func (c *Compiled) Mask(
	ctx context.Context, capture tracepack.UUID, r *tracepack.Record, unobserved bool, keys Keys,
) (tracepack.Record, []tracepack.RedactionEntry, error) {
	if unobserved {
		if !r.Quality.Has(tracepack.QualityUnobserved) {
			return tracepack.Record{}, nil, fmt.Errorf("seq %d: %w", r.Seq, ErrNotUnobserved)
		}

		return *r, nil, nil
	}
	if r.Quality.Has(tracepack.QualityRedacted) {
		return tracepack.Record{}, nil, fmt.Errorf("seq %d: %w", r.Seq, ErrRedacted)
	}

	// Control records, transport events and records of any other kind are never masked.
	var (
		ms  []match
		err error
	)
	if r.Kind == tracepack.KindData {
		ms, err = c.dataMatches(r.Payload)
	}
	if r.Kind == tracepack.KindAnnotation {
		ms, err = c.annotationMatches(r.Payload)
	}
	if err != nil {
		return tracepack.Record{}, nil, fmt.Errorf("seq %d: %w", r.Seq, err)
	}
	if len(ms) == 0 {
		return *r, nil, nil
	}

	if err := ctx.Err(); err != nil {
		return tracepack.Record{}, nil, err
	}
	digestKey, err := keys.DigestKey(ctx, c.policy.DigestKeyID)
	if err != nil {
		return tracepack.Record{}, nil, fmt.Errorf("seq %d: redact: digest key %q: %w", r.Seq, c.policy.DigestKeyID, err)
	}
	var encKey []byte
	if slices.ContainsFunc(ms, func(m match) bool { return m.encrypt }) {
		if err := ctx.Err(); err != nil {
			return tracepack.Record{}, nil, err
		}
		encKey, err = keys.EncryptionKey(ctx, capture, c.policy.Encryption.KeyID)
		if err != nil {
			return tracepack.Record{}, nil, fmt.Errorf("seq %d: redact: encryption key %q: %w", r.Seq, c.policy.Encryption.KeyID, err)
		}
		if len(encKey) != KeySize {
			return tracepack.Record{}, nil, fmt.Errorf("seq %d: %w: key %q holds %d bytes", r.Seq, ErrKeySize, c.policy.Encryption.KeyID, len(encKey))
		}
	}

	masked, entries, err := apply(capture, r, ms, digestKey, encKey)
	if err != nil {
		return tracepack.Record{}, nil, fmt.Errorf("seq %d: %w", r.Seq, err)
	}

	return masked, entries, nil
}

// dataMatches returns the masks of a data record's payload under the policy's item rules.
func (c *Compiled) dataMatches(payload []byte) ([]match, error) {
	// A payload of 14 bytes or fewer has no message text, so no target byte.
	if len(payload) <= framePrefixLen {
		return nil, nil
	}
	rules := c.items[sf{payload[streamOff] & streamMask, payload[functionOff]}]
	if len(rules) == 0 {
		return nil, nil
	}

	text, err := secsitem.Screen(payload[framePrefixLen:])
	if err != nil {
		return nil, fmt.Errorf("%w: %w", ErrCannotScreen, err)
	}
	if text.OneItem() && !text.HasContent() {
		return nil, nil
	}
	if !text.OneItem() {
		return c.wholeText(payload), nil
	}

	var ms []match
	for _, rule := range rules {
		sels, ok := text.Select(rule.pattern)
		if !ok {
			return c.wholeText(payload), nil
		}
		for _, sel := range sels {
			content := text.Content(sel.Item, nil)
			if len(content) == 0 {
				continue
			}
			ranges := make([]tracepack.MaskedRange, len(content))
			for k, cr := range content {
				ranges[k] = tracepack.MaskedRange{Offset: uint64(framePrefixLen + cr.Start), Length: uint64(cr.End - cr.Start)}
			}
			path := sel.Path
			ms = append(ms, match{
				path:    &path,
				ranges:  ranges,
				domain:  rule.domain,
				digest:  secsitem.Range{Start: framePrefixLen + sel.Item.Start, End: framePrefixLen + sel.Item.End},
				encrypt: c.policy.Encryption != nil,
			})
		}
	}
	slices.SortStableFunc(ms, func(a, b match) int { return cmp.Compare(a.ranges[0].Offset, b.ranges[0].Offset) })

	return ms, nil
}

// wholeText returns the one mask of a whole-text mask of a data record's payload:
// the whole message text, in DomainMessageText, without item path, digesting the whole original text.
func (c *Compiled) wholeText(payload []byte) []match {
	return []match{{
		ranges:  []tracepack.MaskedRange{{Offset: framePrefixLen, Length: uint64(len(payload) - framePrefixLen)}},
		domain:  DomainMessageText,
		digest:  secsitem.Range{Start: framePrefixLen, End: len(payload)},
		encrypt: c.policy.Encryption != nil,
	}}
}

// annotationMatches returns the masks of an annotation's payload under the policy's annotation rules:
// one per rule of its kind over the value of its text or raw entry, zeroed whatever the policy's encryption.
func (c *Compiled) annotationMatches(payload []byte) ([]match, error) {
	if len(c.annotations) == 0 {
		return nil, nil
	}
	a, err := tracepack.UnmarshalAnnotation(payload)
	if err != nil {
		return nil, fmt.Errorf("%w: %w", ErrCannotScreen, err)
	}
	domains := c.annotations[a.AnnotationKind]
	if len(domains) == 0 {
		return nil, nil
	}

	// UnmarshalAnnotation found exactly one text or raw entry in a well-formed entry list.
	var value secsitem.Range
	for e := range tlv.Entries(payload) {
		if e.Tag == tagText || e.Tag == tagRaw {
			value.Start = e.Offset + tlv.HeaderLen
			value.End = value.Start + len(e.Value)

			break
		}
	}
	if value.Start == value.End {
		return nil, nil
	}

	ms := make([]match, len(domains))
	for i, d := range domains {
		ms[i] = match{
			ranges: []tracepack.MaskedRange{{Offset: uint64(value.Start), Length: uint64(value.End - value.Start)}},
			domain: d,
			digest: value,
		}
	}

	return ms, nil
}

// apply masks a copy of r with ms and returns it with the entries of ms, each with its digest, and its siv when encrypted;
// encKey is nil when no match of ms is encrypted.
func apply(
	capture tracepack.UUID, r *tracepack.Record, ms []match, digestKey, encKey []byte,
) (tracepack.Record, []tracepack.RedactionEntry, error) {
	mac := hmac.New(sha256.New, digestKey)
	entries := make([]tracepack.RedactionEntry, len(ms))
	for i := range ms {
		m := &ms[i]
		entries[i] = tracepack.RedactionEntry{
			Seq: r.Seq, ItemPath: m.path, MaskedRanges: m.ranges, Domain: m.domain,
			Digest: digestWith(mac, m.domain, r.Payload[m.digest.Start:m.digest.End]),
		}
	}

	out := *r
	out.Payload = slices.Clone(r.Payload)
	out.Quality |= tracepack.QualityRedacted
	for i := range ms {
		m := &ms[i]
		if !m.encrypt {
			for _, mr := range m.ranges {
				clear(out.Payload[mr.Offset : mr.Offset+mr.Length])
			}

			continue
		}
		// Encrypted entries of one record never overlap, so each plaintext is read from the original bytes.
		ad := siv.AssociatedData(capture, r.Seq, m.ranges[0].Offset)
		iv, ct, err := siv.Seal(encKey, ad[:], gather(r.Payload, m.ranges))
		if err != nil {
			return tracepack.Record{}, nil, fmt.Errorf("redact: encrypt: %w", err)
		}
		scatter(out.Payload, m.ranges, ct)
		entries[i].SIV, entries[i].HasSIV = iv[:], true
	}

	return out, entries, nil
}

// gather returns the bytes of payload in ranges, concatenated in range order.
func gather(payload []byte, ranges []tracepack.MaskedRange) []byte {
	var n uint64
	for _, mr := range ranges {
		n += mr.Length
	}
	b := make([]byte, 0, n)
	for _, mr := range ranges {
		b = append(b, payload[mr.Offset:mr.Offset+mr.Length]...)
	}

	return b
}

// scatter writes b over the bytes of payload in ranges, in range order; b holds exactly their total length.
func scatter(payload []byte, ranges []tracepack.MaskedRange, b []byte) {
	for _, mr := range ranges {
		b = b[copy(payload[mr.Offset:mr.Offset+mr.Length], b):]
	}
}
