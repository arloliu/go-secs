package hsmsss

import (
	"context"
	"errors"
	"time"

	"github.com/arloliu/go-secs/v2/hsms"
	"github.com/arloliu/go-secs/v2/internal/pool"
)

// errLinktestFailed is the TCPDown cause when linktestFailThreshold consecutive linktest
// transactions time out (T6): the link is presumed dead, so it is dropped and the engine reconnects.
var errLinktestFailed = errors.New("hsmsss: linktest failure threshold exceeded")

// startLinktest launches the auto-linktest goroutine for a freshly-entered Selected session (D5a-5).
// It runs on the recv goroutine (the sole caller of CommitSelected), which passes its captured
// generation bundle g (NEW-1) so the linktest goroutine registers on — and this generation's Stop
// joins — g, never t.wg. The interval is read ONCE here from the live config, so a reconfig applies
// only on the NEXT entry to Selected. A zero/negative interval disables auto-linktest (no goroutine).
// The goroutine is cancelled by stopLinktest (Deselect) or by genCtx cancellation (teardown/drop).
func (t *transport) startLinktest(g *genWG) {
	interval := t.rt.LinktestInterval()
	if interval <= 0 {
		return
	}

	t.connMu.Lock()
	// Defensive: a prior session's cancel should already be cleared (we only reach a true
	// NotSelected->Selected commit while not Selected). Cancel any stale one before overwriting.
	if t.linktestCancel != nil {
		t.linktestCancel()
	}
	ctx, cancel := context.WithCancel(t.genCtx)
	t.linktestCancel = cancel
	g.linktest.Add(1)
	t.connMu.Unlock()

	go t.runLinktest(ctx, g, interval)
}

// stopLinktest cancels the current Selected session's auto-linktest goroutine (recv goroutine, on a
// Deselect responder transition Selected->NotSelected). It does NOT join — the goroutine exits
// promptly on ctx cancellation and is reaped by Stop's g.linktest.Wait; a brief overlap with a
// subsequently re-spawned goroutine is benign (each has its own fail counter and sends are byte-atomic).
func (t *transport) stopLinktest() {
	t.connMu.Lock()
	if t.linktestCancel != nil {
		t.linktestCancel()
		t.linktestCancel = nil
	}
	t.connMu.Unlock()
}

// runLinktest periodically runs a T6-bound linktest transaction while Selected (D5a-5). It exits on
// ctx cancellation (teardown / Deselect / drop) or when the FSM has left Selected. linktestFailThreshold
// consecutive T6 timeouts drive an involuntary disconnect (rt.TCPDown) so the engine reconnects. The
// interval is fixed for this session (captured by startLinktest); there is deliberately no live re-read.
func (t *transport) runLinktest(ctx context.Context, g *genWG, interval time.Duration) {
	defer g.linktest.Done()

	threshold := t.rt.LinktestFailThreshold()
	fails := 0

	timer := pool.GetTimer(interval)
	defer pool.PutTimer(timer)

	for {
		select {
		case <-ctx.Done():
			return
		case <-timer.C:
		}

		if t.rt.State() != hsms.SelectedState {
			return // left Selected (Deselect / drop) — nothing to linktest
		}

		t6 := t.rt.Timers().T6
		lctx, cancel := context.WithTimeout(ctx, t6)
		_, err := t.rt.WriteMessage(lctx, hsms.NewLinktestReq(t.rt.NextSystemBytes()))
		cancel()

		if err != nil {
			// A cancelled PARENT ctx (teardown / Deselect / drop) is not a linktest failure.
			if ctx.Err() != nil {
				return
			}
			fails++
			if fails >= threshold {
				t.rt.TCPDown(errLinktestFailed)
				return
			}
		} else {
			fails = 0
		}

		timer.Reset(interval)
	}
}

// armT7 starts the T7 NOT-SELECTED dwell goroutine for a freshly-entered NotSelected state (§9.2.2).
// It runs on the recv goroutine (recvLoop entry for NotConnected->NotSelected, and handleDeselectReq
// for Selected->NotSelected), which passes its captured generation bundle g (NEW-1) so the T7
// goroutine registers on — and this generation's Stop joins — g, never t.wg. A zero/negative T7
// disables the dwell (no goroutine). The goroutine is cancelled by cancelT7 (on reaching Selected) or
// by genCtx cancellation (teardown/drop). Cancellation is an optimization: even without it, the core
// no-ops a stale evT7Timeout from Selected/NotConnected.
func (t *transport) armT7(g *genWG) {
	d := t.rt.Timers().T7
	if d <= 0 {
		return
	}

	t.connMu.Lock()
	if t.t7Cancel != nil {
		t.t7Cancel() // defensive: cancel a stale arm before overwriting (recv-goroutine-only path)
	}
	ctx, cancel := context.WithCancel(t.genCtx)
	t.t7Cancel = cancel
	g.t7.Add(1)
	t.connMu.Unlock()

	go t.runT7(ctx, g, d)
}

// cancelT7 cancels the current NotSelected-entry's T7 goroutine (recv goroutine, on reaching Selected).
// It does NOT join — the goroutine exits promptly on ctx cancellation and is reaped by Stop's g.t7.Wait.
func (t *transport) cancelT7() {
	t.connMu.Lock()
	if t.t7Cancel != nil {
		t.t7Cancel()
		t.t7Cancel = nil
	}
	t.connMu.Unlock()
}

// runT7 is the one-shot T7 dwell timer. On expiry it injects evT7Timeout via rt.T7Expired(); the
// supervisor no-ops it unless still NotSelected (§9.2.2), so NO State() re-check is needed here. It
// exits early on ctx cancellation (reaching Selected / teardown / drop).
func (t *transport) runT7(ctx context.Context, g *genWG, d time.Duration) {
	defer g.t7.Done()

	timer := pool.GetTimer(d)
	defer pool.PutTimer(timer)

	select {
	case <-ctx.Done():
		return
	case <-timer.C:
		t.rt.T7Expired()
	}
}
