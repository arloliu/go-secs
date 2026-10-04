package tracepack

import (
	"errors"
	"fmt"
	"math"
	"strconv"
	"strings"
	"time"
)

const (
	// keyPackSuffix ends the key of every pack (the tracepack storage specification §3).
	keyPackSuffix = ".tpk"
	// seqFirstDigits is the width of a segment key's zero-padded seq_first.
	seqFirstDigits = 20
	// uuidKeyLen is the length of a UUID in a key, its canonical 8-4-4-4-12 form.
	uuidKeyLen = 36
	// segmentFileLen is the length of a segment key's last component: <seq_first>-<pack_id>.tpk.
	segmentFileLen = seqFirstDigits + 1 + uuidKeyLen + len(keyPackSuffix)
	// archiveFileLen is the length of an archive key's last component: <capture_id>-<pack_id>.tpk.
	archiveFileLen = uuidKeyLen + 1 + uuidKeyLen + len(keyPackSuffix)
	// commitHourLen is the length of a commit key's hour component, YYYYMMDDHH.
	commitHourLen = 10
	// secondsPerHour converts an hour number to Unix seconds, which never overflows for a key hour.
	secondsPerHour = 3600

	// minKeyHour is the first hour a key can name, hourOf(math.MinInt64), -2562048:
	// the hour holding the smallest nanosecond timestamp, which starts before it.
	minKeyHour int64 = math.MinInt64/hourNs - 1
	// maxKeyHour is the last hour a key can name, hourOf(math.MaxInt64), 2562047:
	// the hour holding the largest nanosecond timestamp, which ends after it.
	maxKeyHour int64 = math.MaxInt64 / hourNs

	// upperHex holds the hex digits a key's escapes use.
	upperHex = "0123456789ABCDEF"
)

// ErrInvalidKey reports a key, or a key component, outside the forms of the tracepack storage specification §3:
// the key builders return an error wrapping it for arguments no key can hold,
// NewStoreSource for a StoreSourceOptions.Tool no key can hold,
// and the Observe of the source NewStoreSource returns fails with an error wrapping it
// for a key a listing returns or the catalog names that does not parse.
// Unlike ErrObjectNotFound, it reports malformed bucket or catalog contents, which a retry does not cure.
var ErrInvalidKey = errors.New("tracepack: invalid object key")

// segmentKeyParts is what a segment key names.
type segmentKeyParts struct {
	tool     string // decoded tool_id
	capture  UUID
	seqFirst uint64 // at most 2^63-1
	pack     UUID
}

// archiveKeyParts is what an archive key names.
type archiveKeyParts struct {
	tool    string // decoded tool_id
	hour    int64  // in [minKeyHour, maxKeyHour]
	capture UUID
	pack    UUID
}

// commitKeyParts is what a commit object's key names.
type commitKeyParts struct {
	tool    string // decoded tool_id
	capture UUID
	hour    int64 // in [minKeyHour, maxKeyHour]
	id      UUID
}

// EscapeToolID returns tool as a key spells it (the tracepack storage specification §3):
// every byte outside the RFC 3986 unreserved set (A-Z, a-z, 0-9, '-', '.', '_', '~') is written %XX with uppercase hex digits,
// and every other byte is kept.
// The result never holds '/', and each tool has exactly one spelling.
// EscapeToolID accepts every string;
// the key builders reject the tools a key cannot hold, "", "." and "..".
func EscapeToolID(tool string) string {
	var b strings.Builder

	for i := range len(tool) {
		c := tool[i]
		if isUnreserved(c) {
			b.WriteByte(c)

			continue
		}
		b.WriteByte('%')
		b.WriteByte(upperHex[c>>4])
		b.WriteByte(upperHex[c&0x0F])
	}

	return b.String()
}

