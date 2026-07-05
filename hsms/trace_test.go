package hsms

import "testing"

func TestHexDump(t *testing.T) {
	got := hexDump([]byte{0x00, 0x0A, 0xFF})
	want := "000aff"
	if got != want {
		t.Errorf("hexDump() = %q, want %q", got, want)
	}
}

func TestHexDump_Empty(t *testing.T) {
	if got := hexDump(nil); got != "" {
		t.Errorf("hexDump(nil) = %q, want empty string", got)
	}
}
