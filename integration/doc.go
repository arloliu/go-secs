// Package integration holds the deterministic, network-free integration tests for the go-secs v2
// transports. It drives the real HSMS-SS and SECS-I connections — the same hsmsss and secs1 clients
// an application uses — over in-memory pipes against scripted peers, so the full connection
// lifecycle is exercised end to end with no real network, no ports, and no wall-clock dependence.
//
// # Harness
//
// Each connection is opened with a custom dialer (the transports' WithDialer option) that returns
// one end of a net.Pipe instead of a TCP socket. The dialer is a factory: it mints a fresh pipe and
// a fresh scripted peer on every dial, so each reconnect generation gets its own pipe and its own
// peer. Message exchange over the unbuffered pipe is synchronous, which makes the suite
// deterministic, while the production dial, framing, timer, and state-machine paths still run
// unchanged.
//
// # Scripted peers
//
// A scripted peer plays the far end of one connection. The HSMS-SS peer is full-duplex: it reads
// and writes concurrently so the unbuffered pipe never deadlocks, answering Select and Linktest
// requests and echoing W-bit data. The SECS-I peer is a single goroutine that owns the half-duplex
// line and speaks the SEMI E4 block protocol (ENQ, EOT, block with checksum, ACK), reassembling
// multi-block messages and echoing them. Both peers can also originate their own messages and be
// scripted to withhold a reply or withhold the Select response, so a test can drive either
// direction of an exchange and both timeout and dwell behaviors.
//
// # What is covered
//
// A single transport-agnostic scenario table runs against both transports through the
// hsms.Connection interface, proving the shared core behaves identically regardless of transport:
// open to Selected then clean close, W-bit round-trips, replies from a registered handler,
// fire-and-forget sends, concurrent bidirectional exchange guarding reply correlation, reply-timeout
// recovery, involuntary peer drop with active reconnect, data-message metric deltas, and generation
// isolation across close and reopen. A separate connection-state matrix asserts the (previous, next)
// transitions each transport actually emits — HSMS-SS steps through the Select handshake and the T7
// not-selected dwell, SECS-I auto-commits a live line straight to Selected and never enters
// not-selected — and a fuzz target drives normalized round-trips through both transports.
//
// It is a test-only package: it exports no production API and compiles to an empty package.
package integration
