package gem_test

import (
	"bytes"
	"fmt"
	"reflect"
	"testing"

	"github.com/arloliu/go-secs/v2/secs2"
)

// itemType is the interface every SECS-II item implements.
var itemType = reflect.TypeFor[secs2.Item]()

// assertDecoded fails t unless got, a decoder's result, equals want, the result the test expects.
//
// The generated round-trip tests build want from the samples they sent and call it;
// it compares by meaning rather than by memory layout:
// an item by its wire bytes, so two items that encode alike are equal;
// a pointer by what it points to, with a nil pointer equal only to another nil;
// a slice or an array by length and then element by element, so a nil slice equals an empty one;
// and a struct field by field.
// Every other value is compared with ==.
// Each mismatch is reported with its path from the result, e.g. S2F33Body.REPORTS[0].VIDS[1].
func assertDecoded(t *testing.T, got any, want any) {
	t.Helper()

	for _, diff := range decodedDiffs(got, want) {
		t.Error(diff)
	}
}

// decodedDiffs returns every difference between got and want, as [assertDecoded] defines equality.
func decodedDiffs(got any, want any) []string {
	gotValue, wantValue := reflect.ValueOf(got), reflect.ValueOf(want)
	if !gotValue.IsValid() || !wantValue.IsValid() {
		if gotValue.IsValid() != wantValue.IsValid() {
			return []string{fmt.Sprintf("got %v, want %v", got, want)}
		}

		return nil
	}

	var diffs []string

	compareDecoded(&diffs, gotValue.Type().Name(), gotValue, wantValue)

	return diffs
}

// compareDecoded appends to diffs every difference between got and want at path.
func compareDecoded(diffs *[]string, path string, got reflect.Value, want reflect.Value) {
	report := func(format string, args ...any) {
		*diffs = append(*diffs, path+": "+fmt.Sprintf(format, args...))
	}

	if got.Type() != want.Type() {
		report("got type %s, want %s", got.Type(), want.Type())

		return
	}

	if got.Type().Implements(itemType) {
		compareItems(report, got, want)

		return
	}

	switch got.Kind() { //nolint:exhaustive // every other kind is a comparable scalar, compared by the default case
	case reflect.Pointer, reflect.Interface:
		if got.IsNil() || want.IsNil() {
			if got.IsNil() != want.IsNil() {
				report("got %s, want %s", nilOrSet(got), nilOrSet(want))
			}

			return
		}

		compareDecoded(diffs, path, got.Elem(), want.Elem())
	case reflect.Slice, reflect.Array:
		if got.Len() != want.Len() {
			report("got %d elements, want %d", got.Len(), want.Len())

			return
		}

		for i := range got.Len() {
			compareDecoded(diffs, fmt.Sprintf("%s[%d]", path, i), got.Index(i), want.Index(i))
		}
	case reflect.Struct:
		for i := range got.NumField() {
			compareDecoded(diffs, path+"."+got.Type().Field(i).Name, got.Field(i), want.Field(i))
		}
	default:
		if !got.Equal(want) {
			report("got %v, want %v", got, want)
		}
	}
}

// compareItems reports through report whether the items got and want differ in their wire bytes, or in being nil.
func compareItems(report func(format string, args ...any), got reflect.Value, want reflect.Value) {
	gotItem, _ := got.Interface().(secs2.Item)
	wantItem, _ := want.Interface().(secs2.Item)

	if isNilItem(gotItem) || isNilItem(wantItem) {
		if isNilItem(gotItem) != isNilItem(wantItem) {
			report("got %s, want %s", nilOrSet(got), nilOrSet(want))
		}

		return
	}

	if gotBytes, wantBytes := gotItem.ToBytes(), wantItem.ToBytes(); !bytes.Equal(gotBytes, wantBytes) {
		report("got %x, want %x", gotBytes, wantBytes)
	}
}

// isNilItem reports whether item is a nil interface or holds a nil pointer.
func isNilItem(item secs2.Item) bool {
	if item == nil {
		return true
	}

	v := reflect.ValueOf(item)

	return v.Kind() == reflect.Pointer && v.IsNil()
}

// nilOrSet describes whether a pointer, interface, or item value is nil, for a mismatch message.
func nilOrSet(v reflect.Value) string {
	if (v.Kind() == reflect.Pointer || v.Kind() == reflect.Interface) && v.IsNil() {
		return "nil"
	}

	return "non-nil"
}

// TestAssertDecoded proves the comparison assertDecoded makes,
// and that a mismatch names its path from the result.
func TestAssertDecoded(t *testing.T) {
	type inner struct {
		NAME  string
		VALUE secs2.Item
	}

	type sample struct {
		ID     *uint32
		ITEMS  []secs2.Item
		GROUPS []inner
	}

	seven, eight := uint32(7), uint32(8)
	base := func() sample {
		return sample{
			ID:     &seven,
			ITEMS:  []secs2.Item{secs2.U2(1)},
			GROUPS: []inner{{NAME: "a", VALUE: secs2.A("x")}, {NAME: "b", VALUE: secs2.L()}},
		}
	}

	tests := []struct {
		name   string
		mutate func(*sample)
		want   []string
	}{
		{"equal", func(*sample) {}, nil},
		{"pointers compare by pointee", func(s *sample) { s.ID = new(uint32(7)) }, nil},
		{"pointee differs", func(s *sample) { s.ID = &eight }, []string{"sample.ID: got 8, want 7"}},
		{"nil pointer", func(s *sample) { s.ID = nil }, []string{"sample.ID: got nil, want non-nil"}},
		{"item bytes differ", func(s *sample) { s.ITEMS[0] = secs2.U4(1) }, []string{"sample.ITEMS[0]: got b10400000001, want a9020001"}},
		{"nil item", func(s *sample) { s.GROUPS[1].VALUE = nil }, []string{"sample.GROUPS[1].VALUE: got nil, want non-nil"}},
		{"length differs", func(s *sample) { s.GROUPS = s.GROUPS[:1] }, []string{"sample.GROUPS: got 1 elements, want 2"}},
		{"nested scalar differs", func(s *sample) { s.GROUPS[1].NAME = "c" }, []string{"sample.GROUPS[1].NAME: got c, want b"}},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := base()
			tt.mutate(&got)

			diffs := decodedDiffs(got, base())
			if fmt.Sprint(diffs) != fmt.Sprint(tt.want) {
				t.Errorf("got %q, want %q", diffs, tt.want)
			}
		})
	}

	if diffs := decodedDiffs(sample{}, sample{ITEMS: []secs2.Item{}}); diffs != nil {
		t.Errorf("nil and empty slice: got %q, want no difference", diffs)
	}

	if diffs := decodedDiffs(seven, int(7)); len(diffs) != 1 {
		t.Errorf("type mismatch: got %q, want one difference", diffs)
	}
}
