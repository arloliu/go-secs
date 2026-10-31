// Package errtext builds the errors of the module's packages
// so that each line of their text names the package's namespace at most once.
//
// The text of an error is fmt's own rendering, or errors.Join's,
// with every occurrence of the namespace after the first on each line removed;
// nothing else changes, and the chain is the one fmt.Errorf or errors.Join builds.
package errtext

// This file depends on the standard library alone:
// a test copies it verbatim into a scratch module to run vet's printf check over the wrappers of Wrapf.

import (
	"errors"
	"fmt"
	"strings"
)

// wrapError is an error Wrapf builds over one cause:
// fmt's wrap of it, with the text that Dedup renders.
type wrapError struct {
	text string
	err  error
}

// wrapMultiError is an error Wrapf builds over several causes, or Join builds:
// fmt's or errors.Join's wrap of them, with the text that Dedup renders.
type wrapMultiError struct {
	text string
	errs []error
}

// Wrapf is fmt.Errorf with each line of the text naming ns at most once.
//
// The chain is the one fmt.Errorf builds from format and args, unchanged:
// the error unwraps to the causes fmt.Errorf's error unwraps to, one or several as it made them,
// so errors.Is and errors.As see every operand as they would through fmt.Errorf.
// The text is fmt's rendering of the same call, with Dedup applied.
//
// It forwards format and args to fmt.Errorf unchanged,
// so vet checks its calls, and the calls of a function that forwards to it, as it checks fmt.Errorf's.
//
// Parameters:
//   - ns: the namespace, such as "tracepack: ".
//   - format, args: as fmt.Errorf takes them.
//
// Returns:
//   - error: the wrapped error, never nil.
func Wrapf(ns, format string, args ...any) error {
	return rendered(ns, fmt.Errorf(format, args...))
}

// Join is errors.Join with each line of the text naming ns at most once:
// it drops the nil errors, keeps the others in order, and returns nil when none is left;
// its text is errors.Join's, with Dedup applied.
func Join(ns string, errs ...error) error {
	err := errors.Join(errs...)
	if err == nil {
		return nil
	}

	return rendered(ns, err)
}

// Dedup returns s with every occurrence of ns after the first on each line removed,
// lines being separated by '\n'; nothing else changes.
func Dedup(ns, s string) string {
	if strings.Count(s, ns) < 2 {
		return s
	}
	lines := strings.Split(s, "\n")
	for i, line := range lines {
		first := strings.Index(line, ns)
		if first < 0 {
			continue
		}
		rest := first + len(ns)
		lines[i] = line[:rest] + strings.ReplaceAll(line[rest:], ns, "")
	}

	return strings.Join(lines, "\n")
}

// rendered returns an error that unwraps as err does, one cause or several,
// and whose text is err's with Dedup applied.
// An err that wraps nothing gives an error that wraps nothing.
func rendered(ns string, err error) error {
	text := Dedup(ns, err.Error())
	switch u := err.(type) { //nolint:errorlint // takes over this link of the chain, not the chain
	case interface{ Unwrap() []error }:
		return &wrapMultiError{text: text, errs: u.Unwrap()}
	case interface{ Unwrap() error }:
		return &wrapError{text: text, err: u.Unwrap()}
	default:
		return errors.New(text)
	}
}

// Error returns the rendered text.
func (e *wrapError) Error() string { return e.text }

// Unwrap returns the cause fmt.Errorf wrapped.
func (e *wrapError) Unwrap() error { return e.err }

// Error returns the rendered text.
func (e *wrapMultiError) Error() string { return e.text }

// Unwrap returns the causes fmt.Errorf or errors.Join wrapped, in their order.
func (e *wrapMultiError) Unwrap() []error { return e.errs }
