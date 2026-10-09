package tracepack

import (
	"bytes"
	"encoding/binary"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/arloliu/go-secs/tracepack/internal/format"
)

// clockTestTime is the time the clock tests' Now returns:
// far from the time the tests run, with a part below the millisecond.
var clockTestTime = time.Unix(0, 1_000_000_000_123_456_789)

// clockTestNow returns clockTestTime.
func clockTestNow() time.Time {
	return clockTestTime
}

// uuidMillis returns the time part of a UUIDv7, its first 48 bits, as Unix milliseconds.
func uuidMillis(u [16]byte) int64 {
	var b [8]byte
	copy(b[2:], u[:6])

	return int64(binary.BigEndian.Uint64(b[:]))
}

// requireClockUUID requires u to be a UUIDv7 whose time part is at's millisecond.
func requireClockUUID(t *testing.T, u [16]byte, at time.Time, name string) {
	t.Helper()

	require.Equal(t, byte(0x70), u[6]&0xF0, "%s version nibble", name)
	require.Equal(t, byte(0x80), u[8]&0xC0, "%s variant bits", name)
	require.Equal(t, at.UnixMilli(), uuidMillis(u), "%s time part", name)
}

func TestWriterClock(t *testing.T) {
	t.Parallel()

	write := func(t *testing.T, opts WriterOptions) format.FileHeader {
		t.Helper()

		opts.Meta = repairTestMeta()
		var buf bytes.Buffer
		w, err := NewWriter(&buf, opts)
		require.NoError(t, err)
		_, err = w.Close()
		require.NoError(t, err)

		return layoutHeader(t, buf.Bytes())
	}

	t.Run("generated ids and writer_start_utc_ns", func(t *testing.T) {
		t.Parallel()

		hdr := write(t, WriterOptions{Now: clockTestNow})
		assert.Equal(t, clockTestTime.UnixNano(), hdr.WriterStartUTCNs)
		requireClockUUID(t, hdr.PackID, clockTestTime, "pack_id")
		requireClockUUID(t, hdr.CaptureID, clockTestTime, "capture_id")
		assert.NotEqual(t, hdr.PackID, hdr.CaptureID)
	})

	t.Run("supplied ids kept", func(t *testing.T) {
		t.Parallel()

		hdr := write(t, WriterOptions{Now: clockTestNow, PackID: UUID{0xAC}, CaptureID: UUID{0xCA}})
		assert.Equal(t, clockTestTime.UnixNano(), hdr.WriterStartUTCNs)
		assert.Equal(t, format.UUID{0xAC}, hdr.PackID)
		assert.Equal(t, format.UUID{0xCA}, hdr.CaptureID)
	})

	t.Run("nil means time.Now", func(t *testing.T) {
		t.Parallel()

		before := time.Now()
		hdr := write(t, WriterOptions{})
		after := time.Now()
		assert.GreaterOrEqual(t, hdr.WriterStartUTCNs, before.UnixNano())
		assert.LessOrEqual(t, hdr.WriterStartUTCNs, after.UnixNano())
		for name, u := range map[string]format.UUID{"pack_id": hdr.PackID, "capture_id": hdr.CaptureID} {
			assert.GreaterOrEqual(t, uuidMillis(u), before.UnixMilli(), name)
			assert.LessOrEqual(t, uuidMillis(u), after.UnixMilli(), name)
		}
	})
}

func TestSegmentWriterClock(t *testing.T) {
	t.Parallel()

	// Close stamps the stop boundary with Now, so Now stays in the capture's period.
	at := segAt(time.Minute + 123_456_789)
	w, sink := newSeg(t, func(o *SegmentWriterOptions) { o.Now = nowAt(at) })
	segFrame(t, w, time.Second)
	require.NoError(t, w.Rotate(t.Context()))
	segFrame(t, w, 2*time.Second)
	require.NoError(t, w.Close(t.Context()))

	requireClockUUID(t, w.CaptureID(), at, "capture_id")
	segs := sink.segments(t)
	require.Len(t, segs, 2, "Rotate opened a second segment")
	ids := make(map[UUID]bool, len(segs))
	for i, r := range segs {
		h := r.Header()
		assert.Equal(t, at.UnixNano(), h.WriterStartUTCNs, "segment %d", i)
		assert.Equal(t, w.CaptureID(), h.CaptureID, "segment %d", i)
		requireClockUUID(t, h.PackID, at, "pack_id")
		ids[h.PackID] = true
	}
	assert.Len(t, ids, len(segs), "every segment has its own pack_id")
}

func TestMergeClock(t *testing.T) {
	t.Parallel()

	seg := mergePack(t, seg0, nil, hourSteps(10, 13, 2))
	v := mergeView(t, nil, seg)
	merge := func(t *testing.T, opts MergeOptions) MergeReport {
		t.Helper()

		opts.Now = clockTestNow
		var out bytes.Buffer
		rep, err := Merge(t.Context(), &out, v, inputsOf(t, v, seg), opts)
		require.NoError(t, err)
		h := mustOpen(t, out.Bytes(), ReaderOptions{}).Header()
		assert.Equal(t, clockTestTime.UnixNano(), h.WriterStartUTCNs)
		assert.Equal(t, rep.PackID, h.PackID)
		assert.Equal(t, &rep.ReplacementSetID, h.Meta.ReplacementSetID)

		return rep
	}

	t.Run("generated ids", func(t *testing.T) {
		t.Parallel()

		rep := merge(t, mergeOpts())
		requireClockUUID(t, rep.PackID, clockTestTime, "pack_id")
		requireClockUUID(t, rep.ReplacementSetID, clockTestTime, "replacement_set_id")
		assert.NotEqual(t, rep.PackID, rep.ReplacementSetID)
	})

	t.Run("supplied ids kept", func(t *testing.T) {
		t.Parallel()

		opts := mergeOpts()
		opts.PackID, opts.ReplacementSetID = archiveID, archiveSet
		rep := merge(t, opts)
		assert.Equal(t, archiveID, rep.PackID)
		assert.Equal(t, archiveSet, rep.ReplacementSetID)
	})
}

func TestRepairClock(t *testing.T) {
	t.Parallel()

	p := defaultRepairPack(t)
	damaged := flipByte(p.file, p.bodyByteOf(2))
	repair := func(t *testing.T, opts RepairOptions) RepairReport {
		t.Helper()

		opts.Now = clockTestNow
		patch, rep, err := repairBytes(t, damaged, opts)
		require.NoError(t, err)
		hdr := layoutHeader(t, patch)
		assert.Equal(t, clockTestTime.UnixNano(), hdr.WriterStartUTCNs)
		assert.Equal(t, format.UUID(rep.PackID), hdr.PackID)
		assert.Equal(t, layoutHeader(t, damaged).CaptureID, hdr.CaptureID, "the damaged pack's capture_id, never a generated one")

		return rep
	}

	t.Run("generated pack_id", func(t *testing.T) {
		t.Parallel()

		rep := repair(t, repairOpts())
		requireClockUUID(t, rep.PackID, clockTestTime, "pack_id")
	})

	t.Run("supplied pack_id kept", func(t *testing.T) {
		t.Parallel()

		opts := repairOpts()
		opts.PackID = UUID{0xAC}
		assert.Equal(t, UUID{0xAC}, repair(t, opts).PackID)
	})
}
