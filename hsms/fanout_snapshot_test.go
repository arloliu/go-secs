package hsms

import (
	"encoding/binary"
	"errors"
	"testing"

	"github.com/arloliu/go-secs/secs2"
	"github.com/stretchr/testify/require"
)

// newSnapshotForTest builds a snapshotMessage from a DataMessage's serialized frame.
func newSnapshotForTest(dm *DataMessage) *snapshotMessage {
	return &snapshotMessage{frame: append([]byte(nil), dm.ToBytes()...)}
}

// TestSnapshotMessage is the main test suite for snapshotMessage.
func TestSnapshotMessage(t *testing.T) {
	t.Run("interface assertion", func(t *testing.T) {
		var _ HSMSMessage = (*snapshotMessage)(nil)
	})

	t.Run("ToBytes decode-free relay", func(t *testing.T) {
		dm, err := NewDataMessage(1, 13, true, 0x1234, []byte{0x00, 0x00, 0x00, 0x01},
			secs2.NewASCIIItem("hello"))
		require.NoError(t, err)
		defer dm.Free()

		s := newSnapshotForTest(dm)
		defer s.Free()

		want := dm.ToBytes()
		got := s.ToBytes()
		require.Equal(t, want, got, "ToBytes should return frame verbatim before any structural access")
		require.Nil(t, s.item, "item must remain nil — no decode triggered")
	})

	t.Run("header reads decode-free", func(t *testing.T) {
		sysBytes := []byte{0xAA, 0xBB, 0xCC, 0xDD}
		dm, err := NewDataMessage(6, 11, true, 0xABCD, sysBytes,
			secs2.NewASCIIItem("test"))
		require.NoError(t, err)
		defer dm.Free()

		s := newSnapshotForTest(dm)
		defer s.Free()

		require.Equal(t, DataMsgType, s.Type(), "Type")
		require.True(t, s.IsDataMessage(), "IsDataMessage")
		require.False(t, s.IsControlMessage(), "IsControlMessage")
		cm, ok := s.ToControlMessage()
		require.Nil(t, cm)
		require.False(t, ok, "ToControlMessage should return (nil,false)")

		require.Equal(t, uint16(0xABCD), s.SessionID(), "SessionID")
		require.Equal(t, uint8(6), s.StreamCode(), "StreamCode")
		require.Equal(t, uint8(11), s.FunctionCode(), "FunctionCode")
		require.True(t, s.WaitBit(), "WaitBit")
		require.Equal(t, binary.BigEndian.Uint32(sysBytes), s.ID(), "ID")
		require.Equal(t, sysBytes, s.SystemBytes(), "SystemBytes")
		require.Equal(t, dm.Header(), s.Header(), "Header")

		require.Nil(t, s.item, "item must remain nil after header-only reads")
	})

	t.Run("SystemBytes returns copy not alias", func(t *testing.T) {
		dm, err := NewDataMessage(1, 1, false, 0x0001, []byte{0x01, 0x02, 0x03, 0x04},
			secs2.NewEmptyItem())
		require.NoError(t, err)
		defer dm.Free()

		s := newSnapshotForTest(dm)
		defer s.Free()

		sb := s.SystemBytes()
		sb[0] = 0xFF // mutate the returned slice
		require.Equal(t, byte(0x01), s.frame[10], "frame must not be aliased by SystemBytes()")
	})

	t.Run("Item lazy decode", func(t *testing.T) {
		dm, err := NewDataMessage(1, 13, true, 0x0001, []byte{0, 0, 0, 1},
			secs2.NewASCIIItem("lazy"))
		require.NoError(t, err)
		defer dm.Free()

		s := newSnapshotForTest(dm)
		defer s.Free()

		require.Nil(t, s.item, "item nil before access")

		item := s.Item()
		require.NotNil(t, item, "Item() must not return nil")
		require.NotNil(t, s.item, "item field set after Item()")

		str, err := item.ToASCII()
		require.NoError(t, err)
		require.Equal(t, "lazy", str, "decoded ASCII value")

		// Second call returns same cached item
		item2 := s.Item()
		require.Equal(t, item, item2, "Item() must return cached item on second call")
	})

	t.Run("mutation reflected in ToBytes via SetValues", func(t *testing.T) {
		dm, err := NewDataMessage(1, 13, true, 0x0001, []byte{0, 0, 0, 1},
			secs2.NewASCIIItem("original"))
		require.NoError(t, err)
		defer dm.Free()

		s := newSnapshotForTest(dm)
		defer s.Free()

		originalFrame := dm.ToBytes()

		// Trigger decode
		_ = s.Item()
		require.NotNil(t, s.item)

		// Mutate via SetValues
		err = s.Item().SetValues("changed")
		require.NoError(t, err)

		rebuilt := s.ToBytes()
		require.NotEqual(t, originalFrame, rebuilt, "ToBytes must rebuild after item mutation")

		// Verify the rebuilt message decodes correctly
		decoded, err := DecodeHSMSMessage(rebuilt)
		require.NoError(t, err)
		ddm, ok := decoded.ToDataMessage()
		require.True(t, ok)
		defer ddm.Free()
		str, err := ddm.Item().ToASCII()
		require.NoError(t, err)
		require.Equal(t, "changed", str)
	})

	t.Run("binary alias mutation reflected in ToBytes", func(t *testing.T) {
		dm, err := NewDataMessage(1, 13, false, 0x0002, []byte{0, 0, 0, 2},
			secs2.NewBinaryItem([]byte{0x01, 0x02, 0x03}))
		require.NoError(t, err)
		defer dm.Free()

		s := newSnapshotForTest(dm)
		defer s.Free()

		// Trigger decode — item is now cached as a Clone() with no rawBytes
		item := s.Item()
		require.NotNil(t, s.item)

		// ToBinary() returns the item's internal values alias
		bs, err := item.ToBinary()
		require.NoError(t, err)
		bs[0] = 0xFF

		// ToBytes rebuilds (item != nil), reflecting the in-place mutation
		rebuilt := s.ToBytes()
		decoded, err := DecodeHSMSMessage(rebuilt)
		require.NoError(t, err)
		ddm, ok := decoded.ToDataMessage()
		require.True(t, ok)
		defer ddm.Free()
		got, err := ddm.Item().ToBinary()
		require.NoError(t, err)
		require.Equal(t, byte(0xFF), got[0], "in-place mutation via ToBinary alias must be reflected in ToBytes")
	})

	t.Run("header setters patch in place decode-free", func(t *testing.T) {
		dm, err := NewDataMessage(1, 1, false, 0x0001, []byte{0, 0, 0, 1},
			secs2.NewASCIIItem("test"))
		require.NoError(t, err)
		defer dm.Free()

		s := newSnapshotForTest(dm)
		defer s.Free()

		s.SetSessionID(0xBEEF)
		require.Equal(t, uint16(0xBEEF), s.SessionID())
		require.Nil(t, s.item, "item must remain nil after SetSessionID")

		s.SetID(0xDEADBEEF)
		require.Equal(t, uint32(0xDEADBEEF), s.ID())
		require.Nil(t, s.item, "item must remain nil after SetID")

		newSys := []byte{0x11, 0x22, 0x33, 0x44}
		require.NoError(t, s.SetSystemBytes(newSys))
		require.Equal(t, newSys, s.SystemBytes())
		require.Nil(t, s.item, "item must remain nil after SetSystemBytes")

		require.Error(t, s.SetSystemBytes([]byte{0x01, 0x02}), "short systemBytes must error")

		hdr := make([]byte, 10)
		hdr[0] = 0x00
		hdr[1] = 0x01 // sessionID = 1
		hdr[2] = 0x05 // stream = 5, no wait
		hdr[3] = 0x02 // function = 2
		hdr[4] = 0x00 // PType
		hdr[5] = 0x00 // SType (data)
		hdr[6] = 0xAA
		hdr[7] = 0xBB
		hdr[8] = 0xCC
		hdr[9] = 0xDD
		require.NoError(t, s.SetHeader(hdr))
		require.Equal(t, hdr, s.Header())

		badHdr := make([]byte, 10)
		badHdr[4] = 1
		require.Error(t, s.SetHeader(badHdr), "nonzero PType must error")

		badHdr2 := make([]byte, 10)
		badHdr2[5] = 1
		require.Error(t, s.SetHeader(badHdr2), "nonzero SType must error")

		require.Error(t, s.SetHeader([]byte{0x00, 0x01}), "wrong header length must error")

		// Verify ToBytes frame reflects in-place changes (item still nil)
		require.Nil(t, s.item)
		tb := s.ToBytes()
		require.Equal(t, uint16(1), binary.BigEndian.Uint16(tb[4:6]), "sessionID in frame")
	})

	t.Run("empty body snapshot", func(t *testing.T) {
		dm, err := NewDataMessage(0, 0, false, 0x0000, []byte{0, 0, 0, 0},
			secs2.NewEmptyItem())
		require.NoError(t, err)
		defer dm.Free()

		s := newSnapshotForTest(dm)
		defer s.Free()

		require.Equal(t, 14, len(s.frame), "empty frame must be exactly 14 bytes")

		item := s.Item()
		require.NotNil(t, item)
		require.True(t, item.IsEmpty(), "item must be empty for no-body message")
		require.Equal(t, dm.ToBytes(), s.ToBytes())
		require.Nil(t, s.Error(), "no error expected for valid empty message")
	})

	t.Run("decode error does not panic; Error returns decodeErr", func(t *testing.T) {
		// Build a 14-byte header + 3 bytes of malformed item
		frame := make([]byte, 17)
		binary.BigEndian.PutUint32(frame[0:4], uint32(13)) // msgLen = 17-4 = 13
		frame[4] = 0x00                                    // sessionID
		frame[5] = 0x01
		frame[6] = 0x01 // stream 1
		frame[7] = 0x01 // function 1
		frame[8] = 0x00 // PType
		frame[9] = 0x00 // SType
		// malformed item: format byte 0xFF is invalid format code
		frame[14] = 0xFF
		frame[15] = 0x00
		frame[16] = 0x00

		s := &snapshotMessage{frame: append([]byte(nil), frame...)}
		item := s.Item()
		require.NotNil(t, item, "Item must return non-nil even on decode error")
		require.NotNil(t, s.Error(), "Error() must be non-nil after decode failure")
	})

	t.Run("error-state separation: SetError survives successful Item()", func(t *testing.T) {
		dm, err := NewDataMessage(1, 13, false, 0x0001, []byte{0, 0, 0, 1},
			secs2.NewASCIIItem("ok"))
		require.NoError(t, err)
		defer dm.Free()

		s := newSnapshotForTest(dm)
		defer s.Free()

		sentinel := errors.New("msg-level error")
		s.SetError(sentinel)

		// Successful Item() must not clear s.err
		item := s.Item()
		require.NotNil(t, item)
		require.Nil(t, s.decodeErr, "decodeErr must remain nil after successful decode")
		require.Equal(t, sentinel, s.err, "s.err must not be cleared by a successful Item()")
		require.True(t, errors.Is(s.Error(), sentinel), "Error() must still contain sentinel after Item()")
	})

	t.Run("error-state separation: decode error + SetError both in Error()", func(t *testing.T) {
		// Build frame with malformed item
		frame := make([]byte, 15)
		binary.BigEndian.PutUint32(frame[0:4], uint32(11)) // msgLen = 15-4 = 11
		frame[6] = 0x01                                    // stream
		frame[7] = 0x01                                    // function
		frame[8] = 0x00                                    // PType
		frame[9] = 0x00                                    // SType
		frame[14] = 0xFF                                   // invalid format byte

		s := &snapshotMessage{frame: append([]byte(nil), frame...)}
		sentinel := errors.New("pre-set msg error")
		s.SetError(sentinel)

		_ = s.Item() // trigger decode error

		combined := s.Error()
		require.NotNil(t, combined)
		require.True(t, errors.Is(combined, sentinel), "combined error must include sentinel")
		require.NotNil(t, s.decodeErr, "decodeErr must be set after failed decode")
	})

	t.Run("ToDataMessage independence", func(t *testing.T) {
		dm, err := NewDataMessage(1, 13, true, 0x0001, []byte{0, 0, 0, 1},
			secs2.NewASCIIItem("fanout"))
		require.NoError(t, err)
		dm.Free()

		// Build snapshot directly from bytes (after dm.Free)
		dmAgain, err := NewDataMessage(1, 13, true, 0x0001, []byte{0, 0, 0, 1},
			secs2.NewASCIIItem("fanout"))
		require.NoError(t, err)
		s := newSnapshotForTest(dmAgain)
		dmAgain.Free()

		dm1, ok1 := s.ToDataMessage()
		require.True(t, ok1)
		require.NotNil(t, dm1)

		dm2, ok2 := s.ToDataMessage()
		require.True(t, ok2)
		require.NotNil(t, dm2)

		require.True(t, dm1 != dm2, "two ToDataMessage calls must return different *DataMessage pointers")

		// Mutate dm1's item
		err = dm1.Item().SetValues("mutated")
		require.NoError(t, err)

		// dm2 and snapshot must be unaffected
		dm2str, err := dm2.Item().ToASCII()
		require.NoError(t, err)
		require.Equal(t, "fanout", dm2str, "dm2 item must be independent of dm1 mutation")

		sstr, err := s.Item().ToASCII()
		require.NoError(t, err)
		require.Equal(t, "fanout", sstr, "snapshot item must be independent of dm1 mutation")

		// Free dm1 then verify snapshot is safe
		dm1.Free()
		tb := s.ToBytes()
		require.NotNil(t, tb, "ToBytes must be safe after dm1.Free()")

		dm2.Free()
		s.Free()
	})

	t.Run("UnmarshalBinary accept valid data message", func(t *testing.T) {
		dm, err := NewDataMessage(1, 13, true, 0x0001, []byte{0, 0, 0, 1},
			secs2.NewASCIIItem("unmarshal"))
		require.NoError(t, err)
		defer dm.Free()

		s := &snapshotMessage{}
		err = s.UnmarshalBinary(dm.ToBytes())
		require.NoError(t, err)
		require.NotNil(t, s.frame)
		require.Equal(t, uint8(1), s.StreamCode())
		require.Equal(t, uint8(13), s.FunctionCode())
		require.True(t, s.WaitBit())
		require.Equal(t, uint16(0x0001), s.SessionID())
		require.Nil(t, s.item, "item must be nil after UnmarshalBinary (lazy)")
		require.Nil(t, s.err)
		require.Nil(t, s.decodeErr)
		s.Free()
	})

	t.Run("UnmarshalBinary reject truncated frame", func(t *testing.T) {
		s := &snapshotMessage{}
		err := s.UnmarshalBinary([]byte{0x00, 0x00, 0x00, 0x0A, 0x00, 0x01, 0x01, 0x01})
		require.Error(t, err, "truncated frame must be rejected")
	})

	t.Run("UnmarshalBinary reject length mismatch", func(t *testing.T) {
		dm, err := NewDataMessage(1, 1, false, 0x0001, []byte{0, 0, 0, 1},
			secs2.NewASCIIItem("x"))
		require.NoError(t, err)
		defer dm.Free()

		frame := dm.ToBytes()
		// Corrupt the length field
		binary.BigEndian.PutUint32(frame[0:4], 9999)

		s := &snapshotMessage{}
		err = s.UnmarshalBinary(frame)
		require.Error(t, err, "length mismatch must be rejected")
	})

	t.Run("UnmarshalBinary reject nonzero PType", func(t *testing.T) {
		dm, err := NewDataMessage(1, 1, false, 0x0001, []byte{0, 0, 0, 1},
			secs2.NewEmptyItem())
		require.NoError(t, err)
		defer dm.Free()

		frame := dm.ToBytes()
		frame[8] = 0x01 // nonzero PType

		// Fix the length field to match the (now corrupted) frame
		binary.BigEndian.PutUint32(frame[0:4], uint32(len(frame)-4))

		s := &snapshotMessage{}
		err = s.UnmarshalBinary(frame)
		require.Error(t, err, "nonzero PType must be rejected")
	})

	t.Run("UnmarshalBinary reject control SType", func(t *testing.T) {
		// Build a valid control message frame (SType=SelectReq=1)
		hdrBytes := []byte{0x00, 0x01, 0x00, 0x00, 0x00, 0x01, 0x00, 0x00, 0x00, 0x01}
		ctrl := NewControlMessage(hdrBytes, false)
		frame, err := ctrl.MarshalBinary()
		require.NoError(t, err)
		ctrl.Free()

		s := &snapshotMessage{}
		err = s.UnmarshalBinary(frame)
		require.Error(t, err, "control SType must be rejected")
	})

	t.Run("UnmarshalBinary reject malformed item", func(t *testing.T) {
		// Frame with valid header but a malformed item (format byte only, no length bytes)
		frame := make([]byte, 15)
		binary.BigEndian.PutUint32(frame[0:4], uint32(11)) // 15-4=11
		frame[4] = 0x00
		frame[5] = 0x01  // sessionID
		frame[6] = 0x01  // stream
		frame[7] = 0x01  // function
		frame[8] = 0x00  // PType
		frame[9] = 0x00  // SType=data
		frame[14] = 0x01 // lenBytesCount=1, formatCode=0 but no following length byte — truncated

		s := &snapshotMessage{}
		err := s.UnmarshalBinary(frame)
		require.Error(t, err, "malformed item must be rejected")
	})

	t.Run("Clone independence undecoded", func(t *testing.T) {
		dm, err := NewDataMessage(1, 13, false, 0x0001, []byte{0, 0, 0, 1},
			secs2.NewASCIIItem("clonetest"))
		require.NoError(t, err)
		defer dm.Free()

		s := newSnapshotForTest(dm)
		defer s.Free()

		require.Nil(t, s.item, "precondition: item nil")
		cloned := s.Clone()
		require.NotNil(t, cloned)

		// Record original byte before mutating
		origByte := s.frame[4]

		// Mutate original frame
		s.frame[4] ^= 0xFF

		// Clone must be independent (cloned is a *snapshotMessage with its own frame copy)
		clonedSnap, isSnap := cloned.(*snapshotMessage)
		if isSnap {
			require.Equal(t, origByte, clonedSnap.frame[4],
				"undecoded clone must not alias original frame")
		}
		cloned.Free()
	})

	t.Run("Clone independence decoded", func(t *testing.T) {
		dm, err := NewDataMessage(1, 13, false, 0x0001, []byte{0, 0, 0, 1},
			secs2.NewASCIIItem("cloneafter"))
		require.NoError(t, err)
		defer dm.Free()

		s := newSnapshotForTest(dm)
		defer s.Free()

		// Trigger decode
		_ = s.Item()
		require.NotNil(t, s.item)

		cloned := s.Clone()
		require.NotNil(t, cloned)

		// Mutate snapshot's item after cloning
		_ = s.Item().SetValues("mutated-original")

		// Clone (returned as *DataMessage from ToDataMessage()) must be independent
		clonedItem := cloned.Item()
		str, err := clonedItem.ToASCII()
		require.NoError(t, err)
		require.Equal(t, "cloneafter", str, "decoded clone must not reflect post-clone mutation of original")

		cloned.Free()
	})

	t.Run("MarshalBinary returns ToBytes", func(t *testing.T) {
		dm, err := NewDataMessage(1, 1, false, 0x0001, []byte{0, 0, 0, 1},
			secs2.NewASCIIItem("marshal"))
		require.NoError(t, err)
		defer dm.Free()

		s := newSnapshotForTest(dm)
		defer s.Free()

		got, err := s.MarshalBinary()
		require.NoError(t, err)
		require.Equal(t, s.ToBytes(), got)
	})
}
