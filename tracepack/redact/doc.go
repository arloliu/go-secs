// Package redact masks the records of a capture under a redaction policy and restores the originals a policy with encryption keeps
// (the tracepack semantics specification §8).
//
// A Policy holds item rules, each a stream and function, an item path pattern and a digest domain,
// and annotation rules, each an annotation kind and a digest domain.
// Compile validates a policy before use: it rejects an invalid domain or path,
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
// The package imports the tracepack package for its types; tracepack never imports it,
// so a program that only reads packs links no masking or encryption code.
package redact
