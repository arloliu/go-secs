package redact

import (
	"errors"
	"fmt"
	"slices"

	"github.com/arloliu/go-secs/tracepack"
	"github.com/arloliu/go-secs/tracepack/internal/marking"
	"github.com/arloliu/go-secs/tracepack/internal/siv"
)

var (
	// ErrNoEncryption reports a redaction_policy that does not carry both encryption tags,
	// encryption_algorithm and encryption_key_id, so its masks keep no original to restore.
	// It is no defect of the pack.
	ErrNoEncryption = errors.New("redact: the policy carries no encryption")
	// ErrUnsupportedAlgorithm reports an encryption_algorithm other than aes-siv-cmac-512, the one Restore supports.
	ErrUnsupportedAlgorithm = errors.New("redact: unsupported encryption algorithm")
	// ErrNotRestorable reports a record that holds no original to restore:
	// no Marking was given, the record is not masked, or its masks are annotation masks, which keep no original.
	ErrNotRestorable = errors.New("redact: the record holds no original to restore")
	// ErrAuthentication reports an entry whose ciphertext does not authenticate
	// under the key, the record's identity, the entry's first offset and its siv:
	// a wrong key, or masked bytes or a siv that changed.
	ErrAuthentication = errors.New("redact: an entry does not authenticate")
)

// Restore returns the original payload of r, a masked data record of the capture, all or nothing
// (the tracepack semantics specification §8 Encryption):
// it decrypts each entry with its siv, the record's identity and the entry's first offset into a copy of the payload,
// and returns the copy only when every entry authenticates; on any failure no byte is restored.
//
// Restore trusts no value its caller built.
// It refuses a nil rr, an rr listing a defect or WhollyMasked,
// a policy without both encryption tags, an algorithm other than aes-siv-cmac-512 (checked before any decryption,
// since the associated data does not bind the algorithm), and a key that is not KeySize bytes;
// and it reruns every Marking check on r and rr.Entries itself:
// each entry on its own (its ranges and a present siv of 16 bytes), the order of the entries,
// and the entries against r: each naming r's seq, r carrying quality.redacted,
// each range inside the payload and past the frame prefix, a siv on every entry of a data record,
// and the encrypted ranges pairwise disjoint.
// Whole-text masks are restored as item masks are, without parsing the text.
// An annotation's masks keep no original, so Restore restores no annotation.
//
// Parameters:
//   - capture: the capture_id of the record's pack.
//   - r: the masked record as stored, its payload included; Restore does not change it.
//   - rr: the record's Marking, as Item.Redaction reports it; Restore does not change it.
//   - policy: the pack's redaction_policy, as PackHeader.Meta reports it; Restore does not change it.
//   - key: the capture's encryption key under the policy's encryption_key_id.
//
// Returns:
//   - []byte: the original payload, a buffer of its own; nil on error.
//   - error: an error wrapping tracepack.ErrRedactionDefect for a listed defect, WhollyMasked or a failed Marking check;
//     ErrNoEncryption, ErrUnsupportedAlgorithm, ErrKeySize, ErrNotRestorable, or ErrAuthentication.
func Restore(capture tracepack.UUID, r *tracepack.Record, rr *tracepack.RecordRedaction, policy *tracepack.RedactionPolicy, key []byte) ([]byte, error) {
	if err := checkRestorable(r, rr); err != nil {
		return nil, err
	}
	if policy == nil || policy.EncryptionAlgorithm == nil || policy.EncryptionKeyID == nil {
		return nil, ErrNoEncryption
	}
	if alg := *policy.EncryptionAlgorithm; alg != tracepack.EncryptionAlgorithmAESSIVCMAC512 {
		return nil, fmt.Errorf("%w: %s", ErrUnsupportedAlgorithm, alg)
	}
	if len(key) != KeySize {
		return nil, fmt.Errorf("%w: %d bytes", ErrKeySize, len(key))
	}
	if err := checkMarking(r, rr.Entries); err != nil {
		return nil, err
	}
	if r.Kind != tracepack.KindData {
		return nil, fmt.Errorf("%w: seq %d is an annotation, whose masks keep no original", ErrNotRestorable, r.Seq)
	}

	out := slices.Clone(r.Payload)
	for i := range rr.Entries {
		e := &rr.Entries[i]
		ad := siv.AssociatedData(capture, r.Seq, e.MaskedRanges[0].Offset)
		plain, err := siv.Open(key, ad[:], e.SIV, gather(r.Payload, e.MaskedRanges))
		if err != nil {
			clear(out)
			return nil, fmt.Errorf("%w: seq %d, entry %d", ErrAuthentication, r.Seq, i)
		}
		scatter(out, e.MaskedRanges, plain)
	}

	return out, nil
}

