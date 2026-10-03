package tracepack

import (
	"math"
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
)

// Fixed UUIDs of the key tests, in canonical lowercase form.
const (
	keyCaptureText = "0190a1b2-c3d4-7e5f-8a6b-1c2d3e4f5a6b"
	keyPackText    = "0190a1b2-c3d4-7e5f-9a6b-aabbccddeeff"
	keyOtherText   = "abcdef01-2345-4678-89ab-cdef01234567"

	// keyPackMixed and keyOtherMixed spell keyPackText and keyOtherText with some hex digits in uppercase.
	keyPackMixed  = "0190a1b2-c3d4-7e5f-9a6b-AAbbccddeeFF"
	keyOtherMixed = "Abcdef01-2345-4678-89AB-cdef01234567"
)

// keyUUID parses a canonical UUID of the key tests.
func keyUUID(t testing.TB, s string) UUID {
	t.Helper()
	u, err := ParseUUID(s)
	require.NoError(t, err)

	return u
}

// keyHourOf returns the hour number of the UTC calendar hour y-m-d h.
func keyHourOf(y int, m time.Month, d, h int) int64 {
	return time.Date(y, m, d, h, 0, 0, 0, time.UTC).Unix() / secondsPerHour
}

func TestEscapeToolID(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name, tool, want string
	}{
		{"empty", "", ""},
		{"unreserved kept", "AZaz09-._~", "AZaz09-._~"},
		{"slash", "a/b", "a%2Fb"},
		{"percent", "100%", "100%25"},
		{"space", "tool 1", "tool%201"},
		{"non-ASCII byte by byte", "é", "%C3%A9"},
		{"invalid UTF-8 byte by byte", "a\xffb\x80", "a%FFb%80"},
		{"control and NUL", "\x00\n", "%00%0A"},
		{"other delimiters", ":@!$&'()*+,;=?#[]", "%3A%40%21%24%26%27%28%29%2A%2B%2C%3B%3D%3F%23%5B%5D"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			require.Equal(t, tt.want, EscapeToolID(tt.tool))
		})
	}
}

func TestKeyBuilders(t *testing.T) {
	t.Parallel()

	capture, pack, other := keyUUID(t, keyCaptureText), keyUUID(t, keyPackText), keyUUID(t, keyOtherText)
	hour := keyHourOf(2026, time.October, 4, 13)

	tests := []struct {
		name, prefix, tool string
		segment, archive   string
		commit             string
	}{
		{
			name: "empty prefix", tool: "eqp-01",
			segment: "staging/eqp-01/" + keyCaptureText + "/00000000000000000042-" + keyPackText + ".tpk",
			archive: "archive/eqp-01/2026/10/04/13/" + keyCaptureText + "-" + keyPackText + ".tpk",
			commit:  "commit/eqp-01/" + keyCaptureText + "/2026100413/" + keyOtherText,
		},
		{
			name: "one component", prefix: "a", tool: "eqp-01",
			segment: "a/staging/eqp-01/" + keyCaptureText + "/00000000000000000042-" + keyPackText + ".tpk",
			archive: "a/archive/eqp-01/2026/10/04/13/" + keyCaptureText + "-" + keyPackText + ".tpk",
			commit:  "a/commit/eqp-01/" + keyCaptureText + "/2026100413/" + keyOtherText,
		},
		{
			name: "two components", prefix: "a/b", tool: "eqp-01",
			segment: "a/b/staging/eqp-01/" + keyCaptureText + "/00000000000000000042-" + keyPackText + ".tpk",
			archive: "a/b/archive/eqp-01/2026/10/04/13/" + keyCaptureText + "-" + keyPackText + ".tpk",
			commit:  "a/b/commit/eqp-01/" + keyCaptureText + "/2026100413/" + keyOtherText,
		},
		{
			name: "trailing slash used as given", prefix: "a/", tool: "eqp-01",
			segment: "a//staging/eqp-01/" + keyCaptureText + "/00000000000000000042-" + keyPackText + ".tpk",
			archive: "a//archive/eqp-01/2026/10/04/13/" + keyCaptureText + "-" + keyPackText + ".tpk",
			commit:  "a//commit/eqp-01/" + keyCaptureText + "/2026100413/" + keyOtherText,
		},
		{
			name: "leading slash used as given", prefix: "/a", tool: "eqp-01",
			segment: "/a/staging/eqp-01/" + keyCaptureText + "/00000000000000000042-" + keyPackText + ".tpk",
			archive: "/a/archive/eqp-01/2026/10/04/13/" + keyCaptureText + "-" + keyPackText + ".tpk",
			commit:  "/a/commit/eqp-01/" + keyCaptureText + "/2026100413/" + keyOtherText,
		},
		{
			name: "repeated slash used as given", prefix: "a//b", tool: "eqp-01",
			segment: "a//b/staging/eqp-01/" + keyCaptureText + "/00000000000000000042-" + keyPackText + ".tpk",
			archive: "a//b/archive/eqp-01/2026/10/04/13/" + keyCaptureText + "-" + keyPackText + ".tpk",
			commit:  "a//b/commit/eqp-01/" + keyCaptureText + "/2026100413/" + keyOtherText,
		},
		{
			name: "escaped tool", prefix: "p", tool: "fab 1/é%",
			segment: "p/staging/fab%201%2F%C3%A9%25/" + keyCaptureText + "/00000000000000000042-" + keyPackText + ".tpk",
			archive: "p/archive/fab%201%2F%C3%A9%25/2026/10/04/13/" + keyCaptureText + "-" + keyPackText + ".tpk",
			commit:  "p/commit/fab%201%2F%C3%A9%25/" + keyCaptureText + "/2026100413/" + keyOtherText,
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()

			seg, err := SegmentKey(tt.prefix, tt.tool, capture, 42, pack)
			require.NoError(t, err)
			require.Equal(t, tt.segment, seg)

			arc, err := ArchiveKey(tt.prefix, tt.tool, capture, hour, pack)
			require.NoError(t, err)
			require.Equal(t, tt.archive, arc)

			com, err := CommitKey(tt.prefix, tt.tool, capture, hour, other)
			require.NoError(t, err)
			require.Equal(t, tt.commit, com)
		})
	}
}

