package gem

import "github.com/arloliu/go-secs/v2/secs2"

// S2F17 creates an S2F17 (Date and Time Request) message (SEMI E5 §10.4).
//
// The message has no data body (header-only). It is sent with W=1 (reply expected; S2F18 is
// the expected response).
func S2F17() secs2.SECS2Message {
	return secs2.NewMessage(2, 17, true, secs2.NewEmptyItem())
}

// S2F18 creates an S2F18 (Date and Time Data) reply message (SEMI E5 §10.4).
//
// The body is A[t] where t is the date-time string (YYYYMMDDHHmmss format per SEMI E30).
// The message is sent with W=0 (no reply expected).
func S2F18(t string) secs2.SECS2Message {
	return secs2.NewMessage(2, 18, false, secs2.A(t))
}

// S2F31 creates an S2F31 (Date and Time Set Request) message (SEMI E5 §10.5).
//
// The body is A[t] where t is the date-time string (YYYYMMDDHHmmss format per SEMI E30).
// The message is sent with W=1 (reply expected; S2F32 is the expected response).
func S2F31(t string) secs2.SECS2Message {
	return secs2.NewMessage(2, 31, true, secs2.A(t))
}

// S2F32 creates an S2F32 (Date and Time Set Acknowledge) reply message (SEMI E5 §10.5).
//
// The body is B[tiack] where tiack is the accept/deny code (0 = accepted).
// The message is sent with W=0 (no reply expected).
func S2F32(tiack byte) secs2.SECS2Message {
	return secs2.NewMessage(2, 32, false, secs2.B(tiack))
}

// S2F37 creates an S2F37 (Enable/Disable Event Report) message (SEMI E5 §10.7).
//
// The body is L[2]{ BOOLEAN[enable] L[n]{ <ceid>... } } where enable selects enable (true) or
// disable (false), and ceids is the list of collection event ID items. An empty ceids argument
// produces L[0], which targets all collection events as defined by GEM (SEMI E30).
// The message is sent with W=1 (reply expected; S2F38 is the expected response).
//
// Equipment-defined CEID items are passed as [secs2.Item] values; callers control the SECS-II
// type (ASCII, integer, …). For W=0 or other non-standard variants use [secs2.NewMessage].
func S2F37(enable bool, ceids ...secs2.Item) secs2.SECS2Message {
	return secs2.NewMessage(2, 37, true,
		secs2.L(secs2.BOOLEAN(enable), secs2.L(ceids...)),
	)
}

// S2F38 creates an S2F38 (Enable/Disable Event Report Acknowledge) reply message (SEMI E5 §10.7).
//
// The body is B[erack] where erack is the accept/deny code (0 = accepted).
// The message is sent with W=0 (no reply expected).
func S2F38(erack byte) secs2.SECS2Message {
	return secs2.NewMessage(2, 38, false, secs2.B(erack))
}
