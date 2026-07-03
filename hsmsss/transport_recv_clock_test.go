package hsmsss

// transport_recv_clock_test.go — deterministic coverage of the HSMS-SS receive loop's injectable
// clock (transport.now, default time.Now, read via the nil-safe clock() accessor). It proves the T8
// inter-byte read deadline armed by readN/readFrame is computed from the injected clock:
// SetReadDeadline == now().Add(t8). A fixed clock set an hour ahead of the wall clock gives the
// assertion teeth (the pre-fix bare time.Now() would arm a deadline ~1h off).

import (
	"net"
	"sync"
	"testing"
	"time"

	"github.com/arloliu/go-secs/v2/hsms"
	"github.com/stretchr/testify/require"
)

// deadlineRecorder wraps a net.Conn and records the argument of every SetReadDeadline call so a test
// can assert the exact T8 deadline the recv loop armed, while still delegating it to the wrapped conn.
type deadlineRecorder struct {
	net.Conn
	mu   sync.Mutex
	last time.Time
}

func (d *deadlineRecorder) SetReadDeadline(t time.Time) error {
	d.mu.Lock()
	d.last = t
	d.mu.Unlock()

	return d.Conn.SetReadDeadline(t)
}

func (d *deadlineRecorder) lastDeadline() time.Time {
	d.mu.Lock()
	defer d.mu.Unlock()

	return d.last
}

// TestTransportRecvClock_T8Deadline proves readFrame/readN arm the T8 inter-byte read deadline from
// the transport's injected clock. The peer sends one byte (so the frame read progresses past the
// idle-first-byte wait into the T8-governed inter-byte reads) then closes, unblocking the second read
// with an error; by then the T8 deadline has already been recorded.
func TestTransportRecvClock_T8Deadline(t *testing.T) {
	endA, endB := net.Pipe()
	rec := &deadlineRecorder{Conn: endA}
	t.Cleanup(func() { _ = endA.Close() })

	// A fixed clock an hour ahead of the wall clock keeps the armed T8 deadline far in the future (so
	// it never fires on its own — the peer Close is what unblocks) yet clearly distinguishable from
	// real time, giving the assertion teeth against a bare time.Now().
	fakeNow := time.Now().Add(time.Hour)
	const t8 = 40 * time.Millisecond

	rt := newRecRT()
	rt.setTimers(hsms.TimerConfig{T8: t8})
	tr := &transport{allocFrame: makeFrame, rt: rt, now: func() time.Time { return fakeNow }}

	go func() {
		_, _ = endB.Write([]byte{0x00}) // one byte: crosses into the T8-governed inter-byte reads
		_ = endB.Close()                // unblock the next (already-deadline-armed) read
	}()

	_, err := tr.readFrame(rec)
	require.Error(t, err) // truncated/closed frame

	want := fakeNow.Add(t8)
	got := rec.lastDeadline()
	require.True(t, want.Equal(got), "T8 read deadline = %v, want %v", got, want)
}
