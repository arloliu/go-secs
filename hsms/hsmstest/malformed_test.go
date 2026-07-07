package hsmstest_test

import (
	"testing"

	"github.com/arloliu/go-secs/v2/hsms/hsmstest"
)

func TestMalformedDataMessage(t *testing.T) {
	msg := hsmstest.MalformedDataMessage(6, 11, true)

	if msg == nil {
		t.Fatal("MalformedDataMessage returned nil")
	}
	if got := msg.Stream(); got != 6 {
		t.Errorf("Stream() = %d, want 6", got)
	}
	if got := msg.Function(); got != 11 {
		t.Errorf("Function() = %d, want 11", got)
	}
	if !msg.WaitBit() {
		t.Error("WaitBit() = false, want true")
	}
	if err := msg.DecodeErr(); err == nil {
		t.Error("DecodeErr() = nil, want a decode error")
	}
}