func TestKeyBuildersReject(t *testing.T) {
	t.Parallel()

	capture, pack := keyUUID(t, keyCaptureText), keyUUID(t, keyPackText)
	hour := keyHourOf(2026, time.October, 4, 13)

	for _, tool := range []string{"", ".", ".."} {
		_, err := SegmentKey("p", tool, capture, 0, pack)
		require.ErrorIs(t, err, errInvalidKey, "segment, tool %q", tool)
		_, err = ArchiveKey("p", tool, capture, hour, pack)
		require.ErrorIs(t, err, errInvalidKey, "archive, tool %q", tool)
		_, err = CommitKey("p", tool, capture, hour, pack)
		require.ErrorIs(t, err, errInvalidKey, "commit, tool %q", tool)
	}

	// Dots inside a longer tool are ordinary unreserved bytes.
	for _, tool := range []string{"...", ".a", "a.", "%2E"} {
		_, err := SegmentKey("p", tool, capture, 0, pack)
		require.NoError(t, err, "tool %q", tool)
	}

	key, err := SegmentKey("p", "t", capture, math.MaxInt64, pack)
	require.NoError(t, err)
	require.Equal(t, "p/staging/t/"+keyCaptureText+"/09223372036854775807-"+keyPackText+".tpk", key)
	for _, seq := range []uint64{math.MaxInt64 + 1, math.MaxUint64} {
		_, err = SegmentKey("p", "t", capture, seq, pack)
		require.ErrorIs(t, err, errInvalidKey, "seq_first %d", seq)
	}

	for _, h := range []int64{minKeyHour - 1, maxKeyHour + 1, math.MinInt64, math.MaxInt64} {
		_, err = ArchiveKey("p", "t", capture, h, pack)
		require.ErrorIs(t, err, errInvalidKey, "archive hour %d", h)
		_, err = CommitKey("p", "t", capture, h, pack)
		require.ErrorIs(t, err, errInvalidKey, "commit hour %d", h)
	}
}

