package gem_test

import (
	"fmt"
	"reflect"
	"testing"

	"github.com/arloliu/go-secs/v2/gem"
	"github.com/arloliu/go-secs/v2/secs2"
)

// The tests here pin what AllowTrailing relaxes and what it leaves strict:
// a list of fixed length may hold more children than declared, in the body and in a group,
// and the extra ones are ignored;
// a list holding fewer, a partly filled list, an item of the wrong format,
// and a list where E5 excludes one are still errors.

var trailing = gem.AllowTrailing()

func TestAllowTrailingBodyList(t *testing.T) {
	alarm := secs2.L(secs2.B(0x86), secs2.U4(12), secs2.A("Door open"), secs2.A("equipment-defined"))

	_, err := gem.DecodeS5F1(wire(t, alarm))
	requireDecodeError(t, err, "gem: S5F1 body: list length 4, want 3")

	got, err := gem.DecodeS5F1(wire(t, alarm), trailing)
	if err != nil {
		t.Fatalf("DecodeS5F1 with AllowTrailing: %v", err)
	}

	assertDecoded(t, got, gem.S5F1Body{ALCD: 0x86, ALID: secs2.U4(12), ALTX: "Door open"})

	_, err = gem.DecodeS5F1(wire(t, secs2.L(secs2.B(0x86), secs2.U4(12))), trailing)
	requireDecodeError(t, err, "gem: S5F1 body: list length 2, want at least 3")

	// A nil option is skipped, leaving the decoder strict.
	_, err = gem.DecodeS5F1(wire(t, alarm), nil)
	requireDecodeError(t, err, "gem: S5F1 body: list length 4, want 3")
}

func TestAllowTrailingGroupElement(t *testing.T) {
	body := secs2.L(secs2.L(secs2.B(0x86), secs2.U4(12), secs2.A("Door open"), secs2.U4(0)))

	_, err := gem.DecodeS5F6(wire(t, body))
	requireDecodeError(t, err, "gem: ALARMS[0]: list length 4, want 3")

	got, err := gem.DecodeS5F6(wire(t, body), trailing)
	if err != nil {
		t.Fatalf("DecodeS5F6 with AllowTrailing: %v", err)
	}

	assertDecoded(t, got.ALARMS, []gem.Alarm{{ALCD: 0x86, ALID: secs2.U4(12), ALTX: "Door open"}})

	_, err = gem.DecodeS5F6(wire(t, secs2.L(secs2.L(secs2.B(0x86), secs2.U4(12)))), trailing)
	requireDecodeError(t, err, "gem: ALARMS[0]: list length 2, want at least 3")

	// A list inside a group element: a report's values follow its ID in a list of two.
	report := secs2.L(secs2.U4(1), secs2.U4(200), secs2.L(secs2.L(secs2.U4(7), secs2.L(secs2.A("v")), secs2.A("extra"))))

	event, err := gem.DecodeS6F11(wire(t, report), trailing)
	if err != nil {
		t.Fatalf("DecodeS6F11 with AllowTrailing: %v", err)
	}

	assertDecoded(t, event.REPORTS, []gem.Report{{RPTID: secs2.U4(7), VALUES: []secs2.Item{secs2.A("v")}}})
}

// TestAllowTrailingBoundedList covers a body list E5 lets be sent short or full:
// S5F14's error group, L[0] or L[2].
func TestAllowTrailingBoundedList(t *testing.T) {
	body := func(errors ...secs2.Item) secs2.Item {
		return secs2.L(secs2.A("EX-1"), secs2.L(secs2.BOOLEAN(false), secs2.L(errors...)))
	}

	overfull := body(secs2.U1(7), secs2.A("bad"), secs2.A("extra"))

	_, err := gem.DecodeS5F14(wire(t, overfull))
	requireDecodeError(t, err, "gem: S5F14 body[1][1]: list length 3, want 0 or 2")

	got, err := gem.DecodeS5F14(wire(t, overfull), trailing)
	if err != nil {
		t.Fatalf("DecodeS5F14 over-full with AllowTrailing: %v", err)
	}

	assertDecoded(t, got, gem.S5F14Reply{EXID: "EX-1", ERRCODE: secs2.U1(7), ERRTEXT: "bad"})

	short, err := gem.DecodeS5F14(wire(t, body()), trailing)
	if err != nil {
		t.Fatalf("DecodeS5F14 short form with AllowTrailing: %v", err)
	}

	assertDecoded(t, short, gem.S5F14Reply{EXID: "EX-1"})

	_, err = gem.DecodeS5F14(wire(t, body(secs2.U1(7))), trailing)
	requireDecodeError(t, err, "gem: S5F14 body[1][1]: list length 1, want 0 or 2")
}

