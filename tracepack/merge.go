package tracepack

import "errors"

// ErrMergeLimit reports a merge output over its budget:
// the archive's decoded footer, or the block summaries a merge keeps for it while it writes, would exceed the merge's footer budget.
// It wraps no other error.
var ErrMergeLimit = errors.New("tracepack: merge output over its budget")
