package gem_test

import (
	"strconv"
	"testing"
	"time"
	_ "time/tzdata" // America/New_York for the daylight-saving cases, whatever the host has installed

	"github.com/stretchr/testify/require"

	"github.com/arloliu/go-secs/v2/gem"
)

// timeRef is the reference time the parse tests use unless a case needs another.
var timeRef = time.Date(2026, 10, 23, 12, 0, 0, 0, time.UTC)

func newYork(t *testing.T) *time.Location {
	t.Helper()

	loc, err := time.LoadLocation("America/New_York")
	require.NoError(t, err)

	return loc
}

func TestTimeFormat_String(t *testing.T) {
	tests := []struct {
		f    gem.TimeFormat
		want string
	}{
		{gem.TimeFormat12, "TimeFormat12"},
		{gem.TimeFormat16, "TimeFormat16"},
		{gem.TimeFormatExtended, "TimeFormatExtended"},
		{3, "TimeFormat(3)"},
		{255, "TimeFormat(255)"},
	}
	for _, tt := range tests {
		t.Run(tt.want, func(t *testing.T) {
			require.Equal(t, tt.want, tt.f.String())
		})
	}
}

func TestFormatTime(t *testing.T) {
	nyc := newYork(t)
	late := time.Date(2026, 12, 31, 23, 59, 59, 999999999, time.UTC)
	e5 := time.Date(1985, 4, 12, 14, 15, 30, 450000000, time.UTC)

	tests := []struct {
		name string
		t    time.Time
		f    gem.TimeFormat
		opts []gem.TimeOption
		want string
	}{
		{"12-byte", e5, gem.TimeFormat12, nil, "850412141530"},
		{"12-byte year 2000", time.Date(2000, 1, 2, 3, 4, 5, 0, time.UTC), gem.TimeFormat12, nil, "000102030405"},
		{"12-byte year 0005", time.Date(5, 1, 1, 0, 0, 0, 0, time.UTC), gem.TimeFormat12, nil, "050101000000"},
		{"12-byte truncates", late, gem.TimeFormat12, nil, "261231235959"},
		{"12-byte local wall clock", e5.In(time.FixedZone("", -4*3600)), gem.TimeFormat12, nil, "850412101530"},
		{"12-byte ignores fraction digits 0", e5, gem.TimeFormat12, []gem.TimeOption{gem.WithFractionDigits(0)}, "850412141530"},
		{"16-byte", e5, gem.TimeFormat16, nil, "1985041214153045"},
		{"16-byte truncates centiseconds", late, gem.TimeFormat16, nil, "2026123123595999"},
		{"16-byte year 0000", time.Date(0, 1, 1, 0, 0, 0, 0, time.UTC), gem.TimeFormat16, nil, "0000010100000000"},
		{"16-byte year 9999", time.Date(9999, 12, 31, 23, 59, 59, 0, time.UTC), gem.TimeFormat16, nil, "9999123123595900"},
		{"16-byte ignores fraction digits 7", e5, gem.TimeFormat16, []gem.TimeOption{gem.WithFractionDigits(7)}, "1985041214153045"},
		{"extended UTC", e5, gem.TimeFormatExtended, nil, "1985-04-12T14:15:30.450Z"},
		{"extended local", e5.In(time.FixedZone("", -4*3600)), gem.TimeFormatExtended, nil, "1985-04-12T10:15:30.450-04:00"},
		{"extended truncates default digits", late, gem.TimeFormatExtended, nil, "2026-12-31T23:59:59.999Z"},
		{"extended 1 digit", late, gem.TimeFormatExtended, []gem.TimeOption{gem.WithFractionDigits(1)}, "2026-12-31T23:59:59.9Z"},
		{"extended 6 digits", late, gem.TimeFormatExtended, []gem.TimeOption{gem.WithFractionDigits(6)}, "2026-12-31T23:59:59.999999Z"},
		{"extended zero fraction", time.Date(2026, 1, 2, 3, 4, 5, 0, time.UTC), gem.TimeFormatExtended, nil, "2026-01-02T03:04:05.000Z"},
		{"extended zero offset outside UTC", e5.In(time.FixedZone("UTC", 0)), gem.TimeFormatExtended, nil, "1985-04-12T14:15:30.450+00:00"},
		{"extended +23:59", e5.In(time.FixedZone("", 23*3600+59*60)), gem.TimeFormatExtended, nil, "1985-04-13T14:14:30.450+23:59"},
		{"extended -23:59", e5.In(time.FixedZone("", -(23*3600 + 59*60))), gem.TimeFormatExtended, nil, "1985-04-11T14:16:30.450-23:59"},
		{"extended New York summer", time.Date(2024, 7, 4, 12, 0, 0, 0, nyc), gem.TimeFormatExtended, nil, "2024-07-04T12:00:00.000-04:00"},
		{"extended New York winter", time.Date(2024, 1, 4, 12, 0, 0, 0, nyc), gem.TimeFormatExtended, nil, "2024-01-04T12:00:00.000-05:00"},
		{"extended 32 bytes", e5.In(time.FixedZone("", -4*3600)), gem.TimeFormatExtended, []gem.TimeOption{gem.WithFractionDigits(6)}, "1985-04-12T10:15:30.450000-04:00"},
		{"nil option skipped", e5, gem.TimeFormatExtended, []gem.TimeOption{nil, gem.WithFractionDigits(2), nil}, "1985-04-12T14:15:30.45Z"},
		{"last option wins", late, gem.TimeFormatExtended, []gem.TimeOption{gem.WithFractionDigits(1), gem.WithFractionDigits(5)}, "2026-12-31T23:59:59.99999Z"},
		{"last option overrides an invalid one", late, gem.TimeFormatExtended, []gem.TimeOption{gem.WithFractionDigits(9), gem.WithFractionDigits(2)}, "2026-12-31T23:59:59.99Z"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got, err := gem.FormatTime(tt.t, tt.f, tt.opts...)
			require.NoError(t, err)
			require.Equal(t, tt.want, got)
			require.LessOrEqual(t, len(got), 32)
		})
	}
}

