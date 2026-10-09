// Fuzz targets for the TLV decoder, the registry check, the typed accessors and the array decoders.
//
// FuzzDecode feeds arbitrary bytes to Decode,
// then runs every registry's Validate and every typed accessor on every decoded entry,
// descending into nested tlv values;
// when Validate accepts an entry list, every known tag's accessor must accept its value too.
// The array targets fuzz DecodeU32Array, DecodeU64Array and DecodeSeqRange on their own,
// and FuzzDecode also runs them on every entry value.
// Each target asserts that nothing panics,
// that a failure is reported through the package's errors,
// and that a successful decode re-encodes with the matching Append function or constructor to a value that decodes equal.
package tlv

import (
	"encoding/hex"
	"errors"
	"maps"
	"slices"
	"testing"
	"unicode/utf8"

	"github.com/stretchr/testify/require"
)

// maxFuzzNestingDepth bounds how deep FuzzDecode descends into nested tlv values.
// Decode itself does not recurse; the bound only keeps one fuzz iteration short.
const maxFuzzNestingDepth = 32

// fuzzRegistries is every registry of the package, each checked against every decoded entry list.
var fuzzRegistries = []Registry{
	PackMetadata, Coverage, RedactionPolicy, Redaction, HSMSTimers,
	TransportEvent, Annotation, F3, F5, Epoch, Boundary,
}

// fuzzSentinels is every sentinel error the package reports.
var fuzzSentinels = []error{
	ErrTruncated, ErrZeroTag, ErrLength, ErrType, ErrDuplicate, ErrMissing, ErrLimit, ErrUTF8, ErrBool, ErrDepth,
}

// fuzzHex decodes a hex seed.
func fuzzHex(f *testing.F, s string) []byte {
	f.Helper()

	b, err := hex.DecodeString(s)
	require.NoError(f, err)

	return b
}

// sampleEntry returns an entry of type vt carrying a well-formed value.
func sampleEntry(tag uint16, vt ValueType) Entry {
	switch vt {
	case TypeU8:
		return U8Entry(tag, 7)
	case TypeBool:
		return BoolEntry(tag, true)
	case TypeI64:
		return I64Entry(tag, -2)
	case TypeU64:
		return U64Entry(tag, MaxU64)
	case TypeUUID:
		return UUIDEntry(tag, goldenUUIDValue)
	case TypeUTF8:
		return UTF8Entry(tag, "EQ-01")
	case TypeBytes:
		return BytesEntry(tag, AppendU32Array(nil, []uint32{3, 0, 7, 1}))
	case TypeTLV:
		return NestedEntry(tag, []Entry{U64Entry(1, 45000), UTF8Entry(2, "x")})
	default:
		return Entry{Tag: tag, Type: vt, Value: []byte("opaque")}
	}
}

// encodeEntries returns the concatenated encoding of entries.
func encodeEntries(entries []Entry) []byte {
	var b []byte
	for _, e := range entries {
		b = AppendEntry(b, e)
	}

	return b
}

// registrySeed returns an entry list holding every tag of reg, in tag order, each with a well-formed value,
// so the seed passes Validate against reg.
func registrySeed(reg Registry) []byte {
	return encodeEntries(registryEntries(reg))
}

// registryEntries returns the entries of registrySeed;
// the value of a field with a nested registry holds every tag of that registry.
func registryEntries(reg Registry) []Entry {
	tags := slices.Sorted(maps.Keys(reg))
	entries := make([]Entry, 0, len(tags))
	for _, tag := range tags {
		f := reg[tag]
		if f.Nested != nil {
			entries = append(entries, NestedEntry(tag, registryEntries(*f.Nested)))
			continue
		}
		entries = append(entries, sampleEntry(tag, f.Type))
	}

	return entries
}

// requireEntryError asserts that err is an *EntryError wrapping one of the package's sentinel errors.
func requireEntryError(t *testing.T, err error) {
	t.Helper()

	var ee *EntryError
	require.ErrorAs(t, err, &ee)
	requireSentinel(t, err)
}

