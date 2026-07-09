package compat

import (
	"context"
	"math"
	"net"
	"strings"
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

func TestSECS1Compatibility_V1HostToV2Equipment(t *testing.T) {
	port := freeLoopbackPort(t)
	ctx := t.Context()

	equipment := newV2SECS1Endpoint(t, port, false, true, v2EchoHandler)
	require.NoError(t, equipment.conn.Open(ctx, v2hsms.OpenBackground))
	defer closeV2Endpoint(t, equipment)

	host := newV1SECS1Endpoint(t, ctx, port, true, false, nil)
	require.NoError(t, host.conn.(*v1secs1.Connection).Open(true))
	defer closeV1Endpoint(t, host)

	reply, err := host.session.SendDataMessage(1, 1, true, v1secs2.A("v1-host"))
	require.NoError(t, err)
	requireV1ASCIIReply(t, reply, "v1-host")
}

func TestSECS1Compatibility_V1EquipmentToV2Host(t *testing.T) {
	port := freeLoopbackPort(t)
	ctx := t.Context()

	host := newV2SECS1Endpoint(t, port, false, false, v2EchoHandler)
	require.NoError(t, host.conn.Open(ctx, v2hsms.OpenBackground))
	defer closeV2Endpoint(t, host)

	equipment := newV1SECS1Endpoint(t, ctx, port, true, true, nil)
	require.NoError(t, equipment.conn.(*v1secs1.Connection).Open(true))
	defer closeV1Endpoint(t, equipment)

	reply, err := equipment.session.SendDataMessage(1, 1, true, v1secs2.A("v1-equipment"))
	require.NoError(t, err)
	requireV1ASCIIReply(t, reply, "v1-equipment")
}

// multiBlockPayload is large enough to force secs1's block splitter (244-byte block bodies in
// both v1 and v2) to fragment it across many blocks, and varies its content per position so
// truncation/reordering across a block boundary is detectable rather than masked by repetition.
func multiBlockPayload() string {
	return strings.Repeat("0123456789", 300) // 3000 bytes, ~13 SECS-I blocks
}

func TestSECS1Compatibility_MultiBlockMessage_V2HostToV1Equipment(t *testing.T) {
	port := freeLoopbackPort(t)
	ctx := t.Context()
	payload := multiBlockPayload()

	equipment := newV1SECS1Endpoint(t, ctx, port, false, true, v1EchoHandler)
	require.NoError(t, equipment.conn.(*v1secs1.Connection).Open(false))
	defer closeV1Endpoint(t, equipment)

	host := newV2SECS1Endpoint(t, port, true, false, nil)
	require.NoError(t, host.conn.Open(ctx, v2hsms.OpenBackground))
	defer closeV2Endpoint(t, host)

	waitV2Selected(t, host)

	reply, err := host.conn.SendDataMessage(ctx, 1, 1, true, v2secs2.A(payload))
	require.NoError(t, err)
	requireV2ASCIIReply(t, reply, payload)
}

func TestSECS1Compatibility_MultiBlockMessage_V1HostToV2Equipment(t *testing.T) {
	port := freeLoopbackPort(t)
	ctx := t.Context()
	payload := multiBlockPayload()

	equipment := newV2SECS1Endpoint(t, port, false, true, v2EchoHandler)
	require.NoError(t, equipment.conn.Open(ctx, v2hsms.OpenBackground))
	defer closeV2Endpoint(t, equipment)

	host := newV1SECS1Endpoint(t, ctx, port, true, false, nil)
	require.NoError(t, host.conn.(*v1secs1.Connection).Open(true))
	defer closeV1Endpoint(t, host)

	reply, err := host.session.SendDataMessage(1, 1, true, v1secs2.A(payload))
	require.NoError(t, err)
	requireV1ASCIIReply(t, reply, payload)
}

// v2ItemCase pairs a v2 secs2.Item with the check to run against the reply item that comes back
// once it has round-tripped through the v1 peer's decoder/re-encoder.
type v2ItemCase struct {
	name  string
	item  v2secs2.Item
	check func(t *testing.T, item v2secs2.Item)
}

func v2ItemMatrixCases() []v2ItemCase {
	return []v2ItemCase{
		{
			name: "ascii",
			item: v2secs2.A("v2-matrix"),
			check: func(t *testing.T, item v2secs2.Item) {
				got, err := item.ToASCII()
				require.NoError(t, err)
				require.Equal(t, "v2-matrix", got)
			},
		},
		{
			name: "binary",
			item: v2secs2.B([]byte{0x00, 0x01, 0x7F, 0xFF}),
			check: func(t *testing.T, item v2secs2.Item) {
				got, err := item.ToBinary()
				require.NoError(t, err)
				require.Equal(t, []byte{0x00, 0x01, 0x7F, 0xFF}, got)
			},
		},
		{
			name: "boolean",
			item: v2secs2.BOOLEAN(true, false, true),
			check: func(t *testing.T, item v2secs2.Item) {
				got, err := item.ToBoolean()
				require.NoError(t, err)
				require.Equal(t, []bool{true, false, true}, got)
			},
		},
		{
			name: "int1",
			item: v2secs2.I1(-128, 0, 127),
			check: func(t *testing.T, item v2secs2.Item) {
				got, err := item.ToInt()
				require.NoError(t, err)
				require.Equal(t, []int64{-128, 0, 127}, got)
			},
		},
		{
			name: "int8",
			item: v2secs2.I8(math.MinInt64, 0, math.MaxInt64),
			check: func(t *testing.T, item v2secs2.Item) {
				got, err := item.ToInt()
				require.NoError(t, err)
				require.Equal(t, []int64{math.MinInt64, 0, math.MaxInt64}, got)
			},
		},
		{
			name: "uint1",
			item: v2secs2.U1(0, 42, math.MaxUint8),
			check: func(t *testing.T, item v2secs2.Item) {
				got, err := item.ToUint()
				require.NoError(t, err)
				require.Equal(t, []uint64{0, 42, math.MaxUint8}, got)
			},
		},
		{
			name: "uint8",
			item: v2secs2.U8(0, 42, uint64(math.MaxUint64)),
			check: func(t *testing.T, item v2secs2.Item) {
				got, err := item.ToUint()
				require.NoError(t, err)
				require.Equal(t, []uint64{0, 42, math.MaxUint64}, got)
			},
		},
		{
			name: "float4",
			item: v2secs2.F4(1.5, -2.25),
			check: func(t *testing.T, item v2secs2.Item) {
				got, err := item.ToFloat()
				require.NoError(t, err)
				require.InDeltaSlice(t, []float64{1.5, -2.25}, got, 1e-6)
			},
		},
		{
			name: "float8",
			item: v2secs2.F8(3.14159265, -0.5),
			check: func(t *testing.T, item v2secs2.Item) {
				got, err := item.ToFloat()
				require.NoError(t, err)
				require.InDeltaSlice(t, []float64{3.14159265, -0.5}, got, 1e-12)
			},
		},
		{
			name: "nested_list",
			item: v2secs2.L(
				v2secs2.A("leaf"),
				v2secs2.L(v2secs2.U4(1, 2, 3), v2secs2.BOOLEAN(true, false)),
			),
			check: func(t *testing.T, item v2secs2.Item) {
				top, err := item.ToList()
				require.NoError(t, err)
				require.Len(t, top, 2)

				s, err := top[0].ToASCII()
				require.NoError(t, err)
				require.Equal(t, "leaf", s)

				nested, err := top[1].ToList()
				require.NoError(t, err)
				require.Len(t, nested, 2)

				u, err := nested[0].ToUint()
				require.NoError(t, err)
				require.Equal(t, []uint64{1, 2, 3}, u)

				b, err := nested[1].ToBoolean()
				require.NoError(t, err)
				require.Equal(t, []bool{true, false}, b)
			},
		},
	}
}

// v1ItemCase mirrors v2ItemCase using v1 secs2.Item — kept as a separate type/table (rather than
// generic over both) because v1secs2.Item and v2secs2.Item are structurally identical but
// nominally distinct interfaces from different module major versions.
type v1ItemCase struct {
	name  string
	item  v1secs2.Item
	check func(t *testing.T, item v1secs2.Item)
}

func v1ItemMatrixCases() []v1ItemCase {
	return []v1ItemCase{
		{
			name: "ascii",
			item: v1secs2.A("v1-matrix"),
			check: func(t *testing.T, item v1secs2.Item) {
				got, err := item.ToASCII()
				require.NoError(t, err)
				require.Equal(t, "v1-matrix", got)
			},
		},
		{
			name: "binary",
			item: v1secs2.B([]byte{0x00, 0x01, 0x7F, 0xFF}),
			check: func(t *testing.T, item v1secs2.Item) {
				got, err := item.ToBinary()
				require.NoError(t, err)
				require.Equal(t, []byte{0x00, 0x01, 0x7F, 0xFF}, got)
			},
		},
		{
			name: "boolean",
			item: v1secs2.BOOLEAN(true, false, true),
			check: func(t *testing.T, item v1secs2.Item) {
				got, err := item.ToBoolean()
				require.NoError(t, err)
				require.Equal(t, []bool{true, false, true}, got)
			},
		},
		{
			name: "int1",
			item: v1secs2.I1(-128, 0, 127),
			check: func(t *testing.T, item v1secs2.Item) {
				got, err := item.ToInt()
				require.NoError(t, err)
				require.Equal(t, []int64{-128, 0, 127}, got)
			},
		},
		{
			name: "int8",
			item: v1secs2.I8(math.MinInt64, 0, math.MaxInt64),
			check: func(t *testing.T, item v1secs2.Item) {
				got, err := item.ToInt()
				require.NoError(t, err)
				require.Equal(t, []int64{math.MinInt64, 0, math.MaxInt64}, got)
			},
		},
		{
			name: "uint1",
			item: v1secs2.U1(0, 42, math.MaxUint8),
			check: func(t *testing.T, item v1secs2.Item) {
				got, err := item.ToUint()
				require.NoError(t, err)
				require.Equal(t, []uint64{0, 42, math.MaxUint8}, got)
			},
		},
		{
			name: "uint8",
			item: v1secs2.U8(0, 42, uint64(math.MaxUint64)),
			check: func(t *testing.T, item v1secs2.Item) {
				got, err := item.ToUint()
				require.NoError(t, err)
				require.Equal(t, []uint64{0, 42, math.MaxUint64}, got)
			},
		},
		{
			name: "float4",
			item: v1secs2.F4(1.5, -2.25),
			check: func(t *testing.T, item v1secs2.Item) {
				got, err := item.ToFloat()
				require.NoError(t, err)
				require.InDeltaSlice(t, []float64{1.5, -2.25}, got, 1e-6)
			},
		},
		{
			name: "float8",
			item: v1secs2.F8(3.14159265, -0.5),
			check: func(t *testing.T, item v1secs2.Item) {
				got, err := item.ToFloat()
				require.NoError(t, err)
				require.InDeltaSlice(t, []float64{3.14159265, -0.5}, got, 1e-12)
			},
		},
		{
			name: "nested_list",
			item: v1secs2.L(
				v1secs2.A("leaf"),
				v1secs2.L(v1secs2.U4(1, 2, 3), v1secs2.BOOLEAN(true, false)),
			),
			check: func(t *testing.T, item v1secs2.Item) {
				top, err := item.ToList()
				require.NoError(t, err)
				require.Len(t, top, 2)

				s, err := top[0].ToASCII()
				require.NoError(t, err)
				require.Equal(t, "leaf", s)

				nested, err := top[1].ToList()
				require.NoError(t, err)
				require.Len(t, nested, 2)

				u, err := nested[0].ToUint()
				require.NoError(t, err)
				require.Equal(t, []uint64{1, 2, 3}, u)

				b, err := nested[1].ToBoolean()
				require.NoError(t, err)
				require.Equal(t, []bool{true, false}, b)
			},
		},
	}
}

func TestHSMSCompatibility_ItemTypeMatrix_V2HostToV1Equipment(t *testing.T) {
	port := freeLoopbackPort(t)
	ctx := t.Context()

	equipment := newV1HSMSSEndpoint(t, ctx, port, false, true, v1EchoHandler)
	require.NoError(t, equipment.conn.(*v1hsmsss.Connection).Open(false))
	defer closeV1Endpoint(t, equipment)

	host := newV2HSMSSEndpoint(t, port, true, false, nil)
	require.NoError(t, host.conn.Open(ctx, v2hsms.OpenBackground))
	defer closeV2Endpoint(t, host)

	waitV2Selected(t, host)

	for _, tc := range v2ItemMatrixCases() {
		t.Run(tc.name, func(t *testing.T) {
			reply, err := host.conn.SendDataMessage(ctx, 1, 1, true, tc.item)
			require.NoError(t, err)
			require.NotNil(t, reply)

			item, err := reply.Item()
			require.NoError(t, err)
			tc.check(t, item)
		})
	}
}

// Note: run under `go test -race`, this test's back-to-back SendDataMessage calls on one v1
// session trip a pre-existing v1.18.0 internal data-message-pool race (ID bytes of a pooled
// *DataMessage rewritten for the next send while the receiver goroutine is still reading the
// previous one in replyToSender) — one of the "dissolved v1 landmines" the v2 rewrite's package
// doc calls out as structurally fixed. It's a v1-only artifact of sustained traffic on a single
// session, not a v1/v2 compatibility bug or an issue with this test; the single-send-per-connection
// tests elsewhere in this suite don't trigger it. `make test-compat` doesn't run under -race.
func TestHSMSCompatibility_ItemTypeMatrix_V1HostToV2Equipment(t *testing.T) {
	port := freeLoopbackPort(t)
	ctx := t.Context()

	equipment := newV2HSMSSEndpoint(t, port, false, true, v2EchoHandler)
	require.NoError(t, equipment.conn.Open(ctx, v2hsms.OpenBackground))
	defer closeV2Endpoint(t, equipment)

	host := newV1HSMSSEndpoint(t, ctx, port, true, false, nil)
	require.NoError(t, host.conn.(*v1hsmsss.Connection).Open(true))
	defer closeV1Endpoint(t, host)

	for _, tc := range v1ItemMatrixCases() {
		t.Run(tc.name, func(t *testing.T) {
			reply, err := host.session.SendDataMessage(1, 1, true, tc.item)
			require.NoError(t, err)
			require.NotNil(t, reply)
			defer reply.Free()

			tc.check(t, reply.Item())
		})
	}
}

func recordingV1Handler(ch chan<- string) v1hsms.DataMessageHandler {
	return func(msg *v1hsms.DataMessage, _ v1hsms.Session) {
		got, err := msg.Item().ToASCII()
		if err != nil {
			return
		}
		ch <- got
	}
}

func recordingV2Handler(ch chan<- string) v2hsms.DataMessageHandler {
	return func(msg *v2hsms.DataMessage, _ v2hsms.SECS2Endpoint) {
		item, err := msg.Item()
		if err != nil {
			return
		}
		got, err := item.ToASCII()
		if err != nil {
			return
		}
		ch <- got
	}
}

// A wbit=false (replyExpected=false) send is a distinct wire path from the synchronous
// send/reply round trips covered elsewhere in this suite: the sender doesn't wait for or expect
// a correlated reply, so these tests confirm both that the call returns immediately with a nil
// reply and that the peer still decodes and delivers the message to its handler.
func TestHSMSCompatibility_NoReplyMessage_V2HostToV1Equipment(t *testing.T) {
	port := freeLoopbackPort(t)
	ctx := t.Context()
	received := make(chan string, 1)

	equipment := newV1HSMSSEndpoint(t, ctx, port, false, true, recordingV1Handler(received))
	require.NoError(t, equipment.conn.(*v1hsmsss.Connection).Open(false))
	defer closeV1Endpoint(t, equipment)

	host := newV2HSMSSEndpoint(t, port, true, false, nil)
	require.NoError(t, host.conn.Open(ctx, v2hsms.OpenBackground))
	defer closeV2Endpoint(t, host)

	waitV2Selected(t, host)

	reply, err := host.conn.SendDataMessage(ctx, 1, 1, false, v2secs2.A("v2-no-reply"))
	require.NoError(t, err)
	require.Nil(t, reply)

	select {
	case got := <-received:
		require.Equal(t, "v2-no-reply", got)
	case <-time.After(2 * time.Second):
		t.Fatal("v1 equipment never received the no-reply message")
	}
}

func TestHSMSCompatibility_NoReplyMessage_V1HostToV2Equipment(t *testing.T) {
	port := freeLoopbackPort(t)
	ctx := t.Context()
	received := make(chan string, 1)

	equipment := newV2HSMSSEndpoint(t, port, false, true, recordingV2Handler(received))
	require.NoError(t, equipment.conn.Open(ctx, v2hsms.OpenBackground))
	defer closeV2Endpoint(t, equipment)

	host := newV1HSMSSEndpoint(t, ctx, port, true, false, nil)
	require.NoError(t, host.conn.(*v1hsmsss.Connection).Open(true))
	defer closeV1Endpoint(t, host)

	reply, err := host.session.SendDataMessage(1, 1, false, v1secs2.A("v1-no-reply"))
	require.NoError(t, err)
	require.Nil(t, reply)

	select {
	case got := <-received:
		require.Equal(t, "v1-no-reply", got)
	case <-time.After(2 * time.Second):
		t.Fatal("v2 equipment never received the no-reply message")
	}
}

// Linktest is a distinct control-message wire path (separate SType) from data messages, so it
// needs its own interop coverage. v2's public Connection surface only exposes data-message sends
// (see hsmsss/doc.go), so a manual linktest send can only be driven from the v1 side; the reverse
// direction is exercised by configuring v2's own auto-linktest and observing it succeed against
// a v1 peer that never initiates one itself (WithAutoLinktest(false) in newV1HSMSSEndpoint).
func TestHSMSCompatibility_Linktest_V1InitiatedToV2(t *testing.T) {
	port := freeLoopbackPort(t)
	ctx := t.Context()

	equipment := newV2HSMSSEndpoint(t, port, false, true, nil)
	require.NoError(t, equipment.conn.Open(ctx, v2hsms.OpenBackground))
	defer closeV2Endpoint(t, equipment)

	host := newV1HSMSSEndpoint(t, ctx, port, true, false, nil)
	require.NoError(t, host.conn.(*v1hsmsss.Connection).Open(true))
	defer closeV1Endpoint(t, host)

	reply, err := host.session.SendMessage(v1hsms.NewLinktestReq(v1hsms.GenerateMsgSystemBytes()))
	require.NoError(t, err)
	require.NotNil(t, reply)
	require.Equal(t, v1hsms.LinkTestRspType, reply.Type())

	v2Metrics := equipment.conn.(v2hsmsss.Connection).ControlMetrics()
	require.EqualValues(t, 1, v2Metrics.LinktestReqRecvCount())
}

func TestHSMSCompatibility_Linktest_V2AutoToV1(t *testing.T) {
	port := freeLoopbackPort(t)
	ctx := t.Context()

	equipment := newV1HSMSSEndpoint(t, ctx, port, false, true, nil)
	require.NoError(t, equipment.conn.(*v1hsmsss.Connection).Open(false))
	defer closeV1Endpoint(t, equipment)

	host := newV2HSMSSEndpoint(t, port, true, false, nil,
		v2hsmsss.WithConnectionOption(v2hsms.WithLinktestInterval(30*time.Millisecond)))
	require.NoError(t, host.conn.Open(ctx, v2hsms.OpenBackground))
	defer closeV2Endpoint(t, host)

	waitV2Selected(t, host)

	hostMetrics := host.conn.(v2hsmsss.Connection).ControlMetrics()
	require.Eventually(t, func() bool {
		return hostMetrics.LinktestRecvCount() >= 2
	}, 2*time.Second, 10*time.Millisecond)
	require.Zero(t, hostMetrics.LinktestErrCount())
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
	extraOpts ...v2hsmsss.Option,
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
	opts = append(opts, extraOpts...)

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