func TestFormatTime_Errors(t *testing.T) {
	e5 := time.Date(1985, 4, 12, 14, 15, 30, 450000000, time.UTC)

	tests := []struct {
		name string
		t    time.Time
		f    gem.TimeFormat
		opts []gem.TimeOption
		want string
	}{
		{"reserved format", e5, 3, nil, "gem: invalid time: undefined format TimeFormat(3)"},
		{"12-byte year 10000", time.Date(10000, 1, 1, 0, 0, 0, 0, time.UTC), gem.TimeFormat12, nil, "gem: invalid time: year 10000 outside 0000 to 9999"},
		{"12-byte year -1", time.Date(-1, 1, 1, 0, 0, 0, 0, time.UTC), gem.TimeFormat12, nil, "gem: invalid time: year -1 outside 0000 to 9999"},
		{"16-byte year 10000", time.Date(10000, 1, 1, 0, 0, 0, 0, time.UTC), gem.TimeFormat16, nil, "gem: invalid time: year 10000 outside 0000 to 9999"},
		{"16-byte year -1", time.Date(-1, 1, 1, 0, 0, 0, 0, time.UTC), gem.TimeFormat16, nil, "gem: invalid time: year -1 outside 0000 to 9999"},
		{"extended year 10000", time.Date(10000, 1, 1, 0, 0, 0, 0, time.UTC), gem.TimeFormatExtended, nil, "gem: invalid time: year 10000 outside 0000 to 9999"},
		{"extended year -1", time.Date(-1, 1, 1, 0, 0, 0, 0, time.UTC), gem.TimeFormatExtended, nil, "gem: invalid time: year -1 outside 0000 to 9999"},
		{"fraction digits 0", e5, gem.TimeFormatExtended, []gem.TimeOption{gem.WithFractionDigits(0)}, "gem: invalid time: 0 fraction digits, want 1 to 6"},
		{"fraction digits 7", e5, gem.TimeFormatExtended, []gem.TimeOption{gem.WithFractionDigits(7)}, "gem: invalid time: 7 fraction digits, want 1 to 6"},
		{"last option invalid", e5, gem.TimeFormatExtended, []gem.TimeOption{gem.WithFractionDigits(3), gem.WithFractionDigits(-1)}, "gem: invalid time: -1 fraction digits, want 1 to 6"},
		{"offset +24:00", e5.In(time.FixedZone("", 24*3600)), gem.TimeFormatExtended, nil, "gem: invalid time: offset 86400s is 24 hours or more"},
		{"offset -24:00", e5.In(time.FixedZone("", -24*3600)), gem.TimeFormatExtended, nil, "gem: invalid time: offset -86400s is 24 hours or more"},
		{"offset with seconds", e5.In(time.FixedZone("LMT", -(4*3600 + 56*60 + 2))), gem.TimeFormatExtended, nil, "gem: invalid time: offset -17762s is not a whole number of minutes"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got, err := gem.FormatTime(tt.t, tt.f, tt.opts...)
			require.ErrorIs(t, err, gem.ErrInvalidTime)
			require.EqualError(t, err, tt.want)
			require.Empty(t, got)
		})
	}
}

