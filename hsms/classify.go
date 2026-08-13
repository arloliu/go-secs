package hsms

import (
	"context"
	"errors"
	"net"
)

// TransientError is implemented by an error that is safe to retry — the same call may succeed on a later attempt.
//
// A caller-defined error, or a sentinel from another transport package such as secs1,
// opts into IsTransient's extensible path by implementing this on its own type.
// secs1 marks its retryable sentinels this way (see secs1's errors.go) without changing their identity.
type TransientError interface {
	error
	Transient() bool
}

// TimeoutError is implemented by an error that reports a protocol or context timer expiry.
//
// It is shape-compatible with net.Error:
// any net.Error implementation (Timeout() bool plus the deprecated Temporary() bool) already satisfies this narrower two-method interface,
// so a wrapped dial/read/write timeout is recognized by IsTimeout without an adapter.
type TimeoutError interface {
	error
	Timeout() bool
}

// transientSentinels lists the hsms sentinels IsTransient recognizes by errors.Is.
// Only a sentinel documented as transient in its own godoc (hsms/errors.go) belongs here,
// and only one that can actually reach a public return — see the classification table in that file.
var transientSentinels = []error{
	ErrNotSelectedState,
	ErrConnClosed,
	ErrT3Timeout,
	ErrT6Timeout,
}

// timeoutSentinels lists the hsms sentinels IsTimeout recognizes by errors.Is.
// It is independent of transientSentinels — transient and timeout are orthogonal axes; see
// ErrCloseTimeout's godoc for a sentinel that is a timeout but not transient.
var timeoutSentinels = []error{
	ErrT3Timeout,
	ErrT6Timeout,
	ErrCloseTimeout,
}

// IsTransient reports whether err is safe to retry — the same call may succeed on a later attempt.
//
// Classification order:
//  1. If err's chain contains a value implementing TransientError, its Transient() result is
//     returned verbatim. This is the extensible path: hsms sentinels documented as transient
//     already satisfy it internally, secs1 marks its own retryable sentinels the same way, and a
//     caller-defined error can opt in without this package knowing about it.
//  2. Otherwise err is matched against the built-in hsms sentinel table via errors.Is, so a wrapped
//     (fmt.Errorf("…: %w", …)) or errors.Join'd form still matches.
//  3. Anything else returns false.
//     An error this function does not recognize is treated as permanent:
//     misclassifying a permanent error as retryable risks a silent retry storm, while the reverse only fails a job early.
//
// errors.Join policy: any matching branch wins.
// errors.Join(ErrMessageTooLarge, ErrNotSelectedState) reports true, the same as ErrNotSelectedState alone —
// a permanent branch elsewhere in the tree does NOT veto a transient one.
// This mirrors how errors.Is itself treats a Join tree (a match anywhere in the tree satisfies the check),
// so IsTransient stays consistent with ordinary errors.Is usage against the same error.
// A caller that joins a permanent cause together with a transient one, and needs the permanent cause to veto a retry,
// must inspect the branches itself.
// IsTransient answers "is there a reason this might succeed," not "will every branch resolve on retry."
func IsTransient(err error) bool {
	if err == nil {
		return false
	}

	var te TransientError
	if errors.As(err, &te) {
		return te.Transient()
	}

	for _, sentinel := range transientSentinels {
		if errors.Is(err, sentinel) {
			return true
		}
	}

	return false
}

// IsTimeout reports whether err is a protocol or context timer expiry.
//
// Classification order:
//  1. If err's chain contains a value implementing TimeoutError — which includes any net.Error, and
//     context.DeadlineExceeded itself — its Timeout() result is returned verbatim.
//  2. Otherwise err is matched against the built-in hsms sentinel table via errors.Is.
//  3. context.DeadlineExceeded and an unmarked net.Error whose Timeout() is true both classify as a
//     timeout — the raw stdlib signal, for an error this function does not otherwise recognize. In
//     practice step 1 already catches both cases (their concrete types implement Timeout() bool),
//     but this step keeps the documented contract explicit rather than relying on that overlap.
//  4. Anything else returns false.
//
// errors.Join policy: any matching branch wins, the same as IsTransient — see its godoc for the full rationale.
// errors.Join(ErrMessageTooLarge, ErrT3Timeout) reports true, even though ErrMessageTooLarge alone is not a timeout:
// a timeout happened somewhere in the tree, which is what this function answers.
func IsTimeout(err error) bool {
	if err == nil {
		return false
	}

	var te TimeoutError
	if errors.As(err, &te) {
		return te.Timeout()
	}

	for _, sentinel := range timeoutSentinels {
		if errors.Is(err, sentinel) {
			return true
		}
	}

	if errors.Is(err, context.DeadlineExceeded) {
		return true
	}

	var ne net.Error
	if errors.As(err, &ne) {
		return ne.Timeout()
	}

	return false
}
