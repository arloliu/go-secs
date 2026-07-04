package sml

import (
	"errors"
	"fmt"
)

// ErrNoMessage is returned by [Parser.ParseMessage] and [Parser.ParseHeader] when the input is
// empty or contains only whitespace and comments. Test for it with errors.Is.
var ErrNoMessage = errors.New("sml: no message parsed")

// ParseError is a syntax error with the position in the input where it occurred.
// Only syntax errors during parsing are reported as *ParseError. Empty-input
// errors are returned as [ErrNoMessage]; construction or validation failures from
// hsms or secs2 are wrapped plain errors. Use errors.As to test for *ParseError
// and errors.Is for sentinel values such as [ErrNoMessage].
type ParseError struct {
	Offset int    // 0-based byte offset into the input
	Line   int    // 1-based
	Col    int    // 1-based
	Msg    string // human-readable description of the syntax error
}

// Error returns the formatted string representation of the ParseError.
func (e *ParseError) Error() string {
	return fmt.Sprintf("sml: line %d col %d (offset %d): %s", e.Line, e.Col, e.Offset, e.Msg)
}

// newParseError computes line/col from the byte offset within input.
func newParseError(input string, offset int, msg string) *ParseError {
	if offset > len(input) {
		offset = len(input)
	}
	line, col := 1, 1
	for i := 0; i < offset; i++ {
		if input[i] == '\n' {
			line++
			col = 1
		} else {
			col++
		}
	}

	return &ParseError{Offset: offset, Line: line, Col: col, Msg: msg}
}
