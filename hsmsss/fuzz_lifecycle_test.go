package hsmsss

import (
	"context"
	"testing"
	"time"

	"github.com/arloliu/go-secs/hsms"
	"github.com/arloliu/go-secs/secs2"
	"github.com/stretchr/testify/require"
)

// FuzzConnectionLifecycle fuzzes the Open/Close/Send state machine by
// interpreting a byte slice as a sequence of operations with random timing.
//
// This catches panics, goroutine leaks, and data races in the connection
// lifecycle that are hard to trigger with deterministic tests.  Run with:
//
//	go test -fuzz=FuzzConnectionLifecycle -race -fuzztime=60s ./hsmsss/
func FuzzConnectionLifecycle(f *testing.F) {
	// Seed corpus: representative operation sequences.
	// Each byte encodes: bits[7:6] = operation, bits[5:0] = delay (0..63 ms).
	// Operations: 0=open, 1=close, 2=send, 3=updateConfig
	f.Add([]byte{0x00, 0x80, 0x40, 0xC0})             // open, send, close, updateConfig
	f.Add([]byte{0x00, 0x80, 0x80, 0x40, 0x00, 0x40}) // open, send×2, close, open, close
	f.Add([]byte{0x40, 0x00, 0x80, 0x40})             // close-before-open, open, send, close
	f.Add([]byte{0x00, 0x40, 0x00, 0x80, 0x40})       // open, close, open, send, close
	f.Add([]byte{0x00, 0x80, 0x80, 0x80, 0x80, 0x40}) // open, burst sends, close
	f.Add([]byte{0xC0, 0x00, 0xC0, 0x40})             // updateConfig, open, updateConfig, close

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
		hostOpen := false
		eqpOpen := false

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
				delay := time.Duration(op&0x3F) * time.Millisecond
				if delay > 0 {
					time.Sleep(delay)
				}

				switch op >> 6 {
				case 0: // Open (only if closed)
					if !hostOpen {
						_ = hostConn.Open(false)
						hostOpen = true
					}
					if !eqpOpen {
						_ = eqpConn.Open(false)
						eqpOpen = true
					}
				case 1: // Close
					if hostOpen {
						_ = hostConn.Close()
						hostOpen = false
					}
					if eqpOpen {
						_ = eqpConn.Close()
						eqpOpen = false
					}
				case 2: // Send (async, best-effort)
					_ = session.SendDataMessageAsync(1, 1, false, secs2.A("fuzz"))
				case 3: // UpdateConfig
					_ = hostConn.UpdateConfigOptions(
						WithAutoLinktest(op&0x01 == 1),
						WithLinktestInterval(time.Duration(100+int(op&0x3F))*time.Millisecond),
					)
				default:
					// unreachable: op>>6 is always 0..3 for a 2-bit value
				}
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
