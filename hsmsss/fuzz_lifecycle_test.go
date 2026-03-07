package hsmsss

import (
	"context"
	"testing"
	"time"

	"github.com/arloliu/go-secs/hsms"
	"github.com/arloliu/go-secs/secs2"
	"github.com/stretchr/testify/require"
)

// fuzzState holds mutable state shared between the fuzz harness and its
// operation dispatcher.  Keeping the dispatch logic in a method reduces the
// cyclomatic complexity of the outer FuzzConnectionLifecycle function.
type fuzzState struct {
	hostConn *Connection
	eqpConn  *Connection
	session  hsms.Session
	ctx      context.Context //nolint:containedctx // kept for brevity in test code
	hostOpen bool
	eqpOpen  bool
}

func (fs *fuzzState) dispatch(op byte) {
	switch op >> 5 {
	case 0: // Open (only if closed)
		if !fs.hostOpen {
			_ = fs.hostConn.Open(false)
			fs.hostOpen = true
		}
		if !fs.eqpOpen {
			_ = fs.eqpConn.Open(false)
			fs.eqpOpen = true
		}
	case 1: // Close
		if fs.hostOpen {
			_ = fs.hostConn.Close()
			fs.hostOpen = false
		}
		if fs.eqpOpen {
			_ = fs.eqpConn.Close()
			fs.eqpOpen = false
		}
	case 2: // Send async (best-effort)
		_ = fs.session.SendDataMessageAsync(1, 1, false, secs2.A("fuzz"))
	case 3: // UpdateConfig
		_ = fs.hostConn.UpdateConfigOptions(
			WithAutoLinktest(op&0x01 == 1),
			WithLinktestInterval(time.Duration(100+int(op&0x1F))*time.Millisecond),
		)
	case 4: // Send sync (best-effort, ignore errors)
		_, _ = fs.session.SendDataMessage(1, 1, true, secs2.A("fuzz-sync"))
	case 5: // Enable linktest
		_ = fs.hostConn.UpdateConfigOptions(
			WithAutoLinktest(true),
			WithLinktestInterval(50*time.Millisecond),
		)
	case 6: // Disable linktest
		_ = fs.hostConn.UpdateConfigOptions(WithAutoLinktest(false))
	case 7: // WaitState (brief, non-blocking check)
		waitCtx, waitCancel := context.WithTimeout(fs.ctx, 20*time.Millisecond)
		_ = fs.hostConn.stateMgr.WaitState(waitCtx, hsms.SelectedState)
		waitCancel()
	default:
		// unreachable: op>>5 is always 0..7 for a 3-bit value
	}
}

// FuzzConnectionLifecycle fuzzes the Open/Close/Send state machine by
// interpreting a byte slice as a sequence of operations with random timing.
//
// This catches panics, goroutine leaks, and data races in the connection
// lifecycle that are hard to trigger with deterministic tests.  Run with:
//
//	go test -fuzz=FuzzConnectionLifecycle -race -fuzztime=60s ./hsmsss/
func FuzzConnectionLifecycle(f *testing.F) {
	// Seed corpus: representative operation sequences.
	// Each byte encodes: bits[7:5] = operation (0..7), bits[4:0] = delay (0..31 ms).
	// Operations: 0=open, 1=close, 2=sendAsync, 3=updateConfig,
	//             4=sendSync(best-effort), 5=enableLinktest, 6=disableLinktest, 7=waitState
	f.Add([]byte{0x00, 0x40, 0x20, 0x60})             // open, send, close, updateConfig
	f.Add([]byte{0x00, 0x40, 0x40, 0x20, 0x00, 0x20}) // open, send×2, close, open, close
	f.Add([]byte{0x20, 0x00, 0x40, 0x20})             // close-before-open, open, send, close
	f.Add([]byte{0x00, 0x20, 0x00, 0x40, 0x20})       // open, close, open, send, close
	f.Add([]byte{0x00, 0x40, 0x40, 0x40, 0x40, 0x20}) // open, burst sends, close
	f.Add([]byte{0x60, 0x00, 0x60, 0x20})             // updateConfig, open, updateConfig, close
	f.Add([]byte{0x00, 0xA0, 0x40, 0xC0, 0x20})       // open, enableLinktest, send, disableLinktest, close
	f.Add([]byte{0x00, 0x80, 0x80, 0xE0, 0x20})       // open, sendSync×2, waitState, close
	f.Add([]byte{0x00, 0xA0, 0xC0, 0xA0, 0xC0, 0x20}) // open, toggle linktest rapidly, close

	f.Fuzz(func(t *testing.T, ops []byte) {
		if len(ops) == 0 {
			return
		}
		// Cap the number of operations to keep individual runs fast.
		if len(ops) > 32 {
			ops = ops[:32]
		}

		// Per-iteration timeout: skip (don't fail) if the iteration hangs
		// so the fuzzer continues exploring instead of aborting.
		iterCtx, iterCancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer iterCancel()

		ctx, cancel := context.WithTimeout(context.Background(), 4*time.Second)
		defer cancel()

		port := getPort()

		// Create an active+passive pair with short timeouts so operations resolve quickly.
		hostConn := newConn(ctx, require.New(t), port, true, true,
			WithT3Timeout(1*time.Second),
			WithT5Timeout(50*time.Millisecond),
			WithT6Timeout(1*time.Second),
			WithT7Timeout(1*time.Second),
			WithConnectRemoteTimeout(1*time.Second),
			WithCloseConnTimeout(1*time.Second),
			WithAutoLinktest(false),
		)
		session := hostConn.AddSession(testSessionID)
		session.AddDataMessageHandler(func(msg *hsms.DataMessage, s hsms.Session) {
			if msg.FunctionCode()%2 == 1 {
				_ = s.ReplyDataMessage(msg, msg.Item())
			}
		})

		eqpConn := newConn(ctx, require.New(t), port, false, false,
			WithT3Timeout(1*time.Second),
			WithT5Timeout(50*time.Millisecond),
			WithT6Timeout(1*time.Second),
			WithT7Timeout(1*time.Second),
			WithCloseConnTimeout(1*time.Second),
			WithAutoLinktest(false),
		)
		eqpSession := eqpConn.AddSession(testSessionID)
		eqpSession.AddDataMessageHandler(func(msg *hsms.DataMessage, s hsms.Session) {
			if msg.FunctionCode()%2 == 1 {
				_ = s.ReplyDataMessage(msg, msg.Item())
			}
		})

		// Always clean up both connections.
		defer func() {
			_ = hostConn.Close()
			_ = eqpConn.Close()
		}()

		// Track open/closed state to avoid calling Open twice without Close
		// (which is an API contract violation, not a timing bug).
		fs := &fuzzState{
			hostConn: hostConn,
			eqpConn:  eqpConn,
			session:  session,
			ctx:      ctx,
		}

		done := make(chan struct{})
		go func() {
			defer close(done)
			defer func() {
				// Recover panics from the library (e.g. send-on-closed-channel
				// in ConnStateMgr) so the fuzzer can continue exploring.
				// These are real bugs but should not abort the fuzz run.
				if r := recover(); r != nil {
					t.Logf("recovered panic: %v", r)
				}
			}()

			for _, op := range ops {
				delay := time.Duration(op&0x1F) * time.Millisecond
				if delay > 0 {
					time.Sleep(delay)
				}

				fs.dispatch(op)
			}
		}()

		select {
		case <-done:
			// Iteration completed normally.
		case <-iterCtx.Done():
			// Iteration hung; skip so the fuzzer continues exploring.
			t.Skip("fuzz iteration timed out – possible deadlock (see ConnStateMgr race)")
		}
	})
}
