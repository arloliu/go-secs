package hsmsss

// byte_chaos_proxy_test.go — sub-frame byte-fault MITM proxy.
//
// ByteChaosProxy is a frame-aware proxy that supports faults the filter-driven
// ChaosProxy cannot express:
//
//   - Write only the first N bytes of the 4-byte length header, then hold
//     (drives the partial-length T8 path in messageReader.ReadMessage).
//   - Write the full length header but only the first M bytes of payload,
//     then hold (drives the mid-payload T8 path).
//   - Overwrite payload[4] (PType) or payload[5] (SType) of the next frame
//     in the chosen direction so the receiver sees an unsupported header
//     and must answer with Reject.req.
//   - Replace the 4-byte length header with an arbitrary value (drives the
//     length-too-large rejection path in messageReader.ReadMessage).
//
// One-shot faults are configured per direction (client→target and
// target→client) and self-clear after firing once.  After the fault is
// consumed the proxy reverts to plain forwarding for that direction.
//
// ByteChaosProxy is intentionally parallel to ChaosProxy, not a refactor:
// scenarios that fit the existing filter-driven frame proxy stay there;
// ByteChaosProxy owns only what that proxy cannot do.
//
// Tests that USE ByteChaosProxy live in T29.

import (
	"encoding/binary"
	"fmt"
	"io"
	"net"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
)

// byteFaultKind identifies the fault to apply to the next frame.
type byteFaultKind int

const (
	byteFaultForward            byteFaultKind = iota
	byteFaultPartialLengthHold                // write first N bytes of the 4-byte length header then hold
	byteFaultPartialPayloadHold               // write full length header + first M bytes of payload then hold
	byteFaultSubstitutePType                  // overwrite payload[4] (PType) before forwarding
	byteFaultSubstituteSType                  // overwrite payload[5] (SType) before forwarding
	byteFaultOverrideLength                   // replace the 4-byte length header with an arbitrary value
)

// byteFaultSpec describes a one-shot fault for the next frame in one direction.
//
//   - Kind selects which fault to apply.
//   - PrefixBytes is the number of bytes to write before the proxy holds
//     (PartialLength: 1..3; PartialPayload: 0..len(payload)-1).
//   - Hold is how long to wait after the partial write before closing the
//     connection.  Should exceed the receiver-side T8 to drive a timeout.
//   - PTypeValue / STypeValue / LengthValue carry the substitution data.
type byteFaultSpec struct {
	Kind        byteFaultKind
	PrefixBytes int
	Hold        time.Duration
	PTypeValue  byte
	STypeValue  byte
	LengthValue uint32
}

// ByteChaosProxy is a frame-aware MITM proxy with per-direction one-shot byte faults.
type ByteChaosProxy struct {
	listener net.Listener
	target   string

	mu         sync.Mutex
	clientConn net.Conn
	targetConn net.Conn

	// One-shot fault specs — read atomically by each pump; cleared once applied.
	nextC2T atomic.Pointer[byteFaultSpec]
	nextT2C atomic.Pointer[byteFaultSpec]

	// Observed Reject.req frames in each direction, recorded by their reason
	// code (header[3]).  Used by tests that need to assert the host emitted a
	// Reject.req with a specific reason.
	observedRejectsC2T []byte // reason codes, in order seen (client→target direction)
	observedRejectsT2C []byte // reason codes, in order seen (target→client direction)
	observeMu          sync.Mutex

	wg     sync.WaitGroup
	closed atomic.Bool
}

// newByteChaosProxy creates a ByteChaosProxy that proxies to targetPort on
// 127.0.0.1.
//
//nolint:unused // T29 byte-fault scenario tests call this constructor.
func newByteChaosProxy(t *testing.T, targetPort int) *ByteChaosProxy {
	t.Helper()

	l, err := net.Listen("tcp", "127.0.0.1:0")
	require.NoError(t, err)

	return &ByteChaosProxy{
		listener: l,
		target:   fmt.Sprintf("127.0.0.1:%d", targetPort),
	}
}

// Port returns the OS-assigned port that the proxy listens on.
func (bp *ByteChaosProxy) Port() int {
	addr, ok := bp.listener.Addr().(*net.TCPAddr)
	if !ok {
		return 0
	}

	return addr.Port
}

// QueuePartialLengthHold makes the next frame in the chosen direction write
// only the first n bytes of its 4-byte length header to the destination, then
// hold for the given duration before closing the connection.  The destination's
// receiver sees a partial length header and (after T8) should report
// ErrT8Timeout.
//
// n must be in 1..3.
func (bp *ByteChaosProxy) QueuePartialLengthHold(toClient bool, n int, hold time.Duration) {
	bp.setFault(toClient, &byteFaultSpec{
		Kind:        byteFaultPartialLengthHold,
		PrefixBytes: n,
		Hold:        hold,
	})
}

