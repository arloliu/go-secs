package hsms

import (
	"encoding/binary"
	"errors"

	"github.com/arloliu/go-secs/secs2"
)

// snapshotMessage is an unexported HSMSMessage that owns a complete serialized
// HSMS frame ([4-byte length][10-byte header][item bytes]).
//
// It serves the decode-free relay path: ToBytes() returns the raw frame verbatim
// until the caller performs a structural access (Item, ToDataMessage, …), at which
// point the item bytes are lazily decoded once and cached.
//
// The frame is GC-owned and never pooled.  snapshotMessage is not safe for
// concurrent use without external locking (single-owner assumption).
type snapshotMessage struct {
	frame     []byte     // owned full frame [4 len][10 header][item]; never reused/shared
	err       error      // message-level error (SetError); separate from decodeErr
	item      secs2.Item // lazily decoded; nil until first structural access
	decodeErr error      // lazy item-decode failure; separate from err
}

// Compile-time interface assertion.
var _ HSMSMessage = (*snapshotMessage)(nil)

// dataItem is the internal helper that performs lazy decode.
// It clears decodeErr, then decodes once, caches decoded.Clone() and frees the intermediate.
// Sets/clears ONLY decodeErr — never s.err.
func (s *snapshotMessage) dataItem() secs2.Item {
	if s.item == nil {
		s.decodeErr = nil
		if len(s.frame) <= 14 {
			s.item = secs2.NewEmptyItem()
		} else {
			decoded, err := DecodeSECS2Item(s.frame[14:])
			if err != nil {
				s.item, s.decodeErr = secs2.NewEmptyItem(), err
			} else {
				s.item = decoded.Clone()
				decoded.Free()
			}
		}
	}

	return s.item
}

// Type returns DataMsgType for all snapshot messages.
func (s *snapshotMessage) Type() int { return DataMsgType }

// IsDataMessage returns true.
func (s *snapshotMessage) IsDataMessage() bool { return true }

// IsControlMessage returns false.
func (s *snapshotMessage) IsControlMessage() bool { return false }

// ToControlMessage always returns (nil, false).
func (s *snapshotMessage) ToControlMessage() (*ControlMessage, bool) { return nil, false }

// SessionID returns the session ID decoded decode-free from frame[4:6].
func (s *snapshotMessage) SessionID() uint16 {
	if len(s.frame) < 14 {
		return 0
	}

	return binary.BigEndian.Uint16(s.frame[4:6])
}

// SetSessionID patches session ID in frame[4:6] in place.
func (s *snapshotMessage) SetSessionID(sessionID uint16) {
	if len(s.frame) < 14 {
		return
	}
	binary.BigEndian.PutUint16(s.frame[4:6], sessionID)
}

// StreamCode returns the stream code (low 7 bits of frame[6]) decode-free.
func (s *snapshotMessage) StreamCode() uint8 {
	if len(s.frame) < 14 {
		return 0
	}

	return s.frame[6] & 0x7F
}

// FunctionCode returns the function code from frame[7] decode-free.
func (s *snapshotMessage) FunctionCode() uint8 {
	if len(s.frame) < 14 {
		return 0
	}

	return s.frame[7]
}

// WaitBit returns true if the wait bit (MSB of frame[6]) is set, decode-free.
func (s *snapshotMessage) WaitBit() bool {
	if len(s.frame) < 14 {
		return false
	}

	return s.frame[6]&waitBitMask != 0
}

// ID returns a numeric representation of system bytes from frame[10:14] decode-free.
func (s *snapshotMessage) ID() uint32 {
	if len(s.frame) < 14 {
		return 0
	}

	return binary.BigEndian.Uint32(s.frame[10:14])
}

// SetID patches system bytes in frame[10:14] in place.
func (s *snapshotMessage) SetID(id uint32) {
	if len(s.frame) < 14 {
		return
	}
	binary.BigEndian.PutUint32(s.frame[10:14], id)
}

// SystemBytes returns a copy of the 4 system bytes from frame[10:14].
// A copy is returned (not an alias into the frame) to match the documented
// SystemBytes lifetime contract.
func (s *snapshotMessage) SystemBytes() []byte {
	if len(s.frame) < 14 {
		return []byte{0, 0, 0, 0}
	}
	result := make([]byte, 4)
	copy(result, s.frame[10:14])

	return result
}

// SetSystemBytes patches the system bytes in frame[10:14] in place.
// Returns ErrInvalidSystemBytes if systemBytes is not exactly 4 bytes.
func (s *snapshotMessage) SetSystemBytes(systemBytes []byte) error {
	if len(systemBytes) != 4 {
		return ErrInvalidSystemBytes
	}
	if len(s.frame) < 14 {
		return ErrInvalidHeaderLength
	}
	copy(s.frame[10:14], systemBytes)

	return nil
}

// Header returns a copy of the 10-byte header from frame[4:14].
func (s *snapshotMessage) Header() []byte {
	if len(s.frame) < 14 {
		return make([]byte, 10)
	}
	result := make([]byte, 10)
	copy(result, s.frame[4:14])

	return result
}

