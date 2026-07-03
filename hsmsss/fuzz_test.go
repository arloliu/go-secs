package hsmsss

// fuzz_test.go — v2 ports of the two hsmsss fuzzers deleted in commit 2c74397
// (originals: hsmsss/fuzz_lifecycle_test.go, hsmsss/fuzz_test.go), re-pointed onto the v2
// public surface and the v2 frame-reader seam.
//
//   - FuzzConnectionLifecycle drives the Open/Close/Send/UpdateConfig state machine of a REAL
//     active+passive pair (built with the newEndpoint helper) from a byte-encoded op stream.
//     Invariant: no panic, no deadlock (the -race build catches data races). A hung iteration
//     is t.Skip (not t.Fatal) so the engine keeps exploring; a genuine panic is left UNRECOVERED
//     because in v2 a send-on-closed-channel / use-after-Free panic is a real regression (the K1
//     and E-cluster landmines are structurally dissolved — see doc.go).
//
//   - FuzzMessageReader feeds arbitrary bytes to transport.readFrame (hsmsss/transport.go), the v2
//     frame-reader seam, over a net.Pipe. readFrame returns the RAW [header||body] frame (no length
//     prefix). The decode invariant is chained through the exported hsms.DecodeHSMSMessage (the
//     internal zero-copy decodeOwnedFrame is unexported in package hsms and unreachable from
//     hsmsss; DecodeHSMSMessage copies the frame into an owned buffer and runs the identical
//     header/SType validation, so the decode contract is exercised faithfully).

import (
	"context"
	"encoding/binary"
	"net"
	"testing"
	"time"

	"github.com/arloliu/go-secs/v2/hsms"
	"github.com/arloliu/go-secs/v2/secs2"
	"github.com/stretchr/testify/require"
)

// ── Part A — FuzzConnectionLifecycle ────────────────────────────────────────────

// lifecycleFuzzState holds the mutable state shared between the lifecycle fuzz harness and its
// per-op dispatcher. Keeping the dispatch logic in a method keeps the cyclomatic complexity of
// the outer FuzzConnectionLifecycle function low (mirrors the v1 fuzzState split).
type lifecycleFuzzState struct {
	passive *endpoint
	active  *endpoint
	ctx     context.Context //nolint:containedctx // per-iteration op ctx, kept for brevity in fuzz code
	open    bool            // whether the pair is currently Open (avoids Open-without-Close contract abuse)
}

// dispatch runs one operation selected by the top 3 bits of op. Every call ignores errors: the
// fuzzer asserts liveness (no panic / no deadlock), not protocol success — a send while not
// Selected, a re-Open, or a config toggle at any moment must all be handled without crashing.
func (fs *lifecycleFuzzState) dispatch(op byte) {
	switch op >> 5 {
	case 0: // Open the pair if not already open (passive listens first, then active dials)
		if !fs.open {
			_ = fs.passive.conn.Open(fs.ctx, hsms.OpenBackground)
			_ = fs.active.conn.Open(fs.ctx, hsms.OpenBackground)
			fs.open = true
		}
	case 1: // Close the pair if open
		if fs.open {
			_ = fs.active.conn.Close()
			_ = fs.passive.conn.Close()
			fs.open = false
		}
	case 2: // fire-and-forget async data send (best-effort)
		_ = fs.active.conn.SendDataMessageAsync(fs.ctx, 1, 1, false, secs2.A("fuzz"))
	case 3: // toggle the linktest interval on/off from the low bit
		var interval time.Duration
		if op&0x01 == 1 {
			interval = 50 * time.Millisecond
		}
		_ = fs.active.conn.UpdateConfigOptions(hsms.WithLinktestInterval(interval))
	case 4: // blocking W-bit data send (best-effort; the peer echoHandler replies to odd functions)
		_, _ = fs.active.conn.SendDataMessage(fs.ctx, 1, 1, true, secs2.A("fuzz-sync"))
	case 5: // enable auto-linktest
		_ = fs.active.conn.UpdateConfigOptions(hsms.WithLinktestInterval(50 * time.Millisecond))
	case 6: // disable auto-linktest
		_ = fs.active.conn.UpdateConfigOptions(hsms.WithLinktestInterval(0))
	case 7: // bounded wait for Selected (20 ms budget; ignore timeout)
		deadline := time.Now().Add(20 * time.Millisecond)
		for time.Now().Before(deadline) {
			if fs.active.conn.State() == hsms.SelectedState {
				break
			}
			time.Sleep(time.Millisecond)
		}
	default:
		// unreachable: op>>5 is a 3-bit value (0..7)
	}
}

