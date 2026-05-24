//go:build debugfree

package hsms

import (
	"fmt"
	"os"
	"runtime"
	"sync/atomic"
	"time"
	"unsafe"
)

// Throwaway instrumentation for the DataMessage pool double-Free race
// investigation (see tmp/secs1-race-investigation.md). Activated via
// `go test -tags debugfree ./...`.
//
// Records every pool Get / Put / Free into a fixed-size ring buffer with a
// lock-free atomic index, so per-event cost is one CAS plus a runtime.Callers
// call (~50–150 ns) and there is no mutex contention to perturb the timing
// window the race lives in. Dump the buffer post-test via DumpDebugLog().
//
// Post-process the log to find any pointer whose event sequence is
// FREE → ... → FREE without an intervening GET. The second FREE without a
// preceding GET is the stale double-Free; its captured stack tells us where.

// debugBufSize is sized for ~4M events. Each entry is ~96 B, so the buffer
// costs roughly 384 MB resident. Adjust if memory is tight.
const debugBufSize = 4 << 20

type debugKind uint8

const (
	debugKindGet  debugKind = 1
	debugKindPut  debugKind = 2
	debugKindFree debugKind = 3
)

type debugEntry struct {
	ns   int64
	msg  uintptr
	pcs  [8]uintptr
	pcsN int32
	kind debugKind
}

var (
	debugBuf [debugBufSize]debugEntry
	debugIdx atomic.Uint64
)

func recordDebugEvent(kind debugKind, msg *DataMessage) {
	i := debugIdx.Add(1) - 1
	if i >= uint64(len(debugBuf)) {
		return // buffer full; drop newest
	}
	e := &debugBuf[i]
	e.ns = time.Now().UnixNano()
	e.kind = kind
	e.msg = uintptr(unsafe.Pointer(msg))
	e.pcsN = int32(runtime.Callers(3, e.pcs[:]))
}

func debugLogGet(msg *DataMessage) { recordDebugEvent(debugKindGet, msg) }
func debugLogPut(msg *DataMessage) { recordDebugEvent(debugKindPut, msg) }
func debugLogFree(msg *DataMessage) {
	// True stale-Free detector: getDataMessage always sets dataItem to a
	// non-nil value (secs2.NewEmptyItem() at minimum). Only putDataMessage
	// sets it back to nil. If Free's CAS succeeded (we're the "owner" of
	// this Free invocation) but dataItem is nil, the msg is in the pool and
	// we are a STALE Free — the actual bug we're hunting. Log loudly and
	// keep going so the test continues and we capture more.
	if msg.dataItem == nil {
		var buf [4096]byte
		n := runtime.Stack(buf[:], false)
		fmt.Fprintf(os.Stderr, "\n\n!!!!! STALE FREE DETECTED on %p (dataItem==nil after CAS) !!!!!\n%s\n", msg, buf[:n])
	}
	recordDebugEvent(debugKindFree, msg)
}

// DumpDebugLog writes all recorded pool events to /tmp/hsms-pool-debug.log.
// Callers (TestMain, t.Cleanup, etc.) should invoke this after the
// suspect tests have run. Safe to call multiple times; each call rewrites
// the file with the current buffer state.
func DumpDebugLog() {
	const path = "/tmp/hsms-pool-debug.log"

	f, err := os.Create(path)
	if err != nil {
		fmt.Fprintf(os.Stderr, "DumpDebugLog: %v\n", err)
		return
	}
	defer f.Close()

	n := debugIdx.Load()
	overflow := false
	if n > uint64(len(debugBuf)) {
		overflow = true
		n = uint64(len(debugBuf))
	}

	fmt.Fprintf(f, "# %d events recorded (overflow=%v); buffer cap %d\n", n, overflow, len(debugBuf))
	fmt.Fprintf(f, "# format: ts_ns kind msgPtr stack...\n")

	for i := uint64(0); i < n; i++ {
		e := &debugBuf[i]

		var kindStr string
		switch e.kind {
		case debugKindGet:
			kindStr = "GET"
		case debugKindPut:
			kindStr = "PUT"
		case debugKindFree:
			kindStr = "FREE"
		default:
			kindStr = "?"
		}

		fmt.Fprintf(f, "%d %s 0x%x", e.ns, kindStr, e.msg)
		if e.pcsN > 0 {
			frames := runtime.CallersFrames(e.pcs[:e.pcsN])
			for {
				frame, more := frames.Next()
				fmt.Fprintf(f, " %s:%d", frame.Function, frame.Line)
				if !more {
					break
				}
			}
		}
		fmt.Fprintln(f)
	}

	fmt.Fprintf(os.Stderr, "DumpDebugLog: wrote %d events to %s (overflow=%v)\n", n, path, overflow)
}
