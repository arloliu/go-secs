package corpus

import (
	"bytes"
	"crypto/sha256"
	"encoding/binary"
	"fmt"
	"slices"
	"strconv"
	"strings"

	"github.com/arloliu/go-secs/tracepack"
	"github.com/arloliu/go-secs/tracepack/internal/format"
	"github.com/arloliu/go-secs/tracepack/internal/siv"
	"github.com/arloliu/go-secs/tracepack/redact"
)

// Names of the CRC rows of primitives.json.
const (
	CRCCheckValue    = "check-value"
	CRCFileHeader    = "file-header"
	CRCBlockEnvelope = "block-envelope"
)

// crcCheckInput is the input of the CRC check value (the tracepack format specification §2).
const crcCheckInput = "123456789"

// Lengths of the bytes each stored CRC covers: bytes 0-75 of a file header and bytes 0-35 of a block envelope,
// the stored CRC following them.
const (
	fileHeaderCRCOff    = 76
	blockEnvelopeCRCOff = 36
)

// Primitives is primitives.json (the tracepack corpus specification §5.1).
type Primitives struct {
	CRC            []CRCRow    `json:"crc"`
	UUIDs          []UUIDRow   `json:"uuids"`
	DigestKeys     []KeyRow    `json:"digest_keys"`
	EncryptionKeys []KeyRow    `json:"encryption_keys"`
	HMAC           []HMACRow   `json:"hmac"`
	AESSIV         []AESSIVRow `json:"aes_siv"`
}

// KeyRow is one published test key of primitives.json: its key id and its bytes.
type KeyRow struct {
	KeyID string `json:"key_id"`
	Key   []byte `json:"key"`
}

// HMACRow is one digest case of primitives.json:
// the digest of the tracepack semantics specification §8 of Input in Domain under the digest key KeyID names.
type HMACRow struct {
	Name   string `json:"name"`
	KeyID  string `json:"key_id"`
	Domain string `json:"domain"`
	Input  []byte `json:"input"`
	Digest []byte `json:"digest"`
}

// AESSIVRow is one encryption case of primitives.json:
// the plaintext of an entry sealed under the encryption key KeyID names,
// with the associated data of the record's capture, its seq and the entry's first masked offset
// (the tracepack semantics specification §8 Encryption);
// Output is the siv followed by the ciphertext.
type AESSIVRow struct {
	Name           string `json:"name"`
	KeyID          string `json:"key_id"`
	CaptureID      string `json:"capture_id"`
	Seq            U64    `json:"seq"`
	FirstOffset    U64    `json:"first_offset"`
	AssociatedData []byte `json:"associated_data"`
	Plaintext      []byte `json:"plaintext"`
	Output         []byte `json:"output"`
}

// Key ids of the published test keys (the tracepack corpus specification §5.1).
const (
	DigestKeyID1     = "test-digest-1"
	DigestKeyID2     = "test-digest-2"
	EncryptionKeyID1 = "test-enc-1"
	EncryptionKeyID2 = "test-enc-2"
)

// publishedKeyLabel starts the input every published test key is derived from.
const publishedKeyLabel = "tracepack-corpus test key/"

// digestKeySize is the length of a published digest key, one SHA-256 output.
const digestKeySize = sha256.Size

// PublishedKey returns the published test key of keyID, n bytes long:
// the SHA-256 outputs of publishedKeyLabel followed by keyID, "/" and the decimal block index 0, 1, …, concatenated,
// cut to n bytes.
// A digest key is the first output alone, an encryption key the first two.
func PublishedKey(keyID string, n int) []byte {
	out := make([]byte, 0, n+sha256.Size)
	for i := 0; len(out) < n; i++ {
		sum := sha256.Sum256([]byte(publishedKeyLabel + keyID + "/" + strconv.Itoa(i)))
		out = append(out, sum[:]...)
	}

	return out[:n]
}

// corpusKeys gives the published test keys to masking, the same encryption key in every capture.
type corpusKeys struct{}

var _ redact.Keys = corpusKeys{}

// DigestKey returns the published digest key of keyID.
func (corpusKeys) DigestKey(keyID string) ([]byte, error) {
	if keyID != DigestKeyID1 && keyID != DigestKeyID2 {
		return nil, fmt.Errorf("corpus: no published digest key %q", keyID)
	}

	return PublishedKey(keyID, digestKeySize), nil
}

// EncryptionKey returns the published encryption key of keyID, whatever the capture.
func (corpusKeys) EncryptionKey(_ tracepack.UUID, keyID string) ([]byte, error) {
	if keyID != EncryptionKeyID1 && keyID != EncryptionKeyID2 {
		return nil, fmt.Errorf("corpus: no published encryption key %q", keyID)
	}

	return PublishedKey(keyID, siv.KeySize), nil
}

