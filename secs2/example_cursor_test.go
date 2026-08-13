package secs2_test

import (
	"fmt"

	"github.com/arloliu/go-secs/v2/secs2"
)

// ExampleCursor extracts a value from an S1F14-shaped body — <L[2] <U1 COMMACK>
// <L[2] <A MDLN> <A SOFTREV>>> — with a single chained call instead of a Get / type-assert /
// ToXxx / index dance.
func ExampleCursor() {
	body := secs2.L(
		secs2.U1(0),
		secs2.L(secs2.A("MDL-X"), secs2.A("1.00")),
	)

	mdln, err := secs2.NewCursor(body).At(1, 0).ASCII()
	if err != nil {
		fmt.Println(err)
		return
	}

	fmt.Println(mdln)
	// Output: MDL-X
}
