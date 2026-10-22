package gem_test

import (
	"bytes"
	"testing"

	"github.com/arloliu/go-secs/v2/gem"
	"github.com/arloliu/go-secs/v2/secs2"
)

// The tests here pin how repeated list elements decode into groups:
// each element's shape is checked at full depth, with an error naming its path from the body,
// and a list SEMI E5 lets be sent as L,0 decodes to nil from exactly that form.
// The generated round trips only ever send a full group,
// so they never reach the omitted or the malformed forms.

// wire round-trips item through its wire bytes, as a peer's body would arrive.
func wire(t *testing.T, item secs2.Item) secs2.Item {
	t.Helper()

	decoded, err := secs2.Decode(item.ToBytes())
	if err != nil {
		t.Fatalf("secs2.Decode: %v", err)
	}

	return decoded
}

// requireSameItem asserts that got and want encode to the same wire bytes.
func requireSameItem(t *testing.T, got, want secs2.Item) {
	t.Helper()

	if !bytes.Equal(got.ToBytes(), want.ToBytes()) {
		t.Errorf("got %x, want %x", got.ToBytes(), want.ToBytes())
	}
}

func TestDecodeGroupElementShapeErrors(t *testing.T) {
	tests := []struct {
		name   string
		decode func(secs2.Item) error
		item   secs2.Item
		want   string
	}{
		{
			"report definition not a list",
			func(i secs2.Item) error { _, err := gem.DecodeS2F33(i); return err },
			secs2.L(secs2.U4(1), secs2.L(secs2.A("junk"))),
			"gem: REPORTS[0]:",
		},
		{
			"command parameter not a list",
			func(i secs2.Item) error { _, err := gem.DecodeS2F41(i); return err },
			secs2.L(secs2.A("START"), secs2.L(secs2.U4(7))),
			"gem: PARAMS[0]:",
		},
		{
			"report definition too short",
			func(i secs2.Item) error { _, err := gem.DecodeS2F33(i); return err },
			secs2.L(secs2.U4(1), secs2.L(secs2.L(secs2.U4(1), secs2.L()), secs2.L(secs2.U4(2)))),
			"gem: REPORTS[1]: list length 1, want 2",
		},
		{
			"variable IDs not a list",
			func(i secs2.Item) error { _, err := gem.DecodeS2F33(i); return err },
			secs2.L(secs2.U4(1), secs2.L(secs2.L(secs2.U4(1), secs2.U4(2)))),
			"gem: REPORTS[0].VIDS:",
		},
		{
			"limit three levels down",
			func(i secs2.Item) error { _, err := gem.DecodeS2F45(i); return err },
			secs2.L(secs2.U4(1), secs2.L(secs2.L(secs2.U4(10), secs2.L(secs2.A("junk"))))),
			"gem: VARIABLES[0].LIMITS[0]:",
		},
		{
			"limit ID of the wrong format",
			func(i secs2.Item) error { _, err := gem.DecodeS2F45(i); return err },
			secs2.L(secs2.U4(1), secs2.L(secs2.L(secs2.U4(10), secs2.L(secs2.L(secs2.U1(1), secs2.L()))))),
			"gem: VARIABLES[0].LIMITS[0].LIMITID:",
		},
		{
			"report of an event report",
			func(i secs2.Item) error { _, err := gem.DecodeS6F11(i); return err },
			secs2.L(secs2.U4(1), secs2.U4(2), secs2.L(secs2.L(secs2.U4(3)))),
			"gem: REPORTS[0]: list length 1, want 2",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			requireDecodeError(t, tt.decode(wire(t, tt.item)), tt.want)
		})
	}
}

