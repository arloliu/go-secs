package gem

import (
	"errors"
	"fmt"
	"strconv"
	"time"
)

// The values of the TimeFormat equipment constant (SEMI E5 Table 4).
const (
	// TimeFormat12 selects the 12-byte form YYMMDDhhmmss.
	TimeFormat12 TimeFormat = 0
	// TimeFormat16 selects the 16-byte form YYYYMMDDhhmmsscc, where cc is centiseconds.
	TimeFormat16 TimeFormat = 1
	// TimeFormatExtended selects the extended form YYYY-MM-DDThh:mm:ss.sTZD of SEMI E148, at most 32 bytes:
	// s is one to six fraction digits, and TZD is Z for UTC or +hh:mm / -hh:mm for a local offset.
	TimeFormatExtended TimeFormat = 2
)

const (
	// defaultFractionDigits is the number of fraction digits FormatTime writes in the extended form without [WithFractionDigits].
	defaultFractionDigits = 3
	// maxFractionDigits is the most fraction digits the extended form carries.
	maxFractionDigits = 6
	// maxExtendedLen is the longest extended-form time, with six fraction digits and a ±hh:mm designator.
	maxExtendedLen = 32
	// extendedFractionStart is the index of the first fraction digit in an extended-form time.
	extendedFractionStart = len("YYYY-MM-DDThh:mm:ss.")
	// secondsPerDay bounds the magnitude of a time zone designator's offset.
	secondsPerDay = 24 * 60 * 60
)

// ErrInvalidTime is wrapped by every error [FormatTime] and [ParseTime] return.
var ErrInvalidTime = errors.New("gem: invalid time")

// TimeFormat is the value of the TimeFormat equipment constant (SEMI E5 Table 4),
// which selects the form of the CLOCK variable and of the TIME, TIMESTAMP, and STIME data items.
//
// E5 reserves the values above [TimeFormatExtended].
type TimeFormat uint8

// TimeOption changes how [FormatTime] writes a time.
type TimeOption func(*timeConfig)

// timeConfig holds the settings the options of one FormatTime call select.
type timeConfig struct {
	fractionDigits int
}

// WithFractionDigits returns the option that sets how many fraction digits [FormatTime] writes in the extended form.
//
// n must be 1 to 6; the default is 3.
// The fraction is truncated to n digits, never rounded.
// The option applies only to [TimeFormatExtended]:
// with [TimeFormat12] or [TimeFormat16] it is ignored, whatever n is,
// so one option set serves whichever form the TimeFormat equipment constant selects.
func WithFractionDigits(n int) TimeOption {
	return func(c *timeConfig) {
		c.fractionDigits = n
	}
}

// FormatTime writes t in the form f selects, for a CLOCK value or a TIME, TIMESTAMP, or STIME data item.
//
// The 12- and 16-byte forms write t's wall clock in t's own Location and carry no zone;
// convert t with t.In(loc) first to write it in another zone.
// The 12-byte form writes the year modulo 100.
// The 16-byte form writes centiseconds, truncated.
//
// The extended form writes the fraction of a second truncated to the digits [WithFractionDigits] selects, 3 by default,
// and a time zone designator:
// Z when t's Location is [time.UTC], and t's offset as +hh:mm or -hh:mm otherwise,
// so a zero offset in another Location writes +00:00.
// The offset must be a whole number of minutes and less than 24 hours in magnitude,
// so the result always fits the 32 bytes E5 allows.
//
// Options are applied in order, a nil option is skipped, and a later option overrides an earlier one.
//
// Parameters:
//   - t: the time to write; its year must be 0000 to 9999 for every form
//   - f: the form, normally the value of the TimeFormat equipment constant
//   - opts: [WithFractionDigits], for the extended form
//
// Returns:
//   - string: the formatted time
//   - error: wraps [ErrInvalidTime] when f is not a defined form,
//     t's year is outside 0000 to 9999,
//     the extended form's fraction digits are outside 1 to 6,
//     or t's offset does not fit the extended form's designator
//
// Example:
//
//	s, err := gem.FormatTime(time.Now(), gem.TimeFormat16)
//	if err != nil {
//		return err
//	}
//	msg := gem.S2F31(s)
func FormatTime(t time.Time, f TimeFormat, opts ...TimeOption) (string, error) {
	cfg := timeConfig{fractionDigits: defaultFractionDigits}
	for _, opt := range opts {
		if opt != nil {
			opt(&cfg)
		}
	}

	switch f {
	case TimeFormat12, TimeFormat16:
	case TimeFormatExtended:
		if cfg.fractionDigits < 1 || cfg.fractionDigits > maxFractionDigits {
			return "", fmt.Errorf("%w: %d fraction digits, want 1 to %d", ErrInvalidTime, cfg.fractionDigits, maxFractionDigits)
		}
	default:
		return "", fmt.Errorf("%w: undefined format %s", ErrInvalidTime, f)
	}

	year := t.Year()
	if year < 0 || year > 9999 {
		return "", fmt.Errorf("%w: year %d outside 0000 to 9999", ErrInvalidTime, year)
	}

	buf := make([]byte, 0, maxExtendedLen)
	if f == TimeFormatExtended {
		return formatExtended(buf, t, cfg.fractionDigits)
	}

	if f == TimeFormat12 {
		buf = appendDigits(buf, year%100, 2)
	} else {
		buf = appendDigits(buf, year, 4)
	}

	buf = appendDateClock(buf, t, false)
	if f == TimeFormat16 {
		buf = appendDigits(buf, t.Nanosecond()/10_000_000, 2)
	}

	return string(buf), nil
}

