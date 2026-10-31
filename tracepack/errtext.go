package tracepack

// This file depends on the standard library and internal/errtext alone:
// a test copies both verbatim into a scratch module to run vet's printf check over wrapf.

import "github.com/arloliu/go-secs/tracepack/internal/errtext"

// errNamespace begins the text of every error the module builds.
const errNamespace = "tracepack: "

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

// dedupNamespace returns s with every occurrence of errNamespace after the first on each line removed,
// lines being separated by '\n'; nothing else changes.
func dedupNamespace(s string) string {
	return errtext.Dedup(errNamespace, s)
}
