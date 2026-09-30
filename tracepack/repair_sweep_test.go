package tracepack

import (
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
			start := blocksStartOf(t, file)
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