func TestFormatTime_LMTZone(t *testing.T) {
	// Before 1883 New York kept local mean time, an offset with seconds no time zone designator can carry.
	_, err := gem.FormatTime(time.Date(1850, 1, 1, 12, 0, 0, 0, newYork(t)), gem.TimeFormatExtended)
	require.ErrorIs(t, err, gem.ErrInvalidTime)

	// The 12- and 16-byte forms carry no zone, so the same time formats.
	got, err := gem.FormatTime(time.Date(1850, 1, 1, 12, 0, 0, 0, newYork(t)), gem.TimeFormat16)
	require.NoError(t, err)
	require.Equal(t, "1850010112000000", got)
}

func TestParseTime(t *testing.T) {
	nyc := newYork(t)
	minus4 := time.FixedZone("", -4*3600)

	tests := []struct {
		name       string
		s          string
		ref        time.Time
		want       time.Time
		wantFormat gem.TimeFormat
		wantOffset int
	}{
		{"12-byte", "261023141530", timeRef, time.Date(2026, 10, 23, 14, 15, 30, 0, time.UTC), gem.TimeFormat12, 0},
		{"12-byte in ref's zone", "261023141530", timeRef.In(minus4), time.Date(2026, 10, 23, 14, 15, 30, 0, minus4), gem.TimeFormat12, -4 * 3600},
		{"16-byte", "2026102314153045", timeRef, time.Date(2026, 10, 23, 14, 15, 30, 450000000, time.UTC), gem.TimeFormat16, 0},
		{"16-byte zero ref is UTC", "2026102314153045", time.Time{}, time.Date(2026, 10, 23, 14, 15, 30, 450000000, time.UTC), gem.TimeFormat16, 0},
		{"16-byte in New York", "2024070412000099", time.Date(2024, 1, 1, 0, 0, 0, 0, nyc), time.Date(2024, 7, 4, 12, 0, 0, 990000000, nyc), gem.TimeFormat16, -4 * 3600},
		{"16-byte year 0000", "0000010100000000", timeRef, time.Date(0, 1, 1, 0, 0, 0, 0, time.UTC), gem.TimeFormat16, 0},
		{"16-byte year 9999", "9999123123595999", timeRef, time.Date(9999, 12, 31, 23, 59, 59, 990000000, time.UTC), gem.TimeFormat16, 0},
		{"16-byte Feb 29 leap year", "2024022900000000", timeRef, time.Date(2024, 2, 29, 0, 0, 0, 0, time.UTC), gem.TimeFormat16, 0},
		{"12-byte Feb 29 leap year", "240229000000", timeRef, time.Date(2024, 2, 29, 0, 0, 0, 0, time.UTC), gem.TimeFormat12, 0},
		{"extended Z", "1985-04-12T14:15:30.450Z", timeRef, time.Date(1985, 4, 12, 14, 15, 30, 450000000, time.UTC), gem.TimeFormatExtended, 0},
		{"extended offset", "1985-04-12T10:15:30.450-04:00", timeRef, time.Date(1985, 4, 12, 14, 15, 30, 450000000, time.UTC), gem.TimeFormatExtended, -4 * 3600},
		{"extended +00:00", "1985-04-12T14:15:30.450+00:00", timeRef, time.Date(1985, 4, 12, 14, 15, 30, 450000000, time.UTC), gem.TimeFormatExtended, 0},
		{"extended -00:00", "1985-04-12T14:15:30.450-00:00", timeRef, time.Date(1985, 4, 12, 14, 15, 30, 450000000, time.UTC), gem.TimeFormatExtended, 0},
		{"extended +23:59", "1985-04-12T23:59:30.1+23:59", timeRef, time.Date(1985, 4, 12, 0, 0, 30, 100000000, time.UTC), gem.TimeFormatExtended, 23*3600 + 59*60},
		{"extended -23:59", "1985-04-12T00:00:30.1-23:59", timeRef, time.Date(1985, 4, 12, 23, 59, 30, 100000000, time.UTC), gem.TimeFormatExtended, -(23*3600 + 59*60)},
		{"extended 1 digit", "2026-01-02T03:04:05.7Z", timeRef, time.Date(2026, 1, 2, 3, 4, 5, 700000000, time.UTC), gem.TimeFormatExtended, 0},
		{"extended 6 digits", "2026-01-02T03:04:05.123456Z", timeRef, time.Date(2026, 1, 2, 3, 4, 5, 123456000, time.UTC), gem.TimeFormatExtended, 0},
		{"extended 32 bytes", "1985-04-12T10:15:30.123456-04:00", timeRef, time.Date(1985, 4, 12, 14, 15, 30, 123456000, time.UTC), gem.TimeFormatExtended, -4 * 3600},
		{"extended ignores zero ref", "2000-02-29T00:00:00.0Z", time.Time{}, time.Date(2000, 2, 29, 0, 0, 0, 0, time.UTC), gem.TimeFormatExtended, 0},
		{"extended year 0000", "0000-01-01T00:00:00.0Z", timeRef, time.Date(0, 1, 1, 0, 0, 0, 0, time.UTC), gem.TimeFormatExtended, 0},
		{"every field at its top", "9999-12-31T23:59:59.999999Z", timeRef, time.Date(9999, 12, 31, 23, 59, 59, 999999000, time.UTC), gem.TimeFormatExtended, 0},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got, format, err := gem.ParseTime(tt.s, tt.ref)
			require.NoError(t, err)
			require.Equal(t, tt.wantFormat, format)
			require.True(t, tt.want.Equal(got), "got %v, want %v", got, tt.want)

			_, offset := got.Zone()
			require.Equal(t, tt.wantOffset, offset)
		})
	}
}