// hmacCase is a digest case of primitives.json before its digest is computed.
type hmacCase struct {
	name, keyID, domain string
	input               []byte
}

// aesSIVCase is an encryption case of primitives.json before it is sealed.
type aesSIVCase struct {
	name, keyID      string
	capture          tracepack.UUID
	seq, firstOffset uint64
	plaintext        []byte
}

// labelCaptureID is the capture_id of the label case of primitives.json,
// whose associated data an implementation checks byte for byte.
var labelCaptureID = tracepack.UUID{0x00, 0x11, 0x22, 0x33, 0x44, 0x55, 0x66, 0x77, 0x88, 0x99, 0xaa, 0xbb, 0xcc, 0xdd, 0xee, 0xff}

// hmacCases are the digest cases of primitives.json, ascending by name:
// the PPBODY item the redaction vectors mask, under each digest key, a whole message text and an annotation's value.
func hmacCases() []hmacCase {
	return []hmacCase{
		{name: "message-text", keyID: DigestKeyID1, domain: redact.DomainMessageText, input: wholeTextInvalid},
		{name: "ppbody", keyID: DigestKeyID1, domain: ppbodyDomain, input: ppbodyItem()},
		{name: "ppbody-second-key", keyID: DigestKeyID2, domain: ppbodyDomain, input: ppbodyItem()},
		{name: "source-text", keyID: DigestKeyID1, domain: sourceTextDomain, input: []byte(unparsedText)},
	}
}

// aesSIVCases are the encryption cases of primitives.json, ascending by name, the label case first:
// the PPBODY content the redaction vectors of two captures encrypt, at the seq and offset of their entries,
// and a case under the second key.
func aesSIVCases() []aesSIVCase {
	return []aesSIVCase{
		{name: "label", keyID: EncryptionKeyID1, capture: labelCaptureID, seq: 1, firstOffset: ppbodyOffset, plaintext: []byte("body!")},
		{
			name: "ppbody-capture-a", keyID: EncryptionKeyID1, capture: IDFor(roleCaptureID, captureVectorA),
			seq: capturePPBODYSeq, firstOffset: ppbodyOffset, plaintext: ppbodyContent(),
		},
		{
			name: "ppbody-capture-b", keyID: EncryptionKeyID1, capture: IDFor(roleCaptureID, captureVectorB),
			seq: capturePPBODYSeq, firstOffset: ppbodyOffset, plaintext: ppbodyContent(),
		},
		{
			name: "ppbody-segment", keyID: EncryptionKeyID1, capture: IDFor(roleCaptureID, encryptedSegmentVector),
			seq: 0, firstOffset: ppbodyOffset, plaintext: ppbodyContent(),
		},
		{name: "second-key", keyID: EncryptionKeyID2, capture: labelCaptureID, seq: 1, firstOffset: ppbodyOffset, plaintext: []byte("body!")},
	}
}

// AddKeys sets the published test keys of p and their digest and encryption cases,
// each array ascending as its schema orders it (the tracepack corpus specification §5.1).
//
// Returns:
//   - error: a case the digest or the encryption refuses, or cases out of order.
func (p *Primitives) AddKeys() error {
	keys := corpusKeys{}
	p.DigestKeys, p.EncryptionKeys, p.HMAC, p.AESSIV = []KeyRow{}, []KeyRow{}, []HMACRow{}, []AESSIVRow{}
	for _, id := range []string{DigestKeyID1, DigestKeyID2} {
		k, err := keys.DigestKey(id)
		if err != nil {
			return err
		}
		p.DigestKeys = append(p.DigestKeys, KeyRow{KeyID: id, Key: k})
	}
	for _, id := range []string{EncryptionKeyID1, EncryptionKeyID2} {
		k, err := keys.EncryptionKey(tracepack.UUID{}, id)
		if err != nil {
			return err
		}
		p.EncryptionKeys = append(p.EncryptionKeys, KeyRow{KeyID: id, Key: k})
	}
	for _, c := range hmacCases() {
		k, err := keys.DigestKey(c.keyID)
		if err != nil {
			return err
		}
		d, err := redact.Digest(k, c.domain, c.input)
		if err != nil {
			return fmt.Errorf("corpus: hmac case %s: %w", c.name, err)
		}
		p.HMAC = append(p.HMAC, HMACRow{Name: c.name, KeyID: c.keyID, Domain: c.domain, Input: bytes.Clone(c.input), Digest: d})
	}
	for _, c := range aesSIVCases() {
		k, err := keys.EncryptionKey(c.capture, c.keyID)
		if err != nil {
			return err
		}
		ad := siv.AssociatedData(c.capture, c.seq, c.firstOffset)
		iv, ct, err := siv.Seal(k, ad[:], c.plaintext)
		if err != nil {
			return fmt.Errorf("corpus: aes_siv case %s: %w", c.name, err)
		}
		p.AESSIV = append(p.AESSIV, AESSIVRow{
			Name: c.name, KeyID: c.keyID, CaptureID: c.capture.String(), Seq: U64(c.seq), FirstOffset: U64(c.firstOffset),
			AssociatedData: ad[:], Plaintext: bytes.Clone(c.plaintext), Output: slices.Concat(iv[:], ct),
		})
	}
	if !slices.IsSortedFunc(p.HMAC, func(a, b HMACRow) int { return strings.Compare(a.Name, b.Name) }) ||
		!slices.IsSortedFunc(p.AESSIV, func(a, b AESSIVRow) int { return strings.Compare(a.Name, b.Name) }) {
		return fmt.Errorf("corpus: the digest or encryption cases are not ascending by name")
	}

	return nil
}

