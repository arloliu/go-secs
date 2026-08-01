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

// suppressionRuntime is the optional capability that activity-based linktest suppression
// needs beyond hsms.TransportRuntime. The hsms connection core implements both methods on
// its (unexported) connection type; startLinktest discovers them via a type assertion.
// Kept package-local ON PURPOSE: widening the exported TransportRuntime interface would
// compile-break external implementers and the in-repo mocks (mockRuntime/mockRT/recRT).
// A runtime that lacks the capability simply runs with suppression off — the mocks in the
// transport unit tests therefore exercise the pre-suppression cadence unchanged.
type suppressionRuntime interface {
	LinktestSuppression() bool
	DataMsgInflight() int64
}

// startLinktest launches the auto-linktest goroutine for a freshly-entered Selected session (D5a-5).
// It runs on the recv goroutine (the sole caller of CommitSelected), which passes its captured
// generation bundle g (NEW-1) so the linktest goroutine registers on — and this generation's Stop
// joins — g, never t.wg. The interval and the suppression capability/flag are read ONCE here from
// the live config, so a reconfig applies only on the NEXT entry to Selected. A zero/negative
// interval disables auto-linktest (no goroutine). The goroutine is cancelled by stopLinktest
// (Deselect) or by genCtx cancellation (teardown/drop).
func (t *transport) startLinktest(g *genWG) {
	interval := t.rt.LinktestInterval()
	if interval <= 0 {
		return
	}

	// Suppression capability + flag, resolved once per Selected entry (like interval): a
	// live reconfig applies on the NEXT entry. sr == nil (capability missing or knob off)
	// runs the loop with suppression disabled.
	var sr suppressionRuntime
	if s, ok := t.rt.(suppressionRuntime); ok && s.LinktestSuppression() {
		sr = s
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

	go t.runLinktest(ctx, g, interval, sr)
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

// linktestFailureStep is the pure state reducer for a failed probe round-trip (D5a-5
// suppression rule 3). Inputs are the loop's snapshot at failure-evaluation time; outputs
// are the new consecutive-failure run state and whether the failure was credited.
//
//   - suppress off: always counts (pre-suppression v2 semantics).
//   - recvNow > sentAt: a frame arrived after the probe went out — credit, run resets.
//   - inflight > 0: a data send won the write path between the probe's pre-send check and
//     its write; the peer sits inside a T3-guarded transaction — credit (race closure).
//   - fails > 0 && recvNow > recvAtLastFail: life between counted failures — run restarts
//     at 1 with this failure.
//   - otherwise: consecutive-in-silence — increment the run.
//
// Note "life" is receive-side only plus inflight: our own send stamps never forgive a
// failure (a write success proves local TCP buffering, not the peer).
func linktestFailureStep(suppress bool, recvNow, sentAt, inflight int64, fails int, recvAtLastFail int64) (newFails int, newRecvAtLastFail int64, credited bool) {
	if suppress && (recvNow > sentAt || inflight > 0) {
		return 0, recvAtLastFail, true
	}
	if suppress && fails > 0 && recvNow > recvAtLastFail {
		return 1, recvNow, false
	}

	return fails + 1, recvNow, false
}

// linktestDisconnectRecheck is the pure decision for runLinktest's final pre-disconnect
// re-check (D5a-5 suppression, accepted-races (b)): a W-bit send may have become inflight —
// or a frame arrived — after the failure snapshot linktestFailureStep evaluated. inflight and
// recvNow must be read fresh at call time (not reused from the earlier snapshot) for the
// re-check to close that race. Returns true to proceed with TCPDown, false to convert the
// failure to a credit instead.
func linktestDisconnectRecheck(suppress bool, inflight, recvNow, sentAt int64) bool {
	if !suppress {
		return true
	}

	return inflight <= 0 && recvNow <= sentAt
}

// runLinktest periodically runs a T6-bound linktest transaction while Selected (D5a-5). It exits on
// ctx cancellation (teardown / Deselect / drop) or when the FSM has left Selected. linktestFailThreshold
// consecutive T6 timeouts drive an involuntary disconnect (rt.TCPDown) so the engine reconnects. The
// interval is fixed for this session (captured by startLinktest); there is deliberately no live re-read.
// sr is the activity-suppression capability resolved once by startLinktest; sr == nil runs the
// pre-suppression cadence unchanged (suppression capability missing or the knob is off).
func (t *transport) runLinktest(ctx context.Context, g *genWG, interval time.Duration, sr suppressionRuntime) {
	defer g.linktest.Done()

	threshold := t.rt.LinktestFailThreshold()
	fails := 0
	recvAtLastFail := int64(0)

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

		if sr != nil {
			// Activity suppression rule 1: probe only a line that has been silent for a full
			// interval. A frame in either direction within the window already proves the link,
			// so re-arm for the remainder and skip — the timer converges on (lastActivity +
			// interval) without any cross-goroutine timer sharing.
			if idle := t.sinceLastActivity(); idle < interval {
				t.metrics.incLinktestSuppressed()
				timer.Reset(interval - idle)

				continue
			}

			// Activity suppression rule 2: a sent data message awaiting its reply means the
			// peer may be busy processing a long-running command; the T3 reply timer already
			// bounds that wait, so probing now only risks a spurious T6 on slow equipment.
			// Re-check at full-interval cadence.
			if sr.DataMsgInflight() > 0 {
				t.metrics.incLinktestSuppressed()
				timer.Reset(interval)

				continue
			}
		}

		sentAt := t.monoNanos()
		t6 := t.rt.Timers().T6
		lctx, cancel := context.WithTimeout(ctx, t6)
		t.metrics.incLinktestSend()
		_, err := t.rt.WriteMessage(lctx, hsms.NewLinktestReq(t.rt.NextSystemBytes()))
		cancel()

		if err != nil {
			// A cancelled PARENT ctx (teardown / Deselect / drop) is not a linktest failure.
			if ctx.Err() != nil {
				return
			}
			t.metrics.incLinktestErr()

			recvNow := t.lastRecvStamp.Load()
			var inflight int64
			if sr != nil {
				inflight = sr.DataMsgInflight()
			}

			prevRecvAtLastFail := recvAtLastFail

			var credited bool
			fails, recvAtLastFail, credited = linktestFailureStep(sr != nil, recvNow, sentAt, inflight, fails, recvAtLastFail)
			if credited {
				// The cumulative err metric above still counts the failure; only the
				// CONSECUTIVE counter driving the disconnect is spared.
				t.metrics.incLinktestCredited()
			}

			if fails >= threshold {
				// Final pre-disconnect re-check (accepted-races (b)): re-read inflight/recvNow
				// fresh (not the snapshot above) so a W-bit send that became inflight, or a
				// frame that arrived, after that snapshot still converts this to a credit
				// rather than dropping a link that just showed life. The remaining unguarded
				// window is the instructions between this check and TCPDown (documented,
				// accepted).
				var finalInflight int64
				if sr != nil {
					finalInflight = sr.DataMsgInflight()
				}

				if linktestDisconnectRecheck(sr != nil, finalInflight, t.lastRecvStamp.Load(), sentAt) {
					t.rt.TCPDown(errLinktestFailed)
					return
				}

				// Credited here ⇒ run state untouched: restore recvAtLastFail to its
				// pre-reducer value, matching the reducer's own credit branch (which leaves
				// recvAtLastFail unchanged) instead of the counting branch's overwrite above.
				recvAtLastFail = prevRecvAtLastFail
				t.metrics.incLinktestCredited()
				fails = 0
			}
		} else {
			t.metrics.incLinktestRecv()
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