// SetHeader replaces the 10-byte header section (frame[4:14]) in place.
// Validates PType == 0 and SType == DataMsgType, mirroring DataMessage.SetHeader.
func (s *snapshotMessage) SetHeader(header []byte) error {
	if len(header) != 10 {
		return ErrInvalidHeaderLength
	}
	if header[4] != 0 {
		return ErrInvalidPType
	}
	if header[5] != DataMsgType {
		return ErrInvalidDataMsgSType
	}
	if len(s.frame) < 14 {
		return ErrInvalidHeaderLength
	}
	copy(s.frame[4:14], header)

	return nil
}

// Error returns errors.Join(s.err, s.decodeErr).
// Returns nil when both are nil.
func (s *snapshotMessage) Error() error {
	return errors.Join(s.err, s.decodeErr)
}

// SetError sets the message-level error (s.err only).
// It never touches decodeErr or the cached item.
func (s *snapshotMessage) SetError(err error) {
	s.err = err
}

// Item triggers lazy decode (if not yet done) and returns the cached item.
// Structural access via this method may set s.decodeErr but never clears s.err.
func (s *snapshotMessage) Item() secs2.Item {
	return s.dataItem()
}

// ToBytes serializes the message.
//
// If item == nil (no structural access performed yet), the raw frame is returned
// verbatim — the decode-free relay path.
//
// If item != nil (structural access was performed and item may have been mutated),
// a fresh byte slice is built as [4-byte len][frame[4:14] header][item.ToBytes()],
// so any in-memory mutations to the cached item are reflected.
func (s *snapshotMessage) ToBytes() []byte {
	if s.item == nil {
		return s.frame
	}

	// Rebuild with the (potentially mutated) cached item.
	itemBytes := s.item.ToBytes()
	totalBytes := 14 + len(itemBytes)
	result := make([]byte, totalBytes)
	binary.BigEndian.PutUint32(result[0:4], uint32(totalBytes-4)) //nolint:gosec
	copy(result[4:14], s.frame[4:14])
	copy(result[14:], itemBytes)

	return result
}

// MarshalBinary implements encoding.BinaryMarshaler.
func (s *snapshotMessage) MarshalBinary() ([]byte, error) {
	return s.ToBytes(), nil
}

// UnmarshalBinary validates the input via DecodeHSMSMessage, requires a data
// message, and re-snapshots the receiver on success.  Invalid input returns an
// error and leaves the receiver unchanged.
func (s *snapshotMessage) UnmarshalBinary(b []byte) error {
	m, err := DecodeHSMSMessage(b)
	if err != nil {
		return err
	}

	dm, ok := m.ToDataMessage()
	if !ok {
		m.Free()
		return errors.New("expected data message")
	}

	// Replace receiver state with the validated frame.
	s.frame = append([]byte(nil), b...)
	s.item = nil
	s.decodeErr = nil
	s.err = nil

	dm.Free()

	return nil
}

// Free releases the cached item if present.
// The frame is GC-owned and is never pooled.
// Free never decodes.
func (s *snapshotMessage) Free() {
	if s.item != nil {
		s.item.Free()
		s.item = nil
	}
}

// Clone returns an independent copy of the snapshot.
//
// If item == nil (not yet decoded), returns a new snapshotMessage with a copied
// frame and the message-level error — a lazy clone that will decode on first
// structural access.
//
// If item != nil (decoded; may be mutated), delegates to ToDataMessage() which
// clones the item and builds an independent *DataMessage.  If ToDataMessage()
// fails (rare — the frame was already validated), falls back to the frame-copy
// lazy clone (preserves serialized bytes; note this loses any uncommitted
// in-memory item mutation, acceptable for the rare error path).
func (s *snapshotMessage) Clone() HSMSMessage {
	if s.item == nil {
		return &snapshotMessage{
			frame: append([]byte(nil), s.frame...),
			err:   s.err,
		}
	}

	dm, ok := s.ToDataMessage()
	if !ok {
		// Fallback: frame-copy lazy clone.
		return &snapshotMessage{
			frame: append([]byte(nil), s.frame...),
			err:   s.err,
		}
	}

	return dm
}

// ToDataMessage builds a fresh, independent *DataMessage from the snapshot's
// header and item on every call.  The result is never shared or cached.
//
// s.Item().Clone() is used so that the returned message owns an independent item —
// sharing s.item would allow the caller's Free() to free the snapshot's cached item.
//
// If s.Error() != nil, the error is propagated to the returned message via SetError.
func (s *snapshotMessage) ToDataMessage() (*DataMessage, bool) {
	dm, err := NewDataMessage(
		s.StreamCode(),
		s.FunctionCode(),
		s.WaitBit(),
		s.SessionID(),
		s.SystemBytes(),
		s.Item().Clone(),
	)
	if err != nil {
		return nil, false
	}

	if s.Error() != nil {
		dm.SetError(s.Error())
	}

	return dm, true
}
