// Package redact masks the records of a capture under a redaction policy and restores the originals a policy with encryption keeps
// (the tracepack semantics specification §8).
//
// A Policy holds item rules, each a stream and function, an item path pattern and a digest domain,
// and annotation rules, each an annotation kind and a digest domain.
// Compile validates a policy before use: it rejects an invalid domain or path,
// a value the pack metadata's redaction_policy cannot hold, so a Writer never refuses PackPolicy's result,
// and, for a policy with encryption, two item rules of one stream and function that can select overlapping items.
//
// (*Compiled).Mask masks one record and returns its redaction entries:
// the content bytes of every leaf item a rule selects, or the whole message text when the text is not one valid item
// or a rule's path does not resolve, and the value of a matching annotation's text or raw entry.
// Under a policy without encryption the masked bytes become zeros;
// under a policy with encryption the masked bytes of a data record become their AES-SIV ciphertext, keyed per capture,
// while an annotation's masked bytes still become zeros and keep no original.
// Every entry carries the HMAC-SHA-256 digest of the original bytes under the policy's digest key, comparable across captures.
// Masking is deterministic, so a redelivered record masks to the same bytes and entries.
//
// The caller keeps custody of the keys, which Mask asks for through the Keys interface.
// A consumer masks its whole flush batch and collects the entries into the pack metadata's Redaction,
// ordered by seq and then by first masked offset;
// it sets RedactionPolicy to (*Compiled).PackPolicy and only then constructs the tracepack.Writer,
// since the Writer writes the pack metadata before the first record.
//
// Restore gives back the original payload of a masked data record, all or nothing,
// to the holder of the capture's encryption key;
// it reruns every Marking check on its own inputs and trusts no value its caller built.
//
// Errors: each error this package builds wraps one of its sentinels or the cause it reports, under the redact namespace,
// which each line of its text names once; Mask names the record's seq after it.
// Every error is deterministic, the same input failing the same way,
// but an error of the caller's Keys, which Mask returns wrapped, may be transient, and ctx's, which Mask returns as it is.
// A caller acts on the first of this package's sentinels an error matches, before any cause it wraps:
// ErrCannotScreen, ErrRedacted and ErrNotUnobserved defer the record;
// ErrKeySize, ErrInvalidPolicy and ErrDomain call for a fix of the configuration;
// Restore's sentinels, and tracepack.ErrRedactionDefect for defective stored entries, say why a record cannot be restored.
//
// The package imports the tracepack package for its types; tracepack never imports it,
// so a program that only reads packs links no masking or encryption code.
package redact
