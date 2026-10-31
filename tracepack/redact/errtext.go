package redact

import "github.com/arloliu/go-secs/tracepack/internal/errtext"

// errNamespace begins the text of every error this package builds.
const errNamespace = "redact: "

// wrapf is fmt.Errorf with each line of the text naming errNamespace at most once:
// the chain is fmt.Errorf's, unchanged, and the text is fmt's rendering with every later errNamespace on a line removed.
// It forwards format and args to fmt.Errorf, so vet checks its calls as it checks fmt.Errorf's.
func wrapf(format string, args ...any) error {
	return errtext.Wrapf(errNamespace, format, args...)
}

// joinErrors is errors.Join with each line of the text naming errNamespace at most once:
// it drops the nil errors, keeps the others in order, and returns nil when none is left.
func joinErrors(errs ...error) error {
	return errtext.Join(errNamespace, errs...)
}
