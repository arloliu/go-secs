package hsms

import (
	"errors"
	"sync"
	"sync/atomic"
	"time"

	"github.com/arloliu/go-secs/v2/internal/throttle"
)

// Compile-time assertions that *connection satisfies BOTH the app-facing Connection
// interface and the transport->core TransportRuntime back-channel (spec §5.4). A single
// concrete engine plays both roles; the circular pointer (session.rt == the connection)
// is wired in NewConnection.
var (
	_ Connection       = (*connection)(nil)
	_ TransportRuntime = (*connection)(nil)
)

// closedDoneChan is a pre-closed channel returned by Done() when there is no live epoch
// (before the first Open or after teardown). Callers use Done() SELECT-ONLY (J5); an
// already-closed channel makes such a select fire immediately rather than nil-block.
var closedDoneChan = func() chan struct{} {
	ch := make(chan struct{})
	close(ch)

	return ch
}()

// connection is the unexported concrete HSMS engine (spec §5.1). It satisfies both the
// app-held Connection interface and the TransportRuntime back-channel, and embeds the
// session so the SECS2Endpoint send/reply/registration surface is promoted onto the
// Connection value.
//
// Lifecycle fields follow the spec §5.1 contract:
//   - cur holds the current per-generation epoch (nil only before the first Open; NOT
//     cleared on Close so idempotent re-Close can return the retained closeErr).
//   - sup holds the E37 logical FSM supervisor, recreated FRESH per Open (no channel reuse).
//   - handlers holds the user StateChangeHandler slice HERE (not on the supervisor) so it
//     persists across Open/Close cycles while the supervisor is recreated.
//   - connectLoopWg joins a dying reconnect loop separately from epoch.wg (§7.C); supWg
//     joins the per-Open supervisor run()+notifier() goroutines (connection-owned).
//
// The full send/lifecycle logic is fleshed by later tasks; this type currently provides
// honest stubs for Open/Close/WriteMessage/SendAsync and the transport-driven callbacks
// that later tasks own (see the per-method TODOs).
type connection struct {
	lifeMu sync.Mutex // serializes Open/Close ENTRY (replaces the v1 ToOpening CAS, H6)

	// publishMu linearizes the reconnect loop's successor publish against a voluntary Close (I1).
	// The reconnect loop holds it across {re-check shutdown/reconnectGen + ArmStart + cur.Store},
	// and Close holds it across {set shutdown + reconnectGen++ + re-pin cur.Load}. This makes Close
	// pin ANY successor the loop just published (so Close tears it down — no orphaned generation) OR
	// forces the loop to observe shutdown first and abandon before publishing. It is NOT lifeMu
	// (Open/Close hold lifeMu across connectLoopWg.Wait(), and the loop must never take lifeMu at its
	// fence — that would deadlock) and is held ONLY for the atomic pin/publish: never across a dial,
	// a Wait, or connectLoopWg.Wait.
	publishMu sync.Mutex

	cur      atomic.Pointer[epoch]                // current generation; nil ONLY before first Open
	sup      atomic.Pointer[supervisor]           // the E37 logical FSM; recreated fresh per Open
	handlers atomic.Pointer[[]StateChangeHandler] // user state-change handlers; persist across Open/Close

	shutdown     atomic.Bool   // set by Close; re-checked by reconnect reactions (F3/G2)
	reconnectGen atomic.Uint64 // bumped by Close/Open; the G2 fence compares against it

	connectLoopWg sync.WaitGroup // SEPARATE reconnect-loop join (§7.C) — NOT epoch.wg
	supWg         sync.WaitGroup // joins the per-Open supervisor run()+notifier()

	// reconnectCancel is closed by Close to promptly interrupt a reconnect loop parked in its
	// T5 backoff, so Close stays bounded even under a long T5. It is created fresh per Open (a
	// closed channel cannot be reused) and closed exactly once by Close. It is deliberately
	// SEPARATE from the supervisor stopCh: the failed-Open rollback stops the supervisor but must
	// NOT cancel reconnect through this channel — the rollback relies on the shutdown fence
	// (E2 reconciliation #2) as its sole reconnect guard.
	reconnectCancel atomic.Pointer[chan struct{}]

	// cfg is an atomic.Pointer so readers (Timers/SessionID and Open's cfg reads) are
	// lock-free and race-free against a concurrent UpdateConfigOptions, which builds a fresh
	// scratch config and atomically Stores it (never mutates the live struct in place). cfgMu
	// serializes the write side so a read-modify-write cannot lose a concurrent update.
	cfg     atomic.Pointer[ConnectionConfig] // shared-core configuration (timers, session ID, close timeout, ...)
	cfgMu   sync.Mutex                       // serializes UpdateConfigOptions callers (the config write side)
	tr      transport                        // the concrete per-transport seam (HSMS-SS / SECS-I)
	metrics ConnectionMetrics                // live lock-free counters

	sysGen sysBytesGen // per-connection System Bytes generator

	// dropWarn rate-limits the B1 not-selected-drop Warn (B3); the metric counter is the
	// authoritative chokepoint and is incremented on every drop regardless.
	dropWarn *throttle.Throttle

	// testHookAfterWriteLock is called by writeFrame immediately after acquiring writeMu
	// but before the B2 write-boundary re-check and the transport Write. It is nil in
	// production (zero cost). The B2 teeth-test sets it to simulate a Selected→NotSelected
	// transition that races past the B1 pre-register gate.
	testHookAfterWriteLock func()

	// testHookConnectLoop is called by the reconnect loop between the T5 backoff and the
	// G2 fence, once per dial attempt. It is nil in production (zero cost). The gen-fence
	// and no-deadlock teeth tests set it to pause the loop at the fence deterministically.
	testHookConnectLoop func()

	*session // embedded: promotes the SECS2Endpoint surface onto the Connection value
}