// requireSentinel asserts that err wraps one of the package's sentinel errors.
func requireSentinel(t *testing.T, err error) {
	t.Helper()

	for _, s := range fuzzSentinels {
		if errors.Is(err, s) {
			return
		}
	}
	require.Failf(t, "unexpected error", "%v wraps none of the package's sentinel errors", err)
}

// FuzzDecode fuzzes Decode, and on success Validate against every registry and every typed accessor on every entry.
//
// On success, every entry has a nonzero tag, a value of the fixed length its known type requires,
// an Offset that matches its position, and a value that aliases the input with cap equal to len;
// the re-encoded entries have the input's length and decode to equal entries.
// Allocation bound: cap(entries) ≤ len(input)/HeaderLen, since every entry takes at least a header;
// entry values alias the input and allocate nothing.
func FuzzDecode(f *testing.F) {
	goldens := []string{
		goldenU8, goldenBool, goldenI64, goldenU64, goldenUUID,
		goldenUTF8, goldenBytes, goldenEmpty, goldenNested, goldenUnknown,
	}
	var all []byte
	for _, g := range goldens {
		b := fuzzHex(f, g)
		f.Add(b)
		all = append(all, b...)
	}
	f.Add(all)
	f.Add(all[:len(all)-1])
	f.Add([]byte{})
	f.Add(fuzzHex(f, goldenU8)[:HeaderLen-1])

	for _, reg := range fuzzRegistries {
		f.Add(registrySeed(reg))
	}

	inner := NestedEntry(0x0016, []Entry{UUIDEntry(1, goldenUUIDValue), U64Entry(2, 1)})
	f.Add(encodeEntries([]Entry{NestedEntry(0x0007, []Entry{inner, NestedEntry(0x0008, nil)})}))
	f.Add(encodeEntries([]Entry{UTF8Entry(0x000E, "a"), UTF8Entry(0x000E, "b"), UTF8Entry(0x0002, "c"), UTF8Entry(0x0002, "d")}))
	f.Add(encodeEntries([]Entry{{Tag: 0x0002, Type: TypeBool, Value: []byte{2}}, {Tag: 0x0003, Type: TypeUTF8, Value: []byte{0xFF}}}))
	f.Add(encodeEntries([]Entry{{Tag: 1, Type: TypeTLV, Value: []byte{0, 0, 1, 0, 1, 0, 0, 0, 1}}}))

	zeroTag := fuzzHex(f, goldenU8)
	zeroTag[0] = 0
	f.Add(zeroTag)
	badFixedLen := fuzzHex(f, "03000100020000000101")
	f.Add(badFixedLen)
	hugeLen := fuzzHex(f, "03000700ffffffff01")
	f.Add(hugeLen)

	f.Fuzz(func(t *testing.T, b []byte) {
		checkEntries(t, b, 0)
	})
}

