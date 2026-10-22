package gem_test

import (
	"bytes"
	"strings"
	"testing"

	"github.com/arloliu/go-secs/v2/gem"
	"github.com/arloliu/go-secs/v2/secs2"
)

// The tests here pin where a decoder rejects a list in place of an open item:
// exactly where the item's E5 formats exclude L,
// at body level, inside a group, and as an element of a repeat.
// Only the list is rejected, never a scalar format E5 does not list,
// since peers send an ASCII ALID or CEID.
// The generated round trips send an ASCII sample for every open item,
// so they never reach a list.

func TestDecodeRejectsListWhereE5ExcludesIt(t *testing.T) {
	tests := []struct {
		name   string
		decode func(secs2.Item) error
		item   secs2.Item
		want   string
	}{
		{
			"process program ID as the whole body",
			func(i secs2.Item) error { _, err := gem.DecodeS7F5(i); return err },
			secs2.L(secs2.A("X"), secs2.L()),
			"gem: PPID: got list, want a non-list item",
		},
		{
			"alarm ID",
			func(i secs2.Item) error { _, err := gem.DecodeS5F3(i); return err },
			secs2.L(secs2.B(0x80), secs2.L(secs2.U4(1))),
			"gem: ALID: got list",
		},
		{
			"variable ID in a group's repeat",
			func(i secs2.Item) error { _, err := gem.DecodeS2F33(i); return err },
			secs2.L(secs2.U4(1), secs2.L(secs2.L(secs2.U4(1), secs2.L(secs2.L())))),
			"gem: REPORTS[0].VIDS[0]: got list",
		},
		{
			"report ID in an event link",
			func(i secs2.Item) error { _, err := gem.DecodeS2F35(i); return err },
			secs2.L(secs2.U4(1), secs2.L(secs2.L(secs2.U4(10), secs2.L(secs2.U4(1), secs2.L(secs2.U4(2)))))),
			"gem: EVENTS[0].RPTIDS[1]: got list",
		},
		{
			"event ID of an event link",
			func(i secs2.Item) error { _, err := gem.DecodeS2F35(i); return err },
			secs2.L(secs2.U4(1), secs2.L(secs2.L(secs2.L(), secs2.L()))),
			"gem: EVENTS[0].CEID: got list",
		},
		{
			"status variable ID in a repeat",
			func(i secs2.Item) error { _, err := gem.DecodeS1F3(i); return err },
			secs2.L(secs2.U4(1), secs2.L()),
			"gem: SVIDS[1]: got list",
		},
		{
			"equipment constant value with children",
			func(i secs2.Item) error { _, err := gem.DecodeS2F14(i); return err },
			secs2.L(secs2.U4(1), secs2.L(secs2.U4(1))),
			"gem: ECVS[1]: got list length 1, want a non-list item or an empty list",
		},
		{
			// Only S2F14's text gives L[0] a meaning; S2F15 sends the same item strictly.
			"empty equipment constant value of a new constant",
			func(i secs2.Item) error { _, err := gem.DecodeS2F15(i); return err },
			secs2.L(secs2.L(secs2.U4(1), secs2.L())),
			"gem: CONSTANTS[0].ECV: got list",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			requireDecodeError(t, tt.decode(wire(t, tt.item)), tt.want)
		})
	}
}

// TestDecodeAcceptsEmptyEquipmentConstantValue covers the one list S2F14 allows for a constant value:
// L[0], which SEMI E5 says means the constant does not exist.
func TestDecodeAcceptsEmptyEquipmentConstantValue(t *testing.T) {
	got, err := gem.DecodeS2F14(wire(t, secs2.L(secs2.U4(7), secs2.L(), secs2.A("on"))))
	if err != nil {
		t.Fatalf("DecodeS2F14: %v", err)
	}

	if len(got.ECVS) != 3 {
		t.Fatalf("ECVS: got %d values, want 3", len(got.ECVS))
	}

	requireSameItem(t, got.ECVS[0], secs2.U4(7))
	requireSameItem(t, got.ECVS[1], secs2.L())
	requireSameItem(t, got.ECVS[2], secs2.A("on"))
}