// SegmentKey returns the key of a staging segment (the tracepack storage specification §3):
// <prefix>/staging/<tool>/<capture>/<seq_first>-<pack>.tpk, seq_first in 20-digit zero-padded decimal.
//
// The prefix is used byte for byte, followed by '/' when it is not empty;
// a configured prefix is empty or does not end in '/', which NewStoreSource checks and SegmentKey does not.
//
// Parameters:
//   - prefix: the bucket prefix, possibly empty.
//   - tool: the tool_id as recorded, which the key spells with EscapeToolID; not "", "." or "..".
//   - capture: the segment's capture_id.
//   - seqFirst: the segment's first seq, its seq_start when it is empty; at most 2^63-1.
//   - pack: the segment's pack_id.
//
// Returns:
//   - string: the key; empty on error.
//   - error: non-nil, wrapping ErrInvalidKey, for a tool a key cannot hold or a seqFirst above 2^63-1.
func SegmentKey(prefix, tool string, capture UUID, seqFirst uint64, pack UUID) (string, error) {
	dir, err := stagingKeyDir(prefix, tool, capture)
	if err != nil {
		return "", err
	}
	if seqFirst > math.MaxInt64 {
		return "", fmt.Errorf("%w: seq_first %d above 2^63-1", ErrInvalidKey, seqFirst)
	}

	return fmt.Sprintf("%s%0*d-%s%s", dir, seqFirstDigits, seqFirst, pack, keyPackSuffix), nil
}

// ArchiveKey returns the key of a pack that is not a staging segment (the tracepack storage specification §3):
// <prefix>/archive/<tool>/<YYYY>/<MM>/<DD>/<HH>/<capture>-<pack>.tpk, the hour in UTC.
//
// The prefix is used byte for byte, followed by '/' when it is not empty;
// a configured prefix is empty or does not end in '/', which NewStoreSource checks and ArchiveKey does not.
//
// Parameters:
//   - prefix: the bucket prefix, possibly empty.
//   - tool: the tool_id as recorded, which the key spells with EscapeToolID; not "", "." or "..".
//   - capture: the pack's capture_id.
//   - hour: the scope's UTC hour number, counted from the Unix epoch;
//     an hour holding an int64 nanosecond timestamp, -2562048 to 2562047 (the years 1677 to 2262).
//   - pack: the pack's pack_id.
//
// Returns:
//   - string: the key; empty on error.
//   - error: non-nil, wrapping ErrInvalidKey, for a tool a key cannot hold or an hour outside that range.
func ArchiveKey(prefix, tool string, capture UUID, hour int64, pack UUID) (string, error) {
	dir, err := archiveKeyDir(prefix, tool, hour)
	if err != nil {
		return "", err
	}

	return dir + capture.String() + "-" + pack.String() + keyPackSuffix, nil
}

// CommitKey returns the key of a commit object (the tracepack storage specification §3):
// <prefix>/commit/<tool>/<capture>/<YYYYMMDDHH>/<id>, the hour in UTC.
//
// The prefix is used byte for byte, followed by '/' when it is not empty;
// a configured prefix is empty or does not end in '/', which NewStoreSource checks and CommitKey does not.
//
// Parameters:
//   - prefix: the bucket prefix, possibly empty.
//   - tool: the tool_id as recorded, which the key spells with EscapeToolID; not "", "." or "..".
//   - capture: the scope's capture_id.
//   - hour: the scope's UTC hour number, counted from the Unix epoch;
//     an hour holding an int64 nanosecond timestamp, -2562048 to 2562047 (the years 1677 to 2262).
//   - id: the committed generation's replacement_set_id or the committed patch's pack_id.
//
// Returns:
//   - string: the key; empty on error.
//   - error: non-nil, wrapping ErrInvalidKey, for a tool a key cannot hold or an hour outside that range.
func CommitKey(prefix, tool string, capture UUID, hour int64, id UUID) (string, error) {
	dir, err := commitKeyDir(prefix, tool, capture, hour)
	if err != nil {
		return "", err
	}

	return dir + id.String(), nil
}

// keyArea returns the start of every key of one area (staging, archive or commit) under prefix:
// prefix, a '/' when prefix is not empty, then area and a '/'.
func keyArea(prefix, area string) string {
	if prefix == "" {
		return area + "/"
	}

	return prefix + "/" + area + "/"
}

