package tracepack

import (
	"bytes"
	"errors"
	"fmt"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// repairSweepPack writes the repair test pack with codec c and widens block 1's record headers,
// so the sweeps also cover a block whose record_header_len exceeds 44.
func repairSweepPack(t testing.TB, c Codec) []byte {
	t.Helper()

	p := writeRepairPack(t, c, nil, false, repairSteps(t))
	file := widenFile(t, p.file, func(i int) bool { return i == 1 })
	require.Equal(t, OutcomeFinalizedConsistent, mustVerify(t, file).Outcome)

	return file
}

// TestRepairTruncationSweep cuts the repair sweep pack at every byte offset:
// a cut inside the file header or the pack metadata cannot be opened;
// every later cut short of the full length is repaired into a patch holding exactly the blocks that end at or before the cut,
// with one new coverage entry, the tail after the last copied seq;
// the full pack needs no repair.
func TestRepairTruncationSweep(t *testing.T) {
	t.Parallel()

	for _, c := range []Codec{CodecNone, CodecZstd} {
		t.Run(c.String(), func(t *testing.T) {
			t.Parallel()

			file := repairSweepPack(t, c)
			start := int(layoutOf(t, file).blocksStart)
			blocks := mustOpen(t, file, ReaderOptions{}).Blocks()
			capture := UUID(layoutHeader(t, file).CaptureID)
			h0, h1 := scopeHour()

			for cut := range len(file) + 1 {
				damaged := file[:cut]
				patch, rep, err := repairBytes(t, damaged, repairOpts())
				switch {
				case cut < start:
					require.Error(t, err, "cut %d", cut)
					require.NotErrorIs(t, err, ErrRepairNotNeeded, "cut %d", cut)

					continue
				case cut == len(file):
					require.ErrorIs(t, err, ErrRepairNotNeeded)

					continue
				default:
					require.NoError(t, err, "cut %d", cut)
				}

				checkPatch(t, file, damaged, patch, rep)
				var copied int
				next := uint64(10) // seq_start
				for _, b := range blocks {
					if b.Offset+uint64(b.OnDiskLen) <= uint64(cut) {
						copied++
						next = b.LastSeq + 1
					}
				}
				require.Equal(t, copied, rep.Blocks, "cut %d", cut)
				require.Equal(t, []Coverage{lostEntry(capture, next, nil, h0, h1)}, rep.Coverage, "cut %d", cut)
			}
		})
	}
}

// TestRepairFlipSweep inverts every byte of the repair sweep pack in turn:
// each result is an error that is not ErrRepairNotNeeded, since every byte lies under a CRC,
// or a patch that copies the validated blocks byte for byte, covers every record it lacks and verifies finalized-consistent.
func TestRepairFlipSweep(t *testing.T) {
	t.Parallel()

	for _, c := range []Codec{CodecNone, CodecZstd} {
		t.Run(c.String(), func(t *testing.T) {
			t.Parallel()

			file := repairSweepPack(t, c)
			causes := make(map[string]int)
			for off := range len(file) {
				damaged := flipByte(file, uint64(off))
				patch, rep, err := repairBytes(t, damaged, repairOpts())
				if err != nil {
					require.NotErrorIs(t, err, ErrRepairNotNeeded, "offset %d", off)
					require.Empty(t, patch, "offset %d: an error before the patch's header writes nothing", off)
					causes[errorClass(err)]++

					continue
				}
				causes["repaired"]++
				checkPatch(t, file, damaged, patch, rep)
			}
			t.Logf("%s: %v", c, causes)
			assert.Positive(t, causes["repaired"])
		})
	}
}

// errorClass names the sentinel err wraps, among those a damaged pack can produce.
func errorClass(err error) string {
	for _, s := range []error{ErrNotTracepack, ErrUnsupportedFormat, ErrChecksum, ErrReadLimit, ErrNotRepairable} {
		if errors.Is(err, s) {
			return s.Error()
		}
	}

	return fmt.Sprintf("other: %v", err)
}

// FuzzRepair repairs mutated packs.
// With an in-memory source and destination, every refusal is decided before the patch's header,
// so Repair either fails having written nothing or writes a patch that verifies finalized-consistent with a correct seq_start,
// copies the validated blocks byte for byte, holds every validated record,
// and declares each new coverage entry with its capture, first seq and time bounds.
func FuzzRepair(f *testing.F) {
	readerOpts := ReaderOptions{MaxPackMetadataLen: 1 << 20, MaxFooterLen: 1 << 20, MaxBlockLen: 1 << 20}
	opts := RepairOptions{Reader: readerOpts, Writer: "fuzz"}

	for _, c := range []Codec{CodecNone, CodecZstd} {
		file := repairSweepPack(f, c)
		f.Add(file)
		f.Add(file[:len(file)/2])
		f.Add(invalidFooterFile(f, file))
	}
	p := writeRepairPack(f, CodecZstd, nil, false, repairSteps(f))
	f.Add(flipAll(p.file, p, 2))
	f.Add(flipAll(invalidFooterFile(f, p.file), p, 1, 3))
	f.Add(withSeqStart(f, p.file, 9))
	for _, file := range fuzzIterateSeeds(f) {
		f.Add(file)
	}

	f.Fuzz(func(t *testing.T, data []byte) {
		var out bytes.Buffer
		rep, err := Repair(t.Context(), bytes.NewReader(data), int64(len(data)), &out, opts)
		if err != nil {
			require.Empty(t, out.Bytes(), "an in-memory repair fails only before writing: %v", err)

			return
		}

		patch := out.Bytes()
		require.Equal(t, uint64(len(patch)), rep.Size)
		v, err := Verify(t.Context(), bytes.NewReader(patch), int64(len(patch)), VerifyOptions{Reader: readerOpts})
		require.NoError(t, err)
		require.Equal(t, OutcomeFinalizedConsistent, v.Outcome, "failed %v, disagreements %v", v.Failed, v.Disagreements)
		require.Equal(t, rep.Verify.Records, v.Records)
		require.Equal(t, rep.Records, v.Records)
		require.False(t, hasSeqStartDefect(v.WriterDefects), "%v", v.WriterDefects)
		if rep.Verify.Outcome == OutcomeFinalizedConsistent {
			require.True(t, hasSeqStartDefect(rep.Verify.WriterDefects), "a consistent pack is repaired only for its seq_start")
		}

		r, err := Open(t.Context(), bytes.NewReader(data), int64(len(data)), readerOpts)
		require.NoError(t, err)
		a, err := r.analyze(t.Context(), true)
		require.NoError(t, err)
		var want [][]byte
		for i, b := range r.Blocks() {
			if !a.blocks[i].failed {
				want = append(want, data[b.Offset:b.Offset+uint64(b.OnDiskLen)])
			}
		}
		pr, err := Open(t.Context(), bytes.NewReader(patch), int64(len(patch)), readerOpts)
		require.NoError(t, err)
		var got [][]byte
		for _, b := range pr.Blocks() {
			got = append(got, patch[b.Offset:b.Offset+uint64(b.OnDiskLen)])
		}
		require.Equal(t, want, got, "the validated blocks, byte for byte")

		for i, c := range rep.Coverage {
			require.True(t, c.CaptureID != nil && c.SeqFirst != nil && c.TimeStart != nil && c.TimeEnd != nil, "new entry %d", i)
		}
	})
}
