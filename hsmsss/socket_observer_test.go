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
	"os"
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

// holdWire returns a wire observer that logs every event into log,
// except that a call whose event match accepts parks, before it logs, until release is called.
// The returned channel is closed as the first such call parks.
// release is safe to call more than once;
// the caller registers it as a cleanup wherever its order against the other cleanups matters.
func holdWire(log *socketLog, match func(hsms.WireEvent) bool) (func(hsms.WireEvent), <-chan struct{}, func()) {
	entered := make(chan struct{})
	released := make(chan struct{})
	enter := closeOnce(entered)

	observe := func(ev hsms.WireEvent) {
		if match(ev) {
			enter()
			<-released
		}
		log.observeWire(ev)
	}

	return observe, entered, closeOnce(released)
}

// requireFrameBeforeClose asserts that socket id's log holds a wire event match accepts,
// and that the last such event precedes the socket's SocketClosed event;
// what names the frame in the failure messages.
func requireFrameBeforeClose(t *testing.T, log *socketLog, id uint64, match func(hsms.WireEvent) bool, what string) {
	t.Helper()

	frameAt, closedAt := -1, -1
	for i, e := range log.of(id) {
		switch {
		case e.isWire && match(e.wire):
			frameAt = i
		case !e.isWire && e.sock.Kind == hsms.SocketClosed:
			closedAt = i
		default:
		}
	}
	require.NotEqual(t, -1, frameAt, "the held %s is reported", what)
	require.Less(t, frameAt, closedAt, "the %s is reported before its socket's close", what)
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

// selectedPassive opens a passive SUT with log's observers and opts, and selects it from a raw active peer.
func selectedPassive(t *testing.T, log *socketLog, opts ...Option) (*endpoint, *net.TCPConn, int) {
	t.Helper()

	port := freeLoopbackPort(t)
	sut := newEndpoint(t, port, false, append(log.options(), opts...))
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
	// The courtesy Separate's report, and the close report it holds back, are delivered from a goroutine of their own,
	// which can trail Close; the close is the last event of the socket, so once it is in the log the log is complete.
	log.waitClosed(t, 1)

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
// accepted, then refused as soon as the connection decides to refuse it, before its exchange,
// then whichever frames of its refusal exchange crossed the wire, then closed.
// The close reports what cut the exchange short, and nothing for a completed exchange.
// The live session is untouched throughout.
func TestSocketObserver_RefusedSocket(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name string
		// opts are the SUT's extra options.
		opts []Option
		// peer drives the refused socket and returns the frames that crossed the wire,
		// in order: what it wrote, then what it read.
		peer    func(t *testing.T, raw *net.TCPConn) (in, out [][]byte)
		wantErr func(t *testing.T, err error)
	}{
		{
			name: "silent peer: the absolute deadline cuts the exchange short",
			opts: []Option{WithConnectionOption(hsms.WithT7(500 * time.Millisecond))},
			peer: func(t *testing.T, raw *net.TCPConn) ([][]byte, [][]byte) {
				t.Helper()
				requirePeerClosed(t, raw)

				return nil, nil
			},
			wantErr: func(t *testing.T, err error) {
				t.Helper()
				require.ErrorIs(t, err, os.ErrDeadlineExceeded, "the refusal retains the read's own deadline error")
			},
		},
		{
			name: "short header: no frame",
			peer: func(t *testing.T, raw *net.TCPConn) ([][]byte, [][]byte) {
				t.Helper()
				_, err := raw.Write(selectReqFrame([4]byte{0, 0, 0, 8})[:4+3])
				require.NoError(t, err)
				require.NoError(t, raw.Close())

				return nil, nil
			},
			wantErr: func(t *testing.T, err error) { t.Helper(); require.ErrorIs(t, err, io.ErrUnexpectedEOF) },
		},
		{
			name: "failed response write: one frame",
			opts: []Option{WithListener(wrappingListen(func(n int64, c net.Conn) net.Conn {
				if n != 2 {
					return c
				}

				fc := &failingWriteConn{Conn: c}
				fc.fail.Store(true)

				return fc
			}))},
			peer: func(t *testing.T, raw *net.TCPConn) ([][]byte, [][]byte) {
				t.Helper()
				req := selectReqFrame([4]byte{0, 0, 0, 9})
				_, err := raw.Write(req)
				require.NoError(t, err)
				requirePeerClosed(t, raw)

				return [][]byte{req}, nil
			},
			wantErr: func(t *testing.T, err error) { t.Helper(); require.ErrorIs(t, err, errInjectedWrite) },
		},
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
			sut, _, port := selectedPassive(t, log, tt.opts...)
			live := log.waitKind(t, hsms.SocketAccepted, 1)[0]

			raw := dialPassive(t, port)
			defer func() { _ = raw.Close() }()

			refused := log.waitKind(t, hsms.SocketAccepted, 2)[1]
			require.Greater(t, refused.Socket, live.Socket, "a refused socket gets its own identity")
			requireAddrs(t, refused, raw)

			// The refusal is reported when the connection decides it, before the peer has sent anything.
			require.Eventually(t, func() bool { return log.countKind(refused.Socket, hsms.SocketRefused) == 1 },
				5*time.Second, 2*time.Millisecond, "the refusal must be reported before the exchange")

			in, out := tt.peer(t, raw)
			closed := log.waitClosed(t, refused.Socket)
			tt.wantErr(t, closed.Err)

			entries := log.of(refused.Socket)
			require.Len(t, entries, 3+len(in)+len(out), "accepted, refused, the frames, closed: %+v", entries)
			require.Equal(t, hsms.SocketAccepted, entries[0].sock.Kind)
			require.Equal(t, hsms.SocketRefused, entries[1].sock.Kind)
			requireAddrs(t, entries[1].sock, raw)
			for i, frame := range in {
				e := entries[2+i]
				require.True(t, e.isWire)
				require.Equal(t, hsms.WireInbound, e.wire.Direction)
				require.Equal(t, frame, e.wire.Frame, "the inbound frame is the bytes the peer wrote")
			}
			for i, frame := range out {
				e := entries[2+len(in)+i]
				require.True(t, e.isWire)
				require.Equal(t, hsms.WireOutbound, e.wire.Direction)
				require.Equal(t, frame, e.wire.Frame, "the outbound frame is the bytes the peer read")
			}
			require.Equal(t, hsms.SocketClosed, entries[len(entries)-1].sock.Kind)

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

// requireOneRefusal asserts the second accepted socket of log, which exchanged no frame,
// was accepted, refused once and closed once, in that order, with no failure.
func requireOneRefusal(t *testing.T, log *socketLog) {
	t.Helper()

	accepted := log.kind(hsms.SocketAccepted)
	require.Len(t, accepted, 2)

	entries := log.of(accepted[1].Socket)
	require.Len(t, entries, 3, "accepted, refused, closed: %+v", entries)
	require.Equal(t, hsms.SocketAccepted, entries[0].sock.Kind)
	require.Equal(t, hsms.SocketRefused, entries[1].sock.Kind, "refused is reported before closed")
	require.Equal(t, hsms.SocketClosed, entries[2].sock.Kind)
	require.NoError(t, entries[2].sock.Err, "Stop's close is a local one")
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

// A read error racing a local Close yields exactly one close event for the socket,
// carrying the failure of whichever side reached the socket's close first.
//
// The read error wins when the peer's drop has already closed the socket through its gate,
// here held inside that close's report while the local Close runs to the point where the transport is stopping.
// The local Close wins when the read error is already in hand but not yet acted on,
// here held inside the socket's Read until the local close has closed the socket.
func TestSocketRace_ReadErrorRacingLocalClose(t *testing.T) {
	t.Parallel()

	t.Run("read error wins", func(t *testing.T) {
		t.Parallel()

		log := &socketLog{}
		entered := make(chan struct{})
		release := make(chan struct{})
		var enteredOnce, releaseOnce sync.Once
		t.Cleanup(func() { releaseOnce.Do(func() { close(release) }) })
		observe := func(ev hsms.SocketEvent) {
			log.observeSocket(ev)
			if ev.Kind == hsms.SocketClosed {
				enteredOnce.Do(func() { close(entered) })
				<-release
			}
		}

		port := freeLoopbackPort(t)
		conn, tr := newObservedPassiveTr(t, port, WithConnectionOption(hsms.WithSocketObserver(observe)))
		require.NoError(t, conn.Open(t.Context(), hsms.OpenBackground))
		raw := rawSelectHandshake(t, port, [4]byte{0, 0, 0, 1})
		id := log.waitKind(t, hsms.SocketAccepted, 1)[0].Socket

		require.NoError(t, raw.Close())
		<-entered // the read error closed the socket and is reporting it

		done := make(chan error, 1)
		go func() { done <- conn.Close() }()

		// The transport is stopped only after the connection's own socket close has returned, having lost the gate.
		require.Eventually(t, tr.isStopping, 5*time.Second, time.Millisecond, "the local Close must reach the transport")
		releaseOnce.Do(func() { close(release) })
		require.NoError(t, <-done)

		require.Equal(t, 1, log.countKind(id, hsms.SocketClosed), "one close event")
		require.ErrorIs(t, log.waitClosed(t, id).Err, io.EOF, "the read error won, so the close reports it")
	})

	t.Run("local close wins", func(t *testing.T) {
		t.Parallel()

		held := &readErrorHoldConn{failed: make(chan struct{}), release: make(chan struct{}), closed: make(chan struct{})}
		var releaseOnce sync.Once
		t.Cleanup(func() { releaseOnce.Do(func() { close(held.release) }) })
		listen := wrappingListen(func(n int64, c net.Conn) net.Conn {
			if n != 1 {
				return c
			}
			held.Conn = c

			return held
		})

		log := &socketLog{}
		port := freeLoopbackPort(t)
		conn, _ := newObservedPassiveTr(t, port, append(log.options(), WithListener(listen))...)
		require.NoError(t, conn.Open(t.Context(), hsms.OpenBackground))
		raw := rawSelectHandshake(t, port, [4]byte{0, 0, 0, 1})
		id := log.waitKind(t, hsms.SocketAccepted, 1)[0].Socket

		require.NoError(t, raw.Close())
		<-held.failed // the receive goroutine holds the read error, not yet acted on

		done := make(chan error, 1)
		go func() { done <- conn.Close() }()

		<-held.closed // the local close reached the socket first
		releaseOnce.Do(func() { close(held.release) })
		require.NoError(t, <-done)

		// The close report can trail Close behind the courtesy Separate's delivery, which holds the socket's report scope.
		require.NoError(t, log.waitClosed(t, id).Err, "the local close won, so the close reports no failure")
		require.Equal(t, 1, log.countKind(id, hsms.SocketClosed), "one close event")
	})
}

// newObservedPassiveTr is newPassiveConnTr taking transport options,
// with the integration timers newEndpoint uses and auto-linktest off.
func newObservedPassiveTr(t *testing.T, port int, opts ...Option) (hsms.Connection, *transport) {
	t.Helper()

	base := []Option{
		WithPassive(),
		WithConnectionOption(hsms.WithT3(3 * time.Second)),
		WithConnectionOption(hsms.WithLinktestInterval(0)),
	}

	cfg, err := NewConfig("127.0.0.1", port, append(base, opts...)...)
	require.NoError(t, err)

	tr := newTransport(cfg)
	conn, err := hsms.NewConnection(&cfg.ConnectionConfig, tr)
	require.NoError(t, err)
	t.Cleanup(func() { _ = conn.Close() })

	return conn, tr
}

// wrappingListener is a real loopback listener that hands every accepted conn to wrap,
// numbered from 1 in accept order.
type wrappingListener struct {
	net.Listener

	n    atomic.Int64
	wrap func(n int64, c net.Conn) net.Conn
}

func (l *wrappingListener) Accept() (net.Conn, error) {
	c, err := l.Listener.Accept()
	if err != nil {
		return nil, err
	}

	return l.wrap(l.n.Add(1), c), nil
}

// wrappingListen is a ListenFunc for WithListener whose listener wraps accepted conns with wrap.
func wrappingListen(wrap func(n int64, c net.Conn) net.Conn) ListenFunc {
	return func(ctx context.Context, network, addr string) (net.Listener, error) {
		ln, err := (&net.ListenConfig{}).Listen(ctx, network, addr)
		if err != nil {
			return nil, err
		}

		return &wrappingListener{Listener: ln, wrap: wrap}, nil
	}
}

// readErrorHoldConn holds the first Read that fails until release is closed, after announcing it on failed,
// and announces its first Close on closed.
type readErrorHoldConn struct {
	net.Conn

	failed, release, closed chan struct{}
	failOnce, closeOnce     sync.Once
}

func (c *readErrorHoldConn) Read(b []byte) (int, error) {
	n, err := c.Conn.Read(b)
	if err != nil {
		first := false
		c.failOnce.Do(func() { first = true; close(c.failed) })
		if first {
			<-c.release
		}
	}

	return n, err
}

func (c *readErrorHoldConn) Close() error {
	c.closeOnce.Do(func() { close(c.closed) })

	return c.Conn.Close()
}

// A frame's wire event is delivered before its socket's close event,
// even when the close lands after the frame crossed the socket but before the frame was reported.
// The observer call for the frame is held until the close has closed the socket,
// and the socket's close must not be reported while it is held;
// it is reported by the goroutine that ends the frame's report, once the observer has returned,
// so the close event is awaited, not expected the moment Close returns.
func TestSocketRace_FrameReportPrecedesClose(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name string
		dir  hsms.WireDirection
		// send makes the frame the observer holds cross the socket.
		send func(t *testing.T, sut *endpoint, raw *net.TCPConn)
	}{
		{
			name: "inbound",
			dir:  hsms.WireInbound,
			send: func(t *testing.T, _ *endpoint, raw *net.TCPConn) {
				t.Helper()
				_, err := raw.Write(dataFrame(0, 1, 1, [4]byte{0, 0, 0, 0x42}, nil))
				require.NoError(t, err)
			},
		},
		{
			name: "outbound",
			dir:  hsms.WireOutbound,
			send: func(t *testing.T, sut *endpoint, _ *net.TCPConn) {
				t.Helper()
				go func() { _, _ = sut.conn.SendDataMessage(context.Background(), 1, 1, false, secs2.A("x")) }()
			},
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()

			log := &socketLog{}
			isData := func(ev hsms.WireEvent) bool { return ev.Direction == tt.dir && ev.Frame[4+5] == byte(hsms.DataMsgType) }
			// Hold the data frame's report before it is logged: the window between the frame crossing the socket and its report.
			observe, entered, release := holdWire(log, isData)
			t.Cleanup(release)

			sut, raw := selectedActive(t, log, WithConnectionOption(hsms.WithWireObserver(observe)))
			id := log.waitKind(t, hsms.SocketConnected, 1)[0].Socket

			tt.send(t, sut, raw)
			<-entered

			done := make(chan error, 1)
			go func() { done <- sut.conn.Close() }()

			requirePeerDrained(t, raw) // the close has closed the socket
			require.Never(t, func() bool { return log.countKind(id, hsms.SocketClosed) > 0 },
				100*time.Millisecond, 5*time.Millisecond, "the close must not be reported while the frame's report is pending")

			release()
			require.NoError(t, <-done)

			log.waitClosed(t, id)
			requireFrameBeforeClose(t, log, id, isData, "frame")
		})
	}
}

// requirePeerDrained reads raw until the SUT has closed it, discarding whatever frames arrive first.
func requirePeerDrained(t *testing.T, raw net.Conn) {
	t.Helper()

	require.NoError(t, raw.SetReadDeadline(time.Now().Add(5*time.Second)))

	buf := make([]byte, 256)
	for {
		if _, err := raw.Read(buf); err != nil {
			var ne net.Error
			require.False(t, errors.As(err, &ne) && ne.Timeout(), "the SUT did not close the socket")

			return
		}
	}
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

// refusalObserverRT is the receive-path test runtime plus both observation capabilities,
// so a transport driven without the core reports its sockets and frames to the test.
// It carries no generation capability, so an adopted socket reaches the runtime through TCPUp.
type refusalObserverRT struct {
	*recRT

	log     *socketLog
	observe func(hsms.WireEvent)
}

func (r *refusalObserverRT) ObserveSocket(ev hsms.SocketEvent) { r.log.observeSocket(ev) }
func (r *refusalObserverRT) ObserveWire(ev hsms.WireEvent)     { r.observe(ev) }

// An observer panic during the delivery of a refused socket's frames still releases the socket's report scope,
// as the panic unwinds the delivering goroutine:
// the refused socket, already closed by the exchange's end, is reported closed once, after the frame delivered before the panic,
// and the frame the panic skipped is never reported.
// The panic itself is not recovered.
//
// The delivery runs on a goroutine of its own in production, which nothing can recover on,
// so the test calls its body directly, the way the goroutine would.
func TestSocketObserver_RefusalReportPanicReleasesScope(t *testing.T) {
	t.Parallel()

	const boom = "refusal observer boom"

	server, client := net.Pipe()
	t.Cleanup(func() { _ = server.Close() })

	log := &socketLog{}
	tr := &transport{}
	tr.rt = &refusalObserverRT{recRT: newRecRT(), log: log, observe: func(ev hsms.WireEvent) {
		log.observeWire(ev)
		if ev.Direction == hsms.WireInbound {
			panic(boom)
		}
	}}

	rec := tr.mintSocket(client, hsms.SocketAccepted)
	tr.observeSocket(rec.event(hsms.SocketRefused, nil, time.Now()))
	capture := tr.captureRefusal(rec)
	require.NotNil(t, capture, "the runtime offers a wire observer and the socket is open")

	capture.inbound(selectReqFrame([4]byte{0, 0, 0, 9}))
	capture.outbound(buildControlFrame(byte(hsms.SelectRspType), byte(hsms.SelectStatusAlreadyActive), [4]byte{0, 0, 0, 9}), time.Now())

	rec.close(nil) // the exchange's deferred close: the scope is held, so the close report is left to its release
	require.Zero(t, log.countKind(rec.id, hsms.SocketClosed), "the close must not be reported while the frames' report is pending")

	require.PanicsWithValue(t, boom, capture.deliver, "the observer's panic unwinds the delivery")

	entries := log.of(rec.id)
	require.Len(t, entries, 4, "accepted, refused, the inbound frame, closed: %+v", entries)
	require.Equal(t, hsms.SocketAccepted, entries[0].sock.Kind)
	require.Equal(t, hsms.SocketRefused, entries[1].sock.Kind)
	require.True(t, entries[2].isWire)
	require.Equal(t, hsms.WireInbound, entries[2].wire.Direction)
	require.Equal(t, hsms.SocketClosed, entries[3].sock.Kind)
	require.Equal(t, 1, log.countKind(rec.id, hsms.SocketClosed))

	_, err := server.Read(make([]byte, 1))
	require.ErrorIs(t, err, io.EOF, "the socket was closed before the report")
}

// A Close that lands while the receive goroutine is parked inside an inbound wire observer call
// does not wait for that call:
// it returns within the close timeout, reporting that bounded join as ErrCloseTimeout,
// and the socket's close is not reported while the frame's report is pending.
// Once the observer returns, the frame's event is followed by exactly one SocketClosed,
// delivered by the receive goroutine as it ends the frame's report.
func TestSocketRace_CloseDoesNotWaitForInboundReport(t *testing.T) {
	t.Parallel()

	const closeTimeout = 200 * time.Millisecond

	log := &socketLog{}
	isData := func(ev hsms.WireEvent) bool {
		return ev.Direction == hsms.WireInbound && ev.Frame[4+5] == byte(hsms.DataMsgType)
	}
	observe, entered, release := holdWire(log, isData)

	sut, raw := selectedActive(t, log,
		WithConnectionOption(hsms.WithWireObserver(observe)),
		WithConnectionOption(hsms.WithCloseTimeout(closeTimeout)))
	// Registered after selectedActive's cleanups, so it runs before them:
	// a failing run releases the observer before the connection's own Close.
	t.Cleanup(release)
	id := log.waitKind(t, hsms.SocketConnected, 1)[0].Socket

	_, err := raw.Write(dataFrame(0, 1, 1, [4]byte{0, 0, 0, 0x42}, nil))
	require.NoError(t, err)
	<-entered // the receive goroutine holds the frame's report

	done := make(chan error, 1)
	go func() { done <- sut.conn.Close() }()

	select {
	case err := <-done:
		require.ErrorIs(t, err, hsms.ErrCloseTimeout, "the receive goroutine parked in the observer is abandoned by the bounded join")
	case <-time.After(25 * closeTimeout):
		t.Fatal("Close waited for the inbound observer call to return")
	}

	requirePeerDrained(t, raw) // the close has closed the socket
	require.Zero(t, log.countKind(id, hsms.SocketClosed), "the close must not be reported while the frame's report is pending")

	release()

	log.waitClosed(t, id)
	requireFrameBeforeClose(t, log, id, isData, "frame")
	require.Equal(t, 1, log.countKind(id, hsms.SocketClosed))
}

// A Close that lands while the wire observer is parked on the courtesy Separate does not wait for that call:
// the Separate is delivered from a goroutine of its own, so the teardown starts and Close returns,
// and the socket's close is not reported while the frame's report is pending.
// Once the observer returns, the Separate's event is followed by exactly one SocketClosed,
// delivered by the goroutine that reported the frame.
func TestSocketRace_CloseDoesNotWaitForFarewellReport(t *testing.T) {
	t.Parallel()

	const closeTimeout = 200 * time.Millisecond

	log := &socketLog{}
	isSeparate := func(ev hsms.WireEvent) bool {
		return ev.Direction == hsms.WireOutbound && ev.Frame[4+5] == byte(hsms.SeparateReqType)
	}
	observe, entered, release := holdWire(log, isSeparate)

	sut, raw := selectedActive(t, log,
		WithConnectionOption(hsms.WithWireObserver(observe)),
		WithConnectionOption(hsms.WithCloseTimeout(closeTimeout)))
	// Registered after selectedActive's cleanups, so it runs before them:
	// a failing run releases the observer before the connection's own Close.
	t.Cleanup(release)
	id := log.waitKind(t, hsms.SocketConnected, 1)[0].Socket

	done := make(chan error, 1)
	go func() { done <- sut.conn.Close() }()

	select {
	case <-entered: // the Separate was written and its report is parked
	case <-time.After(5 * time.Second):
		t.Fatal("the graceful Close did not write the courtesy Separate")
	}

	select {
	case err := <-done:
		require.NoError(t, err, "nothing Close joins is parked in the observer")
	case <-time.After(25 * closeTimeout):
		t.Fatal("Close waited for the courtesy Separate's observer call to return")
	}

	requirePeerDrained(t, raw) // the peer read the Separate and saw the socket close
	require.Zero(t, log.countKind(id, hsms.SocketClosed), "the close must not be reported while the frame's report is pending")

	release()

	log.waitClosed(t, id)
	requireFrameBeforeClose(t, log, id, isSeparate, "Separate")
	require.Equal(t, 1, log.countKind(id, hsms.SocketClosed))
}

// A Stop that lands while the wire observer is parked on a refused socket's inbound frame does not wait for that call:
// the exchange's frames are delivered from a goroutine of their own once the exchange has ended,
// so the accept goroutine is free to be joined and Close returns.
// The refused socket's close is not reported while the frames' report is pending;
// once the observer returns, its events are accepted, refused, the inbound frame, the outbound frame, closed, each once.
func TestSocketRace_StopDoesNotWaitForRefusalReport(t *testing.T) {
	t.Parallel()

	const closeTimeout = 200 * time.Millisecond

	log := &socketLog{}
	// Only a refused socket's frames carry generation 0.
	observe, entered, release := holdWire(log, func(ev hsms.WireEvent) bool {
		return ev.Direction == hsms.WireInbound && ev.Generation == 0
	})

	sut, _, port := selectedPassive(t, log,
		WithConnectionOption(hsms.WithWireObserver(observe)),
		WithConnectionOption(hsms.WithCloseTimeout(closeTimeout)))
	// Registered after selectedPassive's cleanups, so it runs before them.
	t.Cleanup(release)
	live := log.waitKind(t, hsms.SocketAccepted, 1)[0]

	raw := dialPassive(t, port)
	defer func() { _ = raw.Close() }()
	refused := log.waitKind(t, hsms.SocketAccepted, 2)[1]
	require.Eventually(t, func() bool { return log.countKind(refused.Socket, hsms.SocketRefused) == 1 },
		5*time.Second, 2*time.Millisecond, "the refusal is reported before the exchange")

	_, err := raw.Write(selectReqFrame([4]byte{0, 0, 0, 9}))
	require.NoError(t, err)

	select {
	case <-entered: // the refused socket's inbound frame is parked in the observer
	case <-time.After(5 * time.Second):
		t.Fatal("the refused socket's inbound frame was not reported")
	}

	done := make(chan error, 1)
	go func() { done <- sut.conn.Close() }()

	select {
	case err := <-done:
		require.NoError(t, err, "nothing Stop joins is parked in the observer")
	case <-time.After(25 * closeTimeout):
		t.Fatal("Close waited for the refused socket's observer call to return")
	}

	require.Zero(t, log.countKind(refused.Socket, hsms.SocketClosed), "the close must not be reported while the frames' report is pending")

	// The exchange ran to its end before any report: the peer holds the Select.rsp and then sees the socket close.
	rsp, err := peerReadFrame(raw, 5*time.Second)
	require.NoError(t, err)
	require.Equal(t, byte(hsms.SelectStatusAlreadyActive), rsp[3])
	requirePeerClosed(t, raw)

	release()

	closed := log.waitClosed(t, refused.Socket)
	require.NoError(t, closed.Err, "the exchange completed, so the close reports no failure")

	entries := log.of(refused.Socket)
	require.Len(t, entries, 5, "accepted, refused, the inbound frame, the outbound frame, closed: %+v", entries)
	require.Equal(t, hsms.SocketAccepted, entries[0].sock.Kind)
	require.Equal(t, hsms.SocketRefused, entries[1].sock.Kind)
	require.True(t, entries[2].isWire)
	require.Equal(t, hsms.WireInbound, entries[2].wire.Direction)
	require.Equal(t, byte(hsms.SelectReqType), entries[2].wire.Frame[4+5])
	require.True(t, entries[3].isWire)
	require.Equal(t, hsms.WireOutbound, entries[3].wire.Direction)
	require.Equal(t, byte(hsms.SelectRspType), entries[3].wire.Frame[4+5])
	require.Equal(t, hsms.SocketClosed, entries[4].sock.Kind)
	require.Equal(t, 1, log.countKind(refused.Socket, hsms.SocketRefused))
	require.Equal(t, 1, log.countKind(refused.Socket, hsms.SocketClosed))
	// The Close closed the live socket too; its close report can trail Close behind the courtesy Separate's delivery.
	log.waitClosed(t, live.Socket)
	require.Equal(t, 1, log.countKind(live.Socket, hsms.SocketClosed))
}

// A SocketClosed delivered late, by the goroutine that ends the socket's last report scope,
// still carries the time the socket was closed, not the time the event was delivered:
// its At falls between the Close call and Close's return, which the closing socket precedes,
// and never after the moment the parked observer was released.
func TestSocketRace_DelayedCloseCarriesCloseTime(t *testing.T) {
	t.Parallel()

	const closeTimeout = 200 * time.Millisecond

	log := &socketLog{}
	observe, entered, release := holdWire(log, func(ev hsms.WireEvent) bool {
		return ev.Direction == hsms.WireInbound && ev.Frame[4+5] == byte(hsms.DataMsgType)
	})

	sut, raw := selectedActive(t, log,
		WithConnectionOption(hsms.WithWireObserver(observe)),
		WithConnectionOption(hsms.WithCloseTimeout(closeTimeout)))
	t.Cleanup(release)
	id := log.waitKind(t, hsms.SocketConnected, 1)[0].Socket

	_, err := raw.Write(dataFrame(0, 1, 1, [4]byte{0, 0, 0, 0x42}, nil))
	require.NoError(t, err)
	<-entered // the receive goroutine holds the frame's report

	before := time.Now()
	err = sut.conn.Close()
	after := time.Now()
	require.ErrorIs(t, err, hsms.ErrCloseTimeout, "the receive goroutine parked in the observer is abandoned by the bounded join")
	require.Zero(t, log.countKind(id, hsms.SocketClosed), "the close must not be reported while the frame's report is pending")

	release()

	closed := log.waitClosed(t, id)
	require.False(t, closed.At.Before(before), "the close time is not before Close was called")
	require.False(t, closed.At.After(after), "the close time is not after Close returned: it is when the socket was closed, not when the event was delivered")
}

// heldCloseConn is a conn whose Close parks until the test releases it,
// so a test can observe the gate between its decision and the conn's close.
type heldCloseConn struct {
	net.Conn

	once    sync.Once
	entered chan struct{}
	release chan struct{}
}

func (c *heldCloseConn) Close() error {
	c.once.Do(func() { close(c.entered) })
	<-c.release

	return c.Conn.Close()
}

// Once the gate has decided the close, no report scope opens any more,
// even while the winner is still inside the conn's Close:
// a frame a caller-supplied conn still hands out after that point is moved without being reported.
// The close itself is reported once, after the conn is closed.
func TestSocketObserver_NoScopeOnceCloseDecided(t *testing.T) {
	t.Parallel()

	server, client := net.Pipe()
	t.Cleanup(func() { _ = server.Close() })

	conn := &heldCloseConn{Conn: client, entered: make(chan struct{}), release: make(chan struct{})}
	log := &socketLog{}
	tr := &transport{}
	tr.rt = &refusalObserverRT{recRT: newRecRT(), log: log, observe: log.observeWire}

	rec := tr.mintSocket(conn, hsms.SocketConnected)

	done := make(chan struct{})
	go func() {
		defer close(done)
		rec.close(nil)
	}()

	select {
	case <-conn.entered:
	case <-time.After(5 * time.Second):
		t.Fatal("the gate's winner did not reach the conn's Close")
	}

	require.Nil(t, rec.openReport(), "no report scope opens once the close is decided")
	require.Zero(t, log.countKind(rec.id, hsms.SocketClosed), "the close is not reported before the conn is closed")

	close(conn.release)
	select {
	case <-done:
	case <-time.After(5 * time.Second):
		t.Fatal("the gate did not complete")
	}

	require.Equal(t, 1, log.countKind(rec.id, hsms.SocketClosed))
	require.Nil(t, rec.openReport(), "no report scope opens after the close")
}