// stagingKeyDir returns the listing prefix of the capture's segments, <prefix>/staging/<tool>/<capture>/,
// or an error for a tool a key cannot hold.
func stagingKeyDir(prefix, tool string, capture UUID) (string, error) {
	if err := checkKeyTool(tool); err != nil {
		return "", keyArgError(err)
	}

	return keyArea(prefix, "staging") + EscapeToolID(tool) + "/" + capture.String() + "/", nil
}

// archiveKeyDir returns the listing prefix of the tool's archive packs of hour, <prefix>/archive/<tool>/<YYYY>/<MM>/<DD>/<HH>/,
// or an error for a tool a key cannot hold or an hour outside [minKeyHour, maxKeyHour].
func archiveKeyDir(prefix, tool string, hour int64) (string, error) {
	if err := checkKeyTool(tool); err != nil {
		return "", keyArgError(err)
	}
	if err := checkKeyHour(hour); err != nil {
		return "", keyArgError(err)
	}
	t := keyHourTime(hour)

	return fmt.Sprintf("%s%s/%04d/%02d/%02d/%02d/", keyArea(prefix, "archive"), EscapeToolID(tool), t.Year(), t.Month(), t.Day(), t.Hour()), nil
}

// commitKeyDir returns the listing prefix of the scope's commit objects, <prefix>/commit/<tool>/<capture>/<YYYYMMDDHH>/,
// or an error for a tool a key cannot hold or an hour outside [minKeyHour, maxKeyHour].
func commitKeyDir(prefix, tool string, capture UUID, hour int64) (string, error) {
	if err := checkKeyTool(tool); err != nil {
		return "", keyArgError(err)
	}
	if err := checkKeyHour(hour); err != nil {
		return "", keyArgError(err)
	}
	t := keyHourTime(hour)

	return fmt.Sprintf("%s%s/%s/%04d%02d%02d%02d/", keyArea(prefix, "commit"), EscapeToolID(tool), capture, t.Year(), t.Month(), t.Day(), t.Hour()), nil
}

// checkKeyTool reports an error for a tool no key can hold: "", and "." and "..",
// which would be path components of their own since '.' is not escaped.
// The error states only the reason; its caller wraps it with ErrInvalidKey.
func checkKeyTool(tool string) error {
	if tool == "" || tool == "." || tool == ".." {
		return fmt.Errorf("tool %q cannot be stored", tool)
	}

	return nil
}

// checkKeyHour reports an error for an hour outside [minKeyHour, maxKeyHour].
// The error states only the reason; its caller wraps it with ErrInvalidKey.
func checkKeyHour(hour int64) error {
	if hour < minKeyHour || hour > maxKeyHour {
		return fmt.Errorf("hour %d outside [%d, %d]", hour, minKeyHour, maxKeyHour)
	}

	return nil
}

// keyHourTime returns the start of hour in UTC, for an hour in [minKeyHour, maxKeyHour].
// It converts through seconds, since the first such hour does not start within the int64 nanosecond range.
func keyHourTime(hour int64) time.Time {
	return time.Unix(hour*secondsPerHour, 0).UTC()
}

// isUnreserved reports whether c is in the RFC 3986 unreserved set, which a key keeps unescaped.
func isUnreserved(c byte) bool {
	return 'A' <= c && c <= 'Z' || 'a' <= c && c <= 'z' || '0' <= c && c <= '9' || c == '-' || c == '.' || c == '_' || c == '~'
}