// ParseTime reads a CLOCK value or a TIME, TIMESTAMP, or STIME data item in any of the three E5 forms,
// and returns the time with the form it was written in.
//
// The form is detected by shape:
// exactly 12 ASCII digits is the 12-byte form, exactly 16 is the 16-byte form,
// and anything else must be the extended form.
// Parsing is strict SEMI E5:
// every field must be in its E5 range and the date must exist,
// and the extended form must have the T separator, one to six fraction digits, and a Z or ±hh:mm designator,
// with no padding and at most 32 bytes.
// An empty string, which an STIME may be when E5 lets the equipment derive the time, is an error;
// handle that case before calling ParseTime.
// ParseTime checks shape and ranges only, never whether the time is plausible against the current clock.
//
// The 12- and 16-byte forms carry no zone, so their time is in ref's Location.
// The 12-byte form's year is the one with the given last two digits nearest to ref's year in ref's Location,
// chosen from the window ref's year − 50 to ref's year + 49,
// so a tie 50 years away resolves to the past.
// A 12-byte time with a zero ref is an error, since a zero ref would place the year near 0001.
// A wall time that does not exist in ref's Location, such as one in a daylight-saving spring-forward gap, is an error.
// A wall time that occurs twice, in a fall-back fold, takes whichever offset [time.Date] picks, which Go does not specify.
//
// The extended form carries its own zone and ignores ref:
// Z is [time.UTC], and ±hh:mm is a [time.FixedZone] of that offset, -00:00 included as a zero offset.
//
// Parameters:
//   - s: the time text as it arrived, such as the TIME field of a decoded S2F18
//   - ref: the reference for the 12-byte form's century and the Location of the 12- and 16-byte forms, normally time.Now()
//
// Returns:
//   - time.Time: the parsed time
//   - TimeFormat: the form s is written in
//   - error: wraps [ErrInvalidTime] and quotes s, naming what failed; the time and the form are then zero
//
// Example:
//
//	t, f, err := gem.ParseTime(reply.TIME, time.Now())
func ParseTime(s string, ref time.Time) (time.Time, TimeFormat, error) {
	var (
		t      time.Time
		f      TimeFormat
		detail string
	)

	switch {
	case s == "":
		detail = "empty"
	case len(s) == 12 && allDigits(s):
		f = TimeFormat12
		t, detail = parse12(s, ref)
	case len(s) == 16 && allDigits(s):
		f = TimeFormat16
		t, detail = parse16(s, ref)
	case allDigits(s):
		detail = "length " + strconv.Itoa(len(s)) + " matches neither the 12- nor the 16-byte form"
	default:
		f = TimeFormatExtended
		t, detail = parseExtended(s)
	}

	if detail != "" {
		return time.Time{}, 0, fmt.Errorf("%w %q: %s", ErrInvalidTime, s, detail)
	}

	return t, f, nil
}

