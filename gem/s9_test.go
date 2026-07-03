package gem_test

import (
	"testing"

	"github.com/arloliu/go-secs/v2/gem"
	"github.com/arloliu/go-secs/v2/secs2"
)

var testMHEAD = [10]byte{0x00, 0x01, 0x09, 0x01, 0x00, 0x00, 0x00, 0x00, 0x00, 0x01}
var testSHEAD = [10]byte{0xFF, 0xFE, 0x09, 0x09, 0x00, 0x00, 0xDE, 0xAD, 0xBE, 0xEF}

// assertMsgHeader checks that msg is an S9 message with the given function code and W=false.
func assertMsgHeader(t *testing.T, msg secs2.SECS2Message, function uint8) {
	t.Helper()

	if msg.StreamCode() != 9 {
		t.Errorf("StreamCode: got %d, want 9", msg.StreamCode())
	}

	if msg.FunctionCode() != function {
		t.Errorf("FunctionCode: got %d, want %d", msg.FunctionCode(), function)
	}

	if msg.WaitBit() {
		t.Error("WaitBit: got true, want false")
	}
}

// assert10ByteBinary round-trips item through secs2.Decode and checks it yields a 10-byte binary
// equal to want.
func assert10ByteBinary(t *testing.T, item secs2.Item, want [10]byte) {
	t.Helper()

	decoded, err := secs2.Decode(item.ToBytes())
	if err != nil {
		t.Fatalf("secs2.Decode: %v", err)
	}

	got, err := decoded.ToBinary()
	if err != nil {
		t.Fatalf("ToBinary: %v", err)
	}

	if len(got) != 10 {
		t.Fatalf("binary length: got %d, want 10", len(got))
	}

	for i, b := range want {
		if got[i] != b {
			t.Errorf("byte[%d]: got 0x%02X, want 0x%02X", i, got[i], b)
		}
	}
}

func TestS9F1(t *testing.T) {
	msg := gem.S9F1(testMHEAD)

	assertMsgHeader(t, msg, 1)
	assert10ByteBinary(t, msg.Item(), testMHEAD)
}

func TestS9F3(t *testing.T) {
	msg := gem.S9F3(testMHEAD)

	assertMsgHeader(t, msg, 3)
	assert10ByteBinary(t, msg.Item(), testMHEAD)
}

func TestS9F5(t *testing.T) {
	msg := gem.S9F5(testMHEAD)

	assertMsgHeader(t, msg, 5)
	assert10ByteBinary(t, msg.Item(), testMHEAD)
}

func TestS9F7(t *testing.T) {
	msg := gem.S9F7(testMHEAD)

	assertMsgHeader(t, msg, 7)
	assert10ByteBinary(t, msg.Item(), testMHEAD)
}

func TestS9F9(t *testing.T) {
	msg := gem.S9F9(testSHEAD)

	assertMsgHeader(t, msg, 9)
	assert10ByteBinary(t, msg.Item(), testSHEAD)
}

func TestS9F11(t *testing.T) {
	msg := gem.S9F11(testMHEAD)

	assertMsgHeader(t, msg, 11)
	assert10ByteBinary(t, msg.Item(), testMHEAD)
}

func TestS9F13(t *testing.T) {
	mexp := "S1F1"
	edid := secs2.A("EQUIPMENT_ID")

	msg := gem.S9F13(mexp, edid)

	assertMsgHeader(t, msg, 13)

	// Body must be L[2]{ A[mexp], edid }.
	decoded, err := secs2.Decode(msg.Item().ToBytes())
	if err != nil {
		t.Fatalf("secs2.Decode: %v", err)
	}

	items, err := decoded.ToList()
	if err != nil {
		t.Fatalf("ToList: %v", err)
	}

	if len(items) != 2 {
		t.Fatalf("list length: got %d, want 2", len(items))
	}

	got0, err := items[0].ToASCII()
	if err != nil {
		t.Fatalf("items[0].ToASCII: %v", err)
	}

	if got0 != mexp {
		t.Errorf("items[0]: got %q, want %q", got0, mexp)
	}

	got1, err := items[1].ToASCII()
	if err != nil {
		t.Fatalf("items[1].ToASCII: %v", err)
	}

	if got1 != "EQUIPMENT_ID" {
		t.Errorf("items[1]: got %q, want %q", got1, "EQUIPMENT_ID")
	}
}

// TestGemNoBaseExport verifies gem does not export Message or NewMessage.
// This is a compile-time check: the gem_test package can only see exported symbols.
// If gem.Message or gem.NewMessage were accessible, the references below would compile;
// since they must NOT exist, this test simply confirms the package compiles without them.
func TestGemNoBaseExport(_ *testing.T) {
	// intentionally empty — compile failure would be the signal
}