// parseSegmentKey parses key as the key of a staging segment under prefix, as SegmentKey builds it.
// It accepts only the one spelling SegmentKey produces, so SegmentKey rebuilds every key it accepts byte for byte;
// it requires no particular tool, capture or seq_first.
func parseSegmentKey(prefix, key string) (segmentKeyParts, error) {
	comps, err := splitKey(prefix, "staging", key, 3)
	if err != nil {
		return segmentKeyParts{}, err
	}
	var p segmentKeyParts
	if p.tool, err = unescapeToolID(comps[0]); err != nil {
		return segmentKeyParts{}, keyError(key, err)
	}
	if p.capture, err = parseKeyUUID(comps[1]); err != nil {
		return segmentKeyParts{}, keyError(key, err)
	}
	file := comps[2]
	if len(file) != segmentFileLen || file[seqFirstDigits] != '-' || !strings.HasSuffix(file, keyPackSuffix) {
		return segmentKeyParts{}, keyError(key, errors.New("not <seq_first>-<pack_id>.tpk"))
	}
	if p.seqFirst, err = parseKeyDigits(file[:seqFirstDigits]); err != nil {
		return segmentKeyParts{}, keyError(key, err)
	}
	if p.seqFirst > math.MaxInt64 {
		return segmentKeyParts{}, keyError(key, errors.New("seq_first above 2^63-1"))
	}
	if p.pack, err = parseKeyUUID(file[seqFirstDigits+1 : seqFirstDigits+1+uuidKeyLen]); err != nil {
		return segmentKeyParts{}, keyError(key, err)
	}

	return p, nil
}

// parseArchiveKey parses key as the key of an archive pack under prefix, as ArchiveKey builds it.
// It accepts only the one spelling ArchiveKey produces, so ArchiveKey rebuilds every key it accepts byte for byte;
// it requires no particular tool, hour or capture.
func parseArchiveKey(prefix, key string) (archiveKeyParts, error) {
	comps, err := splitKey(prefix, "archive", key, 6)
	if err != nil {
		return archiveKeyParts{}, err
	}
	var p archiveKeyParts
	if p.tool, err = unescapeToolID(comps[0]); err != nil {
		return archiveKeyParts{}, keyError(key, err)
	}
	if p.hour, err = parseKeyHour(comps[1], comps[2], comps[3], comps[4]); err != nil {
		return archiveKeyParts{}, keyError(key, err)
	}
	file := comps[5]
	if len(file) != archiveFileLen || file[uuidKeyLen] != '-' || !strings.HasSuffix(file, keyPackSuffix) {
		return archiveKeyParts{}, keyError(key, errors.New("not <capture_id>-<pack_id>.tpk"))
	}
	if p.capture, err = parseKeyUUID(file[:uuidKeyLen]); err != nil {
		return archiveKeyParts{}, keyError(key, err)
	}
	if p.pack, err = parseKeyUUID(file[uuidKeyLen+1 : 2*uuidKeyLen+1]); err != nil {
		return archiveKeyParts{}, keyError(key, err)
	}

	return p, nil
}

// parseCommitKey parses key as the key of a commit object under prefix, as CommitKey builds it.
// It accepts only the one spelling CommitKey produces, so CommitKey rebuilds every key it accepts byte for byte;
// it requires no particular tool, capture or hour.
func parseCommitKey(prefix, key string) (commitKeyParts, error) {
	comps, err := splitKey(prefix, "commit", key, 4)
	if err != nil {
		return commitKeyParts{}, err
	}
	var p commitKeyParts
	if p.tool, err = unescapeToolID(comps[0]); err != nil {
		return commitKeyParts{}, keyError(key, err)
	}
	if p.capture, err = parseKeyUUID(comps[1]); err != nil {
		return commitKeyParts{}, keyError(key, err)
	}
	h := comps[2]
	if len(h) != commitHourLen {
		return commitKeyParts{}, keyError(key, errors.New("hour not YYYYMMDDHH"))
	}
	if p.hour, err = parseKeyHour(h[0:4], h[4:6], h[6:8], h[8:10]); err != nil {
		return commitKeyParts{}, keyError(key, err)
	}
	if p.id, err = parseKeyUUID(comps[3]); err != nil {
		return commitKeyParts{}, keyError(key, err)
	}

	return p, nil
}

