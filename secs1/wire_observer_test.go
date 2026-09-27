package secs1

import (
	"sync"
	"testing"
	"time"

	"github.com/arloliu/go-secs/v2/hsms"
	"github.com/arloliu/go-secs/v2/secs2"
	"github.com/stretchr/testify/require"
)

// TestWireObserver_SECS1ReportsNothing proves an HSMS wire observer installed on a SECS-I connection is never called.
// SECS-I has no HSMS frames on its wire:
// its transport reports success for an HSMS control frame without any I/O and re-frames data as SECS-I blocks,
// so the shared core must not report the frames it hands that transport.
// The scenario drives both core write sites — a data round trip and a graceful Close from Selected,
// which attempts the courtesy Separate — on both roles.
func TestWireObserver_SECS1ReportsNothing(t *testing.T) {
	t.Parallel()

	var (
		mu     sync.Mutex
		events []hsms.WireEvent
	)
	observe := hsms.WithWireObserver(func(ev hsms.WireEvent) {
		mu.Lock()
		events = append(events, ev)
		mu.Unlock()
	})

	port := freePort(t)
	passiveCfg, err := NewConfig("127.0.0.1", port,
		WithPassive(),
		WithEquipment(),
		WithDeviceID(0),
		WithT1(100*time.Millisecond),
		WithT2(2*time.Second),
		WithT4(2*time.Second),
		WithConnectionOption(hsms.WithT3(3*time.Second)),
		WithConnectionOption(observe),
	)
	require.NoError(t, err)
	passiveConn, err := New(passiveCfg)
	require.NoError(t, err)
	passiveConn.AddDataMessageHandler(secs1EchoHandler)
	passive := &secs1E{conn: passiveConn}

	active := newSECS1Active(t, port, WithConnectionOption(observe))

	require.NoError(t, passive.conn.Open(t.Context(), hsms.OpenBackground))
	require.NoError(t, active.conn.Open(t.Context(), hsms.OpenBackground))
	waitSECS1Selected(t, passive)
	waitSECS1Selected(t, active)

	reply, err := active.conn.SendDataMessage(t.Context(), 1, 1, true, secs2.A("ping"))
	require.NoError(t, err)
	require.NotNil(t, reply)

	closeSECS1Endpoint(t, active)
	closeSECS1Endpoint(t, passive)

	mu.Lock()
	defer mu.Unlock()
	require.Empty(t, events, "a SECS-I connection reports no HSMS wire frames")
}
