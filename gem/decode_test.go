package gem_test

import (
	"strings"
	"testing"

	"github.com/arloliu/go-secs/v2/gem"
	"github.com/arloliu/go-secs/v2/secs2"
)

// The generated round-trip tests in sN_decode_test.go prove every decoder reads back what its
// builder wrote.
// The tests here are the other half, and are deliberately hand-written: they pin the strict-shape
// contract on one representative message per shape class, so a generator change that quietly
// relaxed a check would fail here rather than pass a round trip that never sees a malformed body.
//
// Assertions match only the "gem: <FIELD>:" prefix this package adds, never the wording of the
// underlying [secs2.Cursor] error, which names the SECS-II type family rather than the E5 width.

// requireDecodeError asserts a decoder rejected a body and named the expected E5 field or body
// position in the error.
func requireDecodeError(t *testing.T, err error, want string) {
	t.Helper()

	if err == nil {
		t.Fatalf("got nil error, want an error mentioning %q", want)
	}

	if !strings.Contains(err.Error(), want) {
		t.Errorf("error %q does not mention %q", err.Error(), want)
	}
}

// TestDecodeBareLeafShapeErrors covers the bare fixed-leaf class, whose whole body is one item:
// S1F5 is B[sfcd].
func TestDecodeBareLeafShapeErrors(t *testing.T) {
	tests := []struct {
		name string
		item secs2.Item
		want string
	}{
		{name: "wrong type", item: secs2.A("1"), want: "gem: SFCD:"},
		{name: "wrong size", item: secs2.B(1, 2), want: "gem: SFCD: size 2, want 1"},
		{name: "empty body", item: secs2.NewEmptyItem(), want: "gem: SFCD:"},
		{name: "list instead of leaf", item: secs2.L(secs2.B(1)), want: "gem: SFCD:"},
		{name: "nil item", item: nil, want: "gem: SFCD:"},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			_, err := gem.DecodeS1F5(tt.item)
			requireDecodeError(t, err, tt.want)
		})
	}

	got, err := gem.DecodeS1F5(secs2.B(0x7f))
	if err != nil {
		t.Fatalf("DecodeS1F5: %v", err)
	}

	if got.SFCD != 0x7f {
		t.Errorf("SFCD: got %#x, want %#x", got.SFCD, 0x7f)
	}
}

// TestDecodeOpaqueBody covers the opaque class: S1F6's body is equipment-defined, so the decoder
// hands the item back unexamined and only a missing body is an error.
func TestDecodeOpaqueBody(t *testing.T) {
	for _, item := range []secs2.Item{secs2.A("anything"), secs2.L(secs2.U4(1)), secs2.NewEmptyItem()} {
		got, err := gem.DecodeS1F6(item)
		if err != nil {
			t.Fatalf("DecodeS1F6(%s): %v", item.Type(), err)
		}

		if got.BODY.Type() != item.Type() {
			t.Errorf("BODY: got %s, want %s", got.BODY.Type(), item.Type())
		}
	}

	_, err := gem.DecodeS1F6(nil)
	requireDecodeError(t, err, "gem: body:")
}

// TestDecodeFlatListShapeErrors covers the flat fixed-list class: S1F2 is L[2]{ A[mdln] A[softrev] }.
func TestDecodeFlatListShapeErrors(t *testing.T) {
	tests := []struct {
		name string
		item secs2.Item
		want string
	}{
		{name: "short list", item: secs2.L(secs2.A("m")), want: "gem: S1F2 body: list length 1, want 2"},
		{name: "trailing item", item: secs2.L(secs2.A("m"), secs2.A("s"), secs2.A("x")), want: "gem: S1F2 body: list length 3, want 2"},
		{name: "not a list", item: secs2.A("m"), want: "gem: S1F2 body: got ascii, want list"},
		{name: "empty body", item: secs2.NewEmptyItem(), want: "gem: S1F2 body: got empty, want list"},
		{name: "nil item", item: nil, want: "gem: S1F2 body:"},
		{name: "wrong type at 0", item: secs2.L(secs2.B(1), secs2.A("s")), want: "gem: MDLN:"},
		{name: "wrong type at 1", item: secs2.L(secs2.A("m"), secs2.B(1)), want: "gem: SOFTREV:"},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			_, err := gem.DecodeS1F2(tt.item)
			requireDecodeError(t, err, tt.want)
		})
	}
}