// checkEntries decodes b and checks the result, then checks every entry at the given nesting depth.
// Entries must agree with Decode: the same entries, or the same error after the entries before the malformed one.
func checkEntries(t *testing.T, b []byte, depth int) {
	t.Helper()

	entries, err := Decode(b)
	walked, walkErr := collectEntries(b)
	require.Equal(t, err, walkErr, "Entries stops at the entry Decode rejects, with the same error")
	if err != nil {
		requireEntryError(t, err)
		require.Nil(t, entries)
		var ee *EntryError
		require.ErrorAs(t, err, &ee)
		prefix, prefixErr := Decode(b[:ee.Offset])
		require.NoError(t, prefixErr)
		require.Equal(t, prefix, walked, "Entries yields every entry before the malformed one")

		return
	}
	require.Equal(t, entries, walked, "Entries yields the entries Decode returns")
	if len(b) == 0 {
		require.Nil(t, entries)

		return
	}

	require.LessOrEqual(t, cap(entries), len(b)/HeaderLen, "cap(entries) must stay within len(input)/HeaderLen")

	enc := make([]byte, 0, len(b))
	for _, e := range entries {
		require.NotZero(t, e.Tag)
		require.Equal(t, len(enc), e.Offset)
		require.Equal(t, len(e.Value), cap(e.Value), "a value must not reach into the next entry")
		if n, fixed := e.Type.FixedLen(); fixed {
			require.Len(t, e.Value, n)
		}
		if len(e.Value) > 0 {
			require.Same(t, &b[e.Offset+HeaderLen], &e.Value[0], "a value must alias the input")
		}
		enc = AppendEntry(enc, e)
	}
	require.Len(t, enc, len(b))

	got, err := Decode(enc)
	require.NoError(t, err)
	require.Equal(t, entries, got)

	for _, reg := range fuzzRegistries {
		if err := Validate(entries, reg); err != nil {
			requireEntryError(t, err)
		} else {
			requireValidated(t, entries, reg)
		}
	}

	for _, e := range entries {
		checkFixedAccessors(t, e)
		checkVariableAccessors(t, e)
		checkNested(t, e, depth)
		checkArrays(t, e.Value)
	}
}

// collectEntries returns the entries and the error Entries yields for b.
func collectEntries(b []byte) ([]Entry, error) {
	var out []Entry
	for e, err := range Entries(b) {
		if err != nil {
			return out, err
		}
		out = append(out, e)
	}

	return out, nil
}

// requireValidated asserts what a nil error from Validate promises about entries:
// every known tag carries its registry's type and a value its typed accessor accepts,
// and the value of a field with a nested registry decodes to entries that registry accepts.
func requireValidated(t *testing.T, entries []Entry, reg Registry) {
	t.Helper()

	for _, e := range entries {
		f, known := reg[e.Tag]
		if !known {
			continue
		}
		require.Equal(t, f.Type, e.Type)
		require.NoError(t, accessorError(e))
		if f.Nested != nil {
			nested, err := e.Nested()
			require.NoError(t, err)
			require.NoError(t, Validate(nested, *f.Nested))
		}
	}
}

// accessorError returns the error of the typed accessor for e's type, or nil for an unknown type.
func accessorError(e Entry) error {
	var err error
	switch e.Type {
	case TypeU8:
		_, err = e.U8()
	case TypeBool:
		_, err = e.Bool()
	case TypeI64:
		_, err = e.I64()
	case TypeU64:
		_, err = e.U64()
	case TypeUUID:
		_, err = e.UUID()
	case TypeUTF8:
		_, err = e.UTF8()
	case TypeBytes:
		_, err = e.Bytes()
	case TypeTLV:
		_, err = e.Nested()
	default:
		// An unknown value type has no accessor.
	}

	return err
}

// requireAccessor checks the error of the accessor for type want on e:
// a success requires e to have that type, and a failure must be an *EntryError, ErrType when the types differ.
// It reports whether the accessor succeeded.
func requireAccessor(t *testing.T, e Entry, want ValueType, err error) bool {
	t.Helper()

	if err != nil {
		requireEntryError(t, err)
		if e.Type != want {
			require.ErrorIs(t, err, ErrType)
		}

		return false
	}
	require.Equal(t, want, e.Type)

	return true
}