func TestParseTime_Location(t *testing.T) {
	got, _, err := gem.ParseTime("1985-04-12T14:15:30.450Z", timeRef)
	require.NoError(t, err)
	require.Same(t, time.UTC, got.Location())

	for _, s := range []string{"1985-04-12T14:15:30.450+00:00", "1985-04-12T14:15:30.450-00:00"} {
		got, _, err = gem.ParseTime(s, timeRef)
		require.NoError(t, err)
		require.NotSame(t, time.UTC, got.Location(), s)

		formatted, err := gem.FormatTime(got, gem.TimeFormatExtended)
		require.NoError(t, err)
		require.Equal(t, "1985-04-12T14:15:30.450+00:00", formatted)
	}

	nyc := newYork(t)
	got, _, err = gem.ParseTime("2026102314153045", timeRef.In(nyc))
	require.NoError(t, err)
	require.Same(t, nyc, got.Location())
}

func TestParseTime_Errors(t *testing.T) {
	tests := []struct {
		name   string
		s      string
		ref    time.Time
		detail string
	}{
		{"empty", "", timeRef, "empty"},
		{"14 digits", "20261023141530", timeRef, "length 14 matches neither the 12- nor the 16-byte form"},
		{"11 digits", "26102314153", timeRef, "length 11 matches neither the 12- nor the 16-byte form"},
		{"12-byte zero ref", "261023141530", time.Time{}, "a 12-byte time needs a nonzero reference time"},
		{"12-byte month 00", "260023141530", timeRef, "month 0"},
		{"12-byte month 13", "261323141530", timeRef, "month 13"},
		{"16-byte month 00", "2026002314153045", timeRef, "month 0"},
		{"16-byte month 13", "2026132314153045", timeRef, "month 13"},
		{"16-byte day 00", "2026100014153045", timeRef, "day 0"},
		{"16-byte day 32", "2026103214153045", timeRef, "day 32"},
		{"16-byte hour 24", "2026102324000000", timeRef, "hour 24"},
		{"16-byte minute 60", "2026102314600000", timeRef, "minute 60"},
		{"16-byte second 60", "2026102314156000", timeRef, "second 60"},
		{"16-byte Feb 29 non-leap", "2023022900000000", timeRef, "day 29 does not exist in February 2023"},
		{"16-byte Feb 29 1900", "1900022900000000", timeRef, "day 29 does not exist in February 1900"},
		{"16-byte Feb 30", "2024023000000000", timeRef, "day 30 does not exist in February 2024"},
		{"16-byte Apr 31", "2024043100000000", timeRef, "day 31 does not exist in April 2024"},
		{"12-byte Feb 29 non-leap", "230229000000", timeRef, "day 29 does not exist in February 2023"},
		{"extended month 13", "2026-13-01T00:00:00.0Z", timeRef, "month 13"},
		{"extended day 00", "2026-12-00T00:00:00.0Z", timeRef, "day 0"},
		{"extended hour 24", "2026-12-01T24:00:00.0Z", timeRef, "hour 24"},
		{"extended minute 60", "2026-12-01T00:60:00.0Z", timeRef, "minute 60"},
		{"extended second 60", "2026-12-01T00:00:60.0Z", timeRef, "second 60"},
		{"extended Feb 29 non-leap", "2026-02-29T00:00:00.0Z", timeRef, "day 29 does not exist in February 2026"},
		{"extended space for T", "1985-04-12 14:15:30.450Z", timeRef, "byte 10 is not 'T'"},
		{"extended lowercase t", "1985-04-12t14:15:30.450Z", timeRef, "byte 10 is not 'T'"},
		{"extended slash date", "1985/04/12T14:15:30.450Z", timeRef, "byte 4 is not '-'"},
		{"extended two-digit year", "85-04-12T14:15:30.450Z", timeRef, "byte 2 is not a digit"},
		{"extended padded", " 1985-04-12T14:15:30.450Z", timeRef, "byte 0 is not a digit"},
		{"extended non-ASCII digit", "１985-04-12T14:15:30.450Z", timeRef, "byte 0 is not a digit"},
		{"extended no fraction", "1985-04-12T14:15:30Z", timeRef, "byte 19 is not '.'"},
		{"extended empty fraction", "1985-04-12T14:15:30.Z", timeRef, "0 fraction digits, want 1 to 6"},
		{"extended 7 fraction digits", "1985-04-12T14:15:30.1234567Z", timeRef, "7 fraction digits, want 1 to 6"},
		{"extended comma fraction", "1985-04-12T14:15:30,450Z", timeRef, "byte 19 is not '.'"},
		{"extended no designator", "1985-04-12T14:15:30.450", timeRef, `time zone designator "" is not Z, +hh:mm, or -hh:mm`},
		{"extended lowercase z", "1985-04-12T14:15:30.450z", timeRef, `time zone designator "z" is not Z, +hh:mm, or -hh:mm`},
		{"extended trailing space", "1985-04-12T14:15:30.450Z ", timeRef, `time zone designator "Z " is not Z, +hh:mm, or -hh:mm`},
		{"extended designator without colon", "1985-04-12T10:15:30.450-0400", timeRef, `time zone designator "-0400" is not Z, +hh:mm, or -hh:mm`},
		{"extended designator hour only", "1985-04-12T10:15:30.450-04", timeRef, `time zone designator "-04" is not Z, +hh:mm, or -hh:mm`},
		{"extended designator with seconds", "1985-04-12T10:15:30.45-04:00:00", timeRef, `time zone designator "-04:00:00" is not Z, +hh:mm, or -hh:mm`},
		{"extended designator +24:00", "1985-04-12T10:15:30.450+24:00", timeRef, "time zone designator hour 24"},
		{"extended designator -24:00", "1985-04-12T10:15:30.450-24:00", timeRef, "time zone designator hour 24"},
		{"extended designator minute 60", "1985-04-12T10:15:30.450+00:60", timeRef, "time zone designator minute 60"},
		{"extended 33 bytes", "1985-04-12T10:15:30.1234567-04:00", timeRef, "length 33 exceeds the extended form's 32 bytes"},
		{"extended too short", "1985-04-12T14:15", timeRef, "too short for the extended form"},
		{"12 characters not all digits", "26102314153a", timeRef, "too short for the extended form"},
		{"RFC 3339 without fraction", "2026-10-23T14:15:30+08:00", timeRef, "byte 19 is not '.'"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got, format, err := gem.ParseTime(tt.s, tt.ref)
			require.ErrorIs(t, err, gem.ErrInvalidTime)
			require.EqualError(t, err, "gem: invalid time "+strconv.Quote(tt.s)+": "+tt.detail)
			require.True(t, got.IsZero())
			require.Zero(t, format)
		})
	}
}

