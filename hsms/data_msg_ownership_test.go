package hsms

import (
	"sync/atomic"
	"testing"

	"github.com/arloliu/go-secs/secs2"
	"github.com/stretchr/testify/require"
)

// TestDataMessage_Free_PutResetsFreedAndNilsDataItem locks in the architectural
// invariants that the secs1 DataMessage pool double-Free race investigation
// (2026-05-24, tmp/secs1-race-investigation.md) revealed:
//
//  1. putDataMessage resets msg.freed = 0 BEFORE pool.Put. This is what makes
//     the existing CAS guard in DataMessage.Free insufficient against a stale
//     double-Free across pool round-trips. The CAS in Free passes a second
//     time because freed was reset.
//
//  2. putDataMessage sets msg.dataItem = nil BEFORE pool.Put. This is the
//     content-sentinel that allows the build-tagged stale-Free detector in
//     pool_debug.go to identify true stale Frees unambiguously.
//
// Both properties are LOAD-BEARING for the architecture:
//
//   - Property (1) means: ownership discipline at the call site is the only
//     thing preventing pool pollution. Callers MUST NOT double-Free
//     intentionally or via contract violations (e.g., the original bug at
//     secs1/conn_test.go:1644).
//
//   - Property (2) means: getDataMessage's contract is "dataItem is non-nil
//     after Get" (it forces secs2.NewEmptyItem() at pool.go:39-41 if the
//     caller passed nil). The combination of (1) and (2) gives the detector
//     a deterministic signal: `dataItem == nil after Free's CAS` = stale.
//
// If a future change ALTERS either property — for example, by adding a
// generation counter to DataMessage and removing the freed reset, or by
// moving the dataItem=nil assignment to a different point — this test fires
// and forces a re-review. At that point, also revisit:
//
//   - secs1/queue_send_ownership_test.go (the probabilistic stressor)
//   - hsms/pool_debug.go (the content-aware stale-Free detector)
//
// These all assume the invariants below.
func TestDataMessage_Free_PutResetsFreedAndNilsDataItem(t *testing.T) {
	if !IsUsePool() {
		t.Skip("pool disabled; Free behavior differs")
	}

	msg, err := NewDataMessage(1, 1, false, 1, []byte{0, 0, 0, 0}, secs2.A("payload"))
	require.NoError(t, err)
	require.NotNil(t, msg.dataItem, "getDataMessage must set dataItem non-nil")
	require.Equal(t, uint32(0), atomic.LoadUint32(&msg.freed), "fresh Get must have freed=0")

	// Drive Free through one full cycle.
	msg.Free()

	// Invariants observed by the stale-Free detector and relied on by the
	// ownership contract.
	require.Equal(t, uint32(0), atomic.LoadUint32(&msg.freed),
		"putDataMessage MUST reset freed=0 before pool.Put (architectural property #1)")
	require.Nil(t, msg.dataItem,
		"putDataMessage MUST nil dataItem before pool.Put (architectural property #2)")
}

// TestDataMessage_Free_IdempotentWithinSingleCycle verifies that the CAS guard
// in Free DOES protect against double-Free WITHIN a single allocate-free cycle
// (i.e., before putDataMessage runs). This is the half of the guard that
// works correctly. The half that doesn't — stale Free across pool round-trips
// — is the architectural weakness documented in
// TestDataMessage_Free_PutResetsFreedAndNilsDataItem above.
func TestDataMessage_Free_IdempotentWithinSingleCycle(t *testing.T) {
	if !IsUsePool() {
		t.Skip("pool disabled; Free behavior differs")
	}

	msg, err := NewDataMessage(1, 1, false, 1, []byte{0, 0, 0, 0}, secs2.A("payload"))
	require.NoError(t, err)

	// Manually set freed=1 to simulate the in-flight Free state and verify
	// that a concurrent second Free is a no-op (CAS fails).
	require.True(t, atomic.CompareAndSwapUint32(&msg.freed, 0, 1),
		"prep: first CAS must succeed on fresh msg")

	dataItemBefore := msg.dataItem
	msg.Free() // CAS should fail; Free returns early without touching state.
	require.Equal(t, dataItemBefore, msg.dataItem,
		"Free with freed=1 must be a no-op (CAS fails)")
	require.Equal(t, uint32(1), atomic.LoadUint32(&msg.freed),
		"Free with freed=1 must not change freed")

	// Cleanup: drive the real Free so the msg returns to the pool clean.
	atomic.StoreUint32(&msg.freed, 0)
	msg.Free()
}
