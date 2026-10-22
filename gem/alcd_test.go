package gem_test

import (
	"fmt"
	"testing"

	"github.com/arloliu/go-secs/v2/gem"
	"github.com/arloliu/go-secs/v2/secs2"
)

// ExampleALCD sends an alarm set in the personal safety category, then reads the code back.
func ExampleALCD() {
	msg := gem.S5F1(gem.ALCDAlarmSet|gem.ALCDPersonalSafety, secs2.U4(12), "Door open")

	alarm, err := gem.DecodeS5F1(msg.Item())
	if err != nil {
		fmt.Println("decode:", err)
		return
	}

	fmt.Println(alarm.ALCD.Set(), alarm.ALCD.Category() == gem.ALCDPersonalSafety)

	// Output:
	// true true
}

func TestALCDSetAndCategory(t *testing.T) {
	tests := []struct {
		alcd     gem.ALCD
		set      bool
		category gem.ALCD
	}{
		{alcd: 0x00, set: false, category: 0},
		{alcd: gem.ALCDDataIntegrity, set: false, category: gem.ALCDDataIntegrity},
		{alcd: gem.ALCDAlarmSet | gem.ALCDEquipmentSafety, set: true, category: gem.ALCDEquipmentSafety},
		{alcd: 0x7F, set: false, category: 127},
		{alcd: 0xFF, set: true, category: 127},
	}

	for _, tt := range tests {
		if got := tt.alcd.Set(); got != tt.set {
			t.Errorf("ALCD %#x: Set() = %v, want %v", byte(tt.alcd), got, tt.set)
		}

		if got := tt.alcd.Category(); got != tt.category {
			t.Errorf("ALCD %#x: Category() = %d, want %d", byte(tt.alcd), got, tt.category)
		}
	}
}

// TestDecodeALCDUnknownCategory proves a category E5 does not define,
// reserved or equipment-defined, decodes without error, in S5F1 and in an Alarm.
func TestDecodeALCDUnknownCategory(t *testing.T) {
	body, err := gem.DecodeS5F1(wire(t, secs2.L(secs2.B(0xFD), secs2.U4(12), secs2.A("Door open"))))
	if err != nil {
		t.Fatalf("DecodeS5F1: %v", err)
	}

	if !body.ALCD.Set() || body.ALCD.Category() != 125 {
		t.Errorf("ALCD %#x: Set() = %v, Category() = %d, want true, 125", byte(body.ALCD), body.ALCD.Set(), body.ALCD.Category())
	}

	reply, err := gem.DecodeS5F6(wire(t, secs2.L(secs2.L(secs2.B(0x7D), secs2.U4(12), secs2.A("Door open")))))
	if err != nil {
		t.Fatalf("DecodeS5F6: %v", err)
	}

	// An Alarm's ALCD may arrive zero-length, so it is a pointer: check for nil before reading it.
	alcd := reply.ALARMS[0].ALCD
	if alcd == nil {
		t.Fatal("ALARMS[0].ALCD: got nil, want 0x7D")
	}

	if alcd.Set() || alcd.Category() != 125 {
		t.Errorf("ALARMS[0].ALCD %#x: Set() = %v, Category() = %d, want false, 125", byte(*alcd), alcd.Set(), alcd.Category())
	}
}
