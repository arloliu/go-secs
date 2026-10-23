package gem_test

import (
	"strings"
	"testing"
	"time"
	_ "time/tzdata" // America/New_York for the reference zone, whatever the host has installed

	"github.com/arloliu/go-secs/v2/gem"
)

// FuzzParseTime checks that ParseTime never panics,
// and that every time it accepts formats back in the detected form and parses to an equal time.
//
// The reference is in America/New_York, so the 12- and 16-byte forms cross daylight-saving gaps and folds.
// The check compares instants with Equal, not spellings:
// -00:00 formats back as +00:00, and the Location of a parsed offset is a fresh fixed zone.
func FuzzParseTime(f *testing.F) {
	nyc, err := time.LoadLocation("America/New_York")
	if err != nil {
		f.Fatal(err)
	}

	ref := time.Date(2026, 10, 23, 12, 0, 0, 0, nyc)

	for _, seed := range []string{
		"261023141530",
		"760101000000",
		"750101000000",
		"2026102314153045",
		"0000010100000000",
		"9999123123595999",
		"2024022900000000",
		"2023022900000000",
		"2024031002300000",
		"2024110301300000",
		"1985-04-12T14:15:30.450Z",
		"1985-04-12T10:15:30.450-04:00",
		"1985-04-12T10:15:30.123456-04:00",
		"1985-04-12T14:15:30.4-00:00",
		"1985-04-12T23:59:30.1+23:59",
		"1985-04-12T23:59:30.1+24:00",
		"2000-02-29T00:00:00.0Z",
		"2026-13-01T00:00:00.0Z",
		"20261023141530",
		"",
	} {
		f.Add(seed)
	}

	f.Fuzz(func(t *testing.T, s string) {
		parsed, format, err := gem.ParseTime(s, ref)
		if err != nil {
			return
		}

		var opts []gem.TimeOption
		if format == gem.TimeFormatExtended {
			opts = append(opts, gem.WithFractionDigits(fractionDigits(s)))
		}

		formatted, err := gem.FormatTime(parsed, format, opts...)
		if err != nil {
			t.Fatalf("ParseTime(%q) = %v, but FormatTime fails: %v", s, parsed, err)
		}

		again, againFormat, err := gem.ParseTime(formatted, ref)
		if err != nil {
			t.Fatalf("ParseTime(%q) = %v formats as %q, which does not parse: %v", s, parsed, formatted, err)
		}

		if againFormat != format || !again.Equal(parsed) {
			t.Fatalf("ParseTime(%q) = %v, %s; its form %q parses as %v, %s", s, parsed, format, formatted, again, againFormat)
		}
	})
}

// fractionDigits returns how many fraction digits the extended-form time s carries.
func fractionDigits(s string) int {
	start := strings.IndexByte(s, '.') + 1
	n := 0
	for start+n < len(s) && s[start+n] >= '0' && s[start+n] <= '9' {
		n++
	}

	return n
}
