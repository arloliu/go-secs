// Package hsmsssintegration contains integration tests for the hsmsss package
// that exercise full HSMS-SS connection lifecycles over real TCP.
package hsmsssintegration

import (
	"context"
	"encoding/binary"
	"fmt"
	"io"
	"net"
	"testing"
	"time"

	"github.com/arloliu/go-secs/hsms"
	"github.com/arloliu/go-secs/hsmsss"
	"github.com/arloliu/go-secs/logger"
	"github.com/stretchr/testify/require"
)

const testSessionID = 0x0001

// --- helpers for creating real hsmsss.Connection pairs ---

type endpoint struct {
	conn       *hsmsss.Connection
	session    hsms.Session
	selectedCh chan hsms.ConnState
}

func newEndpoint(
	t *testing.T,
	ctx context.Context,
	port int,
	isEquip bool,
	isActive bool,
	opts []hsmsss.ConnOption,
	handlers ...hsms.DataMessageHandler,
) *endpoint {
	t.Helper()

	base := []hsmsss.ConnOption{
		hsmsss.WithT3Timeout(3 * time.Second),
		hsmsss.WithT5Timeout(500 * time.Millisecond),
		hsmsss.WithT6Timeout(3 * time.Second),
		hsmsss.WithT7Timeout(5 * time.Second),
		hsmsss.WithT8Timeout(1 * time.Second),
		hsmsss.WithConnectRemoteTimeout(2 * time.Second),
		hsmsss.WithAutoLinktest(false),
	}

	if isEquip {
		base = append(base, hsmsss.WithEquipRole())
	} else {
		base = append(base, hsmsss.WithHostRole())
	}

	if isActive {
		base = append(base, hsmsss.WithActive())
	} else {
		base = append(base, hsmsss.WithPassive())
	}

	base = append(base, opts...)

	cfg, err := hsmsss.NewConnectionConfig("127.0.0.1", port, base...)
	require.NoError(t, err)

	conn, err := hsmsss.NewConnection(ctx, cfg)
	require.NoError(t, err)

	session := conn.AddSession(testSessionID)
	stateCh := make(chan hsms.ConnState, 32)
	session.AddConnStateChangeHandler(func(_ hsms.Connection, _ hsms.ConnState, cur hsms.ConnState) {
		select {
		case stateCh <- cur:
		default:
		}
	})

	if len(handlers) > 0 {
		session.AddDataMessageHandler(handlers...)
	}

	return &endpoint{conn: conn, session: session, selectedCh: stateCh}
}

func waitState(t *testing.T, ep *endpoint, state hsms.ConnState) {
	t.Helper()
	timer := time.NewTimer(5 * time.Second)
	defer timer.Stop()

	for {
		select {
		case s := <-ep.selectedCh:
			if s == state {
				return
			}
		case <-timer.C:
			t.Fatalf("timeout waiting for state %s", state.String())
		}
	}
}

func closeEndpoint(t *testing.T, ep *endpoint) {
	t.Helper()
	if ep == nil || ep.conn == nil {
		return
	}
	require.NoError(t, ep.conn.Close())
}

func echoHandler(msg *hsms.DataMessage, s hsms.Session) {
	if msg.FunctionCode()%2 == 1 {
		_ = s.ReplyDataMessage(msg, msg.Item())
	}
}

// --- HSMS frame helpers for raw TCP peers ---

func readFrame(conn net.Conn) ([]byte, error) {
	lenBuf := make([]byte, 4)
	if _, err := io.ReadFull(conn, lenBuf); err != nil {
		return nil, fmt.Errorf("read frame length: %w", err)
	}

	msgLen := binary.BigEndian.Uint32(lenBuf)
	payload := make([]byte, msgLen)

	if _, err := io.ReadFull(conn, payload); err != nil {
		return nil, fmt.Errorf("read frame payload: %w", err)
	}

	return payload, nil
}

func writeFrame(conn net.Conn, payload []byte) error {
	lenBuf := make([]byte, 4)
	binary.BigEndian.PutUint32(lenBuf, uint32(len(payload)))

	if _, err := conn.Write(lenBuf); err != nil {
		return fmt.Errorf("write frame length: %w", err)
	}

	if _, err := conn.Write(payload); err != nil {
		return fmt.Errorf("write frame payload: %w", err)
	}

	return nil
}

//nolint:unparam // hb3 is always 0 in current tests, but kept for clarity.
func buildControlHeader(hb3, stype byte, systemBytes [4]byte) []byte {
	h := make([]byte, 10)
	binary.BigEndian.PutUint16(h[0:2], 0xFFFF)
	h[2] = 0
	h[3] = hb3
	h[4] = 0
	h[5] = stype
	copy(h[6:10], systemBytes[:])

	return h
}

func getSystemBytes(header []byte) [4]byte {
	var sb [4]byte
	copy(sb[:], header[6:10])

	return sb
}

func getFreePort(t *testing.T) int {
	t.Helper()

	l, err := net.Listen("tcp", "127.0.0.1:0")
	require.NoError(t, err)
	defer l.Close()

	addr, ok := l.Addr().(*net.TCPAddr)
	require.True(t, ok)

	return addr.Port
}

func requireStateEvent(t *testing.T, ch <-chan hsms.ConnState, expected hsms.ConnState) {
	t.Helper()
	timer := time.NewTimer(5 * time.Second)
	defer timer.Stop()

	for {
		select {
		case s := <-ch:
			if s == expected {
				return
			}
		case <-timer.C:
			t.Fatalf("timeout waiting for state %s", expected.String())
		}
	}
}

// rawPeerSelectHandshake accepts a TCP connection on the listener, reads
// Select.req, and replies Select.rsp(success). Returns the raw TCP conn.
func rawPeerSelectHandshake(t *testing.T, ln net.Listener) net.Conn {
	t.Helper()

	conn, err := ln.Accept()
	require.NoError(t, err)

	require.NoError(t, conn.SetReadDeadline(time.Now().Add(10*time.Second)))

	req, err := readFrame(conn)
	require.NoError(t, err)
	require.Equal(t, byte(hsms.SelectReqType), req[5], "expected Select.req")

	sysBytes := getSystemBytes(req)
	rsp := buildControlHeader(hsms.SelectStatusSuccess, hsms.SelectRspType, sysBytes)
	require.NoError(t, writeFrame(conn, rsp))

	return conn
}

// rawClientSelectHandshake connects to a passive peer and completes select handshake.
func rawClientSelectHandshake(t *testing.T, port int) net.Conn {
	t.Helper()

	conn, err := net.DialTimeout("tcp", fmt.Sprintf("127.0.0.1:%d", port), 2*time.Second)
	require.NoError(t, err)

	require.NoError(t, conn.SetReadDeadline(time.Now().Add(10*time.Second)))

	sysBytes := [4]byte{0x01, 0x02, 0x03, 0x04}
	selectReq := buildControlHeader(0x00, hsms.SelectReqType, sysBytes)
	require.NoError(t, writeFrame(conn, selectReq))

	rsp, err := readFrame(conn)
	require.NoError(t, err)
	require.Equal(t, byte(hsms.SelectRspType), rsp[5])
	require.Equal(t, byte(hsms.SelectStatusSuccess), rsp[3])

	return conn
}

func newLoggerWith(keysAndValues ...any) logger.Logger {
	return logger.GetLogger().With(keysAndValues...)
}
