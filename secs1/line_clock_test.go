package secs1

// line_clock_test.go — deterministic coverage of the SECS-I line engine's injectable clock
// (lineIO.now, default time.Now). It proves the T1/T2 conn read deadlines are computed from the
// injected clock at every deadline-comparison site: the plain read-deadline reads (readByte/readFull)
// AND the send loop's ENQ->EOT wait, which arms deadline := l.now().Add(l.timers().T2) once and then re-derives
// the remaining budget as deadline.Sub(l.now()) on each pass (never wall-clock time.Until). A fixed
// clock set an hour ahead of the real wall clock gives the send-loop assertion teeth: the pre-fix
// wall-clock time.Until(deadline) path would arm a deadline off by ~1h.

import (
	"context"
	"net"
	"sync"
	"testing"
	"time"

	"github.com/arloliu/go-secs/v2/hsms"
	"github.com/stretchr/testify/require"
)

// deadlineRecorder wraps a net.Conn and records the argument of every SetReadDeadline call so a test
// can assert the exact deadline the line engine armed. It delegates the actual deadline to the
// wrapped conn so real reads still honor it.
type deadlineRecorder struct {
	net.Conn
	mu   sync.Mutex
	all  []time.Time
	last time.Time
}

func (d *deadlineRecorder) SetReadDeadline(t time.Time) error {
	d.mu.Lock()
	d.all = append(d.all, t)
	d.last = t
	d.mu.Unlock()

	return d.Conn.SetReadDeadline(t)
}

func (d *deadlineRecorder) lastDeadline() time.Time {
	d.mu.Lock()
	defer d.mu.Unlock()

	return d.last
}

func (d *deadlineRecorder) firstDeadline() time.Time {
	d.mu.Lock()
	defer d.mu.Unlock()

	if len(d.all) == 0 {
		return time.Time{}
	}

	return d.all[0]
}

// newRecordedLinePair mirrors newLinePair but interposes a deadlineRecorder between the lineIO and its
// loopback conn, so a test observes the exact SetReadDeadline arguments the engine arms.
func newRecordedLinePair(t *testing.T, cfg Config) (*lineIO, *deadlineRecorder, net.Conn) {
	t.Helper()

	ln, err := net.Listen("tcp", "127.0.0.1:0")
	require.NoError(t, err)
	t.Cleanup(func() { _ = ln.Close() })

	type accepted struct {
		conn net.Conn
		err  error
	}
	ch := make(chan accepted, 1)
	go func() {
		c, aerr := ln.Accept()
		ch <- accepted{conn: c, err: aerr}
	}()

	local, err := net.Dial("tcp", ln.Addr().String())
	require.NoError(t, err)
	t.Cleanup(func() { _ = local.Close() })

	a := <-ch
	require.NoError(t, a.err)
	t.Cleanup(func() { _ = a.conn.Close() })

	rec := &deadlineRecorder{Conn: local}

	return newLineIO(rec, cfg, cfg.Timers, &ConnectionMetrics{}), rec, a.conn
}

// newRecordedLinePairWithTimers is identical to newRecordedLinePair except it passes the caller's
// timers closure to newLineIO instead of cfg.Timers, so a test can mutate the closure's captured
// value AFTER construction (cfg.Timers can't be mutated post-construction since Config is a value
// type — see TestLineIO_T1LiveUpdate).
func newRecordedLinePairWithTimers(t *testing.T, cfg Config, timers func() hsms.TimerConfig) (*lineIO, *deadlineRecorder, net.Conn) {
	t.Helper()

	ln, err := net.Listen("tcp", "127.0.0.1:0")
	require.NoError(t, err)
	t.Cleanup(func() { _ = ln.Close() })

	type accepted struct {
		conn net.Conn
		err  error
	}
	ch := make(chan accepted, 1)
	go func() {
		c, aerr := ln.Accept()
		ch <- accepted{conn: c, err: aerr}
	}()

	local, err := net.Dial("tcp", ln.Addr().String())
	require.NoError(t, err)
	t.Cleanup(func() { _ = local.Close() })

	a := <-ch
	require.NoError(t, a.err)
	t.Cleanup(func() { _ = a.conn.Close() })

	rec := &deadlineRecorder{Conn: local}

	return newLineIO(rec, cfg, timers, &ConnectionMetrics{}), rec, a.conn
}

