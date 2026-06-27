package hsms

import (
	"bytes"
	"sync"
	"testing"

	"github.com/arloliu/go-secs/secs2"
	"github.com/stretchr/testify/require"
)

// TestSnapshotForRelay covers DataMessage.SnapshotForRelay behaviour.
func TestSnapshotForRelay(t *testing.T) {
	t.Run("byte identity", func(t *testing.T) {
		// SnapshotForRelay().ToBytes() must be byte-equal to msg.ToBytes() (same frame).
		msg, err := NewDataMessage(1, 13, true, 0x1234, []byte{0x00, 0x00, 0x00, 0x01},
			secs2.NewASCIIItem("hello"))
		require.NoError(t, err)
		defer msg.Free()

		clone := msg.SnapshotForRelay()
		defer clone.Free()

		require.True(t, bytes.Equal(msg.ToBytes(), clone.ToBytes()),
			"SnapshotForRelay().ToBytes() must be byte-equal to msg.ToBytes()")
	})

	t.Run("empty body", func(t *testing.T) {
		// A DataMessage with an empty item serializes to a valid 14-byte frame;
		// SnapshotForRelay must reproduce those 14 bytes exactly.
		msg, err := NewDataMessage(0, 0, false, 0x0000, []byte{0, 0, 0, 0},
			secs2.NewEmptyItem())
		require.NoError(t, err)
		defer msg.Free()

		clone := msg.SnapshotForRelay()
		defer clone.Free()

		cloneBytes := clone.ToBytes()
		require.Equal(t, 14, len(cloneBytes), "empty body clone frame must be exactly 14 bytes")
		require.True(t, bytes.Equal(msg.ToBytes(), cloneBytes),
			"empty body SnapshotForRelay().ToBytes() must equal msg.ToBytes()")
	})

	t.Run("independent structural access", func(t *testing.T) {
		// Mutating the clone's child item via Item().Get(i).SetValues must not
		// affect the source message's item tree.
		msg, err := NewDataMessage(6, 11, true, 0x0001, []byte{0, 0, 0, 1},
			secs2.L(secs2.A("original")))
		require.NoError(t, err)
		defer msg.Free()

		clone := msg.SnapshotForRelay()
		defer clone.Free()

		// Mutate the clone's first child.
		child, err := clone.Item().Get(0)
		require.NoError(t, err)
		require.NoError(t, child.SetValues("mutated"))

		// Source item must remain untouched.
		srcChild, err := msg.Item().Get(0)
		require.NoError(t, err)
		srcStr, err := srcChild.ToASCII()
		require.NoError(t, err)
		require.Equal(t, "original", srcStr,
			"source item must not be affected by clone mutation")
	})

	t.Run("relay never decodes", func(t *testing.T) {
		// A relay that only calls ToBytes() on the clone must not trigger
		// item decode (snapshotMessage.item stays nil).
		msg, err := NewDataMessage(1, 13, true, 0x0001, []byte{0, 0, 0, 1},
			secs2.NewASCIIItem("relay"))
		require.NoError(t, err)
		defer msg.Free()

		clone := msg.SnapshotForRelay()
		defer clone.Free()

		// ToBytes-only relay path.
		_ = clone.ToBytes()

		// Type-assert to probe internal state (test is in-package).
		snap, ok := clone.(*snapshotMessage)
		require.True(t, ok, "SnapshotForRelay must return a *snapshotMessage")
		require.Nil(t, snap.item, "relay-only clone must not decode the item")
	})

	t.Run("concurrent serialize no race", func(t *testing.T) {
		// Serialize original and clone concurrently 200 times under -race.
		for range 200 {
			func() {
				msg, err := NewDataMessage(1, 1, true, 1234, GenerateMsgSystemBytes(),
					secs2.L(secs2.A("aaa"), secs2.A("bbb"), secs2.A("ccc")))
				require.NoError(t, err)
				defer msg.Free()

				clone := msg.SnapshotForRelay()
				defer clone.Free()

				var wg sync.WaitGroup
				wg.Add(2)
				go func() { defer wg.Done(); _ = msg.ToBytes() }()
				go func() { defer wg.Done(); _ = clone.ToBytes() }()
				wg.Wait()
			}()
		}
	})
}
