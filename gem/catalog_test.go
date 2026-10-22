package gem_test

import (
	"bytes"
	"fmt"
	"slices"
	"testing"

	"github.com/arloliu/go-secs/v2/gem"
	"github.com/arloliu/go-secs/v2/secs2"
)

// Who may send a reply, by the reply's direction and the form that carries the acceptance.
var (
	hostReplies      = []gem.Role{gem.RoleHost}
	equipmentReplies = []gem.Role{gem.RoleEquipment}
	eitherReplies    = []gem.Role{gem.RoleHost, gem.RoleEquipment}
)

// acceptance is one reply AcceptReply must build:
// the reply to the primary stream, function, sent by each of repliers, with the body want.
//
// The table is written by hand from SEMI E5, not derived from the generator's data:
// each row is a reply whose acknowledge code has a value meaning acceptance
// and whose lists, if any, E5 sends empty only when nothing went wrong.
type acceptance struct {
	stream   byte
	function byte
	repliers []gem.Role
	want     secs2.Item
}

// ackB is a reply that is one binary acknowledge code, sent as 0.
var ackB = secs2.B(0)

// ackBWithErrors is a binary acknowledge code, sent as 0, followed by an empty list of what went wrong.
var ackBWithErrors = secs2.L(secs2.B(0), secs2.L())

// ackU1WithErrors is a U1 acknowledge code, sent as 0, followed by an empty error list.
var ackU1WithErrors = secs2.L(secs2.U1(0), secs2.L())

// acceptances is every reply AcceptReply builds.
func acceptances() []acceptance {
	return []acceptance{
		// Stream 1: the host's S1F2 is an empty list, and its S1F14 carries an empty list in place of MDLN and SOFTREV.
		{1, 1, hostReplies, secs2.L()},
		{1, 13, hostReplies, secs2.L(secs2.B(0), secs2.L())},
		{1, 15, equipmentReplies, ackB},
		{1, 17, equipmentReplies, ackB},

		// Stream 2.
		{2, 1, eitherReplies, ackB},
		{2, 3, eitherReplies, ackB},
		{2, 7, equipmentReplies, ackB},
		{2, 15, equipmentReplies, ackB},
		{2, 23, equipmentReplies, ackB},
		{2, 31, equipmentReplies, ackB},
		{2, 33, equipmentReplies, ackB},
		{2, 35, equipmentReplies, ackB},
		{2, 37, equipmentReplies, ackB},
		{2, 39, equipmentReplies, ackB},
		{2, 41, equipmentReplies, ackBWithErrors}, // no invalid parameters
		{2, 43, equipmentReplies, ackBWithErrors}, // no streams in error
		{2, 45, equipmentReplies, ackBWithErrors}, // no invalid variable limit attributes

		// Stream 3.
		{3, 5, hostReplies, ackB},
		{3, 7, hostReplies, ackB},
		{3, 9, hostReplies, ackB},
		{3, 13, equipmentReplies, ackB},
		{3, 15, equipmentReplies, ackB},
		{3, 17, equipmentReplies, ackU1WithErrors},
		{3, 19, equipmentReplies, ackU1WithErrors},
		{3, 21, equipmentReplies, ackU1WithErrors},
		{3, 23, equipmentReplies, ackU1WithErrors},
		{3, 25, equipmentReplies, ackU1WithErrors},
		{3, 27, equipmentReplies, ackU1WithErrors},
		{3, 31, equipmentReplies, ackU1WithErrors},
		{3, 33, equipmentReplies, ackU1WithErrors},

		// Stream 4.
		{4, 1, eitherReplies, ackB},
		{4, 17, eitherReplies, ackB},
		{4, 25, equipmentReplies, ackB},

		// Stream 5.
		{5, 1, hostReplies, ackB},
		{5, 3, equipmentReplies, ackB},

		// Stream 6.
		{6, 1, hostReplies, ackB},
		{6, 3, hostReplies, ackB},
		{6, 5, hostReplies, ackB},
		{6, 9, hostReplies, ackB},
		{6, 11, hostReplies, ackB},
		{6, 13, hostReplies, ackB},
		{6, 23, equipmentReplies, ackB},
		{6, 25, eitherReplies, ackB},

		// Stream 7.
		{7, 1, eitherReplies, ackB},
		{7, 3, eitherReplies, ackB},
		{7, 11, equipmentReplies, ackB},
		{7, 13, equipmentReplies, ackB},
		{7, 15, equipmentReplies, ackB},
		{7, 17, equipmentReplies, ackB},
		{7, 23, eitherReplies, ackB},
		{7, 29, hostReplies, ackB},
		{7, 31, equipmentReplies, ackB},
		{7, 37, eitherReplies, ackB},
		{7, 39, eitherReplies, ackB},
		{7, 41, eitherReplies, ackB},
		{7, 43, eitherReplies, ackB},

		// Stream 10.
		{10, 1, hostReplies, ackB},
		{10, 3, equipmentReplies, ackB},
		{10, 5, equipmentReplies, ackB},
		{10, 9, equipmentReplies, ackB},

		// Stream 12.
		{12, 1, hostReplies, ackB},
		{12, 5, hostReplies, ackB},
		{12, 7, hostReplies, ackB},
		{12, 9, hostReplies, ackB},
		{12, 11, hostReplies, ackB},

		// Stream 13.
		{13, 11, eitherReplies, ackB},
		{13, 13, eitherReplies, ackU1WithErrors},

		// Stream 14.
		{14, 21, eitherReplies, ackB},
		{14, 23, eitherReplies, ackB},

		// Stream 15.
		{15, 1, eitherReplies, ackB},
		{15, 3, eitherReplies, ackU1WithErrors},
		{15, 5, eitherReplies, ackU1WithErrors},
		{15, 13, eitherReplies, ackU1WithErrors},
		{15, 19, eitherReplies, ackU1WithErrors},
		{15, 33, equipmentReplies, ackU1WithErrors},
		{15, 35, equipmentReplies, ackU1WithErrors},
		{15, 37, eitherReplies, ackU1WithErrors},
		{15, 39, eitherReplies, ackU1WithErrors},
		{15, 41, eitherReplies, ackU1WithErrors},
		{15, 47, eitherReplies, ackU1WithErrors},
		{15, 49, equipmentReplies, ackB},
		{15, 51, equipmentReplies, ackB},

		// Streams 16 and 19.
		{16, 1, equipmentReplies, ackB},
		{19, 19, eitherReplies, ackB},
	}
}

