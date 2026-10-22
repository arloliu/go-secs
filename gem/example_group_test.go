package gem_test

import (
	"fmt"

	"github.com/arloliu/go-secs/v2/gem"
	"github.com/arloliu/go-secs/v2/secs2"
)

// ExampleReport sends an event report built from Report structs and reads the reports back.
//
// The decoder checks every report's shape: a report missing its value list is an error naming the report.
func ExampleReport() {
	msg := gem.S6F11(secs2.U4(1), secs2.U4(200),
		gem.Report{RPTID: secs2.U4(7), VALUES: []secs2.Item{secs2.A("IDLE"), secs2.U4(42)}},
		gem.Report{RPTID: secs2.U4(8)},
	)

	body, err := gem.DecodeS6F11(msg.Item())
	if err != nil {
		fmt.Println("decode:", err)
		return
	}

	for _, report := range body.REPORTS {
		rptid, _ := report.RPTID.ToUint()
		fmt.Printf("report %d: %d values\n", rptid[0], len(report.VALUES))
	}

	_, err = gem.DecodeS6F11(secs2.L(secs2.U4(1), secs2.U4(200), secs2.L(secs2.L(secs2.U4(7)))))
	fmt.Println(err)

	// Output:
	// report 7: 2 values
	// report 8: 0 values
	// gem: REPORTS[0]: list length 1, want 2
}