// TestDecodeNestedListShapeErrors covers the nested fixed-list class: S1F14 is
// L[2]{ B[commack] L[2]{ A[mdln] A[softrev] } }, so a failure inside the inner list must name that
// list's own position rather than the body root.
func TestDecodeNestedListShapeErrors(t *testing.T) {
	inner := secs2.L(secs2.A("m"), secs2.A("s"))

	tests := []struct {
		name string
		item secs2.Item
		want string
	}{
		{name: "outer arity", item: secs2.L(secs2.B(0)), want: "gem: S1F14 body: list length 1, want 2"},
		{name: "inner arity", item: secs2.L(secs2.B(0), secs2.L(secs2.A("m"))), want: "gem: S1F14 body[1]: list length 1, want 2"},
		{name: "inner not a list", item: secs2.L(secs2.B(0), secs2.A("m")), want: "gem: S1F14 body[1]: got ascii, want list"},
		{name: "wrong enum type", item: secs2.L(secs2.U1(0), inner), want: "gem: COMMACK:"},
		{name: "wrong type inside inner", item: secs2.L(secs2.B(0), secs2.L(secs2.B(1), secs2.A("s"))), want: "gem: MDLN:"},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			_, err := gem.DecodeS1F14(tt.item)
			requireDecodeError(t, err, tt.want)
		})
	}

	got, err := gem.DecodeS1F14(secs2.L(secs2.B(1), inner))
	if err != nil {
		t.Fatalf("DecodeS1F14: %v", err)
	}

	if got.COMMACK != gem.COMMACKDenied || got.MDLN != "m" || got.SOFTREV != "s" {
		t.Errorf("got %+v, want {COMMACKDenied m s}", got)
	}
}

// TestDecodeRepeatedGroup covers the repeated-group class: S1F3 is L[n]{ <svids>... }, so any
// element count decodes, including none, but a non-list body does not.
func TestDecodeRepeatedGroup(t *testing.T) {
	for _, n := range []int{0, 1, 3} {
		values := make([]secs2.Item, n)
		for i := range values {
			values[i] = secs2.U4(uint32(i))
		}

		got, err := gem.DecodeS1F3(secs2.L(values...))
		if err != nil {
			t.Fatalf("DecodeS1F3(%d elements): %v", n, err)
		}

		if len(got.SVIDS) != n {
			t.Errorf("SVIDS: got %d elements, want %d", len(got.SVIDS), n)
		}
	}

	_, err := gem.DecodeS1F3(secs2.A("svid"))
	requireDecodeError(t, err, "gem: SVIDS: got ascii, want list")

	_, err = gem.DecodeS1F3(secs2.NewEmptyItem())
	requireDecodeError(t, err, "gem: SVIDS: got empty, want list")
}

// TestDecodePackedGroup covers the packed class: S1F10's two groups are single binary items
// holding every port's status, not lists of separate items.
func TestDecodePackedGroup(t *testing.T) {
	got, err := gem.DecodeS1F10(secs2.L(secs2.B(1, 2, 3), secs2.B()))
	if err != nil {
		t.Fatalf("DecodeS1F10: %v", err)
	}

	if len(got.TSIPS) != 3 || len(got.TSOPS) != 0 {
		t.Errorf("got TSIPS %v, TSOPS %v, want 3 and 0 values", got.TSIPS, got.TSOPS)
	}

	// A list of separate items is the shape a repeated group has, not a packed one.
	_, err = gem.DecodeS1F10(secs2.L(secs2.L(secs2.B(1)), secs2.B()))
	requireDecodeError(t, err, "gem: TSIPS:")

	_, err = gem.DecodeS1F10(secs2.L(secs2.B(1), secs2.A("x")))
	requireDecodeError(t, err, "gem: TSOPS:")

	_, err = gem.DecodeS1F10(secs2.L(secs2.B(1)))
	requireDecodeError(t, err, "gem: S1F10 body: list length 1, want 2")
}

// TestDecodeEmptyListBody covers the zero-length-list class: S1F2Host's body carries no data, so
// the arity check is the entire contract.
func TestDecodeEmptyListBody(t *testing.T) {
	if _, err := gem.DecodeS1F2Host(secs2.L()); err != nil {
		t.Fatalf("DecodeS1F2Host: %v", err)
	}

	_, err := gem.DecodeS1F2Host(secs2.L(secs2.A("m")))
	requireDecodeError(t, err, "gem: S1F2 body: list length 1, want 0")

	_, err = gem.DecodeS1F2Host(secs2.NewEmptyItem())
	requireDecodeError(t, err, "gem: S1F2 body: got empty, want list")
}

