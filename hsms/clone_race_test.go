package hsms

import (
	"sync"
	"testing"

	"github.com/arloliu/go-secs/secs2"
	"github.com/stretchr/testify/require"
)

// Regression guard for the shallow-ListItem.Clone race: a cloned list message
// and its original must be safe to serialize concurrently. Run under -race.
// Pre-fix (shallow clone) this races on shared child rawBytes at
// secs2/ascii.go (write) reached via ListItem.ToBytes / DataMessage.ToBytes.
func TestDataMessage_Clone_ConcurrentSerialize_NoRace(t *testing.T) {
	for range 200 {
		msg, err := NewDataMessage(1, 1, true, 1234, GenerateMsgSystemBytes(),
			secs2.L(secs2.A("aaaa"), secs2.A("bbbb"), secs2.A("cccc")))
		require.NoError(t, err)

		clone := msg.Clone()

		var wg sync.WaitGroup
		wg.Add(2)
		go func() { defer wg.Done(); _ = msg.ToBytes() }()
		go func() { defer wg.Done(); _ = clone.ToBytes() }()
		wg.Wait()
	}
}
