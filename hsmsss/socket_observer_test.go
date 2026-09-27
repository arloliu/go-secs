package hsmsss

// socket_observer_test.go exercises hsms.WithSocketObserver end to end:
// the socket record this transport mints for every dialed or accepted socket, refused ones included,
// its close gate, and the identity every socket and wire event carries.
//
// Most scenarios run the SUT against a raw TCP peer that holds its own socket open unless the scenario closes it,
// so the error a close reports is the one the scenario provoked, never an EOF the peer's reaction produced.
// One log records socket and wire events together, in the order the observers were called,
// so the per-socket order (up, frames, refused, closed) can be asserted directly.

import (
	"context"
	"errors"
	"io"
	"net"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/arloliu/go-secs/v2/hsms"
	"github.com/arloliu/go-secs/v2/secs2"
	"github.com/stretchr/testify/require"
)

// socketLogEntry is one observer call: a socket event, or a wire event when isWire is set.
type socketLogEntry struct {
	isWire bool
	sock   hsms.SocketEvent
	wire   hsms.WireEvent
}

// socketLog records socket and wire events in one sequence, in the order the observers were called.
// Wire frames are copied, because a frame is valid only for the duration of the call.
type socketLog struct {
	mu      sync.Mutex
	entries []socketLogEntry
}

func (l *socketLog) observeSocket(ev hsms.SocketEvent) {
	l.mu.Lock()
	l.entries = append(l.entries, socketLogEntry{sock: ev})
	l.mu.Unlock()
}

func (l *socketLog) observeWire(ev hsms.WireEvent) {
	ev.Frame = append([]byte(nil), ev.Frame...)

	l.mu.Lock()
	l.entries = append(l.entries, socketLogEntry{isWire: true, wire: ev})
	l.mu.Unlock()
}

// connOptions returns both observers as core options.
func (l *socketLog) connOptions() []hsms.ConnOption {
	return []hsms.ConnOption{hsms.WithSocketObserver(l.observeSocket), hsms.WithWireObserver(l.observeWire)}
}

// options returns both observers as transport options.
func (l *socketLog) options() []Option {
	opts := make([]Option, 0, 2)
	for _, o := range l.connOptions() {
		opts = append(opts, WithConnectionOption(o))
	}

	return opts
}

func (l *socketLog) snapshot() []socketLogEntry {
	l.mu.Lock()
	defer l.mu.Unlock()

	return append([]socketLogEntry(nil), l.entries...)
}

// sockets returns the socket events, in order.
func (l *socketLog) sockets() []hsms.SocketEvent {
	var out []hsms.SocketEvent
	for _, e := range l.snapshot() {
		if !e.isWire {
			out = append(out, e.sock)
		}
	}

	return out
}

// kind returns the socket events of kind k, in order.
func (l *socketLog) kind(k hsms.SocketEventKind) []hsms.SocketEvent {
	var out []hsms.SocketEvent
	for _, ev := range l.sockets() {
		if ev.Kind == k {
			out = append(out, ev)
		}
	}

	return out
}

// of returns every entry for socket id, in order.
func (l *socketLog) of(id uint64) []socketLogEntry {
	var out []socketLogEntry
	for _, e := range l.snapshot() {
		if (e.isWire && e.wire.Socket == id) || (!e.isWire && e.sock.Socket == id) {
			out = append(out, e)
		}
	}

	return out
}

// waitKind waits (bounded poll) until at least n socket events of kind k were observed and returns them.
func (l *socketLog) waitKind(t *testing.T, k hsms.SocketEventKind, n int) []hsms.SocketEvent {
	t.Helper()

	var events []hsms.SocketEvent
	require.Eventuallyf(t, func() bool {
		events = l.kind(k)

		return len(events) >= n
	}, 5*time.Second, 2*time.Millisecond, "timeout waiting for %d %s events", n, k)

	return events
}

// waitClosed waits (bounded poll) until socket id has been reported closed and returns that event.
func (l *socketLog) waitClosed(t *testing.T, id uint64) hsms.SocketEvent {
	t.Helper()

	var closed hsms.SocketEvent
	require.Eventuallyf(t, func() bool {
		for _, ev := range l.kind(hsms.SocketClosed) {
			if ev.Socket == id {
				closed = ev

				return true
			}
		}

		return false
	}, 5*time.Second, 2*time.Millisecond, "timeout waiting for socket %d to be reported closed", id)

	return closed
}

