package compat

import (
	"context"
	"net"
	"testing"
	"time"

	v1hsms "github.com/arloliu/go-secs/hsms"
	v1hsmsss "github.com/arloliu/go-secs/hsmsss"
	v1logger "github.com/arloliu/go-secs/logger"
	v1secs1 "github.com/arloliu/go-secs/secs1"
	v1secs2 "github.com/arloliu/go-secs/secs2"
	v2hsms "github.com/arloliu/go-secs/v2/hsms"
	v2hsmsss "github.com/arloliu/go-secs/v2/hsmsss"
	v2logger "github.com/arloliu/go-secs/v2/logger"
	v2secs1 "github.com/arloliu/go-secs/v2/secs1"
	v2secs2 "github.com/arloliu/go-secs/v2/secs2"
	"github.com/stretchr/testify/require"
)

const compatSessionID = 10

type v1NoopLogger struct{}

func (v1NoopLogger) Debug(string, ...any)          {}
func (v1NoopLogger) Info(string, ...any)           {}
func (v1NoopLogger) Warn(string, ...any)           {}
func (v1NoopLogger) Error(string, ...any)          {}
func (v1NoopLogger) Fatal(string, ...any)          {}
func (n v1NoopLogger) With(...any) v1logger.Logger { return n }
func (v1NoopLogger) Level() v1logger.LogLevel      { return v1logger.FatalLevel }
func (v1NoopLogger) SetLevel(v1logger.LogLevel)    {}

type v2NoopLogger struct{}

func (v2NoopLogger) Debug(string, ...any)          {}
func (v2NoopLogger) Info(string, ...any)           {}
func (v2NoopLogger) Warn(string, ...any)           {}
func (v2NoopLogger) Error(string, ...any)          {}
func (v2NoopLogger) Fatal(string, ...any)          {}
func (n v2NoopLogger) With(...any) v2logger.Logger { return n }
func (v2NoopLogger) Level() v2logger.LogLevel      { return v2logger.FatalLevel }
func (v2NoopLogger) SetLevel(v2logger.LogLevel)    {}

type v1Endpoint struct {
	conn    interface{ Close() error }
	session v1hsms.Session
}

type v2Endpoint struct {
	conn v2hsms.Connection
}

func TestHSMSCompatibility_V2HostToV1Equipment(t *testing.T) {
	port := freeLoopbackPort(t)
	ctx := t.Context()

	equipment := newV1HSMSSEndpoint(t, ctx, port, false, true, v1EchoHandler)
	require.NoError(t, equipment.conn.(*v1hsmsss.Connection).Open(false))
	defer closeV1Endpoint(t, equipment)

	host := newV2HSMSSEndpoint(t, port, true, false, nil)
	require.NoError(t, host.conn.Open(ctx, v2hsms.OpenBackground))
	defer closeV2Endpoint(t, host)

	waitV2Selected(t, host)

	reply, err := host.conn.SendDataMessage(ctx, 1, 1, true, v2secs2.A("v2-host"))
	require.NoError(t, err)
	requireV2ASCIIReply(t, reply, "v2-host")
}

func TestHSMSCompatibility_V1HostToV2Equipment(t *testing.T) {
	port := freeLoopbackPort(t)
	ctx := t.Context()

	equipment := newV2HSMSSEndpoint(t, port, false, true, v2EchoHandler)
	require.NoError(t, equipment.conn.Open(ctx, v2hsms.OpenBackground))
	defer closeV2Endpoint(t, equipment)

	host := newV1HSMSSEndpoint(t, ctx, port, true, false, nil)
	require.NoError(t, host.conn.(*v1hsmsss.Connection).Open(true))
	defer closeV1Endpoint(t, host)

	reply, err := host.session.SendDataMessage(1, 1, true, v1secs2.A("v1-host"))
	require.NoError(t, err)
	requireV1ASCIIReply(t, reply, "v1-host")
}

func TestSECS1Compatibility_V2HostToV1Equipment(t *testing.T) {
	port := freeLoopbackPort(t)
	ctx := t.Context()

	equipment := newV1SECS1Endpoint(t, ctx, port, false, true, v1EchoHandler)
	require.NoError(t, equipment.conn.(*v1secs1.Connection).Open(false))
	defer closeV1Endpoint(t, equipment)

	host := newV2SECS1Endpoint(t, port, true, false, nil)
	require.NoError(t, host.conn.Open(ctx, v2hsms.OpenBackground))
	defer closeV2Endpoint(t, host)

	waitV2Selected(t, host)

	reply, err := host.conn.SendDataMessage(ctx, 1, 1, true, v2secs2.A("v2-host"))
	require.NoError(t, err)
	requireV2ASCIIReply(t, reply, "v2-host")
}

