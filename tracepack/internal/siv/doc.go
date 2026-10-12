// Package siv implements the encryption of originals of the tracepack semantics specification §8:
// AES-SIV-CMAC of RFC 5297 with a 64-byte key (aes-siv-cmac-512),
// over the raw deterministic AES-SIV primitive of Tink, whose keyset format never enters a pack.
//
// AssociatedData builds the 53-byte associated data of one entry,
// which binds its ciphertext to the record's capture id and seq and to the entry's first masked offset;
// S2V receives it as its single associated-data component.
// Seal returns the 16-byte synthetic IV, stored as the entry's siv, and a ciphertext of the plaintext's length,
// which the caller writes over the target bytes in range order.
// Open authenticates and decrypts, and returns no plaintext when authentication fails.
//
// The package checks no range map and no Marking rule;
// its callers do, before they write a ciphertext or a restored plaintext into a record.
package siv
