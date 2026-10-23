package hsms_test

import (
	"context"
	"fmt"
	"log"
	"sync/atomic"

	"github.com/arloliu/go-secs/v2/hsms"
	"github.com/arloliu/go-secs/v2/hsms/hsmstest"
	"github.com/arloliu/go-secs/v2/secs2"
)

// ExampleReplyAbort refuses every host primary that OFF-LINE equipment does not answer.
// The refusal is the first step of the application's only data handler,
// and the handler returns once it has refused, so the primary gets no second answer.
// An hsmstest.FakeEndpoint stands in for the connection and records the S6F0 it would send.
func ExampleReplyAbort() {
	var offline atomic.Bool // the equipment's control state, kept by the application
	offline.Store(true)

	handler := func(msg *hsms.DataMessage, ep hsms.SECS2Endpoint) {
		// OFF-LINE equipment answers every W-bit primary with SxF0, except S1F13 and S1F17.
		answeredOffline := msg.Stream() == 1 && (msg.Function() == 13 || msg.Function() == 17)
		if offline.Load() && msg.WaitBit() && !answeredOffline {
			if err := hsms.ReplyAbort(context.Background(), ep, msg); err != nil {
				log.Printf("abort S%dF%d: %v", msg.Stream(), msg.Function(), err)
			}

			return
		}

		// Handle msg, and answer it once with ep.ReplyDataMessage.
	}

	// A connection from hsmsss.New or secs1.New takes the handler the same way.
	ep := hsmstest.NewFakeEndpoint()
	ep.AddDataMessageHandler(handler)

	// The host sends an S6F11 event report with the W-bit set.
	primary, err := hsms.NewDataMessage(6, 11, true, 0, [4]byte{0, 0, 0, 42},
		secs2.L(secs2.U4(1), secs2.U4(100), secs2.L()))
	if err != nil {
		log.Fatal(err)
	}

	ep.Deliver(primary)

	for _, sent := range ep.Sent() {
		fmt.Printf("%s: S%dF%d W=%t system bytes %v\n",
			sent.Method, sent.Message.Stream(), sent.Message.Function(), sent.Message.WaitBit(), sent.Message.SystemBytes())
	}

	// Output:
	// ReplyAbort: S6F0 W=false system bytes [0 0 0 42]
}