func TestKeyHours(t *testing.T) {
	t.Parallel()

	require.Equal(t, hourOf(math.MinInt64), minKeyHour)
	require.Equal(t, hourOf(math.MaxInt64), maxKeyHour)
	require.Equal(t, int64(-2562048), minKeyHour)
	require.Equal(t, int64(2562047), maxKeyHour)

	capture, pack := keyUUID(t, keyCaptureText), keyUUID(t, keyPackText)

	tests := []struct {
		name    string
		hour    int64
		archive string // the archive key's hour components
		commit  string // the commit key's hour component
	}{
		{"first storage hour, partially representable", minKeyHour, "1677/09/21/00", "1677092100"},
		{"last storage hour, partially representable", maxKeyHour, "2262/04/11/23", "2262041123"},
		{"epoch", 0, "1970/01/01/00", "1970010100"},
		{"hour before the epoch", -1, "1969/12/31/23", "1969123123"},
		{"last hour of a year", keyHourOf(2026, time.December, 31, 23), "2026/12/31/23", "2026123123"},
		{"first hour of a year", keyHourOf(2027, time.January, 1, 0), "2027/01/01/00", "2027010100"},
		{"leap day", keyHourOf(2028, time.February, 29, 5), "2028/02/29/05", "2028022905"},
		{"day after a leap day", keyHourOf(2028, time.March, 1, 0), "2028/03/01/00", "2028030100"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()

			arc, err := ArchiveKey("p", "t", capture, tt.hour, pack)
			require.NoError(t, err)
			require.Equal(t, "p/archive/t/"+tt.archive+"/"+keyCaptureText+"-"+keyPackText+".tpk", arc)
			ap, err := parseArchiveKey("p", arc)
			require.NoError(t, err)
			require.Equal(t, tt.hour, ap.hour)

			com, err := CommitKey("p", "t", capture, tt.hour, pack)
			require.NoError(t, err)
			require.Equal(t, "p/commit/t/"+keyCaptureText+"/"+tt.commit+"/"+keyPackText, com)
			cp, err := parseCommitKey("p", com)
			require.NoError(t, err)
			require.Equal(t, tt.hour, cp.hour)
		})
	}
}

func TestParseKeysRoundTrip(t *testing.T) {
	t.Parallel()

	capture, pack, other := keyUUID(t, keyCaptureText), keyUUID(t, keyPackText), keyUUID(t, keyOtherText)
	tools := []string{"eqp-01", "~", "...", "a b/c%d", "é\xff\x00", "%2F"}
	prefixes := []string{"", "a", "a/b", "a/", "/a", "a//b"}
	seqs := []uint64{0, 1, 42, math.MaxInt64}
	hours := []int64{minKeyHour, -1, 0, keyHourOf(2026, time.October, 4, 13), maxKeyHour}

	for _, prefix := range prefixes {
		for _, tool := range tools {
			for _, seq := range seqs {
				want := segmentKeyParts{tool: tool, capture: capture, seqFirst: seq, pack: pack}
				key, err := SegmentKey(prefix, tool, capture, seq, pack)
				require.NoError(t, err)
				got, err := parseSegmentKey(prefix, key)
				require.NoError(t, err, key)
				require.Equal(t, want, got, key)
			}
			for _, hour := range hours {
				wantA := archiveKeyParts{tool: tool, hour: hour, capture: capture, pack: pack}
				key, err := ArchiveKey(prefix, tool, capture, hour, pack)
				require.NoError(t, err)
				gotA, err := parseArchiveKey(prefix, key)
				require.NoError(t, err, key)
				require.Equal(t, wantA, gotA, key)

				wantC := commitKeyParts{tool: tool, capture: capture, hour: hour, id: other}
				key, err = CommitKey(prefix, tool, capture, hour, other)
				require.NoError(t, err)
				gotC, err := parseCommitKey(prefix, key)
				require.NoError(t, err, key)
				require.Equal(t, wantC, gotC, key)
			}
		}
	}
}

// TestParseKeysAnyCapture checks that a parser accepts a canonical key of any capture, tool and hour:
// classifying it is the caller's.
func TestParseKeysAnyCapture(t *testing.T) {
	t.Parallel()

	seg, err := parseSegmentKey("p", "p/staging/other/"+keyOtherText+"/00000000000000000001-"+keyPackText+".tpk")
	require.NoError(t, err)
	require.Equal(t, segmentKeyParts{tool: "other", capture: keyUUID(t, keyOtherText), seqFirst: 1, pack: keyUUID(t, keyPackText)}, seg)

	arc, err := parseArchiveKey("p", "p/archive/t/2026/10/04/13/"+keyOtherText+"-"+keyPackText+".tpk")
	require.NoError(t, err)
	require.Equal(t, keyUUID(t, keyOtherText), arc.capture)

	com, err := parseCommitKey("p", "p/commit/t/"+keyOtherText+"/2026100413/"+keyPackText)
	require.NoError(t, err)
	require.Equal(t, commitKeyParts{tool: "t", capture: keyUUID(t, keyOtherText), hour: keyHourOf(2026, time.October, 4, 13), id: keyUUID(t, keyPackText)}, com)
}

