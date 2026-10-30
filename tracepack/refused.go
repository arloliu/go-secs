package tracepack

import "errors"

var (
	// errExternal marks an error that came back from an implementation the caller supplied,
	// or from the system's random source; externalError matches it.
	errExternal = errors.New("tracepack: error of a supplied implementation")
	// errArgument marks an error the call's arguments or options determine that wraps no sentinel of its own;
	// argError matches it.
	errArgument = errors.New("tracepack: invalid argument")
)

// refusalSentinels are the sentinels whose errors refuse marks when a writing call returns one:
// each reports a fault the call's own arguments, options or records determine.
var refusalSentinels = [...]error{
	ErrSeqOrder, ErrPayloadTooLarge, ErrFieldValidity, ErrMetadataCommitment, ErrValidation, ErrScopeBreach,
	ErrInvalidRecord, ErrRequiredTag, ErrFieldValue, ErrFieldLength, ErrAnnotationText, ErrReservedTag,
}

// refusedError adds ErrRefused to err without changing its text.
type refusedError struct {
	err error
}

// externalError marks err as the error of a supplied implementation, without changing its text,
// so refuse never adds ErrRefused to a chain holding it.
type externalError struct {
	err error
}

// argError marks err as an invalid argument or option of the call, without changing its text.
type argError struct {
	err error
}

// refuse returns err with ErrRefused added when it is a refusal of a writing call, and err unchanged otherwise.
//
// It returns err unchanged when err is nil or already matches ErrRefused, so nested writing calls mark once;
// when err holds an external error, which wins over any sentinel beside it,
// so a supplied implementation cannot impersonate a refusal and a chain of mixed origin stays unmarked;
// and when err reports the Writer's prior state, ErrWriterFailed or ErrClosed.
// Otherwise it marks err when it wraps one of refusalSentinels or errArgument.
func refuse(err error) error {
	switch {
	case err == nil, errors.Is(err, ErrRefused):
		return err
	case errors.Is(err, errExternal):
		return err
	case errors.Is(err, ErrWriterFailed), errors.Is(err, ErrClosed):
		return err
	case errors.Is(err, errArgument):
		return &refusedError{err: err}
	}
	for _, s := range refusalSentinels {
		if errors.Is(err, s) {
			return &refusedError{err: err}
		}
	}

	return err
}

// external returns err marked as the error of a supplied implementation, or nil when err is nil.
func external(err error) error {
	if err == nil {
		return nil
	}

	return &externalError{err: err}
}

// argument returns err marked as an invalid argument or option of the call.
func argument(err error) error {
	return &argError{err: err}
}

// Error returns the text of the error it marks.
func (e *refusedError) Error() string { return e.err.Error() }

// Unwrap returns the error it marks.
func (e *refusedError) Unwrap() error { return e.err }

// Is reports whether target is ErrRefused.
func (e *refusedError) Is(target error) bool { return target == ErrRefused }

// Error returns the text of the error it marks.
func (e *externalError) Error() string { return e.err.Error() }

// Unwrap returns the error it marks.
func (e *externalError) Unwrap() error { return e.err }

// Is reports whether target is errExternal.
func (e *externalError) Is(target error) bool { return target == errExternal }

// Error returns the text of the error it marks.
func (e *argError) Error() string { return e.err.Error() }

// Unwrap returns the error it marks.
func (e *argError) Unwrap() error { return e.err }

// Is reports whether target is errArgument.
func (e *argError) Is(target error) bool { return target == errArgument }
