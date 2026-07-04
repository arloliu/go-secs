package hsms

import (
	"crypto/rand"
	"encoding/binary"
	"io"
	"sync"
	"sync/atomic"
)

var (
	genInst *msgIDGenerator
	genOnce sync.Once
)

// msgIDGenerator produces process-wide unique message IDs for applications that
// assign their own System Bytes before handing a message to go-secs.
//
// It seeds its counter from a cryptographically-secure random source so IDs do
// not restart from a predictable value across process restarts, then increments
// atomically for concurrency safety. IDs are uint32 and wrap at 2^32.
//
// This global generator is intentionally independent of the per-connection
// System Bytes counter that go-secs uses for messages it originates itself.
type msgIDGenerator struct {
	id atomic.Uint32
}

func newMsgIDGenerator() *msgIDGenerator {
	inst := &msgIDGenerator{}

	var buf [4]byte
	if _, err := io.ReadFull(rand.Reader, buf[:]); err != nil {
		return inst
	}
	inst.id.Store(binary.LittleEndian.Uint32(buf[:]))

	return inst
}

func getMsgIDGenerator() *msgIDGenerator {
	genOnce.Do(func() {
		genInst = newMsgIDGenerator()
	})

	return genInst
}

// GenerateMsgID returns a process-wide unique message ID as a uint32.
//
// The first call seeds the generator from a cryptographically-secure random
// source; subsequent calls increment atomically and are safe for concurrent use.
// Pass the result to [ToSystemBytes] to obtain the System Bytes for a message.
func GenerateMsgID() uint32 {
	return getMsgIDGenerator().genID()
}

// ToSystemBytes converts a message ID into HSMS System Bytes: a 4-byte
// big-endian array suitable for [NewDataMessage] and the control-message
// constructors.
func ToSystemBytes(id uint32) [4]byte {
	var b [4]byte
	binary.BigEndian.PutUint32(b[:], id)

	return b
}

// FromSystemBytes decodes HSMS System Bytes (header bytes 6-9, big-endian) into a
// message ID uint32. It is the inverse of [ToSystemBytes]:
// FromSystemBytes(ToSystemBytes(id)) == id for all id.
func FromSystemBytes(b [4]byte) uint32 {
	return binary.BigEndian.Uint32(b[:])
}

func (m *msgIDGenerator) genID() uint32 {
	return m.id.Add(1)
}