// TestS6F11Reports replaces the Report helper's test:
// a report built as a struct goes out as L[2]{ <rptid> L[n]{ <v>... } }
// and decodes back to the same reports.
func TestS6F11Reports(t *testing.T) {
	reports := []gem.Report{
		{RPTID: secs2.U4(7), VALUES: []secs2.Item{secs2.A("alpha"), secs2.A("beta")}},
		{RPTID: secs2.U4(99)},
	}

	msg := gem.S6F11(secs2.U4(1), secs2.U4(200), reports...)

	requireSameItem(t, msg.Item(), secs2.L(secs2.U4(1), secs2.U4(200), secs2.L(
		secs2.L(secs2.U4(7), secs2.L(secs2.A("alpha"), secs2.A("beta"))),
		secs2.L(secs2.U4(99), secs2.L()),
	)))

	got, err := gem.DecodeS6F11(wire(t, msg.Item()))
	if err != nil {
		t.Fatalf("DecodeS6F11: %v", err)
	}

	assertDecoded(t, got, gem.S6F11Body{DATAID: secs2.U4(1), CEID: secs2.U4(200), REPORTS: reports})
}

func TestS15F24RecipeDescriptors(t *testing.T) {
	desc := secs2.L(secs2.A("BodyDesc"), secs2.A("2026102212000000"), secs2.U4(4096))
	body := func(element secs2.Item) secs2.Item {
		return secs2.L(secs2.L(secs2.L(element)), secs2.L(secs2.U1(0), secs2.L()))
	}

	t.Run("L[0] is nil", func(t *testing.T) {
		got, err := gem.DecodeS15F24(wire(t, body(secs2.L())))
		if err != nil {
			t.Fatalf("DecodeS15F24: %v", err)
		}

		assertDecoded(t, got.RECIPES, [][]*gem.RecipeDescriptor{{nil}})
	})

	t.Run("L[3] is a value", func(t *testing.T) {
		got, err := gem.DecodeS15F24(wire(t, body(desc)))
		if err != nil {
			t.Fatalf("DecodeS15F24: %v", err)
		}

		assertDecoded(t, got.RECIPES, [][]*gem.RecipeDescriptor{{
			{RCPDESCNM: gem.RCPDESCNMBodyDesc, RCPDESCTIME: "2026102212000000", RCPDESCLTH: secs2.U4(4096)},
		}})
	})

	for _, element := range []secs2.Item{
		secs2.L(secs2.A("BodyDesc")),
		secs2.L(secs2.A("BodyDesc"), secs2.A("2026102212000000")),
		secs2.L(secs2.A("BodyDesc"), secs2.A("2026102212000000"), secs2.U4(4096), secs2.U4(1)),
	} {
		_, err := gem.DecodeS15F24(wire(t, body(element)))
		requireDecodeError(t, err, "gem: RECIPES[0][0]: list length")
	}

	t.Run("builder sends nil as L[0]", func(t *testing.T) {
		msg := gem.S15F24([][]*gem.RecipeDescriptor{{nil}}, gem.RMACKSuccessfulCompletion)
		requireSameItem(t, msg.Item(), body(secs2.L()))
	})
}

func TestS2F45Deadband(t *testing.T) {
	body := func(deadband secs2.Item) secs2.Item {
		return secs2.L(secs2.U4(1), secs2.L(secs2.L(secs2.U4(10), secs2.L(secs2.L(secs2.B(1), deadband)))))
	}

	got, err := gem.DecodeS2F45(wire(t, body(secs2.L())))
	if err != nil {
		t.Fatalf("DecodeS2F45 with L[0]: %v", err)
	}

	assertDecoded(t, got.VARIABLES, []gem.VariableLimits{{VID: secs2.U4(10), LIMITS: []gem.Limit{{LIMITID: 1}}}})

	got, err = gem.DecodeS2F45(wire(t, body(secs2.L(secs2.U4(80), secs2.U4(20)))))
	if err != nil {
		t.Fatalf("DecodeS2F45 with L[2]: %v", err)
	}

	assertDecoded(t, got.VARIABLES[0].LIMITS[0].DEADBAND, &gem.Deadband{UPPERDB: secs2.U4(80), LOWERDB: secs2.U4(20)})

	for _, deadband := range []secs2.Item{secs2.L(secs2.U4(80)), secs2.L(secs2.U4(80), secs2.U4(20), secs2.U4(0))} {
		_, err := gem.DecodeS2F45(wire(t, body(deadband)))
		requireDecodeError(t, err, "gem: VARIABLES[0].LIMITS[0].DEADBAND: list length")
	}

	msg := gem.S2F45(secs2.U4(1), gem.VariableLimits{VID: secs2.U4(10), LIMITS: []gem.Limit{{LIMITID: 1}}})
	requireSameItem(t, msg.Item(), body(secs2.L()))
}