func TestSECS1Compatibility_V2EquipmentToV1Host(t *testing.T) {
	port := freeLoopbackPort(t)
	ctx := t.Context()

	host := newV1SECS1Endpoint(t, ctx, port, false, false, v1EchoHandler)
	require.NoError(t, host.conn.(*v1secs1.Connection).Open(false))
	defer closeV1Endpoint(t, host)

	equipment := newV2SECS1Endpoint(t, port, true, true, nil)
	require.NoError(t, equipment.conn.Open(ctx, v2hsms.OpenBackground))
	defer closeV2Endpoint(t, equipment)

	waitV2Selected(t, equipment)

	reply, err := equipment.conn.SendDataMessage(ctx, 1, 1, true, v2secs2.A("v2-equipment"))
	require.NoError(t, err)
	requireV2ASCIIReply(t, reply, "v2-equipment")
}

func freeLoopbackPort(t *testing.T) int {
	t.Helper()

	ln, err := net.ListenTCP("tcp", &net.TCPAddr{IP: net.ParseIP("127.0.0.1"), Port: 0})
	require.NoError(t, err)
	defer ln.Close()

	addr, ok := ln.Addr().(*net.TCPAddr)
	require.True(t, ok)

	return addr.Port
}

func newV1HSMSSEndpoint(
	t *testing.T,
	ctx context.Context,
	port int,
	active bool,
	equipment bool,
	handler v1hsms.DataMessageHandler,
) *v1Endpoint {
	t.Helper()

	opts := []v1hsmsss.ConnOption{
		v1hsmsss.WithT3Timeout(2 * time.Second),
		v1hsmsss.WithT5Timeout(200 * time.Millisecond),
		v1hsmsss.WithT6Timeout(2 * time.Second),
		v1hsmsss.WithT7Timeout(5 * time.Second),
		v1hsmsss.WithT8Timeout(1 * time.Second),
		v1hsmsss.WithConnectRemoteTimeout(1 * time.Second),
		v1hsmsss.WithAutoLinktest(false),
		v1hsmsss.WithLogger(v1NoopLogger{}),
	}
	if active {
		opts = append(opts, v1hsmsss.WithActive())
	} else {
		opts = append(opts, v1hsmsss.WithPassive())
	}
	if equipment {
		opts = append(opts, v1hsmsss.WithEquipRole())
	} else {
		opts = append(opts, v1hsmsss.WithHostRole())
	}

	cfg, err := v1hsmsss.NewConnectionConfig("127.0.0.1", port, opts...)
	require.NoError(t, err)

	conn, err := v1hsmsss.NewConnection(ctx, cfg)
	require.NoError(t, err)

	session := conn.AddSession(compatSessionID)
	if handler != nil {
		session.AddDataMessageHandler(handler)
	}

	return &v1Endpoint{conn: conn, session: session}
}

func newV2HSMSSEndpoint(
	t *testing.T,
	port int,
	active bool,
	equipment bool,
	handler v2hsms.DataMessageHandler,
) *v2Endpoint {
	t.Helper()

	opts := []v2hsmsss.Option{
		v2hsmsss.WithConnectionOption(v2hsms.WithT3(2 * time.Second)),
		v2hsmsss.WithConnectionOption(v2hsms.WithT5(200 * time.Millisecond)),
		v2hsmsss.WithConnectionOption(v2hsms.WithT6(2 * time.Second)),
		v2hsmsss.WithConnectionOption(v2hsms.WithT7(5 * time.Second)),
		v2hsmsss.WithConnectionOption(v2hsms.WithT8(1 * time.Second)),
		v2hsmsss.WithConnectionOption(v2hsms.WithSessionID(compatSessionID)),
		v2hsmsss.WithConnectionOption(v2hsms.WithLinktestInterval(0)),
		v2hsmsss.WithConnectionOption(v2hsms.WithLogger(v2NoopLogger{})),
	}
	if active {
		opts = append(opts, v2hsmsss.WithActive())
	} else {
		opts = append(opts, v2hsmsss.WithPassive())
	}
	if equipment {
		opts = append(opts, v2hsmsss.WithEquipRole())
	} else {
		opts = append(opts, v2hsmsss.WithHostRole())
	}

	cfg, err := v2hsmsss.NewConfig("127.0.0.1", port, opts...)
	require.NoError(t, err)

	conn, err := v2hsmsss.New(cfg)
	require.NoError(t, err)

	if handler != nil {
		conn.AddDataMessageHandler(handler)
	}

	return &v2Endpoint{conn: conn}
}