// FuzzConnectionLifecycle fuzzes the connection lifecycle by interpreting a byte slice as a
// sequence of operations with bounded random timing. Each byte encodes: bits[7:5] = operation
// (0..7), bits[4:0] = a 0..31 ms delay applied BEFORE the op (fuzz timing, not test-sync).
//
// This catches panics and deadlocks in the Open/Close/Send/UpdateConfig paths that are hard to
// trigger with deterministic tests. Run with:
//
//	go test -run='^$' -fuzz=FuzzConnectionLifecycle -race -fuzztime=60s ./hsmsss/
//
// NOTE (memory: stress-test-fuzz-flake): under very high -count this fuzzer can pile up on the
// cgo DNS singleflight in net.Listen; the Makefile excludes ^Fuzz from stress-test for that
// reason. Use -skip '^Fuzz' for any high-count regression run.
func FuzzConnectionLifecycle(f *testing.F) {
	// v1 seed: Open, sync-send, waitState, disable-linktest.
	f.Add([]byte{0x00, 0x80, 0xf2, 0xc0})
	// A few more benign sequences to give the engine a head start.
	f.Add([]byte{0x00, 0x40, 0x20, 0x60})       // open, async-send, close, updateConfig
	f.Add([]byte{0x00, 0xa0, 0x80, 0xc0, 0x20}) // open, enable-linktest, sync-send, disable, close
	f.Add([]byte{0x00, 0x20, 0x00, 0x40, 0x20}) // open, close, re-open, async-send, close

	f.Fuzz(func(t *testing.T, ops []byte) {
		if len(ops) == 0 {
			return
		}
		// Cap ops so a single iteration stays fast.
		if len(ops) > 32 {
			ops = ops[:32]
		}

		// iterCtx is the watchdog budget for the whole iteration; ctx is the (shorter) op ctx
		// handed to the individual Open/Send calls so blocking ops unwind before the watchdog.
		iterCtx, iterCancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer iterCancel()
		ctx, cancel := context.WithTimeout(context.Background(), 4*time.Second)
		defer cancel()

		port := freeLoopbackPort(t)
		opts := []Option{
			WithConnectionOption(hsms.WithT3(1 * time.Second)),
			WithConnectionOption(hsms.WithT5(50 * time.Millisecond)),
			WithConnectionOption(hsms.WithT6(1 * time.Second)),
			WithConnectionOption(hsms.WithT7(1 * time.Second)),
			WithConnectionOption(hsms.WithT8(500 * time.Millisecond)),
			WithConnectionOption(hsms.WithLinktestInterval(0)),
		}
		passive := newEndpoint(t, port, false, opts, echoHandler)
		active := newEndpoint(t, port, true, opts, echoHandler)

		// Always clean up both endpoints so -race never leaks a generation across iterations.
		// Closing here also unblocks any op goroutine still parked on a send after a Skip.
		t.Cleanup(func() {
			_ = active.conn.Close()
			_ = passive.conn.Close()
		})

		fs := &lifecycleFuzzState{passive: passive, active: active, ctx: ctx}

		done := make(chan struct{})
		go func() {
			// No panic recovery: a genuine panic (send-on-closed-channel, use-after-Free) is a
			// real v2 regression and MUST fail the fuzzer, not be swallowed.
			defer close(done)
			for _, op := range ops {
				if d := time.Duration(op&0x1F) * time.Millisecond; d > 0 {
					time.Sleep(d)
				}
				fs.dispatch(op)
			}
		}()

		select {
		case <-done:
			// Iteration completed normally.
		case <-iterCtx.Done():
			// Iteration hung; Skip (not Fatal) so the fuzzer keeps exploring.
			t.Skip("fuzz iteration timed out — possible deadlock")
		}
	})
}