// countKind returns how many events of kind k socket id has.
func (l *socketLog) countKind(id uint64, k hsms.SocketEventKind) int {
	n := 0
	for _, ev := range l.kind(k) {
		if ev.Socket == id {
			n++
		}
	}

	return n
}

// requireAddrs asserts ev names the SUT's side of raw as Local and raw's own side as Remote.
func requireAddrs(t *testing.T, ev hsms.SocketEvent, raw net.Conn) {
	t.Helper()

	require.NotNil(t, ev.Local)
	require.NotNil(t, ev.Remote)
	require.Equal(t, raw.RemoteAddr().String(), ev.Local.String(), "%s: Local is the SUT's address", ev.Kind)
	require.Equal(t, raw.LocalAddr().String(), ev.Remote.String(), "%s: Remote is the peer's address", ev.Kind)
}

// requireWireSocket asserts every wire event of the log names socket id and generation gen.
func requireWireSocket(t *testing.T, entries []socketLogEntry, id, gen uint64) {
	t.Helper()

	for i, e := range entries {
		if !e.isWire {
			continue
		}

		require.Equal(t, id, e.wire.Socket, "wire entry %d", i)
		require.Equal(t, gen, e.wire.Generation, "wire entry %d", i)
	}
}

// selectedActive opens an active SUT with log's observers against a raw passive peer,
// answers its Select.req, and returns the SUT and the raw peer (which holds its socket open).
func selectedActive(t *testing.T, log *socketLog, opts ...Option) (*endpoint, *net.TCPConn) {
	t.Helper()

	ln, port := listenLoopback(t)
	t.Cleanup(func() { _ = ln.Close() })
	peerCh := acceptOneAsync(ln)

	sut := newEndpoint(t, port, true, append(log.options(), opts...))
	require.NoError(t, sut.conn.Open(t.Context(), hsms.OpenBackground))
	t.Cleanup(func() { _ = sut.conn.Close() })

	raw := waitPeer(t, peerCh)
	t.Cleanup(func() { _ = raw.Close() })

	hdr := peerReadSelectReqHeader(t, raw)
	_, err := raw.Write(selectRspFrame(hdr, hsms.SelectStatusSuccess))
	require.NoError(t, err)
	waitSelected(t, sut)

	return sut, raw
}

// selectedPassive opens a passive SUT with log's observers and selects it from a raw active peer.
func selectedPassive(t *testing.T, log *socketLog) (*endpoint, *net.TCPConn, int) {
	t.Helper()

	port := freeLoopbackPort(t)
	sut := newEndpoint(t, port, false, log.options())
	require.NoError(t, sut.conn.Open(t.Context(), hsms.OpenBackground))
	t.Cleanup(func() { _ = sut.conn.Close() })

	raw := rawSelectHandshake(t, port, [4]byte{0, 0, 0, 1})
	t.Cleanup(func() { _ = raw.Close() })
	waitSelected(t, sut)

	return sut, raw, port
}

// A dialed socket is reported connected once and closed once, with its addresses,
// and a local Close reports no failure.
// The connected event carries generation 0 (not yet adopted); the close carries the generation that adopted it,
// the same generation and socket the socket's frames carried.
func TestSocketObserver_ConnectedThenLocalClose(t *testing.T) {
	t.Parallel()

	log := &socketLog{}
	sut, raw := selectedActive(t, log)

	closeEndpoint(t, sut)

	socks := log.sockets()
	require.Len(t, socks, 2, "one connected and one closed event: %+v", socks)

	up, closed := socks[0], socks[1]
	require.Equal(t, hsms.SocketConnected, up.Kind)
	require.Equal(t, hsms.SocketClosed, closed.Kind)
	require.Equal(t, uint64(1), up.Socket, "the first socket of a connection is socket 1")
	require.Equal(t, up.Socket, closed.Socket)
	require.Zero(t, up.Generation, "a socket is not adopted yet when it is reported up")
	require.NotZero(t, closed.Generation, "an adopted socket's close names its generation")
	require.NoError(t, closed.Err, "a local close reports no failure")
	require.False(t, up.At.IsZero())
	require.False(t, closed.At.Before(up.At))
	requireAddrs(t, up, raw)
	requireAddrs(t, closed, raw)

	requireWireSocket(t, log.of(up.Socket), up.Socket, closed.Generation)
	require.Len(t, log.of(up.Socket), 2+3, "Select.req, Select.rsp and the courtesy Separate between the two socket events")
}

