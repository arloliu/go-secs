package hsms

import "context"

// AbortReplier is implemented by the endpoints that can answer a primary with SxF0:
// every connection hsmsss.New, secs1.New, and NewConnection return,
// the SECS2Endpoint those connections pass to a DataMessageHandler or DecodeErrorHandler,
// and the hsmstest.FakeEndpoint test fake, which records the SxF0 instead of sending it.
//
// Call it through [ReplyAbort], which takes any SECS2Endpoint.
type AbortReplier interface {
	// ReplyAbort answers primary with SxF0; see the package function [ReplyAbort].
	ReplyAbort(ctx context.Context, primary *DataMessage) error
}

// The connection inherits ReplyAbort from the session it embeds.
var _ AbortReplier = (*connection)(nil)

// ReplyAbort answers primary with SxF0, the Abort Transaction message of primary's stream (SEMI E5 §10.4.1):
// the same stream and System Bytes as primary, function 0, no W-bit, and no body.
//
// Function 0 closes a transaction in lieu of the expected reply,
// so the originator need not wait for its reply timer to expire.
// SendSECS2Message cannot send it, since it refuses an even function and mints fresh System Bytes;
// ReplyAbort takes them from primary, as ReplyDataMessage does.
// When to refuse a message, and when Stream 9 is the right answer instead, is described in the gem package documentation.
//
// ReplyAbort is called instead of ReplyDataMessage, at most once per primary.
// Nothing stops a second answer:
// a handler that both aborts and replies to the same primary sends both, under the same System Bytes.
// Decide to refuse before any other reply, and return once refused.
//
// The abort is sent exactly as [SECS2Endpoint.ReplyDataMessage] sends a reply:
// asynchronously, and bound to the generation that delivered primary when ep's own connection delivered it,
// so a primary whose generation has ended returns ErrConnClosed instead of going out on a reconnected successor.
//
// Parameters:
//   - ep: a connection from hsmsss.New, secs1.New, or NewConnection, the endpoint a handler receives,
//     or an hsmstest.FakeEndpoint in a test.
//   - primary: the received primary to close, with its W-bit set.
//
// Returns:
//   - error: ErrAbortUnsupported, without sending anything, if ep does not implement [AbortReplier];
//     ErrNilMessage if primary is nil;
//     ErrNoReplyExpected, without sending anything, if primary's W-bit is clear;
//     otherwise the errors of ReplyDataMessage, ErrConnClosed included.
//
// Example:
//
//	conn.AddDataMessageHandler(func(msg *hsms.DataMessage, ep hsms.SECS2Endpoint) {
//		if msg.WaitBit() && !canHandle(msg) {
//			_ = hsms.ReplyAbort(context.Background(), ep, msg)
//			return
//		}
//		// Handle msg, and answer it with ep.ReplyDataMessage.
//	})
func ReplyAbort(ctx context.Context, ep SECS2Endpoint, primary *DataMessage) error {
	ar, ok := ep.(AbortReplier)
	if !ok {
		return ErrAbortUnsupported
	}

	return ar.ReplyAbort(ctx, primary)
}