// checkFixedAccessors runs the fixed-length accessors on e.
// Each succeeds only on its own type, and its value re-encodes with the matching constructor to e.Value exactly.
func checkFixedAccessors(t *testing.T, e Entry) {
	t.Helper()

	u8, err := e.U8()
	if requireAccessor(t, e, TypeU8, err) {
		require.Equal(t, e.Value, U8Entry(e.Tag, u8).Value)
	} else {
		require.NotEqual(t, TypeU8, e.Type, "a u8 of the right length always decodes")
	}

	bv, err := e.Bool()
	if requireAccessor(t, e, TypeBool, err) {
		require.Equal(t, e.Value, BoolEntry(e.Tag, bv).Value)
	} else if e.Type == TypeBool {
		require.ErrorIs(t, err, ErrBool)
	}

	i64, err := e.I64()
	if requireAccessor(t, e, TypeI64, err) {
		require.Equal(t, e.Value, I64Entry(e.Tag, i64).Value)
	} else {
		require.NotEqual(t, TypeI64, e.Type, "an i64 of the right length always decodes")
	}

	u64, err := e.U64()
	if requireAccessor(t, e, TypeU64, err) {
		require.LessOrEqual(t, u64, uint64(MaxU64))
		require.Equal(t, e.Value, U64Entry(e.Tag, u64).Value)
	} else if e.Type == TypeU64 {
		require.ErrorIs(t, err, ErrLimit)
	}

	id, err := e.UUID()
	if requireAccessor(t, e, TypeUUID, err) {
		require.Equal(t, e.Value, UUIDEntry(e.Tag, id).Value)
	} else {
		require.NotEqual(t, TypeUUID, e.Type, "a uuid of the right length always decodes")
	}
}

// checkVariableAccessors runs the utf8 and bytes accessors on e.
// Bytes must alias e.Value; UTF8 must return valid UTF-8 that re-encodes to e.Value exactly.
func checkVariableAccessors(t *testing.T, e Entry) {
	t.Helper()

	s, err := e.UTF8()
	if requireAccessor(t, e, TypeUTF8, err) {
		require.True(t, utf8.ValidString(s))
		require.Equal(t, e.Value, UTF8Entry(e.Tag, s).Value)
	} else if e.Type == TypeUTF8 {
		require.ErrorIs(t, err, ErrUTF8)
	}

	raw, err := e.Bytes()
	if requireAccessor(t, e, TypeBytes, err) {
		require.Len(t, raw, len(e.Value))
		require.Equal(t, cap(e.Value), cap(raw), "Bytes must return e.Value itself")
		if len(raw) > 0 {
			require.Same(t, &e.Value[0], &raw[0], "Bytes must alias e.Value")
		}
	} else {
		require.NotEqual(t, TypeBytes, e.Type, "a bytes entry always decodes")
	}
}

// checkNested runs the tlv accessor on e.
// It succeeds exactly when e is a tlv entry whose value Decode accepts,
// and its entries re-encode with NestedEntry to a value that decodes to equal entries.
// Below maxFuzzNestingDepth, the nested value is checked like the top-level input.
// Allocation bound: cap(nested) ≤ len(e.Value)/HeaderLen.
func checkNested(t *testing.T, e Entry, depth int) {
	t.Helper()

	nested, err := e.Nested()
	if !requireAccessor(t, e, TypeTLV, err) {
		if e.Type == TypeTLV {
			var outer *EntryError
			require.ErrorAs(t, err, &outer)
			require.Equal(t, e.Tag, outer.Tag)

			var inner *EntryError
			require.ErrorAs(t, outer.Err, &inner, "a malformed nested value must wrap the nested entry's error")
		}

		return
	}

	if len(e.Value) == 0 {
		require.Nil(t, nested)
	} else {
		require.LessOrEqual(t, cap(nested), len(e.Value)/HeaderLen, "cap(nested) must stay within len(value)/HeaderLen")
	}

	re := NestedEntry(e.Tag, nested)
	require.Len(t, re.Value, len(e.Value))
	got, err := Decode(re.Value)
	require.NoError(t, err)
	require.Equal(t, nested, got)

	if depth < maxFuzzNestingDepth {
		checkEntries(t, e.Value, depth+1)
	}
}

// checkArrays runs every array decoder on b, as an entry value might hold any of them.
func checkArrays(t *testing.T, b []byte) {
	t.Helper()

	checkU32Array(t, b)
	checkU64Array(t, b)
	checkSeqRange(t, b)
}

