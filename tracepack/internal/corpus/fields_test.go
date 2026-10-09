package corpus

import (
	"testing"

	"github.com/stretchr/testify/require"

	"github.com/arloliu/go-secs/tracepack"
)

func TestReadFields(t *testing.T) {
	t.Parallel()

	full := dataRecord(0, 1, []byte("ab"))
	short := dataRecord(1, 1, nil)
	short.Payload = short.Payload[:9]
	short.SetCapturedFieldValidity()
	hidden := dataRecord(2, 1, nil)
	hidden.FieldValidity &^= tracepack.FieldValiditySystemBytes
	// A Select.rsp: SType 2, bytes 6 and 7 its status, read positionally.
	control := tracepack.Record{
		Seq: 3, TSUTCNs: testHour + 3000, Epoch: 1, Kind: tracepack.KindControl, Dir: tracepack.DirEquipmentToHost,
		Payload: []byte{0, 0, 0, 10, 0xFF, 0xFF, 0x80, 0x03, 0x00, 0x02, 0xDE, 0xAD, 0xBE, 0xEF},
	}
	control.SetCapturedFieldValidity()
	event := eventRecord(t, 4, 1, &tracepack.TransportEvent{Event: tracepack.EventSocketClose})
	tiny := dataRecord(5, 1, nil)
	tiny.Payload = tiny.Payload[:3]
	tiny.SetCapturedFieldValidity()

	pack := writePack(t, testMeta(), [][]tracepack.Record{{full, short, hidden, control, event, tiny}}, true)
	rows, err := ReadFields(t.Context(), openPack(t, pack))
	require.NoError(t, err)
	b, err := rows.Marshal()
	require.NoError(t, err)
	require.Equal(t, `[
  {
    "seq": "0",
    "available": [
      "session_id",
      "stream_and_w",
      "function",
      "ptype",
      "stype",
      "system_bytes"
    ],
    "session_id": 7,
    "stream": 1,
    "w": true,
    "function": 1,
    "ptype": 0,
    "stype": 0,
    "system_bytes": "AAAAAA=="
  },
  {
    "seq": "1",
    "available": [
      "session_id",
      "stream_and_w",
      "function",
      "ptype"
    ],
    "session_id": 7,
    "stream": 1,
    "w": true,
    "function": 1,
    "ptype": 0
  },
  {
    "seq": "2",
    "available": [
      "session_id",
      "stream_and_w",
      "function",
      "ptype",
      "stype"
    ],
    "session_id": 7,
    "stream": 1,
    "w": true,
    "function": 1,
    "ptype": 0,
    "stype": 0
  },
  {
    "seq": "3",
    "available": [
      "session_id",
      "stream_and_w",
      "function",
      "ptype",
      "stype",
      "system_bytes"
    ],
    "session_id": 65535,
    "stream": 0,
    "w": true,
    "function": 3,
    "ptype": 0,
    "stype": 2,
    "system_bytes": "3q2+7w=="
  },
  {
    "seq": "5",
    "available": []
  }
]
`, string(b))
}

func TestMarshalFieldsSortsBySeq(t *testing.T) {
	t.Parallel()

	b, err := FieldRows{{Seq: 10, Available: []string{}}, {Seq: 9, Available: []string{}}}.Marshal()
	require.NoError(t, err)
	require.Equal(t, `[
  {
    "seq": "9",
    "available": []
  },
  {
    "seq": "10",
    "available": []
  }
]
`, string(b))
}
