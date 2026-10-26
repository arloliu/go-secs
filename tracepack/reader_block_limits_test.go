package tracepack

import (
	"encoding/binary"
	"fmt"
	"testing"

	"github.com/arloliu/go-secs/tracepack/internal/format"
	"github.com/stretchr/testify/require"
)

// TestCheckRecordsLimitErrors checks that checkRecords reports a header past the seq or payload_len limit
// with the error UnmarshalRecordHeader gives for it, at the first record and at a later one,
// for 44-byte and longer record headers.
func TestCheckRecordsLimitErrors(t *testing.T) {
	t.Parallel()

	const count = 5
	for _, rhl := range []int{format.RecordHeaderLen, 52} {
		for _, bad := range []struct {
			name string
			set  func(row []byte)
		}{
			{"seq 2^63", func(row []byte) { binary.LittleEndian.PutUint64(row[0:8], format.MaxU64+1) }},
			{"payload_len 2^31", func(row []byte) { binary.LittleEndian.PutUint32(row[28:32], format.MaxLen32+1) }},
			{"both", func(row []byte) {
				binary.LittleEndian.PutUint64(row[0:8], ^uint64(0))
				binary.LittleEndian.PutUint32(row[28:32], ^uint32(0))
			}},
		} {
			for _, at := range []int{0, 3} {
				t.Run(fmt.Sprintf("%d/%s/record %d", rhl, bad.name, at), func(t *testing.T) {
					t.Parallel()

					section := make([]byte, 0, count*rhl)
					for i := range count {
						h := format.RecordHeader{Seq: 10 + uint64(i), PayloadLen: 3, Extra: make([]byte, rhl-format.RecordHeaderLen)}
						section = format.AppendRecordHeader(section, &h)
					}
					row := section[at*rhl : (at+1)*rhl]
					bad.set(row)
					env := format.BlockEnvelope{
						RecordCount: count, RecordHeaderLen: uint16(rhl), FirstSeq: 10, UncompressedLen: uint32(count*rhl + count*3),
					}

					_, uerr := format.UnmarshalRecordHeader(row, format.RecordHeaderLen)
					require.ErrorIs(t, uerr, format.ErrLimit)
					_, _, err := checkRecords(section, &env, nil)
					require.ErrorIs(t, err, format.ErrLimit)
					require.EqualError(t, err, fmt.Sprintf("record %d: %v", at, uerr))
					var oe, uoe *format.OffsetError
					require.ErrorAs(t, err, &oe)
					require.ErrorAs(t, uerr, &uoe)
					require.Equal(t, uoe.Field, oe.Field)
				})
			}
		}
	}
}
