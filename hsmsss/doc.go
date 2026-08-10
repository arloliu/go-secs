// Package hsmsss implements the HSMS-SS (HSMS Single Session) transport for semiconductor-equipment communication per SEMI E37.1, layered over the shared connection engine
// and immutable message model in the sibling package hsms.
//
// HSMS-SS is the single-session profile of HSMS (SEMI E37): one selected session per TCP connection, with the Select/Linktest/Separate control handshake
// and SECS-II data exchange.
// Deselect is NOT part of that handshake — E37.1 §7.3 forbids using it, and communications end with Separate;
// this package answers an inbound Deselect.req only so a non-conformant peer is not stranded on T6, and never initiates one.
// This package provides the concrete TCP transport — dial (active) or listen (passive), the framed reader/writer,
// and the E37.1 state procedures — and returns the app-facing [Connection] (embedding [github.com/arloliu/go-secs/v2/hsms.Connection],
// which the hsms core drives) plus HSMS-SS control-plane metrics.
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
// [NewConfig] takes the peer host and port plus functional [Option] values.
// [WithActive] and [WithPassive] select the connection role (active dials, passive listens).
// Protocol timers and other engine knobs are set through [WithConnectionOption], which wraps an [github.com/arloliu/go-secs/v2/hsms.ConnOption] (for example hsms.WithT3, hsms.WithT6, hsms.WithT8, hsms.WithLinktestInterval).
// [New] returns the consumer-facing [Connection], which embeds hsms.Connection (every shared HSMS-II send/reply/handler operation is available unchanged)
// and adds ControlMetrics, the HSMS-SS control-plane counters: linktest sent/received/errored, Select established, Separate received, Reject sent/received, inbound linktest answered,
// and (per [github.com/arloliu/go-secs/v2/hsms.WithLinktestSuppression]) the suppressed/credited linktest counts.
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
// Once Selected, send SECS-II data messages through the [github.com/arloliu/go-secs/v2/hsms.SECS2Endpoint] surface embedded in the Connection —
// SendDataMessage (blocking, waits for the W-bit reply), SendDataMessageAsync (fire-and-forget), SendSECS2Message,
// and ReplyDataMessage.
// Register inbound handlers with AddDataMessageHandler and lifecycle observers with AddConnStateChangeHandler.
// UpdateConfigOptions retunes live timers, and Close tears the connection down (idempotent).
//
// This package is single-session by design: there is NO AddSession call — the Connection IS its own SECS-II endpoint.
// There is also no Free or pooling API: messages are GC-owned immutable values (see the hsms package doc), so handlers may retain
// and share received messages across goroutines without reference counting.
//
// # E37.1 §10.1 implementation documentation
//
// SEMI E37.1 §10.1 requires an HSMS-SS implementation to document three things.
// They are properties of the application that embeds this package.
// What follows states what the package itself provides and fixes,
// and what the embedding application must decide and document for its own product.
//
// 1. Device IDs supported, and their values.
// This package supports exactly ONE device ID per connection, configured with
// [github.com/arloliu/go-secs/v2/hsms.WithSessionID] and readable via Connection.SessionID.
// Any uint16 is accepted and the default is 0xFFFF.
// Note that E37.1 §4.1.1 defines device ID as a 15-bit field,
// and §8.1 requires the high-order bit of a data message's Session ID to be zero,
// so a conformant value is 0x0000–0x7FFF.
// The range is not enforced, and the 0xFFFF default is outside it.
// Set an explicit device ID for any peer that checks.
// The configured value applies to DATA messages only — every control message carries
// [github.com/arloliu/go-secs/v2/hsms.ControlSessionID] (0xFFFF) per §8.1, regardless of this setting.
//
// 2. Normal or restricted procedure for terminating communications.
// This package implements the NORMAL procedure.
// A graceful Close from Selected sends a courtesy Separate.req before closing the TCP connection,
// and an inbound Separate.req closes the connection immediately in any connected substate (§7.6).
// The courtesy Separate is best-effort.
// It is skipped rather than allowed to block teardown behind a wedged writer,
// and it is suppressed after a communications failure, since the link is already gone.
//
// 3. The host vs. equipment parameter.
// This package DOES carry the parameter, and the default is HOST.
// Select it with [WithHostRole] or [WithEquipRole]; read it back with Config.IsEquip.
//
// Its scope is deliberately narrow.
// §10.2 notes that HSMS-SS itself does not require the distinction,
// so the setting drives exactly one behavior here.
// An equipment-role connection answers its own T3 reply timeout with an S9F9 to the peer (Transaction Timeout, SEMI E5 §10.13);
// a host-role connection does not.
// It is [github.com/arloliu/go-secs/v2/hsms.WithAutoS9F9] under the hood.
// It does not affect the TCP role, the Select procedure, or any header field.
//
// The TCP role is a separate, independent setting: [WithActive] dials, [WithPassive] listens.
// Either TCP role may be combined with either host/equipment role.
// An application that attaches further meaning to host vs. equipment must document that itself.
//
// One further deviation is deliberate and documented at its call site rather than here:
// an inbound Linktest.req is answered in any connected substate, where §7.4 limits Linktest to SELECTED.
// See handleLinktestReq and docs/specs/e37-1-hsms-ss-conformance-audit.md.
//
// # Dissolved v1 landmines
//
// The HSMS-SS transport runs over the shared hsms connection engine and message model and therefore inherits its dissolved-landmine guarantees —
// the send-gate lock-order inversion, stale-frame-across-generations, Free/aliasing, and reply-channel close asymmetry hazards are structurally gone;
// see the "Dissolved v1 landmines" section of the hsms package doc for the per-hazard details.
// The SECS-I half-duplex I/O-ownership sense of the reply-channel hazard does not apply to HSMS-SS
// and is out of scope for this package.
package hsmsss