// An accepted socket is reported accepted once and closed once, with its addresses,
// and a close the peer requested with a Separate.req reports no failure.
func TestSocketObserver_AcceptedThenPeerSeparate(t *testing.T) {
	t.Parallel()

	log := &socketLog{}
	sut, raw, _ := selectedPassive(t, log)

	_, err := raw.Write(buildControlFrame(byte(hsms.SeparateReqType), 0, [4]byte{0, 0, 0, 2}))
	require.NoError(t, err)
	waitState(t, sut, hsms.NotConnectedState)

	up := log.waitKind(t, hsms.SocketAccepted, 1)[0]
	closed := log.waitClosed(t, up.Socket)
	requireAddrs(t, up, raw)
	requireAddrs(t, closed, raw)
	require.NoError(t, closed.Err, "a close the peer requested reports no failure")
	require.NotZero(t, closed.Generation)

	closeEndpoint(t, sut)
	require.Equal(t, 1, log.countKind(up.Socket, hsms.SocketClosed), "the later Close must not report the socket closed again")
	require.Len(t, log.kind(hsms.SocketAccepted), 1)
}

// A peer that drops the link reports the read error as the close's failure.
func TestSocketObserver_PeerDropReportsReadError(t *testing.T) {
	t.Parallel()

	log := &socketLog{}
	sut, raw, _ := selectedPassive(t, log)

	require.NoError(t, raw.Close())

	up := log.waitKind(t, hsms.SocketAccepted, 1)[0]
	closed := log.waitClosed(t, up.Socket)
	require.ErrorIs(t, closed.Err, io.EOF, "the close reports the read error that broke the link")

	closeEndpoint(t, sut)
	require.Equal(t, 1, log.countKind(up.Socket, hsms.SocketClosed))
}

// errInjectedWrite is the failure failingWriteConn returns once armed.
var errInjectedWrite = errors.New("injected write failure")

// failingWriteConn is a dialed socket whose writes fail once armed, while its reads keep working.
// Wrapping the *net.TCPConn also drops the vectored-write fast path, so a frame's buffers are written one Write at a time.
type failingWriteConn struct {
	net.Conn

	fail atomic.Bool
}

func (c *failingWriteConn) Write(b []byte) (int, error) {
	if c.fail.Load() {
		return 0, errInjectedWrite
	}

	return c.Conn.Write(b)
}

// A write that fails reports the write error as the close's failure,
// while the peer, silent and holding its socket open, gives the read side nothing to report first.
func TestSocketObserver_WriteFailureReportsWriteError(t *testing.T) {
	t.Parallel()

	var dialed atomic.Pointer[failingWriteConn]
	dial := func(ctx context.Context, network, addr string) (net.Conn, error) {
		c, err := (&net.Dialer{}).DialContext(ctx, network, addr)
		if err != nil {
			return nil, err
		}

		fc := &failingWriteConn{Conn: c}
		dialed.Store(fc)

		return fc, nil
	}

	log := &socketLog{}
	sut, _ := selectedActive(t, log, WithDialer(dial))

	dialed.Load().fail.Store(true)
	_, err := sut.conn.SendDataMessage(t.Context(), 1, 1, false, secs2.A("x"))
	require.ErrorIs(t, err, errInjectedWrite)

	up := log.waitKind(t, hsms.SocketConnected, 1)[0]
	closed := log.waitClosed(t, up.Socket)
	require.ErrorIs(t, closed.Err, errInjectedWrite, "the close reports the write error that initiated it")

	closeEndpoint(t, sut)
	require.Equal(t, 1, log.countKind(up.Socket, hsms.SocketClosed))
}

