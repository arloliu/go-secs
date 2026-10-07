package classify_test

import (
	"fmt"

	"github.com/arloliu/go-secs/tracepack"
	"github.com/arloliu/go-secs/tracepack/classify"
)

// ExampleNew classifies a header-only S1F1 data message, 14 bytes with its length prefix,
// under a ceiling of 13 bytes and under no ceiling.
func ExampleNew() {
	frame := []byte{0x00, 0x00, 0x00, 0x0a, 0x00, 0x01, 0x01, 0x01, 0x00, 0x00, 0x00, 0x00, 0x00, 0x01}

	for _, c := range []tracepack.Classifier{classify.New(13), classify.New(0)} {
		status, trailing := c.Frame(frame)
		fmt.Println(c.MaxFrameLen(), status, trailing)
	}
	// Output:
	// 13 oversized 0
	// 0 ok 0
}