// TestKeyErrorMessages checks that a builder's and a parser's error name the invalid key once, with the reason.
func TestKeyErrorMessages(t *testing.T) {
	t.Parallel()

	capture, pack := keyUUID(t, keyCaptureText), keyUUID(t, keyPackText)

	_, err := ArchiveKey("p", "t", capture, maxKeyHour+1, pack)
	require.EqualError(t, err, "tracepack: invalid object key: hour 2562048 outside [-2562048, 2562047]")
	_, err = SegmentKey("p", ".", capture, 0, pack)
	require.EqualError(t, err, `tracepack: invalid object key: tool "." cannot be stored`)

	key := "p/archive/t/2262/04/12/00/" + keyCaptureText + "-" + keyPackText + ".tpk"
	_, err = parseArchiveKey("p", key)
	require.EqualError(t, err, `tracepack: invalid object key "`+key+`": hour 2562048 outside [-2562048, 2562047]`)
	key = "p/commit/../" + keyCaptureText + "/2026100413/" + keyOtherText
	_, err = parseCommitKey("p", key)
	require.EqualError(t, err, `tracepack: invalid object key "`+key+`": tool ".." cannot be stored`)
}

// keyRejectCase is a key a parser must reject, under the prefix "p".
type keyRejectCase struct {
	name, key string
}

func TestParseSegmentKeyRejects(t *testing.T) {
	t.Parallel()

	const (
		dir  = "p/staging/t/" + keyCaptureText + "/"
		file = "00000000000000000042-" + keyPackText + ".tpk"
	)
	checkKeyRejects(t, func(k string) error { _, err := parseSegmentKey("p", k); return err }, dir+file, []keyRejectCase{
		{"uppercase capture", "p/staging/t/" + strings.ToUpper(keyCaptureText) + "/" + file},
		{"mixed-case capture", "p/staging/t/0190A1b2-c3d4-7e5f-8a6b-1c2d3e4f5a6b/" + file},
		{"uppercase pack", dir + "00000000000000000042-" + strings.ToUpper(keyPackText) + ".tpk"},
		{"mixed-case pack", dir + "00000000000000000042-" + keyPackMixed + ".tpk"},
		{"capture without hyphens", "p/staging/t/0190a1b2c3d47e5f8a6b1c2d3e4f5a6b/" + file},
		{"extra suffix", dir + file + ".bak"},
		{"other suffix", dir + "00000000000000000042-" + keyPackText + ".tpz"},
		{"no suffix", dir + "00000000000000000042-" + keyPackText},
		{"bytes before the suffix", dir + "00000000000000000042-" + keyPackText + "x.tpk"},
		{"extra component", dir + "x/" + file},
		{"missing component", "p/staging/t/" + file},
		{"trailing slash", dir + file + "/"},
		{"seq_first 2^63", dir + "09223372036854775808-" + keyPackText + ".tpk"},
		{"seq_first over 2^64", dir + "99999999999999999999-" + keyPackText + ".tpk"},
		{"seq_first of 19 digits", dir + "0000000000000000042-" + keyPackText + ".tpk"},
		{"seq_first of 21 digits", dir + "000000000000000000042-" + keyPackText + ".tpk"},
		{"seq_first with a sign", dir + "+0000000000000000042-" + keyPackText + ".tpk"},
		{"seq_first not decimal", dir + "0000000000000000004a-" + keyPackText + ".tpk"},
		{"no separator", dir + "00000000000000000042_" + keyPackText + ".tpk"},
		{"no prefix", "staging/t/" + keyCaptureText + "/" + file},
		{"other prefix", "q/staging/t/" + keyCaptureText + "/" + file},
		{"prefix as a substring", "pp/staging/t/" + keyCaptureText + "/" + file},
		{"other area", "p/archive/t/" + keyCaptureText + "/" + file},
	})
}

