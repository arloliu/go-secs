package tracepack

import (
	"bytes"
	"strings"
	"testing"
	"unsafe"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// txFill fills the hour memTestHour of src, with MaxScopes 1 and limit as MaxCachedBytes (0 for the default),
// and returns the batch, the charge of the hour's record when it was dropped, or as it stood when the fill failed, and the error.
func txFill(t *testing.T, src PackSource, limit int64) (*txBatch, int64, error) {
	t.Helper()

	var batch *txBatch
	var charge int64
	_, err := findTransactionsWith(t.Context(), src, txBatchHour(0),
		TxRangeOptions{Lookup: TxOptions{MaxScopes: 1}, MaxCachedBytes: limit}, func(*TxPairing) error { return nil },
		func(b *txBatch) {
			batch = b
			b.evicting = func(rec *txHourRecord) { charge = rec.charge }
		})
	if rec, ok := batch.hours[memTestHour]; ok {
		charge = rec.charge
	}

	return batch, charge, err
}

// requireExactAdmission requires the hour of src to fill under a MaxCachedBytes of exactly its charge,
// and to fail one byte under it with ErrReadLimit, the failed batch holding exactly the partial record's charge.
func requireExactAdmission(t *testing.T, src PackSource, charge int64) {
	t.Helper()

	_, _, err := txFill(t, src, charge)
	require.NoError(t, err, "a charge of exactly MaxCachedBytes")
	b, partial, err := txFill(t, src, charge-1)
	require.ErrorIs(t, err, ErrReadLimit, "one byte under the charge")
	assert.Equal(t, partial, b.cache.held, "the failed batch holds exactly what its partial record charged")
	assert.LessOrEqual(t, b.cache.held, charge-1)
}

// Each variable-sized part of an hour record is charged by its size:
// the value bytes of a coverage entry's unknown nested entries, the holders of a conflict's versions,
// and the bytes of a record's redaction entries,
// each measured as the difference between two hours that differ only in it;
// and each hour, one of no record among them,
// is admitted under a MaxCachedBytes of exactly its charge and refused one byte under it.
func TestFindTransactionsVariableCharges(t *testing.T) {
	t.Parallel()

	ptr, uuid := int64(unsafe.Sizeof(uintptr(0))), int64(unsafe.Sizeof(UUID{}))

	t.Run("coverage unknown entries", func(t *testing.T) {
		t.Parallel()

		withUnknown := func(n int) PackSource {
			return txSource(t, txPack(t, seg0, func(m *PackMeta) {
				m.Coverage = []Coverage{{SeqFirst: new(uint64(0)), SeqLast: new(uint64(5)),
					Unknown: []RawEntry{{Tag: 0x0050, Type: 7, Value: bytes.Repeat([]byte{1}, n)}}}}
			}, txBlock(txRecord(10, nil))))
		}
		_, small, _ := txFill(t, withUnknown(1), 0)
		_, large, _ := txFill(t, withUnknown(4097), 0)
		assert.Equal(t, int64(4096), large-small, "4096 more value bytes")
		requireExactAdmission(t, withUnknown(4097), large)
	})

	t.Run("an hour of no record", func(t *testing.T) {
		t.Parallel()

		src := txSource(t, txPack(t, seg0, func(m *PackMeta) {
			m.Coverage = []Coverage{{SeqFirst: new(uint64(0)), SeqLast: new(uint64(5)),
				Unknown: []RawEntry{{Tag: 0x0050, Type: 7, Value: bytes.Repeat([]byte{1}, 512)}}}}
		}, nil))
		b, charge, err := txFill(t, src, 0)
		require.NoError(t, err)
		require.Zero(t, b.res.Primaries)
		assert.Greater(t, charge, int64(512), "the coverage entry's diagnostics are charged without any item")
		requireExactAdmission(t, src, charge)
	})

	t.Run("conflict holders", func(t *testing.T) {
		t.Parallel()

		other := func(r *Record) {
			r.Payload = bytes.Clone(r.Payload)
			r.Payload[len(r.Payload)-1]++
		}
		hour := func(holders []UUID) PackSource {
			files := make([][]byte, 0, 1+len(holders))
			files = append(files, txPack(t, seg4, nil, txBlock(txRecord(10, other))))
			for _, id := range holders {
				files = append(files, txPack(t, id, nil, txBlock(txRecord(10, nil))))
			}

			return txSource(t, files...)
		}
		_, two, _ := txFill(t, hour([]UUID{seg0, seg1}), 0)
		_, four, _ := txFill(t, hour([]UUID{seg0, seg1, seg2, seg3}), 0)
		// Two more readers in the observation and two more holders of the first version.
		assert.Equal(t, 2*ptr+2*uuid, four-two)
		requireExactAdmission(t, hour([]UUID{seg0, seg1, seg2, seg3}), four)
	})

	t.Run("redaction entries", func(t *testing.T) {
		t.Parallel()

		withDomain := func(domain string) PackSource {
			m := padMeta(extractMinorMeta(0))
			e := maskedNoteEntry(3, false)
			e.Domain = domain
			m.Redaction = append(m.Redaction, e)
			return txSource(t, writeMinorPack(t, WriterOptions{Meta: m, Facts: PackFacts{AnyRedacted: true}},
				redactedRecord(0), maskedNote(3)))
		}
		_, short, _ := txFill(t, withDomain("note"), 0)
		_, long, _ := txFill(t, withDomain("note"+strings.Repeat("x", 1000)), 0)
		assert.Equal(t, int64(1000), long-short, "1000 more domain bytes in the record's redaction entry")
		requireExactAdmission(t, withDomain("note"+strings.Repeat("x", 1000)), long)
	})
}
