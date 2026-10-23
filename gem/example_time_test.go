package gem_test

import (
	"fmt"
	"time"

	"github.com/arloliu/go-secs/v2/gem"
)

// ExampleFormatTime builds the S2F31 that sets the equipment's clock,
// writing the time in each form the TimeFormat equipment constant can select.
func ExampleFormatTime() {
	// In a session this is time.Now(), in the zone the equipment keeps.
	now := time.Date(2026, 10, 23, 14, 15, 30, 459900000, time.UTC)

	for _, format := range []gem.TimeFormat{gem.TimeFormat12, gem.TimeFormat16, gem.TimeFormatExtended} {
		s, err := gem.FormatTime(now, format)
		if err != nil {
			fmt.Println("format:", err)
			return
		}

		msg := gem.S2F31(s)
		fmt.Printf("%s: %s\n", format, msg.Item().ToSML())
	}

	// Output:
	// TimeFormat12: <A[12] "261023141530">
	// TimeFormat16: <A[16] "2026102314153045">
	// TimeFormatExtended: <A[24] "2026-10-23T14:15:30.459Z">
}

// ExampleParseTime reads the equipment's clock out of an S2F18 reply.
func ExampleParseTime() {
	// The reply as it arrives from the equipment; in a session this is reply.Item().
	reply := gem.S2F18("261023141530")

	body, err := gem.DecodeS2F18(reply.Item())
	if err != nil {
		fmt.Println("decode:", err)
		return
	}

	// The reference picks the 12-byte form's century and the zone of the 12- and 16-byte forms;
	// in a session this is time.Now().
	ref := time.Date(2026, 10, 23, 0, 0, 0, 0, time.UTC)

	t, format, err := gem.ParseTime(body.TIME, ref)
	if err != nil {
		fmt.Println("parse:", err)
		return
	}

	fmt.Println(format, t)

	_, _, err = gem.ParseTime("2026-13-01T00:00:00.0Z", ref)
	fmt.Println(err)

	// Output:
	// TimeFormat12 2026-10-23 14:15:30 +0000 UTC
	// gem: invalid time "2026-13-01T00:00:00.0Z": month 13
}
