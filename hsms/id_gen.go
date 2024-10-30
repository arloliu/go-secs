package hsms

import (
	"crypto/rand"
	"encoding/binary"
	"io"
	"sync"
	"sync/atomic"
)

// msgIDGenerator is a singleton that generates unique message IDs and their corresponding system bytes
// for HSMS messages.
//
// It uses a cryptographically secure random number generator to initialize the starting ID and
// atomically increments the ID to ensure uniqueness in concurrent environments.
//
// The generated message IDs are represented as uint32 values, and the system bytes are 4-byte
// slice in big-endian order.
type msgIDGenerator struct {
	id atomic.Uint32
}

func newMsgIDGenerator() *msgIDGenerator {
	inst := &msgIDGenerator{}
	var buf [4]byte
	_, err := io.ReadFull(rand.Reader, buf[:])
	if err != nil {
		return inst
	}
	inst.id.Store(binary.LittleEndian.Uint32(buf[:]))
	return inst
}

func (m *msgIDGenerator) genID() uint32 {
	return m.id.Add(1)
}

func (m *msgIDGenerator) genSystemBytes() []byte {
	result := make([]byte, 4)
	binary.BigEndian.PutUint32(result, m.id.Add(1))
	return result
}

var (
	genInst = &msgIDGenerator{}
	once    sync.Once
)

func getMsgIDGenerator() *msgIDGenerator {
	once.Do(func() {
		genInst = newMsgIDGenerator()
	})
	return genInst
}

// GenerateMsgID returns a unique message ID as a uint32.
func GenerateMsgID() uint32 {
	return getMsgIDGenerator().genID()
}

// GenerateMsgSystemBytes returns a unique 4-byte slice representing the system bytes for a message.
func GenerateMsgSystemBytes() []byte {
	return getMsgIDGenerator().genSystemBytes()
}

// ToSystemBytes converts id to 4-byte slice system bytes.
func ToSystemBytes(id uint32) []byte {
	systemBytes := make([]byte, 4)
	binary.BigEndian.PutUint32(systemBytes, id)
	return systemBytes
}