func TestParseTime_CenturyWindow(t *testing.T) {
	nyc := newYork(t)

	tests := []struct {
		name     string
		yy       string
		ref      time.Time
		wantYear int
	}{
		{"same year", "26", timeRef, 2026},
		{"window start, a tie resolved to the past", "76", timeRef, 1976},
		{"window end", "75", timeRef, 2075},
		{"year 00", "00", timeRef, 2000},
		{"year 99", "99", timeRef, 1999},
		{"ref year 2050 start", "00", time.Date(2050, 6, 1, 0, 0, 0, 0, time.UTC), 2000},
		{"ref year 2050 end", "99", time.Date(2050, 6, 1, 0, 0, 0, 0, time.UTC), 2099},
		{"ref year 2049 tie", "99", time.Date(2049, 6, 1, 0, 0, 0, 0, time.UTC), 1999},
		{"ref year 0050", "00", time.Date(50, 6, 1, 0, 0, 0, 0, time.UTC), 0},
		{"ref year 9950", "99", time.Date(9950, 6, 1, 0, 0, 0, 0, time.UTC), 9999},
		// 2026-01-01 03:00 UTC is still 2025 in New York, so the window there is 1975 to 2074.
		{"ref's year in ref's zone", "75", time.Date(2026, 1, 1, 3, 0, 0, 0, time.UTC).In(nyc), 1975},
		{"ref's year in UTC", "75", time.Date(2026, 1, 1, 3, 0, 0, 0, time.UTC), 2075},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got, format, err := gem.ParseTime(tt.yy+"0601120000", tt.ref)
			require.NoError(t, err)
			require.Equal(t, gem.TimeFormat12, format)
			require.Equal(t, tt.wantYear, got.Year())
		})
	}
}

