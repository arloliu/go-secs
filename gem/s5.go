package gem

import "github.com/arloliu/go-secs/v2/secs2"

// S5F1 creates an S5F1 (Alarm Report Send) message (SEMI E5 §10.7).
//
// The body is L[3]{ B[alcd] <alid> A[altx] } where alcd is the alarm condition byte (bit 7 set
// means alarm set; bit 7 clear means alarm cleared), alid is an equipment-defined alarm ID item
// (caller controls SECS-II type), and altx is the alarm text.
//
// Per GEM (SEMI E30) the S5F2 acknowledgment is typically expected, so this builder emits W=1.
// Callers that require a no-reply alarm send should use [secs2.NewMessage] directly.
func S5F1(alcd byte, alid secs2.Item, altx string) secs2.SECS2Message {
	return secs2.NewMessage(5, 1, true,
		secs2.L(secs2.B(alcd), alid, secs2.A(altx)),
	)
}

// S5F2 creates an S5F2 (Alarm Report Acknowledge) reply message (SEMI E5 §10.7).
//
// The body is B[ackc5] where ackc5 is the accept/deny code (0 = accepted).
// The message is sent with W=0 (no reply expected).
func S5F2(ackc5 byte) secs2.SECS2Message {
	return secs2.NewMessage(5, 2, false, secs2.B(ackc5))
}