// FuzzU32Array fuzzes DecodeU32Array.
//
// It succeeds exactly when len(input) is a multiple of 4,
// and AppendU32Array of the result reproduces the input exactly.
// Allocation bound: cap(result) ≤ len(input)/4.
func FuzzU32Array(f *testing.F) {
	f.Add(fuzzHex(f, "030000000000000007000000"))
	f.Add(fuzzHex(f, "0300000000000000070000"))
	f.Add(fuzzHex(f, "ffffffff"))
	f.Add([]byte{})
	f.Add(make([]byte, 5))

	f.Fuzz(checkU32Array)
}

func checkU32Array(t *testing.T, b []byte) {
	t.Helper()

	out, err := DecodeU32Array(b)
	if err != nil {
		require.ErrorIs(t, err, ErrLength)
		require.NotZero(t, len(b)%4)
		require.Nil(t, out)

		return
	}

	require.Zero(t, len(b)%4)
	require.Len(t, out, len(b)/4)
	require.LessOrEqual(t, cap(out), len(b)/4, "cap(result) must stay within len(input)/4")
	require.Equal(t, b, AppendU32Array(make([]byte, 0, len(b)), out))
}

// FuzzU64Array fuzzes DecodeU64Array.
//
// It succeeds exactly when len(input) is a multiple of 8 and every element is at most MaxU64,
// and AppendU64Array of the result reproduces the input exactly.
// Allocation bound: cap(result) ≤ len(input)/8.
func FuzzU64Array(f *testing.F) {
	f.Add(fuzzHex(f, "0100000000000000ffffffffffffff7f"))
	f.Add(fuzzHex(f, "01000000000000000000000000000080"))
	f.Add(fuzzHex(f, "0100000000000000ffffffffffffff"))
	f.Add([]byte{})
	f.Add(make([]byte, 12))

	f.Fuzz(checkU64Array)
}

func checkU64Array(t *testing.T, b []byte) {
	t.Helper()

	out, err := DecodeU64Array(b)
	if err != nil {
		require.Nil(t, out)
		if len(b)%8 != 0 {
			require.ErrorIs(t, err, ErrLength)
		} else {
			require.ErrorIs(t, err, ErrLimit)
		}

		return
	}

	require.Zero(t, len(b)%8)
	require.Len(t, out, len(b)/8)
	require.LessOrEqual(t, cap(out), len(b)/8, "cap(result) must stay within len(input)/8")
	for _, v := range out {
		require.LessOrEqual(t, v, uint64(MaxU64))
	}
	require.Equal(t, b, AppendU64Array(make([]byte, 0, len(b)), out))
}

// FuzzSeqRange fuzzes DecodeSeqRange.
//
// It succeeds exactly when the input is SeqRangeLen bytes holding two seqs of at most MaxU64,
// and AppendSeqRange of the result reproduces the input exactly.
// SeqRange holds no slice, so there is no allocation to bound.
func FuzzSeqRange(f *testing.F) {
	f.Add(fuzzHex(f, "05000000000000000900000000000000"))
	f.Add(fuzzHex(f, "09000000000000000500000000000000"))
	f.Add(fuzzHex(f, "00000000000000800900000000000000"))
	f.Add(fuzzHex(f, "05000000000000000000000000000080"))
	f.Add(make([]byte, SeqRangeLen-1))
	f.Add(make([]byte, SeqRangeLen+1))

	f.Fuzz(checkSeqRange)
}

func checkSeqRange(t *testing.T, b []byte) {
	t.Helper()

	r, err := DecodeSeqRange(b)
	if err != nil {
		require.Equal(t, SeqRange{}, r)
		if len(b) != SeqRangeLen {
			require.ErrorIs(t, err, ErrLength)
		} else {
			require.ErrorIs(t, err, ErrLimit)
		}

		return
	}

	require.Len(t, b, SeqRangeLen)
	require.Equal(t, b, AppendSeqRange(nil, r))
}