// NewConnection builds the shared HSMS engine and wires it to tr. It is the
// in-module wiring seam that hsmsss.New / secs1.New call with their concrete transport
// value; it is NOT a documented app-level extension API. cfg must be non-nil.
//
// The returned engine implements TransportRuntime, so it is handed to the session as its
// back-channel (the circular pointer is intentional and safe). The session's persistent
// state-change handler pointer is wired to the connection's handlers field so
// AddConnStateChangeHandler reaches storage that survives Open/Close cycles.
func NewConnection(cfg *ConnectionConfig, tr transport) (Connection, error) {
	if cfg == nil {
		return nil, errors.New("hsms: NewConnection requires a non-nil ConnectionConfig")
	}

	c := &connection{
		lifeMu:        sync.Mutex{},
		cfgMu:         sync.Mutex{},
		tr:            tr,
		shutdown:      atomic.Bool{},
		reconnectGen:  atomic.Uint64{},
		connectLoopWg: sync.WaitGroup{},
		supWg:         sync.WaitGroup{},
		dropWarn:      throttle.New(dropNotSelectedWarnInterval),
	}
	c.cfg.Store(cfg) // atomic publish; UpdateConfigOptions swaps a fresh pointer, never mutates in place

	// The session's back-channel IS this connection (it implements TransportRuntime); the
	// circular pointer is intentional. sysGen lives on the connection and is shared by value
	// address so every generation draws from one monotonic System Bytes space.
	c.session = newSession(cfg.sessionID, c, &c.sysGen)

	// Wire T10's deferred AddConnStateChangeHandler storage to the connection's persistent
	// handler slice (spec §5.1); registration now reaches storage that survives Open/Close.
	// connHandlers is promoted from the embedded session.
	c.connHandlers = &c.handlers

	return c, nil
}

// State returns the current logical E37 state. It nil-guards the supervisor (round-7):
// before the first Open (and after Close, when sup is stopped but still set) it reports
// NotConnectedState and never nil-derefs.
func (c *connection) State() ConnState {
	if s := c.sup.Load(); s != nil {
		return s.State()
	}

	return NotConnectedState
}

// Metrics returns the live connection metrics (lock-free atomic counters).
func (c *connection) Metrics() *ConnectionMetrics {
	return &c.metrics
}

// UpdateConfigOptions applies functional options to the live configuration
// transactionally (validate-all, then commit atomically — T4 apply). It copies the current
// config into a scratch, applies the options to the SCRATCH (validate-all-or-nothing), and
// on success atomically Stores the fresh pointer — it never mutates the live struct in place,
// so concurrent lock-free readers (Timers/SessionID/Open) can never observe a torn or
// half-applied config. The cfgMu lock serializes concurrent writers so a read-modify-write
// cannot lose a competing update.
func (c *connection) UpdateConfigOptions(opts ...ConnOption) error {
	c.cfgMu.Lock()
	defer c.cfgMu.Unlock()

	scratch := *c.cfg.Load() // copy the current live config (value copy)
	if err := scratch.apply(opts...); err != nil {
		return err // validation failed — the live config is untouched (never Stored)
	}
	c.cfg.Store(&scratch) // atomic all-or-nothing commit of the fresh pointer

	return nil
}

// Timers returns the configured protocol timer set (TransportRuntime). Lock-free atomic read.
func (c *connection) Timers() TimerConfig {
	return c.cfg.Load().timers
}

// SessionID returns the configured HSMS session ID (0xFFFF for HSMS-SS control frames,
// TransportRuntime). Lock-free atomic read.
func (c *connection) SessionID() uint16 {
	return c.cfg.Load().sessionID
}

// LinktestInterval returns the live auto-linktest interval (0 = disabled, TransportRuntime).
// Lock-free atomic read: the transport reads it once per entry to Selected, so a reconfig
// applies on the NEXT Selected-entry (D5a-5), never mid-session.
func (c *connection) LinktestInterval() time.Duration {
	return c.cfg.Load().linktestInterval
}

// LinktestFailThreshold returns the live consecutive-timeout count that triggers a
// linktest-failure disconnect (TransportRuntime). Lock-free atomic read.
func (c *connection) LinktestFailThreshold() int {
	return c.cfg.Load().linktestFailThreshold
}

// NextSystemBytes returns the next System Bytes value from the connection's per-connection
// generator (TransportRuntime). It is the single source of unique System Bytes for outbound
// control requests the transport initiates (Select.req, Linktest.req, Separate.req), so those
// draw from the SAME monotonic space as data-message sends (§5.5 / §8.2.6.8). Concurrency-safe.
func (c *connection) NextSystemBytes() [4]byte {
	return c.sysGen.next()
}

// Done returns the current generation's teardown-START signal (TransportRuntime, SELECT-ONLY — J5):
// e.ctx.Done(), which closes the instant teardown begins (epoch.cancel), NOT when the bounded join
// completes (e.done). The session data-handler fan-out selects on it so a handler blocked on a full
// channel unblocks as soon as teardown starts. Returning e.done here would be a CIRCULAR wait: e.done
// closes only after the join, and the join waits (via tr.Stop → recvWg) for this very fan-out to
// return (C1). When no live epoch exists it returns a pre-closed channel so a select never nil-blocks.
func (c *connection) Done() <-chan struct{} {
	if e := c.cur.Load(); e != nil {
		return e.ctx.Done()
	}

	return closedDoneChan
}
