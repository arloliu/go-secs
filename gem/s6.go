package gem

import "github.com/arloliu/go-secs/v2/secs2"

// S6F11 creates an S6F11 (Event Report Send) message (SEMI E5 §10.8).
//
// The body is L[3]{ <dataid> <ceid> L[a]{ <report>... } } where dataid is an equipment-defined
// data ID item, ceid is the collection event ID item, and reports is the list of report elements.
// Build each report element with [Report].
//
// The message is sent with W=1 (reply expected; S6F12 is the expected response).
// Equipment-defined dataid and ceid items are passed as [secs2.Item] values so callers control
// the SECS-II type.
func S6F11(dataid, ceid secs2.Item, reports ...secs2.Item) secs2.SECS2Message {
	return secs2.NewMessage(6, 11, true,
		secs2.L(dataid, ceid, secs2.L(reports...)),
	)
}

// S6F12 creates an S6F12 (Event Report Acknowledge) reply message (SEMI E5 §10.8).
//
// The body is B[ackc6] where ackc6 is the accept/deny code (0 = accepted).
// The message is sent with W=0 (no reply expected).
func S6F12(ackc6 byte) secs2.SECS2Message {
	return secs2.NewMessage(6, 12, false, secs2.B(ackc6))
}