// QueuePartialPayloadHold makes the next frame write the full 4-byte length
// header plus the first n bytes of payload, then hold.  n must be <
// len(payload).
func (bp *ByteChaosProxy) QueuePartialPayloadHold(toClient bool, n int, hold time.Duration) {
	bp.setFault(toClient, &byteFaultSpec{
		Kind:        byteFaultPartialPayloadHold,
		PrefixBytes: n,
		Hold:        hold,
	})
}

// QueueSubstitutePType overwrites payload[4] of the next frame with the
// supplied value before forwarding.  Used to drive the
// pType != 0 → Reject.req(PTypeNotSupported) path in messageReader.
func (bp *ByteChaosProxy) QueueSubstitutePType(toClient bool, pType byte) {
	bp.setFault(toClient, &byteFaultSpec{
		Kind:       byteFaultSubstitutePType,
		PTypeValue: pType,
	})
}

// QueueSubstituteSType overwrites payload[5] of the next frame before
// forwarding.
func (bp *ByteChaosProxy) QueueSubstituteSType(toClient bool, sType byte) {
	bp.setFault(toClient, &byteFaultSpec{
		Kind:       byteFaultSubstituteSType,
		STypeValue: sType,
	})
}

// QueueOverrideLength replaces the next frame's 4-byte length header with the
// given uint32 (big-endian) before forwarding.  The actual payload bytes that
// follow will be of the original length, so the destination blocks on ReadFull
// waiting for bytes that never come — callers should ensure their test's
// deadline or T8 catches this.
func (bp *ByteChaosProxy) QueueOverrideLength(toClient bool, length uint32) {
	bp.setFault(toClient, &byteFaultSpec{
		Kind:        byteFaultOverrideLength,
		LengthValue: length,
	})
}

func (bp *ByteChaosProxy) setFault(toClient bool, spec *byteFaultSpec) {
	if toClient {
		bp.nextT2C.Store(spec)
	} else {
		bp.nextC2T.Store(spec)
	}
}

// ObservedRejects returns the reason codes of Reject.req frames seen in the
// given direction.  toClient=false means client→target (host→equipment),
// which is where a Reject.req sent BY THE HOST travels.  Returns a copy so
// callers can safely iterate.
func (bp *ByteChaosProxy) ObservedRejects(toClient bool) []byte {
	bp.observeMu.Lock()
	defer bp.observeMu.Unlock()

	var src []byte
	if toClient {
		src = bp.observedRejectsT2C
	} else {
		src = bp.observedRejectsC2T
	}

	out := make([]byte, len(src))
	copy(out, src)

	return out
}

// Start begins accepting connections in a background goroutine.
func (bp *ByteChaosProxy) Start(t *testing.T) {
	t.Helper()

	bp.wg.Go(func() {
		for {
			clientConn, err := bp.listener.Accept()
			if err != nil {
				return
			}

			bp.mu.Lock()
			bp.clientConn = clientConn
			bp.mu.Unlock()

			targetConn, err := net.Dial("tcp", bp.target)
			if err != nil {
				_ = clientConn.Close()

				continue
			}

			bp.mu.Lock()
			bp.targetConn = targetConn
			bp.mu.Unlock()

			bp.wg.Add(2)

			go bp.pump(clientConn, targetConn, false) // client → target
			go bp.pump(targetConn, clientConn, true)  // target → client
		}
	})
}

// Stop closes the listener and all active connections, then waits for all
// goroutines to exit.  Idempotent.
func (bp *ByteChaosProxy) Stop() {
	if bp.closed.CompareAndSwap(false, true) {
		_ = bp.listener.Close()
		bp.CloseConnections()
		bp.wg.Wait()
	}
}

// CloseConnections closes the current active client and target connections.
func (bp *ByteChaosProxy) CloseConnections() {
	bp.mu.Lock()
	defer bp.mu.Unlock()

	if bp.clientConn != nil {
		_ = bp.clientConn.Close()
		bp.clientConn = nil
	}

	if bp.targetConn != nil {
		_ = bp.targetConn.Close()
		bp.targetConn = nil
	}
}