func newV1SECS1Endpoint(
	t *testing.T,
	ctx context.Context,
	port int,
	active bool,
	equipment bool,
	handler v1hsms.DataMessageHandler,
) *v1Endpoint {
	t.Helper()

	opts := []v1secs1.ConnOption{
		v1secs1.WithDeviceID(compatSessionID),
		v1secs1.WithT1Timeout(v1secs1.MinT1Timeout),
		v1secs1.WithT2Timeout(v1secs1.MinT2Timeout),
		v1secs1.WithT3Timeout(2 * time.Second),
		v1secs1.WithT4Timeout(2 * time.Second),
		v1secs1.WithRetryLimit(2),
		v1secs1.WithConnectRemoteTimeout(1 * time.Second),
		v1secs1.WithSendTimeout(1 * time.Second),
		v1secs1.WithLogger(v1NoopLogger{}),
	}
	if active {
		opts = append(opts, v1secs1.WithActive())
	} else {
		opts = append(opts, v1secs1.WithPassive())
	}
	if equipment {
		opts = append(opts, v1secs1.WithEquipRole())
	} else {
		opts = append(opts, v1secs1.WithHostRole())
	}

	cfg, err := v1secs1.NewConnectionConfig("127.0.0.1", port, opts...)
	require.NoError(t, err)

	conn, err := v1secs1.NewConnection(ctx, cfg)
	require.NoError(t, err)

	session := conn.AddSession(compatSessionID)
	if handler != nil {
		session.AddDataMessageHandler(handler)
	}

	return &v1Endpoint{conn: conn, session: session}
}

func newV2SECS1Endpoint(
	t *testing.T,
	port int,
	active bool,
	equipment bool,
	handler v2hsms.DataMessageHandler,
) *v2Endpoint {
	t.Helper()

	opts := []v2secs1.Option{
		v2secs1.WithDeviceID(compatSessionID),
		v2secs1.WithT1(100 * time.Millisecond),
		v2secs1.WithT2(200 * time.Millisecond),
		v2secs1.WithT4(2 * time.Second),
		v2secs1.WithConnectionOption(v2hsms.WithT3(2 * time.Second)),
		v2secs1.WithConnectionOption(v2hsms.WithT5(200 * time.Millisecond)),
		v2secs1.WithConnectionOption(v2hsms.WithLogger(v2NoopLogger{})),
	}
	if active {
		opts = append(opts, v2secs1.WithActive())
	} else {
		opts = append(opts, v2secs1.WithPassive())
	}
	if equipment {
		opts = append(opts, v2secs1.WithEquipment())
	} else {
		opts = append(opts, v2secs1.WithHost())
	}

	cfg, err := v2secs1.NewConfig("127.0.0.1", port, opts...)
	require.NoError(t, err)

	conn, err := v2secs1.New(cfg)
	require.NoError(t, err)

	if handler != nil {
		conn.AddDataMessageHandler(handler)
	}

	return &v2Endpoint{conn: conn}
}

func v1EchoHandler(msg *v1hsms.DataMessage, session v1hsms.Session) {
	if msg.FunctionCode()%2 == 0 {
		return
	}
	_ = session.ReplyDataMessage(msg, msg.Item())
}

func v2EchoHandler(msg *v2hsms.DataMessage, ep v2hsms.SECS2Endpoint) {
	if msg.Function()%2 == 0 {
		return
	}

	item, err := msg.Item()
	if err != nil {
		return
	}

	_ = ep.ReplyDataMessage(context.Background(), msg, item)
}

func waitV2Selected(t *testing.T, ep *v2Endpoint) {
	t.Helper()

	require.Eventually(t, func() bool {
		return ep.conn.State() == v2hsms.SelectedState
	}, 5*time.Second, 5*time.Millisecond)
}

func closeV1Endpoint(t *testing.T, ep *v1Endpoint) {
	t.Helper()
	require.NoError(t, ep.conn.Close())
}

func closeV2Endpoint(t *testing.T, ep *v2Endpoint) {
	t.Helper()
	require.NoError(t, ep.conn.Close())
}

func requireV1ASCIIReply(t *testing.T, reply *v1hsms.DataMessage, want string) {
	t.Helper()
	require.NotNil(t, reply)
	defer reply.Free()

	require.Equal(t, uint8(1), reply.StreamCode())
	require.Equal(t, uint8(2), reply.FunctionCode())

	got, err := reply.Item().ToASCII()
	require.NoError(t, err)
	require.Equal(t, want, got)
}

func requireV2ASCIIReply(t *testing.T, reply *v2hsms.DataMessage, want string) {
	t.Helper()
	require.NotNil(t, reply)

	require.Equal(t, uint8(1), reply.Stream())
	require.Equal(t, uint8(2), reply.Function())

	item, err := reply.Item()
	require.NoError(t, err)
	got, err := item.ToASCII()
	require.NoError(t, err)
	require.Equal(t, want, got)
}