func TestParseTime_CenturyWindowOutOfRange(t *testing.T) {
	tests := []struct {
		name   string
		s      string
		ref    time.Time
		detail string
	}{
		{"below year 0000", "900601120000", time.Date(30, 6, 1, 0, 0, 0, 0, time.UTC), "year -10 outside 0000 to 9999"},
		{"above year 9999", "390601120000", time.Date(9990, 6, 1, 0, 0, 0, 0, time.UTC), "year 10039 outside 0000 to 9999"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			_, _, err := gem.ParseTime(tt.s, tt.ref)
			require.ErrorIs(t, err, gem.ErrInvalidTime)
			require.EqualError(t, err, "gem: invalid time "+strconv.Quote(tt.s)+": "+tt.detail)
		})
	}
}

func TestParseTime_DaylightSaving(t *testing.T) {
	nyc := newYork(t)
	ref := time.Date(2024, 6, 1, 0, 0, 0, 0, nyc)

	t.Run("spring-forward gap rejected", func(t *testing.T) {
		for _, s := range []string{"2024031002300000", "240310023000"} {
			_, _, err := gem.ParseTime(s, ref)
			require.ErrorIs(t, err, gem.ErrInvalidTime)
			require.EqualError(t, err, "gem: invalid time "+strconv.Quote(s)+": wall time does not exist in America/New_York")
		}
	})

	t.Run("edges of the gap accepted", func(t *testing.T) {
		for _, s := range []string{"2024031001595999", "2024031003000000"} {
			_, _, err := gem.ParseTime(s, ref)
			require.NoError(t, err)
		}
	})

	t.Run("fall-back fold accepted", func(t *testing.T) {
		for _, s := range []string{"2024110301300000", "241103013000"} {
			got, _, err := gem.ParseTime(s, ref)
			require.NoError(t, err)

			hour, minute, _ := got.Clock()
			require.Equal(t, 1, hour)
			require.Equal(t, 30, minute)

			_, offset := got.Zone()
			require.Contains(t, []int{-4 * 3600, -5 * 3600}, offset)
		}
	})
}