// splitKey checks that key starts with keyArea(prefix, area) and returns the n '/'-separated components after it.
func splitKey(prefix, area, key string, n int) ([]string, error) {
	rest, ok := strings.CutPrefix(key, keyArea(prefix, area))
	if !ok {
		return nil, keyError(key, fmt.Errorf("not under %q", keyArea(prefix, area)))
	}
	comps := strings.Split(rest, "/")
	if len(comps) != n {
		return nil, keyError(key, fmt.Errorf("%d components after %q, want %d", len(comps), keyArea(prefix, area), n))
	}

	return comps, nil
}

// keyArgError wraps err, the reason a builder cannot build a key from its arguments, with ErrInvalidKey.
func keyArgError(err error) error {
	return fmt.Errorf("%w: %w", ErrInvalidKey, err)
}

// keyError wraps err, the reason key does not parse, with ErrInvalidKey and the key.
func keyError(key string, err error) error {
	return fmt.Errorf("%w %q: %w", ErrInvalidKey, key, err)
}

// unescapeToolID decodes a key's tool component, which EscapeToolID must have produced:
// every byte unreserved or a %XX escape with uppercase hex digits of a byte that is not, and the tool not "", "." or "..".
func unescapeToolID(s string) (string, error) {
	var b strings.Builder

	for i := 0; i < len(s); i++ {
		c := s[i]
		if isUnreserved(c) {
			b.WriteByte(c)

			continue
		}
		if c != '%' {
			return "", fmt.Errorf("tool %q: byte %#02x at %d is neither unreserved nor an escape", s, c, i)
		}
		if i+2 >= len(s) {
			return "", fmt.Errorf("tool %q: escape at %d is cut short", s, i)
		}
		hi, lo := strings.IndexByte(upperHex, s[i+1]), strings.IndexByte(upperHex, s[i+2])
		if hi < 0 || lo < 0 {
			return "", fmt.Errorf("tool %q: escape at %d is not two uppercase hex digits", s, i)
		}
		v := byte(hi<<4 | lo) // two hex digits, below 256
		if isUnreserved(v) {
			return "", fmt.Errorf("tool %q: escape at %d encodes the unreserved byte %q", s, i, v)
		}
		b.WriteByte(v)
		i += 2
	}
	tool := b.String()
	if err := checkKeyTool(tool); err != nil {
		return "", err
	}

	return tool, nil
}

// parseKeyUUID parses a UUID of a key, which only the canonical lowercase form spells.
func parseKeyUUID(s string) (UUID, error) {
	u, err := ParseUUID(s)
	if err != nil {
		return UUID{}, err
	}
	if u.String() != s {
		return UUID{}, fmt.Errorf("UUID %q not in lowercase", s)
	}

	return u, nil
}

// parseKeyDigits parses s, decimal digits only, as a uint64.
// strconv.ParseUint in base 10 takes no sign and no underscore, so it accepts exactly the decimal digit strings that fit.
func parseKeyDigits(s string) (uint64, error) {
	return strconv.ParseUint(s, 10, 64)
}

// parseKeyHour parses a key's UTC hour from its four-digit year, two-digit month, day and hour,
// which must name a real calendar hour in [minKeyHour, maxKeyHour].
func parseKeyHour(year, month, day, hour string) (int64, error) {
	if len(year) != 4 || len(month) != 2 || len(day) != 2 || len(hour) != 2 {
		return 0, fmt.Errorf("hour %s/%s/%s/%s not YYYY MM DD HH", year, month, day, hour)
	}
	var f [4]int

	for i, s := range [4]string{year, month, day, hour} {
		// At most four digits, so 16 bits hold every value the lengths above allow.
		v, err := strconv.ParseUint(s, 10, 16)
		if err != nil {
			return 0, err
		}
		f[i] = int(v)
	}
	t := time.Date(f[0], time.Month(f[1]), f[2], f[3], 0, 0, 0, time.UTC)
	if t.Year() != f[0] || int(t.Month()) != f[1] || t.Day() != f[2] || t.Hour() != f[3] {
		return 0, fmt.Errorf("hour %s%s%s%s not a calendar hour", year, month, day, hour)
	}
	h := t.Unix() / secondsPerHour
	if err := checkKeyHour(h); err != nil {
		return 0, err
	}

	return h, nil
}