func TestAcceptReplyBuildsEveryAcceptance(t *testing.T) {
	for _, tt := range acceptances() {
		for _, replier := range []gem.Role{gem.RoleHost, gem.RoleEquipment} {
			msg, ok := gem.AcceptReply(tt.stream, tt.function, replier)

			if !slices.Contains(tt.repliers, replier) {
				if ok {
					t.Errorf("AcceptReply(%d, %d, %d): got S%dF%d, want false", tt.stream, tt.function, replier, msg.StreamCode(), msg.FunctionCode())
				}

				continue
			}

			if !ok {
				t.Errorf("AcceptReply(%d, %d, %d): got false, want a reply", tt.stream, tt.function, replier)

				continue
			}

			if msg.StreamCode() != tt.stream || msg.FunctionCode() != tt.function+1 || msg.WaitBit() {
				t.Errorf("AcceptReply(%d, %d, %d): got S%dF%d W=%t, want S%dF%d without W",
					tt.stream, tt.function, replier, msg.StreamCode(), msg.FunctionCode(), msg.WaitBit(), tt.stream, tt.function+1)
			}

			if got, want := msg.Item().ToBytes(), tt.want.ToBytes(); !bytes.Equal(got, want) {
				t.Errorf("AcceptReply(%d, %d, %d): got body % x, want % x", tt.stream, tt.function, replier, got, want)
			}
		}
	}
}

