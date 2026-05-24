//go:build debugfree

package hsms

import (
	"testing"

	"github.com/arloliu/go-secs/secs2"
	"github.com/stretchr/testify/require"
)

// TestDataMessage_SystemBytesLifetime_PoolReuse documents — by example — the
// lifetime contract on DataMessage.SystemBytes() that was tightened when
// systemBytes became an inline [4]byte field.
//
// The returned slice is a view into the message's internal storage. After
// Free, the underlying message can be re-issued from sync.Pool, and a
// subsequent ID write will silently overwrite the cached bytes. Callers MUST
// NOT retain SystemBytes() past Free.
//
// The test below is best-effort about deterministically forcing the same pool
// slot to be reused. sync.Pool gives no guarantee; we don't try to. What we
// CAN guarantee, and what the test asserts strongly, is the underlying
// invariant: SystemBytes() aliases the struct's array, so mutating the
// message's ID after capturing SystemBytes() is observable through the
// captured slice. That's the property that makes the post-Free / pool-reuse
// scenario unsafe. We then opportunistically attempt the pool-reuse leg and,
// if (and only if) Pool.Get hands the same pointer back, assert that the
// previously cached slice now reflects the new ID. If the pool gives a fresh
// pointer, we log and skip that assertion — without weakening the primary
// alias check.
func TestDataMessage_SystemBytesLifetime_PoolReuse(t *testing.T) {
	require := require.New(t)

	// Primary invariant: SystemBytes() returns a view into the struct's
	// systemBytes array. Mutating the ID must be visible through the cached
	// slice without re-calling SystemBytes().
	msg, err := newDataMessageWithID(1, 1, true, 0, 0x11223344, secs2.NewEmptyItem())
	require.NoError(err)

	cached := msg.SystemBytes()
	require.Equal([]byte{0x11, 0x22, 0x33, 0x44}, cached)

	// In-place mutation: the cached slice header points at the same backing
	// memory the field exposes, so this must be observable without a refetch.
	msg.SetID(0x55667788)
	require.Equal([]byte{0x55, 0x66, 0x77, 0x88}, cached,
		"SystemBytes() must alias the [4]byte field; a mutation via SetID "+
			"is expected to be visible through the previously returned slice")

	// Secondary, best-effort leg: simulate the post-Free pool-reuse hazard
	// using the SAME `cached` slice captured at line 41 (before Free). That
	// is the slice a real caller could be holding when they violate the
	// lifetime contract; comparing fresh re-reads from the reused struct
	// would only prove "two slices into the same array are equal," which is
	// trivial. The load-bearing assertion is that the PRE-Free slice header
	// silently observes the NEW message's ID bytes after pool reuse.
	//
	// sync.Pool does NOT guarantee slot reuse, so this leg is conditional;
	// when it does fire, it's the real hazard demonstration.
	ptrBefore := msg
	msg.Free()

	// Drive the pool a bit to maximize the chance of reuse.
	for i := 0; i < 32; i++ {
		next, err := newDataMessageWithID(2, 1, true, 0, uint32(0xAA000000)|uint32(i), secs2.NewEmptyItem())
		require.NoError(err)
		if next == ptrBefore {
			// Same struct came back from the pool. The cached slice captured
			// before Free still points at the same [4]byte storage, which
			// now holds the new message's ID bytes — exactly the silent
			// data-corruption the Godoc warns about.
			expected := []byte{0xAA, 0x00, 0x00, byte(i)}
			require.Equal(expected, cached,
				"after pool reuse, the pre-Free cached SystemBytes slice "+
					"must silently observe the new message's bytes (the "+
					"hazard documented on SystemBytes)")
			next.Free()
			return
		}
		next.Free()
	}

	// No reuse observed in this run; the primary alias invariant above is
	// the durable property. Surface the situation so a flaky-seeming test
	// failure (or absence of one) is easy to diagnose.
	t.Log("sync.Pool did not reuse the same slot in this run; the alias " +
		"invariant above is the load-bearing check")
}