// CRCRow is one CRC of primitives.json: its input bytes and their CRC.
type CRCRow struct {
	Name  string `json:"name"`
	Input []byte `json:"input"`
	CRC   uint32 `json:"crc"`
}

// UUIDRow is one UUID of primitives.json: its 16 bytes in format byte order, its canonical string,
// and the id of the vector whose pack has it as pack_id, if any.
type UUIDRow struct {
	Bytes  []byte `json:"bytes"`
	String string `json:"string"`
	Pack   string `json:"pack,omitempty"`
}

// PrimitiveUUID is a UUID primitives.json lists, with the id of the vector whose pack_id it is, or "" for none.
type PrimitiveUUID struct {
	UUID tracepack.UUID
	Pack string
}

// NewPrimitives returns primitives.json:
// the CRC check value, then the CRCs that fileHeader and blockEnvelope store over their bytes 0-75 and 0-35,
// and the UUID rows of uuids, ascending by their bytes;
// its key arrays are empty until AddKeys sets them.
//
// Parameters:
//   - fileHeader: at least the 80 bytes of a file header.
//   - blockEnvelope: at least the 40 bytes of a block envelope.
//   - uuids: the UUIDs to list.
//
// Returns:
//   - Primitives: the file's content.
//   - error: a structure too short, or a stored CRC other than the CRC of the bytes it covers.
func NewPrimitives(fileHeader, blockEnvelope []byte, uuids []PrimitiveUUID) (Primitives, error) {
	p := Primitives{CRC: []CRCRow{{Name: CRCCheckValue, Input: []byte(crcCheckInput), CRC: format.CRC([]byte(crcCheckInput))}}}

	for _, s := range []struct {
		name  string
		b     []byte
		crcAt int
	}{
		{CRCFileHeader, fileHeader, fileHeaderCRCOff},
		{CRCBlockEnvelope, blockEnvelope, blockEnvelopeCRCOff},
	} {
		if len(s.b) < s.crcAt+4 {
			return Primitives{}, fmt.Errorf("corpus: %s of %d bytes holds no CRC at %d", s.name, len(s.b), s.crcAt)
		}
		input := s.b[:s.crcAt]
		stored := binary.LittleEndian.Uint32(s.b[s.crcAt:])
		if crc := format.CRC(input); crc != stored {
			return Primitives{}, fmt.Errorf("corpus: %s stores CRC 0x%08X over bytes whose CRC is 0x%08X", s.name, stored, crc)
		}
		p.CRC = append(p.CRC, CRCRow{Name: s.name, Input: bytes.Clone(input), CRC: stored})
	}

	p.UUIDs = make([]UUIDRow, 0, len(uuids))
	for _, u := range uuids {
		p.UUIDs = append(p.UUIDs, UUIDRow{Bytes: bytes.Clone(u.UUID[:]), String: u.UUID.String(), Pack: u.Pack})
	}
	slices.SortFunc(p.UUIDs, func(a, b UUIDRow) int { return bytes.Compare(a.Bytes, b.Bytes) })
	p.DigestKeys, p.EncryptionKeys, p.HMAC, p.AESSIV = []KeyRow{}, []KeyRow{}, []HMACRow{}, []AESSIVRow{}

	return p, nil
}

// Marshal returns primitives.json in the canonical form.
func (p *Primitives) Marshal() ([]byte, error) {
	return Marshal(p)
}
