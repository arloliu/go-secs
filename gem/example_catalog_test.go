package gem_test

import (
	"fmt"

	"github.com/arloliu/go-secs/v2/gem"
)

// ExampleLookup names a received message and decodes its body without a switch over stream and function.
func ExampleLookup() {
	// A message as it arrives at the host; in a session the stream, function, and body come from it.
	msg := gem.S1F13("GEM-2000", "1.4.2")

	info, ok := gem.Lookup(msg.StreamCode(), msg.FunctionCode(), gem.RoleEquipment)
	if !ok {
		fmt.Println("not a message SEMI E5 defines")
		return
	}

	fmt.Printf("S%dF%d %s (%s), reply S%dF%d\n", info.Stream, info.Function, info.Name, info.Mnemonic, info.Stream, info.ReplyFunction)

	body, err := info.Decode(msg.Item())
	if err != nil {
		fmt.Println("decode:", err)
		return
	}

	// Decode returns the form's own result type, here the equipment's S1F13Body.
	req, ok := body.(gem.S1F13Body)
	if !ok {
		fmt.Printf("unexpected %T\n", body)
		return
	}

	fmt.Printf("model=%s rev=%s\n", req.MDLN, req.SOFTREV)

	// Output:
	// S1F13 Establish Communications Request (CR), reply S1F14
	// model=GEM-2000 rev=1.4.2
}

// ExampleAcceptReply builds the host's automatic acknowledgement of an event report.
func ExampleAcceptReply() {
	reply, ok := gem.AcceptReply(6, 11, gem.RoleHost)
	fmt.Println(ok)
	fmt.Printf("S%dF%d %s\n", reply.StreamCode(), reply.FunctionCode(), reply.Item().ToSML())

	// The equipment's S1F14 carries its model and software revision, so it is no generic acceptance.
	_, ok = gem.AcceptReply(1, 13, gem.RoleEquipment)
	fmt.Println(ok)

	// Output:
	// true
	// S6F12 <B[1] 0x00>
	// false
}
