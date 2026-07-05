package hsmsss

import (
	"testing"
	"time"

	"github.com/arloliu/go-secs/v2/hsms"
	"github.com/arloliu/go-secs/v2/secs2"
	"github.com/stretchr/testify/require"
)

// TestIntegration_EquipRole_AutoS9F9OnTimeout proves that WithEquipRole's restored auto-S9F9
// behavior (see hsms.WithAutoS9F9) fires end-to-end over a real active+passive HSMS-SS pair: the
// equipment-role side's T3 timeout on a W-bit send is answered with a wire-level S9F9 (SEMI E5
// §10.13) that the peer's DataMessageHandler receives, carrying the timed-out message's 10-byte
// SHEAD as its body.
func TestIntegration_EquipRole_AutoS9F9OnTimeout(t *testing.T) {
	t.Parallel()

	port := freeLoopbackPort(t)
	ctx := t.Context()

	s9f9 := make(chan *hsms.DataMessage, 1)
	captureS9F9 := func(msg *hsms.DataMessage, _ hsms.SECS2Endpoint) {
		if msg.Stream() == 9 && msg.Function() == 9 {
			select {
			case s9f9 <- msg:
			default:
			}
		}
	}
	silent := func(_ *hsms.DataMessage, _ hsms.SECS2Endpoint) {} // never replies, so T3 fires

	passive := newEndpoint(t, port, false, nil, silent, captureS9F9)
	active := newEndpoint(t, port, true, []Option{
		WithEquipRole(),
		WithConnectionOption(hsms.WithT3(100 * time.Millisecond)),
	})
	defer closeEndpoint(t, active)
	defer closeEndpoint(t, passive)

	require.NoError(t, passive.conn.Open(ctx, hsms.OpenBackground))
	require.NoError(t, active.conn.Open(ctx, hsms.OpenBackground))
	waitSelected(t, active)
	waitSelected(t, passive)

	_, err := active.conn.SendDataMessage(ctx, 1, 1, true, secs2.A("ping"))
	require.ErrorIs(t, err, hsms.ErrT3Timeout)

	select {
	case msg := <-s9f9:
		item, err := msg.Item()
		require.NoError(t, err)
		body, err := item.ToBinary()
		require.NoError(t, err)
		require.Len(t, body, 10, "S9F9 body must be the 10-byte SHEAD of the timed-out message")
	case <-time.After(3 * time.Second):
		t.Fatal("passive peer never received the equipment-role S9F9 notification")
	}
}