// A socket that never selects is closed by the T7 dwell, and the close reports ErrT7Timeout.
func TestSocketObserver_T7ExpiryReportsErrT7Timeout(t *testing.T) {
	t.Parallel()

	log := &socketLog{}
	port := freeLoopbackPort(t)
	sut := newEndpoint(t, port, false, append(log.options(), WithConnectionOption(hsms.WithT7(100*time.Millisecond))))
	require.NoError(t, sut.conn.Open(t.Context(), hsms.OpenBackground))
	defer closeEndpoint(t, sut)

	raw := dialPassive(t, port) // connects and never selects
	defer func() { _ = raw.Close() }()

	up := log.waitKind(t, hsms.SocketAccepted, 1)[0]
	closed := log.waitClosed(t, up.Socket)
	require.ErrorIs(t, closed.Err, hsms.ErrT7Timeout)
	require.NotZero(t, closed.Generation)
}

// A refused socket is observed in its own identity with generation 0:
// accepted, then whichever frames of its refusal exchange crossed the wire, then refused, then closed.
// The close reports what cut the exchange short, and nothing for a completed exchange.
// The live session is untouched throughout.
func TestSocketObserver_RefusedSocket(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name string
		// peer drives the refused socket and returns the frames that crossed the wire,
		// in order: what it wrote, then what it read.
		peer    func(t *testing.T, raw *net.TCPConn) (in, out [][]byte)
		wantErr func(t *testing.T, err error)
	}{
		{
			name: "peer hangs up at once: no frame",
			peer: func(t *testing.T, raw *net.TCPConn) ([][]byte, [][]byte) {
				t.Helper()
				require.NoError(t, raw.Close())

				return nil, nil
			},
			wantErr: func(t *testing.T, err error) { t.Helper(); require.ErrorIs(t, err, io.EOF) },
		},
		{
			name: "malformed length: no frame",
			peer: func(t *testing.T, raw *net.TCPConn) ([][]byte, [][]byte) {
				t.Helper()
				_, err := raw.Write(frameBytes(11, header10(0, byte(hsms.SelectReqType)), []byte{0}))
				require.NoError(t, err)
				requirePeerClosed(t, raw)

				return nil, nil
			},
			wantErr: func(t *testing.T, err error) { t.Helper(); require.ErrorIs(t, err, errRefusalBadLength) },
		},
		{
			name: "not a Select.req: one frame",
			peer: func(t *testing.T, raw *net.TCPConn) ([][]byte, [][]byte) {
				t.Helper()
				frame := buildControlFrame(byte(hsms.LinktestReqType), 0, [4]byte{0, 0, 0, 7})
				_, err := raw.Write(frame)
				require.NoError(t, err)
				requirePeerClosed(t, raw)

				return [][]byte{frame}, nil
			},
			wantErr: func(t *testing.T, err error) { t.Helper(); require.ErrorIs(t, err, errRefusalNotSelectReq) },
		},
		{
			name: "completed exchange: two frames",
			peer: func(t *testing.T, raw *net.TCPConn) ([][]byte, [][]byte) {
				t.Helper()
				req := selectReqFrame([4]byte{0, 0, 0, 9})
				_, err := raw.Write(req)
				require.NoError(t, err)
				rsp, err := peerReadFrame(raw, 5*time.Second)
				require.NoError(t, err)
				require.Equal(t, byte(hsms.SelectStatusAlreadyActive), rsp[3])

				return [][]byte{req}, [][]byte{frameBytes(uint32(len(rsp)), rsp, nil)}
			},
			wantErr: func(t *testing.T, err error) { t.Helper(); require.NoError(t, err) },
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()

			log := &socketLog{}
			sut, _, port := selectedPassive(t, log)
			live := log.waitKind(t, hsms.SocketAccepted, 1)[0]

			raw := dialPassive(t, port)
			defer func() { _ = raw.Close() }()

			refused := log.waitKind(t, hsms.SocketAccepted, 2)[1]
			require.Greater(t, refused.Socket, live.Socket, "a refused socket gets its own identity")
			requireAddrs(t, refused, raw)

			in, out := tt.peer(t, raw)
			closed := log.waitClosed(t, refused.Socket)
			tt.wantErr(t, closed.Err)

			entries := log.of(refused.Socket)
			require.Len(t, entries, 3+len(in)+len(out), "accepted, the frames, refused, closed: %+v", entries)
			require.Equal(t, hsms.SocketAccepted, entries[0].sock.Kind)
			for i, frame := range in {
				e := entries[1+i]
				require.True(t, e.isWire)
				require.Equal(t, hsms.WireInbound, e.wire.Direction)
				require.Equal(t, frame, e.wire.Frame, "the inbound frame is the bytes the peer wrote")
			}
			for i, frame := range out {
				e := entries[1+len(in)+i]
				require.True(t, e.isWire)
				require.Equal(t, hsms.WireOutbound, e.wire.Direction)
				require.Equal(t, frame, e.wire.Frame, "the outbound frame is the bytes the peer read")
			}
			require.Equal(t, hsms.SocketRefused, entries[len(entries)-2].sock.Kind)
			require.Equal(t, hsms.SocketClosed, entries[len(entries)-1].sock.Kind)
			requireAddrs(t, entries[len(entries)-2].sock, raw)

			for i, e := range entries {
				if e.isWire {
					require.Zero(t, e.wire.Generation, "entry %d: a refused socket's frames carry generation 0", i)
				} else {
					require.Zero(t, e.sock.Generation, "entry %d: a refused socket carries generation 0", i)
				}
			}

			require.Equal(t, hsms.SelectedState, sut.conn.State(), "the live session must stay Selected")
			require.Zero(t, log.countKind(live.Socket, hsms.SocketClosed), "the live socket must not be touched")
		})
	}
}