// TestAllowTrailingOptionalGroups sends each list E5 lets be sent as L[0] inside an element
// at every length from L[0] to one child more than its full form, in both modes:
// L[0] is nil and the full form a value in both,
// every length in between is an error in both,
// and the over-full form is an error when strict
// and, with AllowTrailing, a value equal to the full form's.
func TestAllowTrailingOptionalGroups(t *testing.T) {
	limits := secs2.L(secs2.L(secs2.B(1), secs2.U4(80), secs2.U4(20)))

	tests := []struct {
		name     string
		children []secs2.Item // the full form's children
		body     func(group secs2.Item) secs2.Item
		decode   func(item secs2.Item, opts ...gem.DecodeOption) (any, error) // the group's field, a typed pointer
		field    string
	}{
		{
			name:     "recipe descriptor",
			children: []secs2.Item{secs2.A("BodyDesc"), secs2.A("2026102212000000"), secs2.U4(4096)},
			body: func(g secs2.Item) secs2.Item {
				return secs2.L(secs2.L(secs2.L(g)), secs2.L(secs2.U1(0), secs2.L()))
			},
			decode: func(item secs2.Item, opts ...gem.DecodeOption) (any, error) {
				got, err := gem.DecodeS15F24(item, opts...)
				if err != nil {
					return nil, err
				}

				return got.RECIPES[0][0], nil
			},
			field: "gem: RECIPES[0][0]",
		},
		{
			name:     "deadband",
			children: []secs2.Item{secs2.U4(80), secs2.U4(20)},
			body: func(g secs2.Item) secs2.Item {
				return secs2.L(secs2.U4(1), secs2.L(secs2.L(secs2.U4(10), secs2.L(secs2.L(secs2.B(1), g)))))
			},
			decode: func(item secs2.Item, opts ...gem.DecodeOption) (any, error) {
				got, err := gem.DecodeS2F45(item, opts...)
				if err != nil {
					return nil, err
				}

				return got.VARIABLES[0].LIMITS[0].DEADBAND, nil
			},
			field: "gem: VARIABLES[0].LIMITS[0].DEADBAND",
		},
		{
			name:     "limit acknowledge",
			children: []secs2.Item{secs2.B(3), secs2.B(1)},
			body: func(g secs2.Item) secs2.Item {
				return secs2.L(secs2.B(1), secs2.L(secs2.L(secs2.U4(10), secs2.B(2), g)))
			},
			decode: func(item secs2.Item, opts ...gem.DecodeOption) (any, error) {
				got, err := gem.DecodeS2F46(item, opts...)
				if err != nil {
					return nil, err
				}

				return got.VARIABLES[0].LIMIT, nil
			},
			field: "gem: VARIABLES[0].LIMIT",
		},
		{
			name:     "limit attributes",
			children: []secs2.Item{secs2.A("C"), secs2.U4(0), secs2.U4(100), limits},
			body: func(g secs2.Item) secs2.Item {
				return secs2.L(secs2.L(secs2.U4(10), g))
			},
			decode: func(item secs2.Item, opts ...gem.DecodeOption) (any, error) {
				got, err := gem.DecodeS2F48(item, opts...)
				if err != nil {
					return nil, err
				}

				return got.VARIABLES[0].ATTRIBUTES, nil
			},
			field: "gem: VARIABLES[0].ATTRIBUTES",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			full := len(tt.children)
			send := func(n int) secs2.Item {
				children := append(append([]secs2.Item{}, tt.children...), secs2.A("extra"))

				return wire(t, tt.body(secs2.L(children[:n]...)))
			}

			want, err := tt.decode(send(full))
			if err != nil {
				t.Fatalf("full form: %v", err)
			}

			for _, mode := range []struct {
				name string
				opts []gem.DecodeOption
			}{{"strict", nil}, {"trailing", []gem.DecodeOption{trailing}}} {
				got, err := tt.decode(send(0), mode.opts...)
				if err != nil {
					t.Fatalf("%s, L[0]: %v", mode.name, err)
				}

				if !isNilPointer(got) {
					t.Errorf("%s, L[0]: got %v, want nil", mode.name, got)
				}

				for n := 1; n < full; n++ {
					_, err := tt.decode(send(n), mode.opts...)
					requireDecodeError(t, err, tt.field+": list length")
				}

				got, err = tt.decode(send(full), mode.opts...)
				if err != nil {
					t.Fatalf("%s, full form: %v", mode.name, err)
				}

				if isNilPointer(got) {
					t.Fatalf("%s, full form: got nil, want a value", mode.name)
				}

				assertDecoded(t, got, want)

				got, err = tt.decode(send(full+1), mode.opts...)
				if mode.opts == nil {
					requireDecodeError(t, err, fmt.Sprintf("%s: list length %d, want 0 or %d", tt.field, full+1, full))

					continue
				}

				if err != nil {
					t.Fatalf("%s, over-full form: %v", mode.name, err)
				}

				assertDecoded(t, got, want)
			}
		})
	}
}

