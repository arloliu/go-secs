package integration

import (
	"context"
	"net"
	"sync"
)

// scriptedPeer is the peer abstraction the dial factory stores. Each concrete peer
// (the HSMS peer here, and the SECS-I peer added later) layers its own scripting on top
// of the shared close method, so the harness can hold a slice of peers regardless of the
// wire protocol they speak.
type scriptedPeer interface {
	close()
}

// scriptablePeer is the extended peer surface the driven scenarios reach for. It adds
// peer-originated sends, an inbound-body stream, and a reply-withholding toggle on top of
// scriptedPeer. Both concrete peers implement it, so a test reaches the live generation via
// df.latest().(scriptablePeer) regardless of the wire protocol.
type scriptablePeer interface {
	scriptedPeer
	// sendPrimary makes the PEER originate a W-bit primary to the connection and returns the
	// correlated reply body. It MUST execute on the peer's own line-owning goroutine (never a
	// second writer): it posts a command to that goroutine and waits on a response channel.
	// stream and function name the primary; body is the raw SECS-II-encoded item bytes, and the
	// returned slice is the reply's raw SECS-II-encoded item bytes.
	sendPrimary(stream, function byte, body []byte) ([]byte, error)
	// inbound returns the buffered stream of data-message BODIES the connection sent to the peer,
	// used to assert a fire-and-forget send arrived. The peer pushes every received data message
	// here non-blockingly (buffered channel + select/default drop-if-full) so it never wedges.
	inbound() <-chan []byte
	// setWithholdReplies toggles the peer's default auto-echo of W-bit primaries off/on, for the
	// reply-timeout scenario. It is safe to call from the test goroutine.
	setWithholdReplies(withhold bool)
	// dropLine simulates an involuntary peer disconnect: it abruptly stops the peer and closes its
	// end of the pipe so the connection's recv sees EOF and tears down. The core's reconnect loop
	// then redials, minting a fresh peer (a new generation) via the dial factory. It reuses the
	// goroutine-safe teardown, so a later closeAll double-close is a safe no-op.
	dropLine()
}

// dialFactory is a WithDialer-compatible dial factory that runs the real transports over
// in-memory net.Pipe connections. On EACH dial it mints a fresh net.Pipe pair, spawns a
// scripted peer on the far end, records the peer, and hands the near end to the connection
// under test.
//
// It is a factory (not a single captured conn) precisely so reconnect scenarios work: the
// core's reconnect loop re-invokes the dialer, and each generation must get its own pipe
// and its own peer. It is generic over the peer type via spawn, so the SECS-I peer added
// later reuses the same harness unchanged.
type dialFactory struct {
	mu    sync.Mutex
	spawn func(net.Conn) scriptedPeer // builds the scripted peer on the far (endB) end
	peers []scriptedPeer              // one per dial (generation), newest last
}

// dial satisfies the transport DialFunc signature (hsmsss.DialFunc / secs1.DialFunc). The
// network and address are ignored — the pipe is purely in-memory — so a fresh net.Pipe is
// minted, the scripted peer is spawned on the far end, and the near end is returned to the
// connection under test.
func (f *dialFactory) dial(_ context.Context, _, _ string) (net.Conn, error) {
	endA, endB := net.Pipe()

	peer := f.spawn(endB)

	f.mu.Lock()
	f.peers = append(f.peers, peer)
	f.mu.Unlock()

	return endA, nil
}

// latest returns the most recently spawned peer (the current generation), or nil if no
// dial has happened yet. The driven scenarios use it to address the live peer (asserting it
// to scriptablePeer); reconnect scenarios use it to address the newest generation.
func (f *dialFactory) latest() scriptedPeer {
	f.mu.Lock()
	defer f.mu.Unlock()

	if len(f.peers) == 0 {
		return nil
	}

	return f.peers[len(f.peers)-1]
}

// closeAll stops every spawned peer. It is registered with t.Cleanup so peer goroutines
// and pipes are always released, including reconnect generations a test never addressed.
func (f *dialFactory) closeAll() {
	f.mu.Lock()
	peers := append([]scriptedPeer(nil), f.peers...)
	f.mu.Unlock()

	for _, p := range peers {
		p.close()
	}
}
