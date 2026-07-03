package gem_test

import (
	"testing"

	"github.com/arloliu/go-secs/v2/gem"
	"github.com/arloliu/go-secs/v2/secs2"
)

// assertS1Header checks that msg is an S1 message with the given function code and wait bit.
func assertS1Header(t *testing.T, msg secs2.SECS2Message, function uint8, wantW bool) {
	t.Helper()

	if msg.StreamCode() != 1 {
		t.Errorf("StreamCode: got %d, want 1", msg.StreamCode())
	}

	if msg.FunctionCode() != function {
		t.Errorf("FunctionCode: got %d, want %d", msg.FunctionCode(), function)
	}

	if msg.WaitBit() != wantW {
		t.Errorf("WaitBit: got %v, want %v", msg.WaitBit(), wantW)
	}
}

func TestS1F1(t *testing.T) {
	msg := gem.S1F1()

	assertS1Header(t, msg, 1, true)

	if !msg.Item().IsEmpty() {
		t.Errorf("item type: got %s, want empty", msg.Item().Type())
	}
}

func TestS1F2(t *testing.T) {
	msg := gem.S1F2("MODEL-X", "2.3.1")

	assertS1Header(t, msg, 2, false)

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

	mdln, err := items[0].ToASCII()
	if err != nil {
		t.Fatalf("items[0].ToASCII: %v", err)
	}

	if mdln != "MODEL-X" {
		t.Errorf("mdln: got %q, want %q", mdln, "MODEL-X")
	}

	softrev, err := items[1].ToASCII()
	if err != nil {
		t.Fatalf("items[1].ToASCII: %v", err)
	}

	if softrev != "2.3.1" {
		t.Errorf("softrev: got %q, want %q", softrev, "2.3.1")
	}
}

func TestS1F2Host(t *testing.T) {
	msg := gem.S1F2Host()

	assertS1Header(t, msg, 2, false)

	decoded, err := secs2.Decode(msg.Item().ToBytes())
	if err != nil {
		t.Fatalf("secs2.Decode: %v", err)
	}

	items, err := decoded.ToList()
	if err != nil {
		t.Fatalf("ToList: %v", err)
	}

	if len(items) != 0 {
		t.Fatalf("list length: got %d, want 0", len(items))
	}
}

func TestS1F13(t *testing.T) {
	msg := gem.S1F13("MODEL-X", "2.3.1")

	assertS1Header(t, msg, 13, true)

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

	mdln, err := items[0].ToASCII()
	if err != nil {
		t.Fatalf("items[0].ToASCII: %v", err)
	}

	if mdln != "MODEL-X" {
		t.Errorf("mdln: got %q, want %q", mdln, "MODEL-X")
	}

	softrev, err := items[1].ToASCII()
	if err != nil {
		t.Fatalf("items[1].ToASCII: %v", err)
	}

	if softrev != "2.3.1" {
		t.Errorf("softrev: got %q, want %q", softrev, "2.3.1")
	}
}

func TestS1F13Host(t *testing.T) {
	msg := gem.S1F13Host()

	assertS1Header(t, msg, 13, true)

	decoded, err := secs2.Decode(msg.Item().ToBytes())
	if err != nil {
		t.Fatalf("secs2.Decode: %v", err)
	}

	items, err := decoded.ToList()
	if err != nil {
		t.Fatalf("ToList: %v", err)
	}

	if len(items) != 0 {
		t.Fatalf("list length: got %d, want 0", len(items))
	}
}

func TestS1F14(t *testing.T) {
	msg := gem.S1F14(0, "MODEL-X", "2.3.1")

	assertS1Header(t, msg, 14, false)

	decoded, err := secs2.Decode(msg.Item().ToBytes())
	if err != nil {
		t.Fatalf("secs2.Decode: %v", err)
	}

	// L[2]{ B[commack] L[2]{ A[mdln] A[softrev] } }
	outer, err := decoded.ToList()
	if err != nil {
		t.Fatalf("outer ToList: %v", err)
	}

	if len(outer) != 2 {
		t.Fatalf("outer list length: got %d, want 2", len(outer))
	}

	commack, err := outer[0].ToBinary()
	if err != nil {
		t.Fatalf("outer[0].ToBinary: %v", err)
	}

	if len(commack) != 1 || commack[0] != 0 {
		t.Errorf("commack: got %v, want [0x00]", commack)
	}

	inner, err := outer[1].ToList()
	if err != nil {
		t.Fatalf("outer[1].ToList: %v", err)
	}

	if len(inner) != 2 {
		t.Fatalf("inner list length: got %d, want 2", len(inner))
	}

	mdln, err := inner[0].ToASCII()
	if err != nil {
		t.Fatalf("inner[0].ToASCII: %v", err)
	}

	if mdln != "MODEL-X" {
		t.Errorf("mdln: got %q, want %q", mdln, "MODEL-X")
	}

	softrev, err := inner[1].ToASCII()
	if err != nil {
		t.Fatalf("inner[1].ToASCII: %v", err)
	}

	if softrev != "2.3.1" {
		t.Errorf("softrev: got %q, want %q", softrev, "2.3.1")
	}
}

func TestS1F14Host(t *testing.T) {
	msg := gem.S1F14Host(0)

	assertS1Header(t, msg, 14, false)

	decoded, err := secs2.Decode(msg.Item().ToBytes())
	if err != nil {
		t.Fatalf("secs2.Decode: %v", err)
	}

	// L[2]{ B[commack] L[0] }
	outer, err := decoded.ToList()
	if err != nil {
		t.Fatalf("outer ToList: %v", err)
	}

	if len(outer) != 2 {
		t.Fatalf("outer list length: got %d, want 2", len(outer))
	}

	commack, err := outer[0].ToBinary()
	if err != nil {
		t.Fatalf("outer[0].ToBinary: %v", err)
	}

	if len(commack) != 1 || commack[0] != 0 {
		t.Errorf("commack: got %v, want [0x00]", commack)
	}

	inner, err := outer[1].ToList()
	if err != nil {
		t.Fatalf("outer[1].ToList: %v", err)
	}

	if len(inner) != 0 {
		t.Fatalf("inner list length: got %d, want 0", len(inner))
	}
}