func TestTime_RoundTrip(t *testing.T) {
	nyc := newYork(t)

	tests := []struct {
		name string
		t    time.Time
		f    gem.TimeFormat
		opts []gem.TimeOption
		ref  time.Time
	}{
		{"12-byte", time.Date(2026, 10, 23, 14, 15, 30, 0, time.UTC), gem.TimeFormat12, nil, timeRef},
		{"16-byte", time.Date(2026, 10, 23, 14, 15, 30, 450000000, time.UTC), gem.TimeFormat16, nil, timeRef},
		{"16-byte New York", time.Date(2024, 3, 10, 3, 0, 0, 0, nyc), gem.TimeFormat16, nil, timeRef.In(nyc)},
		{"extended UTC", time.Date(2026, 10, 23, 14, 15, 30, 123000000, time.UTC), gem.TimeFormatExtended, nil, timeRef},
		{"extended offset", time.Date(2026, 10, 23, 14, 15, 30, 123456000, time.FixedZone("", 8*3600)), gem.TimeFormatExtended, []gem.TimeOption{gem.WithFractionDigits(6)}, timeRef},
		{"extended New York", time.Date(2024, 11, 3, 12, 0, 0, 500000000, nyc), gem.TimeFormatExtended, []gem.TimeOption{gem.WithFractionDigits(1)}, timeRef},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			s, err := gem.FormatTime(tt.t, tt.f, tt.opts...)
			require.NoError(t, err)

			got, format, err := gem.ParseTime(s, tt.ref)
			require.NoError(t, err)
			require.Equal(t, tt.f, format)
			require.True(t, tt.t.Equal(got), "%s: got %v, want %v", s, got, tt.t)
		})
	}
}
