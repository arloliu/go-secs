package hsms

import (
	"encoding/binary"
	"reflect"
	"testing"

	"github.com/arloliu/go-secs/v2/secs2"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// TestDataMessage_Equal_OrderDependenceRegression is the headline case §5 exists for: two
// messages built from identical wire bytes — one via NewDataMessage (decode cache pre-fired),
// one via DecodeHSMSMessage (decode cache unfired until Item() is called) — must compare Equal
// regardless of whether Item() has been called on either side first. reflect.DeepEqual is
// asserted false as a teeth check documenting why Equal exists.
func TestDataMessage_Equal_OrderDependenceRegression(t *testing.T) {
	t.Parallel()

	treeMsg, err := NewDataMessage(1, 1, true, 5, [4]byte{0, 0, 0, 7}, secs2.A("hello"))
	require.NoError(t, err)

	rawMsg, err := DecodeHSMSMessage(treeMsg.ToBytes())
	require.NoError(t, err)
	rawDM, ok := rawMsg.(*DataMessage)
	require.True(t, ok)

	// Do not call Item() on rawDM before this Equal call: the decode-cache once must still be
	// unfired on one side while the other side (treeMsg) is pre-fired.
	assert.True(t, treeMsg.Equal(rawDM))
	assert.False(t, reflect.DeepEqual(treeMsg, rawDM), //nolint:govet // intentional: proving DeepEqual is unsafe here is the point of this test
		"reflect.DeepEqual is expected to disagree due to the decode-cache ordering hazard — this is why Equal exists")

	// Order shouldn't matter, and calling Item() first shouldn't change the result.
	_, _ = rawDM.Item()
	assert.True(t, rawDM.Equal(treeMsg))
}

func TestDataMessage_Equal_NilHandling(t *testing.T) {
	t.Parallel()

	var nilMsg *DataMessage
	other, err := NewDataMessage(1, 1, false, 0, [4]byte{}, nil)
	require.NoError(t, err)

	assert.True(t, nilMsg.Equal(nil))
	assert.False(t, nilMsg.Equal(other))
	assert.False(t, other.Equal(nilMsg))
}

func TestDataMessage_Equal_DifferingHeaderField(t *testing.T) {
	t.Parallel()

	base, err := NewDataMessage(1, 1, true, 5, [4]byte{0, 0, 0, 1}, secs2.A("x"))
	require.NoError(t, err)

	diffStream, err := NewDataMessage(2, 1, true, 5, [4]byte{0, 0, 0, 1}, secs2.A("x"))
	require.NoError(t, err)
	assert.False(t, base.Equal(diffStream))

	diffSession, err := NewDataMessage(1, 1, true, 6, [4]byte{0, 0, 0, 1}, secs2.A("x"))
	require.NoError(t, err)
	assert.False(t, base.Equal(diffSession))

	diffSysBytes, err := NewDataMessage(1, 1, true, 5, [4]byte{0, 0, 0, 2}, secs2.A("x"))
	require.NoError(t, err)
	assert.False(t, base.Equal(diffSysBytes))
}

func TestDataMessage_Equal_DifferingBody(t *testing.T) {
	t.Parallel()

	a, err := NewDataMessage(1, 1, false, 0, [4]byte{}, secs2.A("x"))
	require.NoError(t, err)
	b, err := NewDataMessage(1, 1, false, 0, [4]byte{}, secs2.A("y"))
	require.NoError(t, err)

	assert.False(t, a.Equal(b))
}

// TestDataMessage_Equal_DecodeErrorNeverPanics verifies a message whose lazy body decode fails
// compares unequal to any other message, without panicking, per Equal's documented contract.
func TestDataMessage_Equal_DecodeErrorNeverPanics(t *testing.T) {
	t.Parallel()

	header := [10]byte{0, 1, 1, 1, 0, 0, 0, 0, 0, 1}
	// Malformed list: format byte 0x03 (list, 3-byte length field) claiming 0xFFFFFF children
	// with no following bytes — the same crafted malformed-body case secs2/decode_test.go uses.
	body := []byte{0x03, 0xFF, 0xFF, 0xFF}
	msgLen := uint32(10 + len(body))
	frame := make([]byte, 0, 4+msgLen)
	frame = binary.BigEndian.AppendUint32(frame, msgLen)
	frame = append(frame, header[:]...)
	frame = append(frame, body...)

	badMsg, err := DecodeHSMSMessage(frame)
	require.NoError(t, err, "frame-level decode must succeed; the body decode error is lazy")
	badDM, ok := badMsg.(*DataMessage)
	require.True(t, ok)

	goodMsg, err := NewDataMessage(1, 1, true, 1, [4]byte{}, secs2.NewEmptyItem())
	require.NoError(t, err)

	assert.NotPanics(t, func() {
		assert.False(t, badDM.Equal(goodMsg))
		assert.False(t, goodMsg.Equal(badDM))
		assert.False(t, badDM.Equal(badDM), "an item with a decode error is never equal, even to itself") //nolint:gocritic // intentional self-comparison
	})

	require.Error(t, badDM.DecodeErr(), "test setup must actually exercise a lazy decode error")
}
