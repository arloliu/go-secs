package gem_test

import (
	"fmt"

	"github.com/arloliu/go-secs/v2/gem"
)

// ExampleAbort builds the S6F0 that closes an S6F11 transaction in lieu of its S6F12 reply.
func ExampleAbort() {
	msg := gem.Abort(6)

	fmt.Printf("S%dF%d W=%t body=%d bytes\n", msg.StreamCode(), msg.FunctionCode(), msg.WaitBit(), len(msg.Item().ToBytes()))

	// Output:
	// S6F0 W=false body=0 bytes
}