func TestParseArchiveKeyRejects(t *testing.T) {
	t.Parallel()

	const (
		dir  = "p/archive/t/2026/10/04/13/"
		file = keyCaptureText + "-" + keyPackText + ".tpk"
	)
	checkKeyRejects(t, func(k string) error { _, err := parseArchiveKey("p", k); return err }, dir+file, []keyRejectCase{
		{"uppercase capture", dir + strings.ToUpper(keyCaptureText) + "-" + keyPackText + ".tpk"},
		{"uppercase pack", dir + keyCaptureText + "-" + strings.ToUpper(keyPackText) + ".tpk"},
		{"mixed-case pack", dir + keyCaptureText + "-" + keyPackMixed + ".tpk"},
		{"extra suffix", dir + file + ".bak"},
		{"no suffix", dir + keyCaptureText + "-" + keyPackText},
		{"other suffix", dir + keyCaptureText + "-" + keyPackText + ".tpz"},
		{"bytes before the suffix", dir + keyCaptureText + "-" + keyPackText + "x.tpk"},
		{"extra component", dir + "x/" + file},
		{"missing component", "p/archive/t/2026/10/04/" + file},
		{"no separator", dir + keyCaptureText + "_" + keyPackText + ".tpk"},
		{"invalid calendar day", "p/archive/t/2026/02/29/03/" + file},
		{"hour 24", "p/archive/t/2026/10/04/24/" + file},
		{"month 13", "p/archive/t/2026/13/04/13/" + file},
		{"day 0", "p/archive/t/2026/10/00/13/" + file},
		{"year of 3 digits", "p/archive/t/026/10/04/13/" + file},
		{"year of 5 digits", "p/archive/t/02026/10/04/13/" + file},
		{"month of 1 digit", "p/archive/t/2026/1/04/13/" + file},
		{"hour with a sign", "p/archive/t/2026/10/04/+1/" + file},
		{"before the first storage hour", "p/archive/t/1677/09/20/23/" + file},
		{"after the last storage hour", "p/archive/t/2262/04/12/00/" + file},
		{"year 0", "p/archive/t/0000/01/01/00/" + file},
	})
}

func TestParseCommitKeyRejects(t *testing.T) {
	t.Parallel()

	const dir = "p/commit/t/" + keyCaptureText + "/2026100413/"
	checkKeyRejects(t, func(k string) error { _, err := parseCommitKey("p", k); return err }, dir+keyOtherText, []keyRejectCase{
		{"uppercase id", dir + strings.ToUpper(keyOtherText)},
		{"mixed-case id", dir + keyOtherMixed},
		{"uppercase capture", "p/commit/t/" + strings.ToUpper(keyCaptureText) + "/2026100413/" + keyOtherText},
		{"suffix after the id", dir + keyOtherText + ".tpk"},
		{"extra component", dir + keyOtherText + "/x"},
		{"trailing slash", dir + keyOtherText + "/"},
		{"missing component", "p/commit/t/2026100413/" + keyOtherText},
		{"invalid calendar hour", "p/commit/t/" + keyCaptureText + "/2026022930/" + keyOtherText},
		{"hour of 9 digits", "p/commit/t/" + keyCaptureText + "/202610041/" + keyOtherText},
		{"hour of 11 digits", "p/commit/t/" + keyCaptureText + "/20261004130/" + keyOtherText},
		{"hour not decimal", "p/commit/t/" + keyCaptureText + "/2026-10-04/" + keyOtherText},
		{"after the last storage hour", "p/commit/t/" + keyCaptureText + "/2262041200/" + keyOtherText},
	})
}

