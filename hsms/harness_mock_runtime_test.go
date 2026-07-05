package hsms

import (
	"context"
	"net"
	"sync"
	"testing"
	"time"
)

// mockRuntime is a test double implementing [TransportRuntime].
//
// Most methods are no-ops or return canned values. WriteMessage and SendAsync record
// calls for assertion. Done() returns a controllable channel; call closeDone() to
// simulate connection teardown.
//
// This is the seed of the Task-26 consolidated mock; keep it minimal and sufficient
// for session tests only.
type mockRuntime struct {
	t         *testing.T
	sessionID uint16
	sysGen    sysBytesGen

	doneCh   chan struct{}
	doneOnce sync.Once

	mu          sync.Mutex
	writeCalled bool
	writeMsg    Message
	writeReply  Message
	writeErr    error

	asyncCalled bool
	asyncMsg    Message
	asyncErr    error
}

// newMockRuntime creates a MockRuntime with SessionID=0xFFFF and an open Done channel.
func newMockRuntime(t *testing.T) *mockRuntime {
	t.Helper()

	return &mockRuntime{
		t:         t,
		sessionID: 0xFFFF,
		doneCh:    make(chan struct{}),
	}
}

// closeDone simulates connection teardown by closing the Done channel. Idempotent.
func (m *mockRuntime) closeDone() {
	m.doneOnce.Do(func() {
		close(m.doneCh)
	})
}

// ── TransportRuntime implementation ──────────────────────────────────────────

func (m *mockRuntime) TCPUp(_ net.Conn) {}
func (m *mockRuntime) TCPDown(_ error)  {}
func (m *mockRuntime) SelectLost()      {}
func (m *mockRuntime) T7Expired()       {}

func (m *mockRuntime) CommitSelected() (committed bool) { return false }

func (m *mockRuntime) DeliverOwnedFrame(_ []byte) error { return nil }

func (m *mockRuntime) RouteReply(_ Message) bool { return false }

func (m *mockRuntime) RouteData(_ *DataMessage) error { return nil }

// WriteMessage records the call and returns the configured canned reply/error.
func (m *mockRuntime) WriteMessage(_ context.Context, msg Message) (Message, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.writeCalled = true
	m.writeMsg = msg

	return m.writeReply, m.writeErr
}

// WriteMessageNoReply records the call and returns the configured canned write error (no reply
// is ever awaited on this path).
func (m *mockRuntime) WriteMessageNoReply(_ context.Context, msg Message) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.writeCalled = true
	m.writeMsg = msg

	return m.writeErr
}

// SendAsync records the call and returns the configured canned error.
func (m *mockRuntime) SendAsync(_ context.Context, msg Message) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.asyncCalled = true
	m.asyncMsg = msg

	return m.asyncErr
}

func (m *mockRuntime) State() ConnState      { return SelectedState }
func (m *mockRuntime) Done() <-chan struct{} { return m.doneCh }
func (m *mockRuntime) Timers() TimerConfig   { return TimerConfig{} }
func (m *mockRuntime) SessionID() uint16     { return m.sessionID }

func (m *mockRuntime) LinktestInterval() time.Duration { return 0 }
func (m *mockRuntime) LinktestFailThreshold() int      { return 3 }

// NextSystemBytes returns a fresh System Bytes value from a private generator so the mock
// satisfies the TransportRuntime contract without sharing the connection's generator.
func (m *mockRuntime) NextSystemBytes() [4]byte { return m.sysGen.next() }
