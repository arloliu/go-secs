package corpus

import (
	"bytes"
	"encoding/binary"
	"fmt"
	"slices"

	"github.com/arloliu/go-secs/tracepack"
	"github.com/arloliu/go-secs/tracepack/internal/format"
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
	CRC   []CRCRow  `json:"crc"`
	UUIDs []UUIDRow `json:"uuids"`
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
// and the UUID rows of uuids, ascending by their bytes.
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

	return p, nil
}

// Marshal returns primitives.json in the canonical form.
func (p *Primitives) Marshal() ([]byte, error) {
	return Marshal(p)
}