// TestAcceptReplyNothingElse checks every stream and function, with both roles and some invalid ones,
// so a reply that the table above leaves out cannot be accepted unnoticed.
func TestAcceptReplyNothingElse(t *testing.T) {
	listed := map[[2]byte]bool{}
	for _, tt := range acceptances() {
		listed[[2]byte{tt.stream, tt.function}] = true
	}

	for stream := range 256 {
		for function := range 256 {
			if listed[[2]byte{byte(stream), byte(function)}] {
				continue
			}

			for _, role := range []gem.Role{0, gem.RoleHost, gem.RoleEquipment, 3, 255} {
				if msg, ok := gem.AcceptReply(byte(stream), byte(function), role); ok {
					t.Errorf("AcceptReply(%d, %d, %d): got S%dF%d, want false", stream, function, role, msg.StreamCode(), msg.FunctionCode())
				}
			}
		}
	}
}

func TestAcceptReplyRefusals(t *testing.T) {
	tests := []struct {
		name     string
		stream   byte
		function byte
		replier  gem.Role
	}{
		{"the equipment's S1F14 carries MDLN and SOFTREV", 1, 13, gem.RoleEquipment},
		{"the equipment's S1F2 carries MDLN and SOFTREV", 1, 1, gem.RoleEquipment},
		{"S6F11 is sent by the equipment, so the equipment does not reply to it", 6, 11, gem.RoleEquipment},
		{"S5F1 is sent by the equipment, so the equipment does not reply to it", 5, 1, gem.RoleEquipment},
		{"S2F41 is sent by the host, so the host does not reply to it", 2, 41, gem.RoleHost},
		{"an empty recipe descriptor list in S15F24 means no data", 15, 23, gem.RoleHost},
		{"an empty recipe descriptor list in S15F24 means no data, either way", 15, 23, gem.RoleEquipment},
		{"an empty object list in S14F2 means no data", 14, 1, gem.RoleHost},
		{"an empty object list in S14F2 means no data, either way", 14, 1, gem.RoleEquipment},
		{"an empty attribute list in S14F12 means no attributes returned", 14, 11, gem.RoleHost},
		{"an empty change list in S15F44 means no data", 15, 43, gem.RoleHost},
		{"E5 gives S3F36's empty error list no meaning", 3, 35, gem.RoleEquipment},
		{"S6F12 is a reply, not a primary", 6, 12, gem.RoleHost},
		{"S1F3 asks for data", 1, 3, gem.RoleEquipment},
		{"S9F1 has no reply", 9, 1, gem.RoleHost},
		{"E5 defines no stream 11", 11, 1, gem.RoleHost},
		{"the zero Role is not a role", 6, 11, gem.Role(0)},
		{"Role 3 is not a role", 6, 11, gem.Role(3)},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if msg, ok := gem.AcceptReply(tt.stream, tt.function, tt.replier); ok {
				t.Errorf("AcceptReply(%d, %d, %d): got S%dF%d, want false", tt.stream, tt.function, tt.replier, msg.StreamCode(), msg.FunctionCode())
			}
		})
	}
}

func TestLookupRejectsAnInvalidRole(t *testing.T) {
	for _, role := range []gem.Role{0, 3, 255} {
		if _, ok := gem.Lookup(1, 13, role); ok {
			t.Errorf("Lookup(1, 13, %d): got a form, want false", role)
		}
	}
}

func TestLookupGivesEachRoleItsOwnForm(t *testing.T) {
	host, ok := gem.Lookup(1, 13, gem.RoleHost)
	if !ok {
		t.Fatal("Lookup(1, 13, RoleHost): not found")
	}

	equipment, ok := gem.Lookup(1, 13, gem.RoleEquipment)
	if !ok {
		t.Fatal("Lookup(1, 13, RoleEquipment): not found")
	}

	for _, info := range []gem.MessageInfo{host, equipment} {
		if info.Stream != 1 || info.Function != 13 || info.Name != "Establish Communications Request" || info.Mnemonic != "CR" ||
			info.Direction != gem.Bidirectional || !info.ReplyExpected || info.ReplyFunction != 14 || !info.HasBody || !info.Verified {
			t.Errorf("Lookup(1, 13): got %+v", info)
		}
	}

	// The host sends an empty list, the equipment its MDLN and SOFTREV; each form decodes only its own.
	if _, err := host.Decode(gem.S1F13Host().Item()); err != nil {
		t.Errorf("host form: decoding the host's body: %v", err)
	}

	if _, err := host.Decode(gem.S1F13("mdln", "softrev").Item()); err == nil {
		t.Error("host form: decoding the equipment's body: got nil error")
	}

	got, err := equipment.Decode(gem.S1F13("mdln", "softrev").Item())
	if err != nil {
		t.Fatalf("equipment form: %v", err)
	}

	assertDecoded(t, got, gem.S1F13Body{MDLN: "mdln", SOFTREV: "softrev"})
}

