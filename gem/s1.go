package gem

import "github.com/arloliu/go-secs/v2/secs2"

// S1F1 creates an S1F1 (Are You There) request message (SEMI E5 §8.1).
//
// The message has no data body (header-only). It is sent with W=1 (reply expected; S1F2 is
// the expected response).
func S1F1() secs2.SECS2Message {
	return secs2.NewMessage(1, 1, true, secs2.NewEmptyItem())
}

// S1F2 creates an S1F2 (On Line Data) reply message for equipment (SEMI E5 §8.1).
//
// The body is L[2]{ A[mdln] A[softrev] } where mdln is the equipment model number and softrev is
// the software revision. The message is sent with W=0 (no reply expected).
func S1F2(mdln, softrev string) secs2.SECS2Message {
	return secs2.NewMessage(1, 2, false, secs2.L(secs2.A(mdln), secs2.A(softrev)))
}

// S1F2Host creates an S1F2 (On Line Data) reply message for a host (SEMI E5 §8.1).
//
// A host-originated S1F2 body is L[0] (empty list). The message is sent with W=0 (no reply
// expected).
func S1F2Host() secs2.SECS2Message {
	return secs2.NewMessage(1, 2, false, secs2.L())
}

// S1F13 creates an S1F13 (Establish Communications Request) message for equipment (SEMI E5 §8.4).
//
// The body is L[2]{ A[mdln] A[softrev] } where mdln is the equipment model number and softrev is
// the software revision. The message is sent with W=1 (reply expected; S1F14 is the expected
// response).
func S1F13(mdln, softrev string) secs2.SECS2Message {
	return secs2.NewMessage(1, 13, true, secs2.L(secs2.A(mdln), secs2.A(softrev)))
}

// S1F13Host creates an S1F13 (Establish Communications Request) message for a host (SEMI E5 §8.4).
//
// A host-originated S1F13 body is L[0] (empty list). The message is sent with W=1 (reply
// expected; S1F14 is the expected response).
func S1F13Host() secs2.SECS2Message {
	return secs2.NewMessage(1, 13, true, secs2.L())
}

// S1F14 creates an S1F14 (Establish Communications Acknowledge) message for equipment
// (SEMI E5 §8.4).
//
// The body is L[2]{ B[commack] L[2]{ A[mdln] A[softrev] } } where commack is the
// accept/deny code (0 = accepted), mdln is the equipment model number, and softrev is the
// software revision. The message is sent with W=0 (no reply expected).
func S1F14(commack byte, mdln, softrev string) secs2.SECS2Message {
	return secs2.NewMessage(1, 14, false,
		secs2.L(
			secs2.B(commack),
			secs2.L(secs2.A(mdln), secs2.A(softrev)),
		),
	)
}

// S1F14Host creates an S1F14 (Establish Communications Acknowledge) message for a host
// (SEMI E5 §8.4).
//
// A host-originated S1F14 body is L[2]{ B[commack] L[0] } where commack is the accept/deny
// code (0 = accepted). The message is sent with W=0 (no reply expected).
func S1F14Host(commack byte) secs2.SECS2Message {
	return secs2.NewMessage(1, 14, false,
		secs2.L(
			secs2.B(commack),
			secs2.L(),
		),
	)
}
