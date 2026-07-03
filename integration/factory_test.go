package integration

import (
	"context"
	"net"
	"testing"
	"time"

	"github.com/arloliu/go-secs/v2/hsms"
	"github.com/arloliu/go-secs/v2/hsmsss"
	"github.com/arloliu/go-secs/v2/secs1"
	"github.com/stretchr/testify/require"
)

// transportFactory drives one real go-secs transport over the net.Pipe harness through the
// transport-agnostic hsms.Connection surface. open dials over net.Pipe, brings the
// connection to Selected, and returns it alongside the dialFactory (so a test can reach the
// current-generation peer via df.latest()); close tears it down.
type transportFactory struct {
	name string
	// open brings a connection up to Selected with no pre-open customization. It is a thin
	// wrapper over openWith(t, nil), so every scenario that does not need to observe the connect
	// transitions keeps using it unchanged.
	open func(*testing.T) (hsms.Connection, *dialFactory)
	// openWith is the same bring-up as open but runs pre(conn) after the connection is constructed
	// and BEFORE Open. State-change handlers must be registered before Open to observe the connect
	// transitions (they fire DURING Open), so a scenario that records the state sequence registers
	// its recorder from pre.
	openWith func(t *testing.T, pre func(hsms.Connection)) (hsms.Connection, *dialFactory)
	close    func(*testing.T, hsms.Connection)
}

// hsmsssFactory wires the HSMS-SS transport onto the net.Pipe harness: WithDialer injects the
// in-memory dial factory whose peers are scripted hsmsPeers, Open blocks until Selected, and
// the factory asserts the connection reached SelectedState before handing it back.
func hsmsssFactory() transportFactory {
	f := transportFactory{
		name: "hsmsss",
		openWith: func(t *testing.T, pre func(hsms.Connection)) (hsms.Connection, *dialFactory) {
			t.Helper()

			df := &dialFactory{
				spawn: func(c net.Conn) scriptedPeer { return newHSMSPeer(c) },
			}
			t.Cleanup(df.closeAll)

			// A short T5 (connection-separation timer) keeps the core's reconnect cadence fast so an
			// involuntary-drop scenario re-Selects within the test's patience. It is benign for the
			// scenarios that never drop the line.
			cfg, err := hsmsss.NewConfig("127.0.0.1", 5000,
				hsmsss.WithActive(),
				hsmsss.WithDialer(df.dial),
				hsmsss.WithConnectionOption(hsms.WithT5(50*time.Millisecond)),
			)
			require.NoError(t, err)

			conn, err := hsmsss.New(cfg)
			require.NoError(t, err)

			if pre != nil {
				pre(conn)
			}

			ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
			defer cancel()

			require.NoError(t, conn.Open(ctx, hsms.OpenWaitSelected))
			require.Equal(t, hsms.SelectedState, conn.State())

			return conn, df
		},
		close: func(t *testing.T, conn hsms.Connection) {
			t.Helper()

			require.NoError(t, conn.Close())
		},
	}

	f.open = func(t *testing.T) (hsms.Connection, *dialFactory) {
		return f.openWith(t, nil)
	}

	return f
}

// secs1Factory wires the SECS-I (SEMI E4) transport onto the net.Pipe harness: WithDialer injects the
// in-memory dial factory whose peers are scripted secs1Peers, and Open brings the connection up. SECS-I
// has no HSMS-style Select handshake — a live line IS the selected session, so the transport
// auto-commits to Selected the instant the pipe connects; the factory asserts SelectedState before
// handing the connection back, exactly as the HSMS-SS factory does.
func secs1Factory() transportFactory {
	f := transportFactory{
		name: "secs1",
		openWith: func(t *testing.T, pre func(hsms.Connection)) (hsms.Connection, *dialFactory) {
			t.Helper()

			df := &dialFactory{
				spawn: func(c net.Conn) scriptedPeer { return newSECS1Peer(c) },
			}
			t.Cleanup(df.closeAll)

			// A short T5 (connection-separation timer) keeps the core's reconnect cadence fast so an
			// involuntary-drop scenario re-Selects within the test's patience. It is benign for the
			// scenarios that never drop the line.
			cfg, err := secs1.NewConfig("127.0.0.1", 5000,
				secs1.WithActive(),
				secs1.WithDialer(df.dial),
				secs1.WithConnectionOption(hsms.WithT5(50*time.Millisecond)),
			)
			require.NoError(t, err)

			conn, err := secs1.New(cfg)
			require.NoError(t, err)

			if pre != nil {
				pre(conn)
			}

			ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
			defer cancel()

			require.NoError(t, conn.Open(ctx, hsms.OpenWaitSelected))
			require.Equal(t, hsms.SelectedState, conn.State())

			return conn, df
		},
		close: func(t *testing.T, conn hsms.Connection) {
			t.Helper()

			require.NoError(t, conn.Close())
		},
	}

	f.open = func(t *testing.T) (hsms.Connection, *dialFactory) {
		return f.openWith(t, nil)
	}

	return f
}