// TestDecodeOptionalGroup covers the optional-group class: SEMI E5 lets S5F14's error group be
// omitted when the recovery request was accepted, so both the empty and the full form decode and
// the omitted fields keep their zero value.
// A partially filled group names no field and is rejected.
func TestDecodeOptionalGroup(t *testing.T) {
	short := secs2.L(secs2.A("EX-1"), secs2.L(secs2.BOOLEAN(true), secs2.L()))

	got, err := gem.DecodeS5F14(short)
	if err != nil {
		t.Fatalf("DecodeS5F14(short form): %v", err)
	}

	if got.EXID != "EX-1" || !got.ACKA {
		t.Errorf("got %+v, want EXID EX-1 and ACKA true", got)
	}

	if got.ERRCODE != nil || got.ERRTEXT != "" {
		t.Errorf("omitted fields: got ERRCODE %v, ERRTEXT %q, want the zero values", got.ERRCODE, got.ERRTEXT)
	}

	full := secs2.L(secs2.A("EX-1"), secs2.L(secs2.BOOLEAN(false), secs2.L(secs2.U1(7), secs2.A("bad"))))

	got, err = gem.DecodeS5F14(full)
	if err != nil {
		t.Fatalf("DecodeS5F14(full form): %v", err)
	}

	if got.ERRTEXT != "bad" || got.ERRCODE == nil {
		t.Errorf("got %+v, want ERRTEXT bad and a non-nil ERRCODE", got)
	}

	partial := secs2.L(secs2.A("EX-1"), secs2.L(secs2.BOOLEAN(true), secs2.L(secs2.U1(7))))

	_, err = gem.DecodeS5F14(partial)
	requireDecodeError(t, err, "gem: S5F14 body[1][1]: list length 1, want 0 or 2")

	overfull := secs2.L(secs2.A("EX-1"), secs2.L(secs2.BOOLEAN(true), secs2.L(secs2.U1(7), secs2.A("bad"), secs2.A("x"))))

	_, err = gem.DecodeS5F14(overfull)
	requireDecodeError(t, err, "gem: S5F14 body[1][1]: list length 3, want 0 or 2")
}

// TestDecodeNumericWidthIsChecked covers the narrowing a numeric field performs: S6F25's LINKID is
// U4, so any unsigned width decodes but a value too wide for uint32 is rejected rather than
// truncated.
func TestDecodeNumericWidthIsChecked(t *testing.T) {
	body := func(linkid secs2.Item) secs2.Item {
		return secs2.L(
			secs2.U4(1), secs2.U4(2), linkid, secs2.A("spec"), secs2.U1(0),
			secs2.L(), secs2.L(secs2.U1(0), secs2.L()),
		)
	}

	got, err := gem.DecodeS6F25(body(secs2.U8(uint64(1) << 31)))
	if err != nil {
		t.Fatalf("DecodeS6F25: %v", err)
	}

	if got.LINKID != 1<<31 {
		t.Errorf("LINKID: got %d, want %d", got.LINKID, uint32(1)<<31)
	}

	_, err = gem.DecodeS6F25(body(secs2.U8(uint64(1) << 32)))
	requireDecodeError(t, err, "gem: LINKID: value 4294967296 overflows uint32")

	_, err = gem.DecodeS6F25(body(secs2.B(1)))
	requireDecodeError(t, err, "gem: LINKID:")
}

// TestDecodeFixedWidthArray covers the fixed-width binary class: S9F1's MHEAD is exactly the ten
// bytes of a SECS message header, so a shorter or longer item is rejected before the conversion.
func TestDecodeFixedWidthArray(t *testing.T) {
	header := make([]any, 10)
	for i := range header {
		header[i] = byte(i)
	}

	got, err := gem.DecodeS9F1(secs2.B(header...))
	if err != nil {
		t.Fatalf("DecodeS9F1: %v", err)
	}

	if got.MHEAD[9] != 9 {
		t.Errorf("MHEAD: got %v, want the ten header bytes", got.MHEAD)
	}

	_, err = gem.DecodeS9F1(secs2.B(header[:9]...))
	requireDecodeError(t, err, "gem: MHEAD: size 9, want 10")

	_, err = gem.DecodeS9F1(secs2.A("0123456789"))
	requireDecodeError(t, err, "gem: MHEAD:")
}