// requirePeerClosed waits until the SUT has closed raw: a read returns an error and no bytes.
func requirePeerClosed(t *testing.T, raw net.Conn) {
	t.Helper()

	require.NoError(t, raw.SetReadDeadline(time.Now().Add(5*time.Second)))

	n, err := raw.Read(make([]byte, 32))
	require.Error(t, err)
	require.Zero(t, n, "a refused socket closed without a response writes nothing")
}

// Stop halting the refusal exchange on either side of its publication still yields one refused and one closed event,
// in that order, reporting no failure, because Stop's close is a local one.
func TestSocketRace_HaltRefusalRacingPublication(t *testing.T) {
	t.Parallel()

	t.Run("Stop before publication", func(t *testing.T) {
		t.Parallel()

		log := &socketLog{}
		port := freeLoopbackPort(t)
		conn, tr := newPassiveConnTr(t, port, append(log.connOptions(), hsms.WithT7(30*time.Second))...)

		reached := make(chan struct{})
		release := make(chan struct{})
		var reachedOnce, releaseOnce sync.Once
		t.Cleanup(func() { releaseOnce.Do(func() { close(release) }) })
		tr.refusePrePublishHook = func() {
			reachedOnce.Do(func() { close(reached) })
			<-release
		}

		require.NoError(t, conn.Open(t.Context(), hsms.OpenBackground))
		raw1 := rawSelectHandshake(t, port, [4]byte{0, 0, 0, 1})
		defer func() { _ = raw1.Close() }()

		raw2 := dialPassive(t, port)
		defer func() { _ = raw2.Close() }()
		<-reached

		done := make(chan error, 1)
		go func() { done <- conn.Close() }()

		require.Eventually(t, func() bool {
			tr.refuseMu.Lock()
			defer tr.refuseMu.Unlock()

			return tr.refuseStopped
		}, 5*time.Second, time.Millisecond, "Stop must reach haltRefusal before the socket is published")
		releaseOnce.Do(func() { close(release) })
		require.NoError(t, <-done)

		requireOneRefusal(t, log)
	})

	t.Run("Stop after publication", func(t *testing.T) {
		t.Parallel()

		log := &socketLog{}
		port := freeLoopbackPort(t)
		conn, tr := newPassiveConnTr(t, port, append(log.connOptions(), hsms.WithT7(30*time.Second))...)

		require.NoError(t, conn.Open(t.Context(), hsms.OpenBackground))
		raw1 := rawSelectHandshake(t, port, [4]byte{0, 0, 0, 1})
		defer func() { _ = raw1.Close() }()

		raw2 := dialPassive(t, port) // silent: the refusal parks in its first read once published
		defer func() { _ = raw2.Close() }()

		require.Eventually(t, func() bool {
			tr.refuseMu.Lock()
			defer tr.refuseMu.Unlock()

			return tr.refuseSock != nil
		}, 5*time.Second, time.Millisecond, "the extra socket must be published to the refusal slot")

		require.NoError(t, conn.Close())

		requireOneRefusal(t, log)
	})
}

