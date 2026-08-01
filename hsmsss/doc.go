// Package hsmsss implements the HSMS-SS (HSMS Single Session) transport for
// semiconductor-equipment communication per SEMI E37.1, layered over the shared connection
// engine and immutable message model in the sibling package hsms.
//
// HSMS-SS is the single-session profile of HSMS (SEMI E37): one selected session per TCP
// connection, with the Select/Deselect/Linktest/Separate control handshake and SECS-II data
// exchange. This package provides the concrete TCP transport — dial (active) or listen
// (passive), the framed reader/writer, and the E37.1 state procedures — and returns the
// app-facing [Connection] (embedding [github.com/arloliu/go-secs/v2/hsms.Connection], which the
// hsms core drives) plus HSMS-SS control-plane metrics.
//
// # Entry point
//
// Build a configuration, then construct a connection:
//
//	cfg, err := hsmsss.NewConfig("127.0.0.1", 5000,
//	    hsmsss.WithActive(),                              // or hsmsss.WithPassive()
//	    hsmsss.WithConnectionOption(hsms.WithT3(45*time.Second)),
//	    hsmsss.WithConnectionOption(hsms.WithLinktestInterval(30*time.Second)),
//	)
//	if err != nil {
//	    return err
//	}
//
//	conn, err := hsmsss.New(cfg) // returns hsmsss.Connection
//	if err != nil {
//	    return err
//	}
//
// [NewConfig] takes the peer host and port plus functional [Option] values. [WithActive] and
// [WithPassive] select the connection role (active dials, passive listens). Protocol timers and
// other engine knobs are set through [WithConnectionOption], which wraps an
// [github.com/arloliu/go-secs/v2/hsms.ConnOption] (for example hsms.WithT3, hsms.WithT6,
// hsms.WithT8, hsms.WithLinktestInterval). [New] returns the consumer-facing [Connection], which
// embeds hsms.Connection (every shared HSMS-II send/reply/handler operation is available unchanged)
// and adds ControlMetrics, the HSMS-SS control-plane counters: linktest sent/received/errored,
// Select established, Separate received, Reject sent/received, inbound linktest answered, and
// (per [github.com/arloliu/go-secs/v2/hsms.WithLinktestSuppression]) the suppressed/credited
// linktest counts.
//
// # Lifecycle and messaging
//
// Open the connection with a mode:
//
//	// Block until the session is Selected (or ctx expires):
//	err := conn.Open(ctx, hsms.OpenWaitSelected)
//	// Or kick off the lifecycle in the background (passive typically uses this):
//	err := conn.Open(ctx, hsms.OpenBackground)
//
// Once Selected, send SECS-II data messages through the
// [github.com/arloliu/go-secs/v2/hsms.SECS2Endpoint] surface embedded in the Connection —
// SendDataMessage (blocking, waits for the W-bit reply), SendDataMessageAsync (fire-and-forget),
// SendSECS2Message, and ReplyDataMessage. Register inbound handlers with AddDataMessageHandler
// and lifecycle observers with AddConnStateChangeHandler. UpdateConfigOptions retunes live
// timers, and Close tears the connection down (idempotent).
//
// This package is single-session by design: there is NO AddSession call — the Connection IS its
// own SECS-II endpoint. There is also no Free or pooling API: messages are GC-owned immutable
// values (see the hsms package doc), so handlers may retain and share received messages across
// goroutines without reference counting.
//
// # Dissolved v1 landmines
//
// The HSMS-SS transport runs over the shared hsms connection engine and message model and
// therefore inherits its dissolved-landmine guarantees — the send-gate lock-order inversion,
// stale-frame-across-generations, Free/aliasing, and reply-channel close asymmetry hazards are
// structurally gone; see the "Dissolved v1 landmines" section of the hsms package doc for the
// per-hazard details. The SECS-I half-duplex I/O-ownership sense of the reply-channel hazard
// does not apply to HSMS-SS and is out of scope for this package.
package hsmsss