// pump reads one frame at a time from src, applies any queued one-shot fault,
// and writes the (possibly mutated / partial / replaced) bytes to dst.  Errors
// at any point close both ends.
func (bp *ByteChaosProxy) pump(src, dst net.Conn, toClient bool) {
	defer bp.wg.Done()
	defer func() { _ = src.Close() }()
	defer func() { _ = dst.Close() }()

	for {
		lenBuf, payload, ok := bp.readFrame(src)
		if !ok {
			return
		}

		bp.recordIfReject(payload, toClient)

		spec := bp.consumeFault(toClient)
		if !bp.applyFault(dst, lenBuf, payload, spec) {
			return
		}
	}
}

// readFrame reads one length-prefixed HSMS frame from src.  Returns false on
// any error or absurd length, which the caller treats as terminal.
func (bp *ByteChaosProxy) readFrame(src net.Conn) (lenBuf, payload []byte, ok bool) {
	lenBuf = make([]byte, 4)

	if _, err := io.ReadFull(src, lenBuf); err != nil {
		return nil, nil, false
	}

	length := binary.BigEndian.Uint32(lenBuf)

	// Cap to a sane upper bound to avoid a malicious-length allocation in the
	// proxy; the real receiver will already have failed if length is absurd.
	if length > 10*1024*1024 {
		return nil, nil, false
	}

	payload = make([]byte, length)

	if _, err := io.ReadFull(src, payload); err != nil {
		return nil, nil, false
	}

	return lenBuf, payload, true
}

// recordIfReject appends the reason code (payload[3]) to the per-direction
// observed list when the payload carries SType=7 (Reject.req).
func (bp *ByteChaosProxy) recordIfReject(payload []byte, toClient bool) {
	if len(payload) < 10 || payload[5] != 7 {
		return
	}

	bp.observeMu.Lock()
	defer bp.observeMu.Unlock()

	if toClient {
		bp.observedRejectsT2C = append(bp.observedRejectsT2C, payload[3])
	} else {
		bp.observedRejectsC2T = append(bp.observedRejectsC2T, payload[3])
	}
}

func (bp *ByteChaosProxy) consumeFault(toClient bool) *byteFaultSpec {
	if toClient {
		return bp.nextT2C.Swap(nil)
	}

	return bp.nextC2T.Swap(nil)
}

// applyFault writes the frame to dst with any requested mutation.  Returns
// true to continue the pump loop, false to terminate.
func (bp *ByteChaosProxy) applyFault(dst net.Conn, lenBuf, payload []byte, spec *byteFaultSpec) bool {
	if spec == nil {
		return writeFrameBytes(dst, lenBuf, payload)
	}

	switch spec.Kind {
	case byteFaultPartialLengthHold:
		return bp.applyPartialLength(dst, lenBuf, spec)
	case byteFaultPartialPayloadHold:
		return bp.applyPartialPayload(dst, lenBuf, payload, spec)
	case byteFaultSubstitutePType:
		if len(payload) >= 10 {
			payload[4] = spec.PTypeValue
		}

		return writeFrameBytes(dst, lenBuf, payload)
	case byteFaultSubstituteSType:
		if len(payload) >= 10 {
			payload[5] = spec.STypeValue
		}

		return writeFrameBytes(dst, lenBuf, payload)
	case byteFaultOverrideLength:
		var newLen [4]byte

		binary.BigEndian.PutUint32(newLen[:], spec.LengthValue)
		_, _ = dst.Write(newLen[:])

		// Don't write any payload — the destination blocks on ReadFull waiting
		// for the (probably absurd) length and either fails validation or times out.
		return false
	case byteFaultForward:
		fallthrough
	default:
		return writeFrameBytes(dst, lenBuf, payload)
	}
}

func (bp *ByteChaosProxy) applyPartialLength(dst net.Conn, lenBuf []byte, spec *byteFaultSpec) bool {
	n := min(max(spec.PrefixBytes, 1), 4)

	_, _ = dst.Write(lenBuf[:n])
	time.Sleep(spec.Hold)

	return false
}

func (bp *ByteChaosProxy) applyPartialPayload(dst net.Conn, lenBuf, payload []byte, spec *byteFaultSpec) bool {
	if _, err := dst.Write(lenBuf); err != nil {
		return false
	}

	n := max(min(spec.PrefixBytes, len(payload)), 0)

	_, _ = dst.Write(payload[:n])
	time.Sleep(spec.Hold)

	return false
}

// writeFrameBytes writes a complete length-prefixed HSMS frame to dst.
func writeFrameBytes(dst net.Conn, lenBuf, payload []byte) bool {
	if _, err := dst.Write(lenBuf); err != nil {
		return false
	}

	if _, err := dst.Write(payload); err != nil {
		return false
	}

	return true
}