// requireOneRefusal asserts the second accepted socket of log was refused once and closed once,
// in that order, with no failure.
func requireOneRefusal(t *testing.T, log *socketLog) {
	t.Helper()

	accepted := log.kind(hsms.SocketAccepted)
	require.Len(t, accepted, 2)

	id := accepted[1].Socket
	require.Equal(t, 1, log.countKind(id, hsms.SocketRefused))
	require.Equal(t, 1, log.countKind(id, hsms.SocketClosed))

	entries := log.of(id)
	last := entries[len(entries)-1].sock
	require.Equal(t, hsms.SocketClosed, last.Kind)
	require.NoError(t, last.Err, "Stop's close is a local one")
	require.Equal(t, hsms.SocketRefused, entries[len(entries)-2].sock.Kind, "refused is reported before closed")
}

// sockRecRT is genRecRT plus the socket-observation capability, recording every socket event.
type sockRecRT struct {
	*genRecRT

	log *socketLog
}

func (r *sockRecRT) ObserveSocket(ev hsms.SocketEvent) { r.log.observeSocket(ev) }

// A TCP-up the core refuses closes the socket through the gate exactly once,
// reporting it up and then closed, never adopted (generation 0) and with no failure.
// The runtime here offers no adoption capability, so this also covers the transport's fallback to TCPUpFromGeneration.
func TestSocketRace_RefusedTCPUpClosesThroughGateOnce(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name   string
		wantUp hsms.SocketEventKind
		opts   func(tracked net.Conn) []Option
		run    func(t *testing.T, tr *transport, tracked net.Conn)
	}{
		{
			name:   "active",
			wantUp: hsms.SocketConnected,
			opts: func(tracked net.Conn) []Option {
				return []Option{WithDialer(func(context.Context, string, string) (net.Conn, error) { return tracked, nil })}
			},
			run: func(t *testing.T, tr *transport, _ net.Conn) {
				t.Helper()
				require.ErrorIs(t, tr.startActive(t.Context()), errStartSealed)
			},
		},
		{
			name:   "passive",
			wantUp: hsms.SocketAccepted,
			opts:   func(net.Conn) []Option { return []Option{WithPassive()} },
			run: func(t *testing.T, tr *transport, tracked net.Conn) {
				t.Helper()
				g := &genWG{gen: 1}
				g.accept.Add(1)
				tr.acceptLoop(t.Context(), g, &singleAcceptListener{conn: tracked})
			},
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()

			server, client := net.Pipe()
			t.Cleanup(func() { _ = server.Close(); _ = client.Close() })
			tracked := &sealCloseTrackingConn{Conn: client}

			cfg, err := NewConfig("127.0.0.1", 0, tt.opts(tracked)...)
			require.NoError(t, err)

			log := &socketLog{}
			rt := &sockRecRT{genRecRT: newGenRecRT(1), log: log}
			rt.setRefuseTCPUp(true)

			tr := newTransport(cfg)
			tr.rt = rt

			tt.run(t, tr, tracked)

			require.Equal(t, int64(1), tracked.closes.Load(), "the refused socket is closed exactly once")

			socks := log.sockets()
			require.Len(t, socks, 2, "%+v", socks)
			require.Equal(t, tt.wantUp, socks[0].Kind)
			require.Equal(t, hsms.SocketClosed, socks[1].Kind)
			require.Equal(t, socks[0].Socket, socks[1].Socket)
			require.Zero(t, socks[1].Generation, "a socket the core refused was never adopted")
			require.NoError(t, socks[1].Err)
		})
	}
}

// A reconnect advances both counters: the second socket has a larger identity and belongs to a later generation,
// and every event and frame of each socket names its own pair.
func TestSocketObserver_ReconnectAdvancesSocketAndGeneration(t *testing.T) {
	t.Parallel()

	log := &socketLog{}
	sut, raw1, port := selectedPassive(t, log)

	_, err := raw1.Write(buildControlFrame(byte(hsms.SeparateReqType), 0, [4]byte{0, 0, 0, 2}))
	require.NoError(t, err)
	waitState(t, sut, hsms.NotConnectedState)

	raw2 := rawSelectHandshake(t, port, [4]byte{0, 0, 0, 3})
	defer func() { _ = raw2.Close() }()
	waitSelected(t, sut)

	accepted := log.waitKind(t, hsms.SocketAccepted, 2)
	s1, s2 := accepted[0].Socket, accepted[1].Socket
	require.Greater(t, s2, s1)

	g1 := log.waitClosed(t, s1).Generation
	closeEndpoint(t, sut)
	g2 := log.waitClosed(t, s2).Generation
	require.Greater(t, g2, g1)

	requireWireSocket(t, log.of(s1), s1, g1)
	requireWireSocket(t, log.of(s2), s2, g2)
	require.NotEmpty(t, log.of(s2), "the second socket's frames are observed")
}

