package hsms

import (
	"testing"

	"github.com/arloliu/go-secs/v2/secs2"
	"github.com/stretchr/testify/require"
)

// TestDataMessage_RawFrame_ZeroLengthLocalizedStr decodes a received frame whose body is,
// or contains, a zero-length localized-string item (49 00),
// which SEMI E5 allows and which must not make the whole body undecodable.
func TestDataMessage_RawFrame_ZeroLengthLocalizedStr(t *testing.T) {
	tests := []struct {
		name string
		body []byte
		pick func(t *testing.T, it secs2.Item) secs2.Item
	}{
		{
			name: "alone",
			body: []byte{0x49, 0x00},
			pick: func(_ *testing.T, it secs2.Item) secs2.Item { return it },
		},
		{
			name: "inside a list",
			body: []byte{0x01, 0x02, 0x41, 0x01, 'x', 0x49, 0x00},
			pick: func(t *testing.T, it secs2.Item) secs2.Item {
				t.Helper()

				child, err := it.Get(1)
				require.NoError(t, err)

				return child
			},
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			// Session 1, S1F1 with the W-bit, data message, system bytes 1.
			frame := append([]byte{0x00, 0x01, 0x81, 0x01, 0x00, 0x00, 0x00, 0x00, 0x00, 0x01}, tt.body...)

			msg, err := decodeOwnedFrame(frame)
			require.NoError(t, err)

			dm, ok := msg.(*DataMessage)
			require.True(t, ok, "expected *DataMessage from decodeOwnedFrame")

			item, err := dm.Item()
			require.NoError(t, err)
			require.Equal(t, tt.body, item.ToBytes())

			w := tt.pick(t, item)
			require.True(t, w.IsLocalizedStr())
			require.Equal(t, 0, w.Size())

			lstr, ok := w.(*secs2.LocalizedStrItem)
			require.True(t, ok)
			require.False(t, lstr.HasLocalizedStrHeader())
		})
	}
}
