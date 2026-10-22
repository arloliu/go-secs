package gem_test

import (
	"bytes"
	"testing"

	"github.com/arloliu/go-secs/v2/gem"
	"github.com/arloliu/go-secs/v2/secs2"
)

// requireItemAt checks that the item at path in the body of msg is want, compared by wire bytes.
func requireItemAt(t *testing.T, msg secs2.SECS2Message, want secs2.Item, path ...int) {
	t.Helper()

	got, err := msg.Item().Get(path...)
	if err != nil {
		t.Fatalf("Get%v: %v", path, err)
	}

	if !bytes.Equal(got.ToBytes(), want.ToBytes()) {
		t.Errorf("item at %v: got %x, want %x", path, got.ToBytes(), want.ToBytes())
	}
}

// s6f25 builds an S6F25 around linkid.
func s6f25(linkid *uint32) secs2.SECS2Message {
	return gem.S6F25(secs2.U4(1), secs2.A(""), linkid, "spec", secs2.U1(0), nil, 0)
}

// TestOptionalLINKID proves a nil linkid is sent as a zero-length U4 and decodes as nil,
// while a pointer to 0 is sent as the value 0 and decodes as a non-nil 0.
func TestOptionalLINKID(t *testing.T) {
	absent := s6f25(nil)
	requireItemAt(t, absent, secs2.U4(), 2)

	got, err := gem.DecodeS6F25(wire(t, absent.Item()))
	if err != nil {
		t.Fatalf("DecodeS6F25: %v", err)
	}

	if got.LINKID != nil {
		t.Errorf("LINKID: got %d, want nil", *got.LINKID)
	}

	zero := s6f25(new(uint32(0)))
	requireItemAt(t, zero, secs2.U4(0), 2)

	got, err = gem.DecodeS6F25(wire(t, zero.Item()))
	if err != nil {
		t.Fatalf("DecodeS6F25: %v", err)
	}

	if got.LINKID == nil || *got.LINKID != 0 {
		t.Errorf("LINKID: got %v, want a pointer to 0", got.LINKID)
	}
}

// s17f5 builds an S17F5 with its full trace list around trautod and rptoc.
func s17f5(trautod, rptoc *bool) secs2.SECS2Message {
	return gem.S17F5(secs2.U4(1), secs2.U4(2), true, nil, secs2.A("000001"),
		secs2.U4(10), secs2.U4(1), "", secs2.U4(100), "", secs2.U4(101), trautod, rptoc)
}

// TestOptionalTRAUTOD proves a nil trautod is sent as a zero-length BOOLEAN and decodes as nil,
// while a pointer to false is sent as false and decodes as a non-nil false.
func TestOptionalTRAUTOD(t *testing.T) {
	msg := s17f5(nil, new(false))
	requireItemAt(t, msg, secs2.BOOLEAN(), 5, 6)
	requireItemAt(t, msg, secs2.BOOLEAN(false), 5, 7)

	got, err := gem.DecodeS17F5(wire(t, msg.Item()))
	if err != nil {
		t.Fatalf("DecodeS17F5: %v", err)
	}

	if got.TRAUTOD != nil {
		t.Errorf("TRAUTOD: got %v, want nil", *got.TRAUTOD)
	}

	if got.RPTOC == nil || *got.RPTOC {
		t.Errorf("RPTOC: got %v, want a pointer to false", got.RPTOC)
	}

	msg = s17f5(new(false), nil)
	requireItemAt(t, msg, secs2.BOOLEAN(false), 5, 6)

	got, err = gem.DecodeS17F5(wire(t, msg.Item()))
	if err != nil {
		t.Fatalf("DecodeS17F5: %v", err)
	}

	if got.TRAUTOD == nil || *got.TRAUTOD {
		t.Errorf("TRAUTOD: got %v, want a pointer to false", got.TRAUTOD)
	}

	if got.RPTOC != nil {
		t.Errorf("RPTOC: got %v, want nil", *got.RPTOC)
	}
}

// TestOptionalFNLOCOfAnotherWidth proves S12F4's FNLOC, a U2, decodes as nil from a zero-length U4,
// since the decoder accepts any unsigned width there.
func TestOptionalFNLOCOfAnotherWidth(t *testing.T) {
	msg := gem.S12F4(secs2.A("wafer"), gem.IDTYP(0), new(uint16(180)), nil, 0, nil, "",
		secs2.U4(1), secs2.U4(1), secs2.U4(1), secs2.U4(1), secs2.U4(1), secs2.U1(0), secs2.U1(0), secs2.U1(0))
	requireItemAt(t, msg, secs2.U2(180), 2)
	requireItemAt(t, msg, secs2.B(), 3)

	list, err := msg.Item().ToList()
	if err != nil {
		t.Fatalf("ToList: %v", err)
	}

	list[2] = secs2.U4()

	got, err := gem.DecodeS12F4(wire(t, secs2.L(list...)))
	if err != nil {
		t.Fatalf("DecodeS12F4: %v", err)
	}

	if got.FNLOC != nil || got.ORLOC != nil {
		t.Errorf("FNLOC, ORLOC: got %v, %v, want both nil", got.FNLOC, got.ORLOC)
	}
}

// TestOptionalAlarmCode proves an Alarm with a nil ALCD is sent with a zero-length B item,
// which E5 lets S5F6 return when the code does not exist, and decodes as nil,
// while a code of 0 decodes as a non-nil 0.
func TestOptionalAlarmCode(t *testing.T) {
	msg := gem.S5F6(gem.Alarm{ALID: secs2.U4(1), ALTX: "unknown"}, gem.Alarm{ALCD: new(gem.ALCD(0)), ALID: secs2.U4(2), ALTX: "cleared"})
	requireItemAt(t, msg, secs2.B(), 0, 0)
	requireItemAt(t, msg, secs2.B(0), 1, 0)

	got, err := gem.DecodeS5F6(wire(t, msg.Item()))
	if err != nil {
		t.Fatalf("DecodeS5F6: %v", err)
	}

	if got.ALARMS[0].ALCD != nil {
		t.Errorf("ALARMS[0].ALCD: got %#x, want nil", byte(*got.ALARMS[0].ALCD))
	}

	if got.ALARMS[1].ALCD == nil || *got.ALARMS[1].ALCD != 0 {
		t.Errorf("ALARMS[1].ALCD: got %v, want a pointer to 0", got.ALARMS[1].ALCD)
	}
}

// TestOptionalVerifyStatus proves a PDESendResult with a nil VERIFYRSPSTAT,
// which E5 has S19F13 send when the service provider is not an equipment,
// is sent as a zero-length U1 and decodes as nil.
func TestOptionalVerifyStatus(t *testing.T) {
	msg := gem.S19F13("tc", 0, gem.PDESendResult{UID: "uid", SENDRSPSTAT: 0, STATUSTXT: ""})
	requireItemAt(t, msg, secs2.U1(), 2, 0, 2)

	got, err := gem.DecodeS19F13(wire(t, msg.Item()))
	if err != nil {
		t.Fatalf("DecodeS19F13: %v", err)
	}

	if got.RESULTS[0].VERIFYRSPSTAT != nil {
		t.Errorf("RESULTS[0].VERIFYRSPSTAT: got %d, want nil", *got.RESULTS[0].VERIFYRSPSTAT)
	}
}
