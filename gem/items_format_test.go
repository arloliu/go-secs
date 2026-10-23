package gem_test

import (
	"fmt"
	"testing"

	"github.com/arloliu/go-secs/v2/gem"
)

// TestItemTypeFormatting pins how fmt renders a named item type with a String method:
// the verbs that accept a Stringer print or encode the name,
// and the integer verbs and %#v still print the number.
func TestItemTypeFormatting(t *testing.T) {
	tests := []struct {
		format string
		value  gem.COMMACK
		want   string
	}{
		{format: "%v", value: gem.COMMACKDenied, want: "COMMACKDenied"},
		{format: "%s", value: gem.COMMACKDenied, want: "COMMACKDenied"},
		{format: "%q", value: gem.COMMACKDenied, want: `"COMMACKDenied"`},
		{format: "%x", value: gem.COMMACKDenied, want: "434f4d4d41434b44656e696564"},
		{format: "%X", value: gem.COMMACKDenied, want: "434F4D4D41434B44656E696564"},
		{format: "%d", value: gem.COMMACKDenied, want: "1"},
		{format: "%#v", value: gem.COMMACKDenied, want: "0x1"},
		{format: "%v", value: 7, want: "COMMACK(7)"},
		{format: "%d", value: 7, want: "7"},
	}

	for _, tt := range tests {
		if got := fmt.Sprintf(tt.format, tt.value); got != tt.want {
			t.Errorf("Sprintf(%q, COMMACK %d) = %q, want %q", tt.format, byte(tt.value), got, tt.want)
		}
	}

	if got := fmt.Sprint(gem.COMMACKAccepted); got != "COMMACKAccepted" {
		t.Errorf("Sprint(COMMACKAccepted) = %q, want %q", got, "COMMACKAccepted")
	}

	if got := fmt.Sprintf("%x", byte(gem.COMMACKDenied)); got != "1" {
		t.Errorf("Sprintf(%%x, byte(COMMACKDenied)) = %q, want %q", got, "1")
	}
}