// A read error racing a local Close yields exactly one close event for the socket, whichever wins.
func TestSocketRace_ReadErrorRacingLocalClose(t *testing.T) {
	t.Parallel()

	log := &socketLog{}
	sut, raw, _ := selectedPassive(t, log)
	id := log.waitKind(t, hsms.SocketAccepted, 1)[0].Socket

	start := make(chan struct{})
	var wg sync.WaitGroup
	wg.Go(func() {
		<-start
		_ = raw.Close()
	})
	wg.Go(func() {
		<-start
		_ = sut.conn.Close()
	})
	close(start)
	wg.Wait()

	log.waitClosed(t, id)
	require.Equal(t, 1, log.countKind(id, hsms.SocketClosed), "one close event whichever side won")
}

// downCountingRT is the receive-path test runtime plus the socket-observation capability,
// counting every disconnect report and keeping the cause of each.
type downCountingRT struct {
	*recRT

	log *socketLog

	mu     sync.Mutex
	causes []error
}

func (r *downCountingRT) ObserveSocket(ev hsms.SocketEvent) { r.log.observeSocket(ev) }

func (r *downCountingRT) TCPDown(cause error) {
	r.mu.Lock()
	r.causes = append(r.causes, cause)
	r.mu.Unlock()
}

func (r *downCountingRT) reported() []error {
	r.mu.Lock()
	defer r.mu.Unlock()

	return append([]error(nil), r.causes...)
}

// tcpDown closes the generation's socket through its gate before it reports,
// hands the gate nil for a peer Separate while still reporting errPeerSeparate,
// and drops a report only when another report already closed the socket:
// a report that loses to any other close (teardown, Stop) is still made.
func TestSocketObserver_TCPDownGatesBeforeReporting(t *testing.T) {
	t.Parallel()

	errFirst := errors.New("first report")
	errSecond := errors.New("second report")

	tests := []struct {
		name         string
		run          func(tr *transport, g *genWG)
		wantReports  []error
		wantCloseErr error
	}{
		{
			name: "a second report after a report is dropped",
			run: func(tr *transport, g *genWG) {
				tr.tcpDown(g, errFirst, hsms.CauseLinktestFail)
				tr.tcpDown(g, errSecond, hsms.CauseIOError)
			},
			wantReports:  []error{errFirst},
			wantCloseErr: errFirst,
		},
		{
			name: "a report after a local close is still made",
			run: func(tr *transport, g *genWG) {
				g.sock.close(nil)
				tr.tcpDown(g, errSecond, hsms.CauseIOError)
			},
			wantReports:  []error{errSecond},
			wantCloseErr: nil,
		},
		{
			name: "a peer Separate closes with no failure",
			run: func(tr *transport, g *genWG) {
				tr.tcpDown(g, errPeerSeparate, hsms.CausePeerSeparate)
			},
			wantReports:  []error{errPeerSeparate},
			wantCloseErr: nil,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()

			server, client := net.Pipe()
			t.Cleanup(func() { _ = server.Close() })

			log := &socketLog{}
			rt := &downCountingRT{recRT: newRecRT(), log: log}
			tr := &transport{rt: rt}
			g := &genWG{gen: 1, sock: tr.mintSocket(client, hsms.SocketAccepted)}

			tt.run(tr, g)

			require.Equal(t, tt.wantReports, rt.reported())

			closed := log.kind(hsms.SocketClosed)
			require.Len(t, closed, 1)
			require.Equal(t, tt.wantCloseErr, closed[0].Err)

			_, err := server.Read(make([]byte, 1))
			require.ErrorIs(t, err, io.EOF, "the gate closed the socket before the report")
		})
	}
}
