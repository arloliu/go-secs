package hsms

import (
	"testing"
)

func TestGenerateMsgID(t *testing.T) {
	gen := getMsgIDGenerator()
	id1 := gen.genID()
	id2 := gen.genID()

	if id1 == id2 {
		t.Errorf("Expected different IDs, got %d and %d", id1, id2)
	}

	id1 = GenerateMsgID()
	id2 = GenerateMsgID()

	if id1 == id2 {
		t.Errorf("Expected different IDs, got %d and %d", id1, id2)
	}
}

func TestGenerateMsgSystemBytes(t *testing.T) {
	gen := getMsgIDGenerator()
	sysBytes1 := gen.genSystemBytes()
	sysBytes2 := gen.genSystemBytes()

	if string(sysBytes1) == string(sysBytes2) {
		t.Errorf("Expected different system bytes, got %v and %v", sysBytes1, sysBytes2)
	}

	sysBytes1 = GenerateMsgSystemBytes()
	sysBytes2 = GenerateMsgSystemBytes()

	if string(sysBytes1) == string(sysBytes2) {
		t.Errorf("Expected different system bytes, got %v and %v", sysBytes1, sysBytes2)
	}
}

func TestToSystemBytes(t *testing.T) {
	id := uint32(123456)
	expected := []byte{0x00, 0x01, 0xe2, 0x40}
	result := ToSystemBytes(id)

	for i, b := range expected {
		if result[i] != b {
			t.Errorf("Expected byte %v at position %d, got %v", b, i, result[i])
		}
	}
}