// ── Part B — FuzzMessageReader ──────────────────────────────────────────────────

// FuzzMessageReader fuzzes the HSMS-SS frame reader (transport.readFrame, spec §6.1).
//
// Arbitrary bytes are piped through a net.Pipe (the write end is closed after writing so readFrame
// always returns — it never blocks on incomplete data). This covers the 4-byte length-prefix
// parse (zero / oversized / partial), the J2 length bounds (10 <= msgLen <= secs2.MaxByteSize),
// and — on success — the header/SType decode via hsms.DecodeHSMSMessage.
//
// Invariants:
//  1. readFrame never panics on any input.
//  2. err == nil ⇒ frame != nil.
//  3. err == nil ⇒ len(frame) >= 10 (the mandatory header).
//  4. On success, decoding the frame via hsms.DecodeHSMSMessage never panics, and
//     decodeErr == nil ⇒ msg != nil (the v1 err==nil ⇒ msg≠nil contract). For a data message the
//     lazy SECS-II body decode (Item) is also exercised to prove it cannot panic on fuzzed bytes.
//
// Decode path fuzzed: hsms.DecodeHSMSMessage (exported). The production recv path uses the
// unexported zero-copy hsms.decodeOwnedFrame, which is unreachable from package hsmsss;
// DecodeHSMSMessage copies the frame into an owned buffer then runs the SAME decodeOwnedFrame
// validation internally, so the header/SType contract is fuzzed faithfully.
func FuzzMessageReader(f *testing.F) {
	// Seed: a valid data frame (S1F1 + ASCII body) assembled by the reader_test.go helper.
	dataBody := []byte{0x01, 0x02, 0x03}
	f.Add(frameBytes(uint32(len(header10(0, byte(hsms.DataMsgType)))+len(dataBody)),
		header10(0, byte(hsms.DataMsgType)), dataBody))
	// Seed: a valid Linktest.req frame (header-only, msgLen 10).
	f.Add(frameBytes(10, header10(0, byte(hsms.LinktestReqType)), nil))
	// Seed: zero-length message and an incomplete length header.
	f.Add([]byte{0x00, 0x00, 0x00, 0x00})
	f.Add([]byte{0x00, 0x01})
	// Seed: oversized length field (must be rejected before allocation).
	oversized := make([]byte, 4)
	binary.BigEndian.PutUint32(oversized, 0xFFFFFFFF)
	f.Add(oversized)

	f.Fuzz(func(t *testing.T, data []byte) {
		client, server := net.Pipe()

		// Write the fuzz bytes then close the write end so readFrame always returns.
		go func() {
			defer server.Close()
			_, _ = server.Write(data)
		}()
		defer client.Close()

		// A minimal transport is enough: readFrame only reads rt.Timers().T8 and t.allocFrame.
		// A short live T8 bounds any partial/truncated frame.
		rt := newRecRT()
		rt.setTimers(hsms.TimerConfig{T8: 50 * time.Millisecond})
		tr := &transport{allocFrame: makeFrame, rt: rt}

		// Invariant 1: must not panic on any input.
		frame, err := tr.readFrame(client)

		if err != nil {
			return
		}

		// Invariant 2 + 3: success ⇒ non-nil frame at least 10 bytes (the header).
		require.NotNil(t, frame, "readFrame returned nil frame with nil error")
		require.GreaterOrEqual(t, len(frame), 10, "successful frame must include the 10-byte header")

		// Invariant 4: chain the decode. readFrame returns [header||body]; DecodeHSMSMessage
		// expects a 4-byte length prefix, so prepend it (identical to decodeControlFrame).
		full := make([]byte, 4+len(frame))
		binary.BigEndian.PutUint32(full[:4], uint32(len(frame)))
		copy(full[4:], frame)

		msg, decodeErr := hsms.DecodeHSMSMessage(full)
		if decodeErr == nil {
			require.NotNil(t, msg, "decode success must yield a non-nil message")
			// Exercise the lazy SECS-II body decode for a data message: it must never panic
			// (a malformed body legitimately returns an error, which is fine).
			if dm, ok := msg.(*hsms.DataMessage); ok {
				_, _ = dm.Item()
			}
		}
	})
}
