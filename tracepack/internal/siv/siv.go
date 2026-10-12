package siv

import (
	"encoding/binary"
	"errors"
	"slices"

	"github.com/tink-crypto/tink-go/v2/daead/subtle"
)

// Sizes of the construction's inputs and outputs.
const (
	// KeySize is the length of an aes-siv-cmac-512 key: the S2V key, then the CTR key, 32 bytes each.
	KeySize = 64
	// SIVSize is the length of the synthetic IV stored as an entry's siv.
	SIVSize = 16
	// ADSize is the length of the associated data AssociatedData builds:
	// the label, the capture id, the seq and the first offset.
	ADSize = len(label) + 16 + 8 + 8
)

// label opens the associated data, with no terminator, padding or length prefix.
const label = "tracepack-original-v1"

var (
	// ErrKeySize reports a key that is not KeySize bytes long.
	ErrKeySize = errors.New("siv: key is not 64 bytes")
	// ErrSIVSize reports a synthetic IV that is not SIVSize bytes long.
	ErrSIVSize = errors.New("siv: synthetic IV is not 16 bytes")
	// ErrAuthentication reports a ciphertext that does not authenticate under the key, the associated data and the synthetic IV given.
	ErrAuthentication = errors.New("siv: authentication failed")
)

// AssociatedData builds the associated data of one entry:
// the 21 ASCII bytes of the label "tracepack-original-v1",
// the record's capture id as its 16 bytes are stored,
// the record's seq as a little-endian u64,
// and the offset of the entry's first masked range as a little-endian u64.
//
// Parameters:
//   - capture: the record's capture_id, in the byte order of the tracepack format specification §2.
//   - seq: the record's seq.
//   - firstOffset: the payload offset of the entry's first masked range.
//
// Returns:
//   - [ADSize]byte: the 53 bytes given to S2V as its single associated-data component.
func AssociatedData(capture [16]byte, seq, firstOffset uint64) [ADSize]byte {
	var ad [ADSize]byte

	n := copy(ad[:], label)
	n += copy(ad[n:], capture[:])
	binary.LittleEndian.PutUint64(ad[n:], seq)
	binary.LittleEndian.PutUint64(ad[n+8:], firstOffset)

	return ad
}

// Seal encrypts plaintext under key with ad as the single associated-data component.
// Equal key, associated data and plaintext always give equal output.
//
// Parameters:
//   - key: a KeySize-byte key; Seal keeps no reference to it.
//   - ad: the associated data, normally AssociatedData's result.
//   - plaintext: the target bytes of one entry, in range order; left unchanged.
//
// Returns:
//   - [SIVSize]byte: the synthetic IV.
//   - []byte: a new ciphertext of len(plaintext) bytes, to write over the target bytes.
//   - error: ErrKeySize for a key of another length.
func Seal(key, ad, plaintext []byte) ([SIVSize]byte, []byte, error) {
	var iv [SIVSize]byte

	cipher, err := newCipher(key)
	if err != nil {
		return iv, nil, err
	}

	out, err := cipher.EncryptDeterministically(plaintext, ad)
	if err != nil {
		return iv, nil, err
	}
	copy(iv[:], out)

	return iv, out[SIVSize:], nil
}

// Open authenticates and decrypts ciphertext under key, ad and iv.
// It returns no plaintext unless authentication succeeds.
//
// Parameters:
//   - key: a KeySize-byte key; Open keeps no reference to it.
//   - ad: the associated data the ciphertext was sealed with.
//   - iv: the synthetic IV, an entry's siv; left unchanged.
//   - ciphertext: the bytes read from the entry's ranges, in range order; left unchanged.
//
// Returns:
//   - []byte: a new plaintext of len(ciphertext) bytes; nil on error.
//   - error: ErrKeySize, ErrSIVSize, or ErrAuthentication when the inputs do not authenticate.
func Open(key, ad, iv, ciphertext []byte) ([]byte, error) {
	cipher, err := newCipher(key)
	if err != nil {
		return nil, err
	}

	if len(iv) != SIVSize {
		return nil, ErrSIVSize
	}

	plaintext, err := cipher.DecryptDeterministically(slices.Concat(iv, ciphertext), ad)
	if err != nil {
		return nil, ErrAuthentication
	}

	return plaintext, nil
}

// newCipher checks the key length before Tink sees the key,
// and gives Tink a copy, since Tink keeps slices of the key it is given.
func newCipher(key []byte) (*subtle.AESSIV, error) {
	if len(key) != KeySize {
		return nil, ErrKeySize
	}

	cipher, err := subtle.NewAESSIV(slices.Clone(key))
	if err != nil {
		return nil, ErrKeySize
	}

	return cipher, nil
}