func TestLookupIgnoresDirection(t *testing.T) {
	// S6F11 is equipment-to-host, yet Lookup returns it for the host too, for the caller to compare Direction.
	info, ok := gem.Lookup(6, 11, gem.RoleHost)
	if !ok {
		t.Fatal("Lookup(6, 11, RoleHost): not found")
	}

	if info.Direction != gem.EquipmentToHost || !info.ReplyExpected || info.ReplyFunction != 12 {
		t.Errorf("Lookup(6, 11, RoleHost): got %+v", info)
	}
}

func TestLookupFields(t *testing.T) {
	tests := []struct {
		name  string
		check func(gem.MessageInfo) bool
		s, f  byte
	}{
		{"a header-only form has no body and no decoder", func(i gem.MessageInfo) bool { return !i.HasBody && i.Decode == nil }, 1, 1},
		{"a form with a body has its decoder", func(i gem.MessageInfo) bool { return i.HasBody && i.Decode != nil }, 1, 3},
		{"a reply has no reply function", func(i gem.MessageInfo) bool { return i.ReplyFunction == 0 && !i.ReplyExpected }, 1, 4},
		{"a primary without a reply has no reply function", func(i gem.MessageInfo) bool { return i.ReplyFunction == 0 }, 9, 1},
		{"a message reconstructed from an external reference is not verified", func(i gem.MessageInfo) bool { return !i.Verified }, 1, 12},
		{"a message from E5 is verified", func(i gem.MessageInfo) bool { return i.Verified }, 1, 11},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			info, ok := gem.Lookup(tt.s, tt.f, gem.RoleEquipment)
			if !ok {
				t.Fatalf("Lookup(%d, %d): not found", tt.s, tt.f)
			}

			if !tt.check(info) {
				t.Errorf("Lookup(%d, %d): got %+v", tt.s, tt.f, info)
			}
		})
	}
}

func TestLookupDecodeTakesOptions(t *testing.T) {
	info, ok := gem.Lookup(5, 1, gem.RoleEquipment)
	if !ok {
		t.Fatal("Lookup(5, 1, RoleEquipment): not found")
	}

	// An S5F1 from equipment that appends its own fourth item to the standard body.
	alarm := wire(t, secs2.L(secs2.B(0x86), secs2.U4(12), secs2.A("Door open"), secs2.A("equipment-defined")))

	_, err := info.Decode(alarm)
	requireDecodeError(t, err, "gem: S5F1 body: list length 4, want 3")

	got, err := info.Decode(alarm, gem.AllowTrailing())
	if err != nil {
		t.Fatalf("Decode with AllowTrailing: %v", err)
	}

	assertDecoded(t, got, gem.S5F1Body{ALCD: 0x86, ALID: secs2.U4(12), ALTX: "Door open"})
}

func TestRoleAndDirectionString(t *testing.T) {
	tests := []struct {
		got  fmt.Stringer
		want string
	}{
		{gem.RoleHost, "host"},
		{gem.RoleEquipment, "equipment"},
		{gem.Role(0), "Role(0)"},
		{gem.Role(3), "Role(3)"},
		{gem.HostToEquipment, "host-to-equipment"},
		{gem.EquipmentToHost, "equipment-to-host"},
		{gem.Bidirectional, "both"},
		{gem.Direction(0), "Direction(0)"},
		{gem.Direction(255), "Direction(255)"},
	}

	for _, tt := range tests {
		if s := tt.got.String(); s != tt.want {
			t.Errorf("got %q, want %q", s, tt.want)
		}
	}
}
