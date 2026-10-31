package bus

import "github.com/arloliu/go-secs/tracepack/internal/errtext"

// errNamespace begins the text of every error the module builds.
const errNamespace = "tracepack: "

// wrapf is fmt.Errorf with each line of the text naming errNamespace at most once,
// as the root package renders its errors:
// the chain is fmt.Errorf's, unchanged, and the text is fmt's rendering with every later errNamespace on a line removed.
// It forwards format and args to fmt.Errorf, so vet checks its calls as it checks fmt.Errorf's.
func wrapf(format string, args ...any) error {
	return errtext.Wrapf(errNamespace, format, args...)
}