// TestDecodeAcceptsScalarFormatsE5DoesNotList proves the format list itself is not enforced:
// an ASCII ALID or CEID decodes and comes back as sent.
func TestDecodeAcceptsScalarFormatsE5DoesNotList(t *testing.T) {
	alarm, err := gem.DecodeS5F1(wire(t, gem.S5F1(0x86, secs2.A("CLEAN-A"), "Disk clean").Item()))
	if err != nil {
		t.Fatalf("DecodeS5F1: %v", err)
	}

	requireSameItem(t, alarm.ALID, secs2.A("CLEAN-A"))

	event, err := gem.DecodeS6F11(wire(t, gem.S6F11(secs2.U4(1), secs2.A("LOT-START")).Item()))
	if err != nil {
		t.Fatalf("DecodeS6F11: %v", err)
	}

	requireSameItem(t, event.CEID, secs2.A("LOT-START"))
}

// TestDecodeAcceptsListWhereE5AllowsIt covers the controls:
// a value whose formats include L, and an opaque position, still decode a list.
func TestDecodeAcceptsListWhereE5AllowsIt(t *testing.T) {
	value := secs2.L(secs2.U4(1), secs2.A("x"))

	event, err := gem.DecodeS6F11(wire(t, gem.S6F11(secs2.U4(1), secs2.U4(2), gem.Report{RPTID: secs2.U4(3), VALUES: []secs2.Item{value}}).Item()))
	if err != nil {
		t.Fatalf("DecodeS6F11 with a list V: %v", err)
	}

	requireSameItem(t, event.REPORTS[0].VALUES[0], value)

	attrs, err := gem.DecodeS14F3(wire(t, gem.S14F3("", secs2.A("Carrier"), nil, gem.Attribute{ATTRID: secs2.A("Slots"), ATTRDATA: value}).Item()))
	if err != nil {
		t.Fatalf("DecodeS14F3 with a list ATTRDATA: %v", err)
	}

	requireSameItem(t, attrs.ATTRS[0].ATTRDATA, value)

	command := gem.ProcessCommandDefinition{
		CCODE: secs2.U2(1), CNAME: "RUN", RQCMD: true, BLKDEF: secs2.I1(0),
		BCDS: secs2.U2(), IBCDS: secs2.U2(), NBCDS: secs2.U2(), ACDS: secs2.U2(), IACDS: secs2.U2(), NACDS: secs2.U2(),
		PARAMETERS: []secs2.Item{secs2.L(secs2.A("TEMP"), secs2.BOOLEAN(true), secs2.U4(0), secs2.U4(100))},
	}

	capabilities, err := gem.DecodeS7F22(wire(t, gem.S7F22("TOOL", "1.0", secs2.U4(1), secs2.U4(1024), command).Item()))
	if err != nil {
		t.Fatalf("DecodeS7F22 with a list parameter specification: %v", err)
	}

	requireSameItem(t, capabilities.COMMANDS[0].PARAMETERS[0], command.PARAMETERS[0])
}

// TestDecodePackedFieldIsOneItem proves a packed field is still read as the one binary item it is on the wire,
// not as an open item that the list check applies to:
// its values come back as bytes, and a list in its place fails the binary read.
func TestDecodePackedFieldIsOneItem(t *testing.T) {
	got, err := gem.DecodeS1F10(wire(t, gem.S1F10([]byte{1, 2, 3}).Item()))
	if err != nil {
		t.Fatalf("DecodeS1F10: %v", err)
	}

	if !bytes.Equal(got.TSIPS, []byte{1, 2, 3}) || len(got.TSOPS) != 0 {
		t.Errorf("got TSIPS %v, TSOPS %v, want [1 2 3] and none", got.TSIPS, got.TSOPS)
	}

	_, err = gem.DecodeS1F10(wire(t, secs2.L(secs2.L(secs2.B(1)), secs2.B())))
	requireDecodeError(t, err, "gem: TSIPS:")

	if strings.Contains(err.Error(), "want a non-list item") {
		t.Errorf("error %q comes from the list check, want the binary read's", err.Error())
	}
}
