package tracepack

import (
	"testing"

	"github.com/stretchr/testify/assert"
)

func TestClassifyOutcome(t *testing.T) {
	t.Parallel()

	const (
		ok   = false
		fail = true
	)
	tests := []struct {
		name         string
		finalized    bool
		failed       []bool
		walkStopped  bool
		inconsistent bool
		want         Outcome
	}{
		{name: "finalized without blocks", finalized: true, want: OutcomeFinalizedConsistent},
		{name: "finalized, every block validated", finalized: true, failed: []bool{ok, ok, ok}, want: OutcomeFinalizedConsistent},
		{name: "finalized, footer inconsistent", finalized: true, failed: []bool{ok, ok, ok}, inconsistent: true,
			want: OutcomeFinalizedInconsistent},
		{name: "finalized, failed middle block", finalized: true, failed: []bool{ok, fail, ok}, want: OutcomeCorruptMiddle},
		{name: "finalized, failed first block", finalized: true, failed: []bool{fail, ok, ok}, want: OutcomeCorruptMiddle},
		{name: "finalized, failed middle block and inconsistent", finalized: true, failed: []bool{ok, fail, ok}, inconsistent: true,
			want: OutcomeCorruptMiddle},
		{name: "finalized, failed last block", finalized: true, failed: []bool{ok, ok, fail}, want: OutcomeFinalizedTruncated},
		{name: "finalized, failed last block and inconsistent", finalized: true, failed: []bool{ok, ok, fail}, inconsistent: true,
			want: OutcomeFinalizedTruncated},
		{name: "finalized, every block failed", finalized: true, failed: []bool{fail, fail}, want: OutcomeFinalizedTruncated},
		{name: "finalized, walk stopped", finalized: true, failed: []bool{ok, ok}, walkStopped: true, want: OutcomeFinalizedTruncated},
		{name: "finalized, walk stopped and inconsistent", finalized: true, failed: []bool{ok, ok}, walkStopped: true, inconsistent: true,
			want: OutcomeFinalizedTruncated},
		{name: "finalized, walk stopped at the first envelope", finalized: true, walkStopped: true, want: OutcomeFinalizedTruncated},
		{name: "unfinalized without blocks", want: OutcomeUnfinalized},
		{name: "unfinalized, every block validated", failed: []bool{ok, ok}, want: OutcomeUnfinalized},
		{name: "unfinalized, failed last block", failed: []bool{ok, fail}, want: OutcomeUnfinalized},
		{name: "unfinalized, walk stopped", failed: []bool{ok, ok}, walkStopped: true, want: OutcomeUnfinalized},
		{name: "unfinalized, inconsistent", failed: []bool{ok}, inconsistent: true, want: OutcomeUnfinalized},
		{name: "unfinalized, failed first block", failed: []bool{fail, ok}, want: OutcomeCorruptMiddle},
		{name: "unfinalized, failed middle block, walk stopped", failed: []bool{ok, fail, ok}, walkStopped: true, want: OutcomeCorruptMiddle},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()

			assert.Equal(t, tt.want, classifyOutcome(tt.finalized, tt.failed, tt.walkStopped, tt.inconsistent))
		})
	}
}
