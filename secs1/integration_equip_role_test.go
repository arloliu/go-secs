package secs1

import (
	"testing"
	"time"

	"github.com/arloliu/go-secs/v2/hsms"
	"github.com/arloliu/go-secs/v2/secs2"
	"github.com/stretchr/testify/require"
)

// TestIntegration_EquipRole_AutoS9F9OnTimeout proves that WithEquipment's restored auto-S9F9
// behavior fires end-to-end over a real secs1 active+passive pair. newSECS1Passive hardcodes
// WithEquipment (equipment/master), so the PASSIVE side is the sender here — its T3 timeout (fixed
// at 3s by newSECS1Passive, which takes no timer-override opts) is answered with a wire-level S9F9
// that the active (host) side's DataMessageHandler receives.
func TestIntegration_EquipRole_AutoS9F9OnTimeout(t *testing.T) {
	t.Parallel()

	port := freePort(t)
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

	passive := newSECS1Passive(t, port) // equipment role, no reply handler
	active := newSECS1Active(t, port)   // host role
	active.conn.AddDataMessageHandler(captureS9F9)
	defer closeSECS1Endpoint(t, active)
	defer closeSECS1Endpoint(t, passive)

	require.NoError(t, passive.conn.Open(ctx, hsms.OpenBackground))
	require.NoError(t, active.conn.Open(ctx, hsms.OpenBackground))
	waitSECS1Selected(t, passive)
	waitSECS1Selected(t, active)

	_, err := passive.conn.SendDataMessage(ctx, 1, 1, true, secs2.A("ping"))
	require.ErrorIs(t, err, hsms.ErrT3Timeout)

	select {
	case msg := <-s9f9:
		item, err := msg.Item()
		require.NoError(t, err)
		body, err := item.ToBinary()
		require.NoError(t, err)
		require.Len(t, body, 10, "S9F9 body must be the 10-byte SHEAD of the timed-out message")
	case <-time.After(5 * time.Second):
		t.Fatal("host peer never received the equipment-role S9F9 notification")
	}
}
