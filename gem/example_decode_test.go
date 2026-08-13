package gem_test

import (
	"fmt"

	"github.com/arloliu/go-secs/v2/gem"
	"github.com/arloliu/go-secs/v2/secs2"
)

// ExampleDecodeS1F14 reads an Establish Communications acknowledgement out of a reply body.
//
// The second half shows the strict-shape contract: a body that does not match the E5 shape is an
// error naming the position that failed, never a zero value with a nil error.
func ExampleDecodeS1F14() {
	// The reply as it arrives from the equipment; in a session this is reply.Item().
	reply := gem.S1F14(gem.COMMACKAccepted, "GEM-2000", "1.4.2")

	ack, err := gem.DecodeS1F14(reply.Item())
	if err != nil {
		fmt.Println("decode:", err)
		return
	}

	fmt.Printf("commack=%d model=%s rev=%s\n", ack.COMMACK, ack.MDLN, ack.SOFTREV)

	// The inner list carrying MDLN and SOFTREV is missing.
	_, err = gem.DecodeS1F14(secs2.L(secs2.B(0), secs2.A("GEM-2000")))
	fmt.Println(err)

	// Output:
	// commack=0 model=GEM-2000 rev=1.4.2
	// gem: S1F14 body[1]: got ascii, want list
}
