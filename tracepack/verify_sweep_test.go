package tracepack

import (
	"bytes"
	"fmt"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// verifySweep judges the Reports of Verify over mutated copies of one sweep pack against its baseline.
type verifySweep struct {
	b        *sweepBaseline
	failures int
}

// failf reports a failed rule, in full for the first sweepFailureLimit failures.
func (s *verifySweep) failf(t *testing.T, format string, args ...any) {
	t.Helper()

	s.failures++
	if s.failures <= sweepFailureLimit {
		t.Errorf(format, args...)
	}
}

// verify runs Verify over data and returns its Report, or ok false when it failed.
// It must fail exactly when wantErr is set.
func (s *verifySweep) verify(t *testing.T, data []byte, what string, wantErr bool) (Report, bool) {
	t.Helper()

	rep, err := Verify(t.Context(), bytes.NewReader(data), int64(len(data)), VerifyOptions{Reader: s.b.opts})
	switch {
	case err != nil && !wantErr:
		s.failf(t, "%s: Verify failed: %v", what, err)
	case err == nil && wantErr:
		s.failf(t, "%s: Verify succeeded with %s, want an error", what, rep.Outcome)
	default:
	}

	return rep, err == nil
}

// checkValidated reads, in data, every located block rep does not list as failed,
// and requires each to read and each of its records to equal the stored record of its seq, extension bytes and payload included,
// so a report that validates a block holding anything else, or counts records it does not hold, fails the sweep.
// It also requires rep's block, validated-block and record counts to agree with what it read.
func (s *verifySweep) checkValidated(t *testing.T, data []byte, what string, rep *Report) {
	t.Helper()

	r, err := Open(t.Context(), bytes.NewReader(data), int64(len(data)), s.b.opts)
	if err != nil {
		s.failf(t, "%s: Open failed after Verify succeeded: %v", what, err)
		return
	}
	failed := make(map[int]bool)
	for _, f := range rep.Failed {
		if f.Block >= 0 {
			failed[f.Block] = true
		}
	}

	var (
		buf       blockBuf
		validated int
		records   uint64
	)
	for i := range r.blocks {
		if failed[i] {
			continue
		}
		d, _, err := r.readBlock(i, &buf)
		if err != nil || d == nil {
			s.failf(t, "%s: block %d is validated but does not read: %v", what, i, err)
			continue
		}
		validated++
		for j := range d.count() {
			h := d.header(j)
			ref, ok := s.b.stored[h.Seq]
			got := storedRecord(&h, d.payload(j))
			if !ok || !sameRecord(got, ref.rec) || !bytes.Equal(h.Extra, ref.extra) {
				s.failf(t, "%s: block %d record %d (seq %d) differs from the stored record", what, i, j, h.Seq)
			}
			records++
		}
	}

	if rep.Blocks != len(r.blocks) || rep.Validated != validated || rep.Records != records {
		s.failf(t, "%s: report counts %d blocks, %d validated, %d records; the pack reads %d, %d, %d",
			what, rep.Blocks, rep.Validated, rep.Records, len(r.blocks), validated, records)
	}
}

// blockAt returns the index of the block of the unmutated pack holding the file offset off, or -1.
func (b *sweepBaseline) blockAt(off uint64) int {
	for i := range b.blocks {
		blk := &b.blocks[i]
		if off >= blk.Offset && off < blk.Offset+uint64(blk.OnDiskLen) {
			return i
		}
	}

	return -1
}

// TestVerifyCorruptionSweep inverts every byte of the sweep packs, one at a time, and verifies each mutated pack.
//
// Every byte lies under a CRC (see TestReaderCorruptionSweep), so the outcome of each mutation is exact:
// a byte before the first block is an error;
// a byte of a block fails that block, whose F-2 entry locates the blocks after it,
// so the outcome is corrupt-middle for any block but the last and finalized-truncated for the last;
// a byte of the footer or the trailer leaves the pack unfinalized with every block validated.
// Never is a mutated pack finalized-consistent, and every block reported validated reads its stored records exactly.
func TestVerifyCorruptionSweep(t *testing.T) {
	t.Parallel()

	for _, v := range sweepVariants() {
		t.Run(v.name, func(t *testing.T) {
			t.Parallel()

			file := sweepPack(t, v.codec, v.validate)
			s := &verifySweep{b: newSweepBaseline(t, file, &v)}
			rep := mustVerify(t, file)
			require.Equal(t, OutcomeFinalizedConsistent, rep.Outcome, "%+v", rep)
			last := len(s.b.blocks) - 1

			data := bytes.Clone(file)
			for off := range uint64(len(data)) {
				data[off] ^= 0xFF
				what := fmt.Sprintf("byte %d (%s) inverted", off, s.b.region(off))

				rep, ok := s.verify(t, data, what, off < s.b.blocksStart)
				if ok {
					want := OutcomeUnfinalized
					if blk := s.b.blockAt(off); blk == last {
						want = OutcomeFinalizedTruncated
					} else if blk >= 0 {
						want = OutcomeCorruptMiddle
					}
					if rep.Outcome != want {
						s.failf(t, "%s: outcome %s, want %s", what, rep.Outcome, want)
					}
					s.checkValidated(t, data, what, &rep)
				}
				data[off] ^= 0xFF
			}

			if s.failures > sweepFailureLimit {
				t.Errorf("%d more failures", s.failures-sweepFailureLimit)
			}
		})
	}
}

// TestVerifyTruncationSweep verifies every prefix of the sweep packs.
//
// A prefix without the whole pack metadata is an error.
// A longer strict prefix is unfinalized, and its validated blocks are exactly the blocks that end within it,
// read with their stored records: no block of the validated prefix is lost.
// The whole file is finalized-consistent.
func TestVerifyTruncationSweep(t *testing.T) {
	t.Parallel()

	for _, v := range sweepVariants() {
		t.Run(v.name, func(t *testing.T) {
			t.Parallel()

			file := sweepPack(t, v.codec, v.validate)
			s := &verifySweep{b: newSweepBaseline(t, file, &v)}

			for n := range uint64(len(file)) + 1 {
				what := fmt.Sprintf("prefix of %d bytes", n)
				rep, ok := s.verify(t, file[:n], what, n < s.b.blocksStart)
				if !ok {
					continue
				}

				whole := n == uint64(len(file))
				want, within, end := OutcomeUnfinalized, 0, s.b.blocksStart
				for i := range s.b.blocks {
					if blk := &s.b.blocks[i]; blk.Offset+uint64(blk.OnDiskLen) <= n {
						within, end = i+1, blk.Offset+uint64(blk.OnDiskLen)
					}
				}
				if whole {
					want = OutcomeFinalizedConsistent
				}

				switch {
				case rep.Outcome != want:
					s.failf(t, "%s: outcome %s, want %s", what, rep.Outcome, want)
				case rep.Validated != within:
					s.failf(t, "%s: %d blocks validated, %d end within the prefix", what, rep.Validated, within)
				case rep.PrefixEnd != end:
					s.failf(t, "%s: validated prefix ends at %d, want %d", what, rep.PrefixEnd, end)
				default:
				}
				s.checkValidated(t, file[:n], what, &rep)
			}

			if s.failures > sweepFailureLimit {
				t.Errorf("%d more failures", s.failures-sweepFailureLimit)
			}
			assert.Zero(t, s.failures)
		})
	}
}