// TestLineIO_T1LiveUpdate proves readFull arms T1 from whatever the LIVE timers source currently
// returns, not a value cached at construction — the closure is mutated mid-test, between two reads,
// and the SECOND read's actual armed deadline (via deadlineRecorder, not just l.timers().T1) must
// reflect the new value.
func TestLineIO_T1LiveUpdate(t *testing.T) {
	cfg := newLineTestConfig(t)

	current := hsms.TimerConfig{T1: 5 * time.Second, T2: cfg.T2()}
	timers := func() hsms.TimerConfig { return current }

	line, rec, peer := newRecordedLinePairWithTimers(t, cfg, timers)

	fakeNow := time.Now().Add(time.Hour)
	line.now = func() time.Time { return fakeNow }

	// First read at the ORIGINAL T1 (5s).
	go peerWrite(t, peer, []byte{0x01})
	buf := make([]byte, 1)
	require.NoError(t, line.readFull(buf))
	require.True(t, fakeNow.Add(5*time.Second).Equal(rec.lastDeadline()))

	// Mutate the live source — no lineIO API call, just the closure's captured value changing.
	current.T1 = 250 * time.Millisecond

	// Second read must arm the NEW T1, proving readFull re-reads live rather than caching.
	go peerWrite(t, peer, []byte{0x02})
	require.NoError(t, line.readFull(buf))
	require.True(t, fakeNow.Add(250*time.Millisecond).Equal(rec.lastDeadline()),
		"readFull must arm the LIVE T1 (250ms), not the value cached at construction (5s)")
}

// TestLineClock_ReadByteDeadline proves readByte arms the read deadline from the injected clock:
// SetReadDeadline == l.now().Add(timeout).
func TestLineClock_ReadByteDeadline(t *testing.T) {
	cfg := newLineTestConfig(t)
	line, rec, peer := newRecordedLinePair(t, cfg)

	// A fixed clock an hour ahead of the wall clock: the armed deadline stays in the future (the read
	// never spuriously times out) while being clearly distinguishable from real time.
	fakeNow := time.Now().Add(time.Hour)
	line.now = func() time.Time { return fakeNow }

	go peerWrite(t, peer, []byte{0x42})

	b, err := line.readByte(line.timers().T2)
	require.NoError(t, err)
	require.Equal(t, byte(0x42), b)

	want := fakeNow.Add(line.timers().T2)
	got := rec.lastDeadline()
	require.True(t, want.Equal(got), "readByte deadline = %v, want %v", got, want)
}

// TestLineClock_ReadFullDeadline proves readFull arms the per-read T1 deadline from the injected clock.
func TestLineClock_ReadFullDeadline(t *testing.T) {
	cfg := newLineTestConfig(t)
	line, rec, peer := newRecordedLinePair(t, cfg)

	fakeNow := time.Now().Add(time.Hour)
	line.now = func() time.Time { return fakeNow }

	payload := []byte{0x01, 0x02, 0x03, 0x04}
	go peerWrite(t, peer, payload)

	buf := make([]byte, len(payload))
	require.NoError(t, line.readFull(buf))
	require.Equal(t, payload, buf)

	want := fakeNow.Add(line.timers().T1)
	got := rec.lastDeadline()
	require.True(t, want.Equal(got), "readFull deadline = %v, want %v", got, want)
}

// TestLineClock_SendLoopDeadline drives the send loop's ENQ->EOT wait (sendBlockOnce): it arms
// deadline := l.now().Add(l.timers().T2) and, on each pass, reads with remaining := deadline.Sub(l.now()). With
// the fixed clock, remaining == T2, so the first SetReadDeadline after ENQ equals fakeNow.Add(T2).
// This is the teeth case for the deadline.Sub(l.now()) fix: the pre-fix wall-clock time.Until(deadline)
// would arm a deadline ~1h off.
func TestLineClock_SendLoopDeadline(t *testing.T) {
	cfg := newLineTestConfig(t)
	line, rec, peer := newRecordedLinePair(t, cfg)

	fakeNow := time.Now().Add(time.Hour)
	line.now = func() time.Time { return fakeNow }

	blk := makeTestBlock(t, []byte("hi"))
	wire := blk.appendTo(nil)

	done := make(chan struct{})
	go func() {
		defer close(done)
		_ = peerReadN(t, peer, 1) // ENQ
		peerWrite(t, peer, []byte{eot})
		_ = peerReadN(t, peer, len(wire)) // the transmitted block
		peerWrite(t, peer, []byte{ack})
	}()

	result, err := line.sendBlockOnce(context.Background(), blk)
	require.NoError(t, err)
	require.Equal(t, sendOK, result)
	<-done

	// The FIRST SetReadDeadline is the ENQ->EOT wait (the send loop's deadline.Sub(l.now()) path).
	want := fakeNow.Add(line.timers().T2)
	got := rec.firstDeadline()
	require.True(t, want.Equal(got), "send-loop EOT-wait deadline = %v, want %v", got, want)
}