// checkRestorable refuses a record and Marking that hold nothing to restore, or a Marking that lists a defect.
func checkRestorable(r *tracepack.Record, rr *tracepack.RecordRedaction) error {
	switch {
	case r == nil || rr == nil:
		return fmt.Errorf("%w: no Marking", ErrNotRestorable)
	case len(rr.Defects) > 0:
		return fmt.Errorf("%w: seq %d is wholly masked: %w", tracepack.ErrRedactionDefect, r.Seq, rr.Defects[0].Err)
	case rr.WhollyMasked:
		return fmt.Errorf("%w: seq %d is wholly masked", tracepack.ErrRedactionDefect, r.Seq)
	case len(rr.Entries) == 0 && !r.Quality.Has(tracepack.QualityRedacted):
		return fmt.Errorf("%w: seq %d is not masked", ErrNotRestorable, r.Seq)
	}

	return nil
}

// checkMarking runs every Marking check on entries, the entries naming r, and on r, under a policy with encryption.
//
// Returns:
//   - error: an error wrapping tracepack.ErrRedactionDefect and the failed check's error; nil when every check passes.
func checkMarking(r *tracepack.Record, entries []tracepack.RedactionEntry) error {
	var errs []error
	mes := make([]marking.Entry, len(entries))
	for i := range entries {
		mes[i] = markingEntry(&entries[i])
		for _, err := range marking.CheckEntry(&mes[i]) {
			errs = append(errs, fmt.Errorf("entry %d: %w", i, err))
		}
		if i > 0 {
			if err := marking.CheckOrder(&mes[i-1], &mes[i]); err != nil {
				errs = append(errs, fmt.Errorf("entry %d: %w", i, err))
			}
		}
	}
	rec := markingRecord(r)
	for _, d := range marking.CheckRecord(&rec, mes, true) {
		errs = append(errs, fmt.Errorf("entry %d: %w", d.Entry, d.Err))
	}
	if len(errs) > 0 {
		return fmt.Errorf("%w: seq %d: %w", tracepack.ErrRedactionDefect, r.Seq, errors.Join(errs...))
	}

	return nil
}

// markingEntry returns e as the checks of internal/marking see it, as a pack reader does:
// a stored RawMaskedRanges is a malformed ranges array, and a non-empty SIV is a present siv.
func markingEntry(e *tracepack.RedactionEntry) marking.Entry {
	me := marking.Entry{
		Seq: e.Seq, Malformed: len(e.RawMaskedRanges) > 0,
		HasSIV: e.HasSIV || len(e.SIV) > 0, SIVLen: len(e.SIV),
	}
	if !me.Malformed {
		me.Ranges = make([]marking.Range, len(e.MaskedRanges))
		for k, mr := range e.MaskedRanges {
			me.Ranges[k] = marking.Range{Offset: mr.Offset, Length: mr.Length}
		}
	}

	return me
}

// markingRecord returns r as the checks of internal/marking see it.
func markingRecord(r *tracepack.Record) marking.Record {
	// Every kind but data and annotation is one the checks find never masked.
	kind := marking.KindOther
	if r.Kind == tracepack.KindData {
		kind = marking.KindData
	}
	if r.Kind == tracepack.KindAnnotation {
		kind = marking.KindAnnotation
	}

	return marking.Record{Seq: r.Seq, Kind: kind, Redacted: r.Quality.Has(tracepack.QualityRedacted), Payload: r.Payload}
}