// TestParseKeysRejectTools checks the tool component every parser shares.
func TestParseKeysRejectTools(t *testing.T) {
	t.Parallel()

	accept := map[string]string{
		"eqp-01":     "eqp-01",
		"%2F":        "/",
		"%2Fx%25":    "/x%",
		"%C3%A9":     "é",
		"%FF%00":     "\xff\x00",
		"...":        "...",
		"~._-AZaz09": "~._-AZaz09",
	}
	reject := []string{
		"",         // empty tool
		".",        // a path component of its own
		"..",       // a path component of its own
		"%2f",      // lowercase hex
		"%41",      // escape of an unreserved byte
		"%2E",      // escape of '.', which would spell "." twice
		"%7E",      // escape of '~'
		"%G0",      // not hex
		"%0G",      // not hex
		"a%",       // trailing '%'
		"a%2",      // cut short
		"a b",      // raw space
		"a%25%",    // trailing '%' after an escape
		"\xc3\xa9", // raw non-ASCII
		"a+b",      // raw reserved byte
	}

	for tool, want := range accept {
		got, err := parseCommitKey("p", "p/commit/"+tool+"/"+keyCaptureText+"/2026100413/"+keyOtherText)
		require.NoError(t, err, "tool %q", tool)
		require.Equal(t, want, got.tool, "tool %q", tool)
	}
	for _, tool := range reject {
		_, err := parseSegmentKey("p", "p/staging/"+tool+"/"+keyCaptureText+"/00000000000000000042-"+keyPackText+".tpk")
		require.ErrorIs(t, err, errInvalidKey, "segment, tool %q", tool)
		_, err = parseArchiveKey("p", "p/archive/"+tool+"/2026/10/04/13/"+keyCaptureText+"-"+keyPackText+".tpk")
		require.ErrorIs(t, err, errInvalidKey, "archive, tool %q", tool)
		_, err = parseCommitKey("p", "p/commit/"+tool+"/"+keyCaptureText+"/2026100413/"+keyOtherText)
		require.ErrorIs(t, err, errInvalidKey, "commit, tool %q", tool)
	}
}

// FuzzKeys checks that every key a parser accepts is the one spelling its builder produces,
// and that every tool a key can hold survives a build and a parse.
func FuzzKeys(f *testing.F) {
	capture, pack := keyUUID(f, keyCaptureText), keyUUID(f, keyPackText)
	for _, prefix := range []string{"", "p", "a/", "/a"} {
		seg, err := SegmentKey(prefix, "t%/é", capture, math.MaxInt64, pack)
		require.NoError(f, err)
		arc, err := ArchiveKey(prefix, "t", capture, minKeyHour, pack)
		require.NoError(f, err)
		com, err := CommitKey(prefix, "t", capture, maxKeyHour, pack)
		require.NoError(f, err)
		for _, key := range []string{seg, arc, com} {
			f.Add(prefix, key, "eqp-01")
		}
	}
	f.Add("p", "p/commit/t/"+keyCaptureText+"/2026022930/"+keyOtherText, ".")
	f.Add("p", "p/staging/%2f/"+keyCaptureText+"/09223372036854775808-"+keyPackText+".tpk", "a b")
	f.Add("", "archive/%41/2262/04/12/00/"+keyCaptureText+"-"+keyPackText+".tpk", "\xff")

	f.Fuzz(func(t *testing.T, prefix, key, tool string) {
		if p, err := parseSegmentKey(prefix, key); err == nil {
			got, err := SegmentKey(prefix, p.tool, p.capture, p.seqFirst, p.pack)
			require.NoError(t, err)
			require.Equal(t, key, got)
		} else {
			require.ErrorIs(t, err, errInvalidKey)
		}
		if p, err := parseArchiveKey(prefix, key); err == nil {
			got, err := ArchiveKey(prefix, p.tool, p.capture, p.hour, p.pack)
			require.NoError(t, err)
			require.Equal(t, key, got)
		} else {
			require.ErrorIs(t, err, errInvalidKey)
		}
		if p, err := parseCommitKey(prefix, key); err == nil {
			got, err := CommitKey(prefix, p.tool, p.capture, p.hour, p.id)
			require.NoError(t, err)
			require.Equal(t, key, got)
		} else {
			require.ErrorIs(t, err, errInvalidKey)
		}

		built, err := CommitKey(prefix, tool, capture, 0, pack)
		if checkKeyTool(tool) != nil {
			require.ErrorIs(t, err, errInvalidKey)

			return
		}
		require.NoError(t, err)
		p, err := parseCommitKey(prefix, built)
		require.NoError(t, err)
		require.Equal(t, tool, p.tool)
	})
}

// checkKeyRejects checks that parse accepts valid, the same key with no defect, and rejects every case with errInvalidKey.
func checkKeyRejects(t *testing.T, parse func(string) error, valid string, cases []keyRejectCase) {
	t.Helper()
	require.NoError(t, parse(valid), "control key %q", valid)

	for _, tc := range cases {
		require.ErrorIs(t, parse(tc.key), errInvalidKey, "%s: %q", tc.name, tc.key)
	}
}
