package bus

// This file mirrors the root package's errtext.go, whose helpers are unexported:
// the errors of this package render the tracepack namespace by the same rule.

import (
	"errors"
	"fmt"
	"strings"
)

// errNamespace begins the text of every error the module builds.
const errNamespace = "tracepack: "

// wrapError is an error wrapf builds over one cause:
// fmt's wrap of it, with the text that dedupNamespace renders.
type wrapError struct {
	text string
	err  error
}

// wrapMultiError is an error wrapf builds over several causes:
// fmt's wrap of them, with the text that dedupNamespace renders.
type wrapMultiError struct {
	text string
	errs []error
}

// wrapf is fmt.Errorf with each line of the text naming errNamespace at most once.
//
// The chain is the one fmt.Errorf builds from format and args, unchanged,
// so errors.Is and errors.As see every operand as they would through fmt.Errorf.
// The text is fmt's rendering of the same call, with dedupNamespace applied.
//
// It forwards format and args to fmt.Errorf unchanged, so vet checks its calls as it checks fmt.Errorf's.
func wrapf(format string, args ...any) error {
	err := fmt.Errorf(format, args...)
	text := dedupNamespace(err.Error())
	switch u := err.(type) { //nolint:errorlint // takes over this link of the chain, not the chain
	case interface{ Unwrap() []error }:
		return &wrapMultiError{text: text, errs: u.Unwrap()}
	case interface{ Unwrap() error }:
		return &wrapError{text: text, err: u.Unwrap()}
	default:
		return errors.New(text)
	}
}

// dedupNamespace returns s with every occurrence of errNamespace after the first on each line removed,
// lines being separated by '\n'; nothing else changes.
func dedupNamespace(s string) string {
	if strings.Count(s, errNamespace) < 2 {
		return s
	}
	lines := strings.Split(s, "\n")
	for i, line := range lines {
		first := strings.Index(line, errNamespace)
		if first < 0 {
			continue
		}
		rest := first + len(errNamespace)
		lines[i] = line[:rest] + strings.ReplaceAll(line[rest:], errNamespace, "")
	}

	return strings.Join(lines, "\n")
}

// Error returns the rendered text.
func (e *wrapError) Error() string { return e.text }

// Unwrap returns the cause fmt.Errorf wrapped.
func (e *wrapError) Unwrap() error { return e.err }

// Error returns the rendered text.
func (e *wrapMultiError) Error() string { return e.text }

// Unwrap returns the causes fmt.Errorf wrapped, in their order.
func (e *wrapMultiError) Unwrap() []error { return e.errs }