// formatExtended appends t to buf in the extended form with digits fraction digits, and returns the result.
func formatExtended(buf []byte, t time.Time, digits int) (string, error) {
	_, offset := t.Zone()
	isUTC := t.Location() == time.UTC
	if !isUTC {
		if offset%60 != 0 {
			return "", fmt.Errorf("%w: offset %ds is not a whole number of minutes", ErrInvalidTime, offset)
		}

		if offset <= -secondsPerDay || offset >= secondsPerDay {
			return "", fmt.Errorf("%w: offset %ds is 24 hours or more", ErrInvalidTime, offset)
		}
	}

	buf = appendDigits(buf, t.Year(), 4)
	buf = appendDateClock(buf, t, true)
	buf = append(buf, '.')

	fraction := t.Nanosecond()
	for range 9 - digits {
		fraction /= 10
	}

	buf = appendDigits(buf, fraction, digits)

	if isUTC {
		return string(append(buf, 'Z')), nil
	}

	sign := byte('+')
	if offset < 0 {
		sign = '-'
		offset = -offset
	}

	buf = append(buf, sign)
	buf = appendDigits(buf, offset/3600, 2)
	buf = append(buf, ':')
	buf = appendDigits(buf, offset/60%60, 2)

	return string(buf), nil
}

// appendDateClock appends t's MM, DD, hh, mm, and ss after a year already in buf,
// with the extended form's -, T, and : separators when extended is true and none otherwise.
func appendDateClock(buf []byte, t time.Time, extended bool) []byte {
	dateSep, clockStart, clockSep := "", "", ""
	if extended {
		dateSep, clockStart, clockSep = "-", "T", ":"
	}

	hour, minute, sec := t.Clock()
	buf = append(buf, dateSep...)
	buf = appendDigits(buf, int(t.Month()), 2)
	buf = append(buf, dateSep...)
	buf = appendDigits(buf, t.Day(), 2)
	buf = append(buf, clockStart...)
	buf = appendDigits(buf, hour, 2)
	buf = append(buf, clockSep...)
	buf = appendDigits(buf, minute, 2)
	buf = append(buf, clockSep...)

	return appendDigits(buf, sec, 2)
}

// appendDigits appends the non-negative v to buf as exactly width decimal digits, zero-padded;
// v must be less than 10^width.
func appendDigits(buf []byte, v, width int) []byte {
	start := len(buf)
	for range width {
		buf = append(buf, '0')
	}

	for i := len(buf) - 1; i >= start; i-- {
		buf[i] = byte('0' + v%10)
		v /= 10
	}

	return buf
}

// parse12 parses the 12-byte form YYMMDDhhmmss, placing the year in the window around ref's year.
func parse12(s string, ref time.Time) (time.Time, string) {
	if ref.IsZero() {
		return time.Time{}, "a 12-byte time needs a nonzero reference time"
	}

	base := ref.Year() - 50
	year := base + ((digitsAt(s, 0, 2)-base)%100+100)%100
	if year < 0 || year > 9999 {
		return time.Time{}, "year " + strconv.Itoa(year) + " outside 0000 to 9999"
	}

	return buildTime(year, s[2:], 0, ref.Location())
}

// parse16 parses the 16-byte form YYYYMMDDhhmmsscc in ref's Location.
func parse16(s string, ref time.Time) (time.Time, string) {
	return buildTime(digitsAt(s, 0, 4), s[4:14], digitsAt(s, 14, 2)*10_000_000, ref.Location())
}

// parseExtended parses the extended form YYYY-MM-DDThh:mm:ss.sTZD.
func parseExtended(s string) (time.Time, string) {
	if len(s) > maxExtendedLen {
		return time.Time{}, "length " + strconv.Itoa(len(s)) + " exceeds the extended form's 32 bytes"
	}

	const layout = "dddd-dd-ddTdd:dd:dd."
	if len(s) < len(layout) {
		return time.Time{}, "too short for the extended form"
	}

	for i := range len(layout) {
		if layout[i] == 'd' {
			if !isDigit(s[i]) {
				return time.Time{}, "byte " + strconv.Itoa(i) + " is not a digit"
			}
		} else if s[i] != layout[i] {
			return time.Time{}, "byte " + strconv.Itoa(i) + " is not " + strconv.QuoteRune(rune(layout[i]))
		}
	}

	end := extendedFractionStart
	for end < len(s) && isDigit(s[end]) {
		end++
	}

	digits := end - extendedFractionStart
	if digits < 1 || digits > maxFractionDigits {
		return time.Time{}, strconv.Itoa(digits) + " fraction digits, want 1 to 6"
	}

	nsec := digitsAt(s, extendedFractionStart, digits)
	for range 9 - digits {
		nsec *= 10
	}

	loc, detail := parseTZD(s[end:])
	if detail != "" {
		return time.Time{}, detail
	}

	clock := s[5:7] + s[8:10] + s[11:13] + s[14:16] + s[17:19]

	return buildTime(digitsAt(s, 0, 4), clock, nsec, loc)
}

