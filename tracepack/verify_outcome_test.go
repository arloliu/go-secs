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
		want         VerifyOutcome
	}{
		{name: "finalized without blocks", finalized: true, want: VerifyOutcomeFinalizedConsistent},
		{name: "finalized, every block validated", finalized: true, failed: []bool{ok, ok, ok}, want: VerifyOutcomeFinalizedConsistent},
		{name: "finalized, footer inconsistent", finalized: true, failed: []bool{ok, ok, ok}, inconsistent: true,
			want: VerifyOutcomeFinalizedInconsistent},
		{name: "finalized, failed middle block", finalized: true, failed: []bool{ok, fail, ok}, want: VerifyOutcomeCorruptMiddle},
		{name: "finalized, failed first block", finalized: true, failed: []bool{fail, ok, ok}, want: VerifyOutcomeCorruptMiddle},
		{name: "finalized, failed middle block and inconsistent", finalized: true, failed: []bool{ok, fail, ok}, inconsistent: true,
			want: VerifyOutcomeCorruptMiddle},
		{name: "finalized, failed last block", finalized: true, failed: []bool{ok, ok, fail}, want: VerifyOutcomeFinalizedTruncated},
		{name: "finalized, failed last block and inconsistent", finalized: true, failed: []bool{ok, ok, fail}, inconsistent: true,
			want: VerifyOutcomeFinalizedTruncated},
		{name: "finalized, every block failed", finalized: true, failed: []bool{fail, fail}, want: VerifyOutcomeFinalizedTruncated},
		{name: "finalized, walk stopped", finalized: true, failed: []bool{ok, ok}, walkStopped: true, want: VerifyOutcomeFinalizedTruncated},
		{name: "finalized, walk stopped and inconsistent", finalized: true, failed: []bool{ok, ok}, walkStopped: true, inconsistent: true,
			want: VerifyOutcomeFinalizedTruncated},
		{name: "finalized, walk stopped at the first envelope", finalized: true, walkStopped: true, want: VerifyOutcomeFinalizedTruncated},
		{name: "unfinalized without blocks", want: VerifyOutcomeUnfinalized},
		{name: "unfinalized, every block validated", failed: []bool{ok, ok}, want: VerifyOutcomeUnfinalized},
		{name: "unfinalized, failed last block", failed: []bool{ok, fail}, want: VerifyOutcomeUnfinalized},
		{name: "unfinalized, walk stopped", failed: []bool{ok, ok}, walkStopped: true, want: VerifyOutcomeUnfinalized},
		{name: "unfinalized, inconsistent", failed: []bool{ok}, inconsistent: true, want: VerifyOutcomeUnfinalized},
		{name: "unfinalized, failed first block", failed: []bool{fail, ok}, want: VerifyOutcomeCorruptMiddle},
		{name: "unfinalized, failed middle block, walk stopped", failed: []bool{ok, fail, ok}, walkStopped: true, want: VerifyOutcomeCorruptMiddle},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()

			assert.Equal(t, tt.want, classifyOutcome(tt.finalized, tt.failed, tt.walkStopped, tt.inconsistent))
		})
	}
}
