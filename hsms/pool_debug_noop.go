//go:build !debugfree

package hsms

// Pool debug hooks — no-op variants for production builds.
//
// The instrumented variants live in pool_debug.go behind `//go:build debugfree`.
// To investigate a DataMessage pool double-Free, rebuild and rerun the test
// suite with `-tags debugfree`. The empty function bodies here compile away to
// nothing under the default build, so there is no runtime cost.

func debugLogGet(msg *DataMessage)  {} //nolint:unused
func debugLogPut(msg *DataMessage)  {} //nolint:unused
func debugLogFree(msg *DataMessage) {} //nolint:unused

// DumpDebugLog flushes the pool-event ring buffer to disk. No-op under the
// default build. The instrumented build writes to /tmp/hsms-pool-debug.log.
func DumpDebugLog() {}