// parseTZD parses an extended-form time zone designator: Z, +hh:mm, or -hh:mm.
func parseTZD(tzd string) (*time.Location, string) {
	if tzd == "Z" {
		return time.UTC, ""
	}

	if len(tzd) != len("+hh:mm") || (tzd[0] != '+' && tzd[0] != '-') ||
		!isDigit(tzd[1]) || !isDigit(tzd[2]) || tzd[3] != ':' || !isDigit(tzd[4]) || !isDigit(tzd[5]) {
		return nil, "time zone designator " + strconv.Quote(tzd) + " is not Z, +hh:mm, or -hh:mm"
	}

	hours, minutes := digitsAt(tzd, 1, 2), digitsAt(tzd, 4, 2)
	if hours > 23 {
		return nil, "time zone designator hour " + strconv.Itoa(hours)
	}

	if minutes > 59 {
		return nil, "time zone designator minute " + strconv.Itoa(minutes)
	}

	offset := hours*3600 + minutes*60
	if tzd[0] == '-' {
		offset = -offset
	}

	return time.FixedZone("", offset), ""
}

// buildTime checks the ten digits MMDDhhmmss of clock against their E5 ranges,
// and returns the time they give with year and nsec in loc,
// or a detail naming the field that is out of range or the date or wall time that does not exist.
func buildTime(year int, clock string, nsec int, loc *time.Location) (time.Time, string) {
	month, day := digitsAt(clock, 0, 2), digitsAt(clock, 2, 2)
	hour, minute, sec := digitsAt(clock, 4, 2), digitsAt(clock, 6, 2), digitsAt(clock, 8, 2)

	switch {
	case month < 1 || month > 12:
		return time.Time{}, "month " + strconv.Itoa(month)
	case day < 1 || day > 31:
		return time.Time{}, "day " + strconv.Itoa(day)
	case hour > 23:
		return time.Time{}, "hour " + strconv.Itoa(hour)
	case minute > 59:
		return time.Time{}, "minute " + strconv.Itoa(minute)
	case sec > 59:
		return time.Time{}, "second " + strconv.Itoa(sec)
	}

	if day > daysIn(month, year) {
		return time.Time{}, "day " + strconv.Itoa(day) + " does not exist in " + time.Month(month).String() + " " + strconv.Itoa(year)
	}

	// time.Date normalizes a wall time that does not exist in loc, such as one in a spring-forward gap,
	// so any field that differs from the input marks such a time.
	t := time.Date(year, time.Month(month), day, hour, minute, sec, nsec, loc)
	gotHour, gotMinute, gotSec := t.Clock()
	if t.Year() != year || int(t.Month()) != month || t.Day() != day || gotHour != hour || gotMinute != minute || gotSec != sec {
		return time.Time{}, "wall time does not exist in " + loc.String()
	}

	return t, ""
}

// daysIn returns the number of days in month of year in the proleptic Gregorian calendar, as package time counts them.
func daysIn(month, year int) int {
	switch month {
	case 2:
		if year%4 == 0 && (year%100 != 0 || year%400 == 0) {
			return 29
		}

		return 28
	case 4, 6, 9, 11:
		return 30
	default:
		return 31
	}
}

// digitsAt returns the value of the n decimal digits of s starting at i, which the caller has checked are digits.
func digitsAt(s string, i, n int) int {
	v := 0
	for _, c := range []byte(s[i : i+n]) {
		v = v*10 + int(c-'0')
	}

	return v
}

// allDigits reports whether s is all ASCII digits.
func allDigits(s string) bool {
	for i := range len(s) {
		if !isDigit(s[i]) {
			return false
		}
	}

	return true
}

// isDigit reports whether c is an ASCII digit.
func isDigit(c byte) bool {
	return '0' <= c && c <= '9'
}

// String returns the name of f's constant, such as "TimeFormat16",
// or "TimeFormat(n)" for a reserved value.
func (f TimeFormat) String() string {
	switch f {
	case TimeFormat12:
		return "TimeFormat12"
	case TimeFormat16:
		return "TimeFormat16"
	case TimeFormatExtended:
		return "TimeFormatExtended"
	default:
		return "TimeFormat(" + strconv.Itoa(int(f)) + ")"
	}
}