// isNilPointer reports whether v, one of the typed group pointers the optional-group cases return, is nil.
func isNilPointer(v any) bool {
	rv := reflect.ValueOf(v)

	return rv.Kind() == reflect.Pointer && rv.IsNil()
}

// TestAllowTrailingKeepsItemChecks proves AllowTrailing relaxes list lengths only:
// a list where E5 excludes one, a list S2F14 allows only empty,
// and an item of the wrong format are still errors.
func TestAllowTrailingKeepsItemChecks(t *testing.T) {
	_, err := gem.DecodeS7F5(wire(t, secs2.L(secs2.A("X"), secs2.L())), trailing)
	requireDecodeError(t, err, "gem: PPID: got list, want a non-list item")

	_, err = gem.DecodeS2F33(wire(t, secs2.L(secs2.U4(1), secs2.L(secs2.L(secs2.U4(1), secs2.L(secs2.L()))))), trailing)
	requireDecodeError(t, err, "gem: REPORTS[0].VIDS[0]: got list")

	_, err = gem.DecodeS2F14(wire(t, secs2.L(secs2.U4(1), secs2.L(secs2.U4(1)))), trailing)
	requireDecodeError(t, err, "gem: ECVS[1]: got list length 1, want a non-list item or an empty list")

	if _, err := gem.DecodeS2F14(wire(t, secs2.L(secs2.U4(1), secs2.L())), trailing); err != nil {
		t.Errorf("DecodeS2F14 with an empty ECV and AllowTrailing: %v", err)
	}

	_, err = gem.DecodeS2F15(wire(t, secs2.L(secs2.L(secs2.U4(1), secs2.L(), secs2.A("extra")))), trailing)
	requireDecodeError(t, err, "gem: CONSTANTS[0].ECV: got list")

	_, err = gem.DecodeS5F1(wire(t, secs2.L(secs2.U1(0x86), secs2.U4(12), secs2.A("Door open"), secs2.A("extra"))), trailing)
	requireDecodeError(t, err, "gem: ALCD:")
}

// TestFuzzRegistryForwardsOptions proves a registered decoder passes its options on to the decoder it wraps,
// so the trailing mode of the fuzz run really decodes with AllowTrailing:
// an over-full body fails the strict mode and passes the trailing one.
func TestFuzzRegistryForwardsOptions(t *testing.T) {
	alarm := wire(t, secs2.L(secs2.B(0x86), secs2.U4(12), secs2.A("Door open"), secs2.A("equipment-defined")))

	for _, d := range fuzzDecoders {
		if d.name != "DecodeS5F1" {
			continue
		}

		failed := map[string]bool{}

		runDecoder(d, alarm, func(mode string, err error) {
			failed[mode] = err != nil
		})

		for _, mode := range []string{"strict", "trailing"} {
			if _, ok := failed[mode]; !ok {
				t.Fatalf("over-full S5F1 did not run in the %s mode; ran %v", mode, failed)
			}
		}

		if !failed["strict"] || failed["trailing"] {
			t.Errorf("over-full S5F1 failed per mode %v, want strict only", failed)
		}

		return
	}

	t.Fatal("DecodeS5F1 is not registered")
}