func TestS2F46LimitAck(t *testing.T) {
	body := func(limit secs2.Item) secs2.Item {
		return secs2.L(secs2.B(1), secs2.L(secs2.L(secs2.U4(10), secs2.B(2), limit)))
	}

	got, err := gem.DecodeS2F46(wire(t, body(secs2.L())))
	if err != nil {
		t.Fatalf("DecodeS2F46 with L[0]: %v", err)
	}

	assertDecoded(t, got.VARIABLES, []gem.VariableLimitAck{{VID: secs2.U4(10), LVACK: 2}})

	got, err = gem.DecodeS2F46(wire(t, body(secs2.L(secs2.B(3), secs2.B(1)))))
	if err != nil {
		t.Fatalf("DecodeS2F46 with L[2]: %v", err)
	}

	assertDecoded(t, got.VARIABLES[0].LIMIT, &gem.LimitAck{LIMITID: 3, LIMITACK: gem.LIMITACKLIMITIDDoesNotExist})

	for _, limit := range []secs2.Item{secs2.L(secs2.B(3)), secs2.L(secs2.B(3), secs2.B(1), secs2.B(0))} {
		_, err := gem.DecodeS2F46(wire(t, body(limit)))
		requireDecodeError(t, err, "gem: VARIABLES[0].LIMIT: list length")
	}

	msg := gem.S2F46(1, gem.VariableLimitAck{VID: secs2.U4(10), LVACK: 2})
	requireSameItem(t, msg.Item(), body(secs2.L()))
}

func TestS2F48LimitAttributes(t *testing.T) {
	body := func(attributes secs2.Item) secs2.Item {
		return secs2.L(secs2.L(secs2.U4(10), attributes))
	}

	limits := secs2.L(secs2.L(secs2.B(1), secs2.U4(80), secs2.U4(20)))

	got, err := gem.DecodeS2F48(wire(t, body(secs2.L())))
	if err != nil {
		t.Fatalf("DecodeS2F48 with L[0]: %v", err)
	}

	assertDecoded(t, got.VARIABLES, []gem.VariableLimitAttributes{{VID: secs2.U4(10)}})

	got, err = gem.DecodeS2F48(wire(t, body(secs2.L(secs2.A("C"), secs2.U4(0), secs2.U4(100), limits))))
	if err != nil {
		t.Fatalf("DecodeS2F48 with L[4]: %v", err)
	}

	assertDecoded(t, got.VARIABLES[0].ATTRIBUTES, &gem.LimitAttributes{
		UNITS: "C", LIMITMIN: secs2.U4(0), LIMITMAX: secs2.U4(100),
		LIMITS: []gem.LimitValues{{LIMITID: 1, UPPERDB: secs2.U4(80), LOWERDB: secs2.U4(20)}},
	})

	for _, attributes := range []secs2.Item{
		secs2.L(secs2.A("C")),
		secs2.L(secs2.A("C"), secs2.U4(0)),
		secs2.L(secs2.A("C"), secs2.U4(0), secs2.U4(100)),
		secs2.L(secs2.A("C"), secs2.U4(0), secs2.U4(100), limits, secs2.U4(1)),
	} {
		_, err := gem.DecodeS2F48(wire(t, body(attributes)))
		requireDecodeError(t, err, "gem: VARIABLES[0].ATTRIBUTES: list length")
	}

	msg := gem.S2F48(gem.VariableLimitAttributes{VID: secs2.U4(10)})
	requireSameItem(t, msg.Item(), body(secs2.L()))
}
