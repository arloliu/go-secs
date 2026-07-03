package gem_test

import (
	"testing"

	"github.com/arloliu/go-secs/v2/gem"
	"github.com/arloliu/go-secs/v2/secs2"
)

// assertS2Header checks that msg is an S2 message with the given function code and wait bit.
func assertS2Header(t *testing.T, msg secs2.SECS2Message, function uint8, wantW bool) {
	t.Helper()

	if msg.StreamCode() != 2 {
		t.Errorf("StreamCode: got %d, want 2", msg.StreamCode())
	}

	if msg.FunctionCode() != function {
		t.Errorf("FunctionCode: got %d, want %d", msg.FunctionCode(), function)
	}

	if msg.WaitBit() != wantW {
		t.Errorf("WaitBit: got %v, want %v", msg.WaitBit(), wantW)
	}
}

func TestS2F17(t *testing.T) {
	msg := gem.S2F17()

	assertS2Header(t, msg, 17, true)

	if !msg.Item().IsEmpty() {
		t.Errorf("item type: got %s, want empty", msg.Item().Type())
	}
}

func TestS2F18(t *testing.T) {
	ts := "20260702120000"
	msg := gem.S2F18(ts)

	assertS2Header(t, msg, 18, false)

	decoded, err := secs2.Decode(msg.Item().ToBytes())
	if err != nil {
		t.Fatalf("secs2.Decode: %v", err)
	}

	got, err := decoded.ToASCII()
	if err != nil {
		t.Fatalf("ToASCII: %v", err)
	}

	if got != ts {
		t.Errorf("date-time: got %q, want %q", got, ts)
	}
}

func TestS2F31(t *testing.T) {
	ts := "20260702120000"
	msg := gem.S2F31(ts)

	assertS2Header(t, msg, 31, true)

	decoded, err := secs2.Decode(msg.Item().ToBytes())
	if err != nil {
		t.Fatalf("secs2.Decode: %v", err)
	}

	got, err := decoded.ToASCII()
	if err != nil {
		t.Fatalf("ToASCII: %v", err)
	}

	if got != ts {
		t.Errorf("date-time: got %q, want %q", got, ts)
	}
}

func TestS2F32(t *testing.T) {
	msg := gem.S2F32(0)

	assertS2Header(t, msg, 32, false)

	decoded, err := secs2.Decode(msg.Item().ToBytes())
	if err != nil {
		t.Fatalf("secs2.Decode: %v", err)
	}

	bs, err := decoded.ToBinary()
	if err != nil {
		t.Fatalf("ToBinary: %v", err)
	}

	if len(bs) != 1 || bs[0] != 0 {
		t.Errorf("tiack: got %v, want [0x00]", bs)
	}
}

func TestS2F37NoCEIDs(t *testing.T) {
	msg := gem.S2F37(true)

	assertS2Header(t, msg, 37, true)

	decoded, err := secs2.Decode(msg.Item().ToBytes())
	if err != nil {
		t.Fatalf("secs2.Decode: %v", err)
	}

	// L[2]{ BOOLEAN[true] L[0] }
	items, err := decoded.ToList()
	if err != nil {
		t.Fatalf("ToList: %v", err)
	}

	if len(items) != 2 {
		t.Fatalf("list length: got %d, want 2", len(items))
	}

	bools, err := items[0].ToBoolean()
	if err != nil {
		t.Fatalf("items[0].ToBoolean: %v", err)
	}

	if len(bools) != 1 || !bools[0] {
		t.Errorf("enable: got %v, want [true]", bools)
	}

	ceids, err := items[1].ToList()
	if err != nil {
		t.Fatalf("items[1].ToList: %v", err)
	}

	if len(ceids) != 0 {
		t.Fatalf("ceid list length: got %d, want 0", len(ceids))
	}
}

func TestS2F37WithCEIDs(t *testing.T) {
	ceid1 := secs2.U4(100)
	ceid2 := secs2.U4(200)

	msg := gem.S2F37(false, ceid1, ceid2)

	assertS2Header(t, msg, 37, true)

	decoded, err := secs2.Decode(msg.Item().ToBytes())
	if err != nil {
		t.Fatalf("secs2.Decode: %v", err)
	}

	// L[2]{ BOOLEAN[false] L[2]{ U4[100] U4[200] } }
	items, err := decoded.ToList()
	if err != nil {
		t.Fatalf("ToList: %v", err)
	}

	if len(items) != 2 {
		t.Fatalf("list length: got %d, want 2", len(items))
	}

	bools, err := items[0].ToBoolean()
	if err != nil {
		t.Fatalf("items[0].ToBoolean: %v", err)
	}

	if len(bools) != 1 || bools[0] {
		t.Errorf("enable: got %v, want [false]", bools)
	}

	ceids, err := items[1].ToList()
	if err != nil {
		t.Fatalf("items[1].ToList: %v", err)
	}

	if len(ceids) != 2 {
		t.Fatalf("ceid list length: got %d, want 2", len(ceids))
	}

	v0, err := ceids[0].ToUint()
	if err != nil {
		t.Fatalf("ceids[0].ToUint: %v", err)
	}

	if len(v0) != 1 || v0[0] != 100 {
		t.Errorf("ceid[0]: got %v, want [100]", v0)
	}

	v1, err := ceids[1].ToUint()
	if err != nil {
		t.Fatalf("ceids[1].ToUint: %v", err)
	}

	if len(v1) != 1 || v1[0] != 200 {
		t.Errorf("ceid[1]: got %v, want [200]", v1)
	}
}

func TestS2F38(t *testing.T) {
	msg := gem.S2F38(0)

	assertS2Header(t, msg, 38, false)

	decoded, err := secs2.Decode(msg.Item().ToBytes())
	if err != nil {
		t.Fatalf("secs2.Decode: %v", err)
	}

	bs, err := decoded.ToBinary()
	if err != nil {
		t.Fatalf("ToBinary: %v", err)
	}

	if len(bs) != 1 || bs[0] != 0 {
		t.Errorf("erack: got %v, want [0x00]", bs)
	}
}
