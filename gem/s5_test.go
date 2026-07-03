package gem_test

import (
	"testing"

	"github.com/arloliu/go-secs/v2/gem"
	"github.com/arloliu/go-secs/v2/secs2"
)

// assertS5Header checks that msg is an S5 message with the given function code and wait bit.
func assertS5Header(t *testing.T, msg secs2.SECS2Message, function uint8, wantW bool) {
	t.Helper()

	if msg.StreamCode() != 5 {
		t.Errorf("StreamCode: got %d, want 5", msg.StreamCode())
	}

	if msg.FunctionCode() != function {
		t.Errorf("FunctionCode: got %d, want %d", msg.FunctionCode(), function)
	}

	if msg.WaitBit() != wantW {
		t.Errorf("WaitBit: got %v, want %v", msg.WaitBit(), wantW)
	}
}

func TestS5F1(t *testing.T) {
	alcd := byte(0x81) // bit 7 set = alarm active
	alid := secs2.U4(42)
	altx := "Low Temperature Alarm"

	msg := gem.S5F1(alcd, alid, altx)

	assertS5Header(t, msg, 1, true)

	decoded, err := secs2.Decode(msg.Item().ToBytes())
	if err != nil {
		t.Fatalf("secs2.Decode: %v", err)
	}

	// L[3]{ B[alcd] U4[alid] A[altx] }
	items, err := decoded.ToList()
	if err != nil {
		t.Fatalf("ToList: %v", err)
	}

	if len(items) != 3 {
		t.Fatalf("list length: got %d, want 3", len(items))
	}

	bs, err := items[0].ToBinary()
	if err != nil {
		t.Fatalf("items[0].ToBinary: %v", err)
	}

	if len(bs) != 1 || bs[0] != alcd {
		t.Errorf("alcd: got %v, want [0x%02X]", bs, alcd)
	}

	vals, err := items[1].ToUint()
	if err != nil {
		t.Fatalf("items[1].ToUint: %v", err)
	}

	if len(vals) != 1 || vals[0] != 42 {
		t.Errorf("alid: got %v, want [42]", vals)
	}

	txt, err := items[2].ToASCII()
	if err != nil {
		t.Fatalf("items[2].ToASCII: %v", err)
	}

	if txt != altx {
		t.Errorf("altx: got %q, want %q", txt, altx)
	}
}

func TestS5F2(t *testing.T) {
	msg := gem.S5F2(0)

	assertS5Header(t, msg, 2, false)

	decoded, err := secs2.Decode(msg.Item().ToBytes())
	if err != nil {
		t.Fatalf("secs2.Decode: %v", err)
	}

	bs, err := decoded.ToBinary()
	if err != nil {
		t.Fatalf("ToBinary: %v", err)
	}

	if len(bs) != 1 || bs[0] != 0 {
		t.Errorf("ackc5: got %v, want [0x00]", bs)
	}
}
