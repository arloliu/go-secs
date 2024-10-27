package sml

import (
	"fmt"
	"regexp"
	"strings"
	"sync"
	"unicode"
	"unicode/utf8"

	"github.com/arloliu/go-secs/internal/queue"
)

var tokenPool = sync.Pool{New: func() interface{} { return new(token) }}

func getToken(typ tokenType, val string) *token {
	t, _ := tokenPool.Get().(*token)
	t.typ = typ
	t.val = val
	return t
}

func putToken(t *token) {
	tokenPool.Put(t)
}

// token represents a tokenized text string that a lexer identified.
type token struct {
	typ tokenType // token type
	val string    // tokenized text
}

type tokenType int

const (
	tokenTypeEOF     tokenType = iota // EOF
	tokenTypeError                    // lexing error
	tokenTypeComment                  // starting with '//', ending with newline or EOF
	tokenTypeMsgEnd                   // '.'

	// Message header
	tokenTypeMsgName    // Series of characters except whitespaces and comment delimiter
	tokenTypeStreamFunc // 'S' [0-9]+ 'F' [0-9]+, case insensitive
	tokenTypeWaitBit    // 'W', '[W]', case insensitive

	// Message text
	tokenTypeLeftAngleBracket  // '<'
	tokenTypeRightAngleBracket // '>'
	tokenTypeItemType          // 'L', 'B', 'BOOLEAN', 'A', 'F4', 'F8', 'I1', 'I2', 'I4', 'I8', 'U1', 'U2', 'U4', 'U8', case insensitive
	tokenTypeItemSize          // '[' [0-9]+ ('..' [0-9]+)? ']'
	tokenTypeNumber            // decimal, hexadecimal, octal, binary, floating-point number including scientific notation, case insensitive
	tokenTypeBool              // 'T', 'F', case insensitive
	tokenTypeQuotedString      // string enclosed with double quotes, e.g. "quoted string"
)

// lexer represents the state of the lexical scanner.
type lexer struct {
	input          string  // input string being lexed
	lastState      stateFn // last lexing state function
	state          stateFn // next lexing state function to enter
	pos            int     // current position in the input
	start          int     // start position of a token being lexed in input string
	width          int     // width of last rune read from input
	msgNameRegexp  *regexp.Regexp
	sfRegexp       *regexp.Regexp
	wbitRegexp     *regexp.Regexp
	itemTypeRegexp *regexp.Regexp
	arrayRegexp    *regexp.Regexp
	// tokens    chan token // the queue to report scanned tokens
	tokens queue.Queue
}

var lexerPool = sync.Pool{New: func() interface{} { return newLexer("") }}

func getLexer(input string) *lexer {
	l, _ := lexerPool.Get().(*lexer)
	l.input = input
	l.state = lexMessageHeader
	return l
}

func putLexer(l *lexer) {
	l.input = ""
	l.pos = 0
	l.start = 0
	l.width = 0
	l.lastState = nil
	l.tokens.Reset()
	lexerPool.Put(l)
}

// lex creates a new scanner for the input string.
func newLexer(input string) *lexer {
	return &lexer{
		input: input,
		state: lexMessageHeader,
		// tokens: queue.NewLockFreeQueue(),
		tokens:         queue.NewSliceQueue(2),
		msgNameRegexp:  regexp.MustCompile(`^([\w\-]+\s*):['"\s]*S`),
		sfRegexp:       regexp.MustCompile(`^['"]?[Ss]\d+[Ff]\d+['"]?`),
		wbitRegexp:     regexp.MustCompile(`^([Ww]|\[[Ww]\])`),
		itemTypeRegexp: regexp.MustCompile(`^[A-Za-z_]\w*`),
		arrayRegexp:    regexp.MustCompile(`^(\[\d+\])+`),
	}
}

// next returns the next rune in the input and move position.
func (l *lexer) next() (r rune) {
	if l.pos >= len(l.input) {
		l.width = 0
		return eof
	}

	r, l.width = utf8.DecodeRuneInString(l.input[l.pos:])
	l.pos += l.width
	return r
}

// ignore skips over the pending input before this point.
func (l *lexer) ignore() {
	l.start = l.pos
}

// skip skips n bytes.
func (l *lexer) skip(n int) {
	l.pos += n
	l.start = l.pos
}

// back steps back one rune.
func (l *lexer) back() {
	l.pos -= l.width
}

// peek returns the next rune in the input.
func (l *lexer) peek() rune {
	r := l.next()
	l.back()
	return r
}

// emit passes a token to the client.
func (l *lexer) emit(t tokenType) {
	l.tokens.Enqueue(getToken(t, l.input[l.start:l.pos]))
	l.start = l.pos
}

// emitUppercase passes a token with a uppercase token value to the client.
func (l *lexer) emitUppercase(t tokenType) {
	l.tokens.Enqueue(getToken(t, strings.ToUpper(l.input[l.start:l.pos])))
	l.start = l.pos
}

// emitSpaceRemoved passes a token to the client, with all spaces in token value removed.
func (l *lexer) emitSpaceRemoved(t tokenType) {
	val := make([]rune, 0, l.pos-l.start)
	for _, r := range l.input[l.start:l.pos] {
		if !unicode.IsSpace(r) {
			val = append(val, r)
		}
	}
	// l.tokens.Enqueue(&token{typ: t, val: string(val)})
	l.tokens.Enqueue(getToken(t, string(val)))
	l.start = l.pos
}

// emitEOF passes a EOF token to the client.
func (l *lexer) emitEOF() {
	// l.tokens.Enqueue(&token{typ: tokenTypeEOF, val: "EOF"})
	l.tokens.Enqueue(getToken(tokenTypeEOF, "EOF"))
	l.start = l.pos
}

// accept consumes the next rune if it's from the valid set.
func (l *lexer) accept(valid string) bool {
	if strings.ContainsRune(valid, l.next()) {
		return true
	}
	l.back()
	return false
}

// acceptRun consumes a run of runes from the valid set.
func (l *lexer) acceptRun(valid string) {
	for strings.ContainsRune(valid, l.next()) {
	}
	l.back()
}

// errorf returns an error token and terminates the running lexer.
func (l *lexer) errorf(format string, args ...interface{}) stateFn {
	// l.tokens.Enqueue(&token{tokenTypeError, fmt.Sprintf(format, args...)})
	l.tokens.Enqueue(getToken(tokenTypeError, fmt.Sprintf(format, args...)))
	return l.terminate()
}

// nextToken returns the next token from the input.
// If lexer.tokens channel is closed, it will return EOF token.
func (l *lexer) nextToken() *token {
	for {
		if l.tokens.IsEmpty() {
			l.lastState, l.state = l.state, l.state(l)
		} else {
			data := l.tokens.Dequeue()
			if data == nil {
				// return &token{typ: tokenTypeEOF}
				return getToken(tokenTypeEOF, "")
			}

			token, _ := data.(*token)
			return token
		}
	}
	// should not reach here
}

// stateFn represents the state of the lexer as a function that returns the next state
type stateFn func(*lexer) stateFn

// terminate closes the l.tokens channel and terminates the scan by
// passing back a nil pointer as the next state function.
func (l *lexer) terminate() stateFn {
	l.tokens.Enqueue(nil)
	return nil
}

// lexMessageHeader scans the elements that can appear in the message header.
func lexMessageHeader(l *lexer) stateFn {
	for {
		// Handle a line comment
		if strings.HasPrefix(l.input[l.pos:], "//") {
			return lexComment
		}

		// Handle optional message name
		re := l.msgNameRegexp
		if loc := re.FindStringSubmatchIndex(l.input[l.pos:]); loc != nil {
			l.pos += loc[3]
			l.emit(tokenTypeMsgName)
			l.pos += 1
			l.start = l.pos
			return lexMessageHeader
		}

		// Handle stream function code
		re = l.sfRegexp
		if loc := re.FindStringIndex(l.input[l.pos:]); loc != nil {
			l.pos += loc[1]
			l.emitUppercase(tokenTypeStreamFunc)
			return lexMessageHeader
		}

		// Handle wait bit
		re = l.wbitRegexp
		if loc := re.FindStringIndex(l.input[l.pos:]); loc != nil {
			l.pos += loc[1]
			l.emitUppercase(tokenTypeWaitBit)
			return lexMessageHeader
		}

		switch r := l.next(); r {
		case eof:
			return lexEOF
		case ' ', '\t', '\r', '\n':
			l.ignore()
		case '.':
			l.emit(tokenTypeMsgEnd)
			return lexMessageHeader
		case '<':
			l.emit(tokenTypeLeftAngleBracket)
			return lexMessageText
		default:
			for {
				r := l.next()
				if r == eof || unicode.IsSpace(r) || strings.HasPrefix(l.input[l.pos-1:], "//") {
					l.back()
					break
				}
			}
			l.emit(tokenTypeMsgName)
			return lexMessageHeader
		}
	}
	// should not reach here
}

// lexMessageText scans the elements inside the message text.
func lexMessageText(l *lexer) stateFn {
	for {
		// Handle a line comment
		if strings.HasPrefix(l.input[l.pos:], "//") {
			return lexComment
		}

		// Handle data types
		re := l.itemTypeRegexp
		if loc := re.FindStringIndex(l.input[l.pos:]); loc != nil {
			switch strings.ToUpper(l.input[l.pos : l.pos+loc[1]]) {
			case "L", "A", "B", "BOOLEAN", "F4", "F8",
				"I1", "I2", "I4", "I8", "U1", "U2", "U4", "U8":
				l.pos += loc[1]
				l.emitUppercase(tokenTypeItemType)
				return lexMessageText
			case "T", "F":
				l.pos += loc[1]
				l.emitUppercase(tokenTypeBool)
				return lexMessageText
			default:
				l.pos += loc[1]
				// Handle optional array-like notation
				re = l.arrayRegexp
				if loc = re.FindStringIndex(l.input[l.pos:]); loc != nil {
					fmt.Printf("Handle optional array-like notation: %s\n", l.input[l.pos:l.pos+loc[1]])
					l.pos += loc[1]
				}
				return lexMessageText
			}
		}

		r := l.next()
		// Handle number
		if r == '+' || r == '-' || isDigit(r) || (r == '.' && isDigit(l.peek())) {
			l.back()
			return lexNumber
		}

		switch r {
		case eof:
			return lexEOF
		case '<':
			l.emit(tokenTypeLeftAngleBracket)
			return lexMessageText
		case '>':
			l.emit(tokenTypeRightAngleBracket)
			return lexMessageText
		case '.':
			l.emit(tokenTypeMsgEnd)
			return lexMessageHeader
		case '[':
			l.back()
			return lexDataItemSize
		case '\'':
			l.back()
			return lexSingleQuotedString
		case '"':
			l.back()
			return lexDoubleQuotedString
		case ' ', '\t', '\r', '\n':
			l.ignore()
		default:
			return l.errorf("unexpected character in data item: %#U", r)
		}
	}
	// should not reach here
}

// lexEOF scans a EOF which is known to be present, and terminates the running lexer.
func lexEOF(l *lexer) stateFn {
	l.emitEOF()
	return l.terminate()
}

// lexComment scans a line comment.
// The line comment delimiter "//" is known to be present.
// Returns the previous state function which called lexComment.
func lexComment(l *lexer) stateFn {
	i := strings.Index(l.input[l.pos:], "\n")
	if i < 0 {
		l.pos = len(l.input)
		l.emit(tokenTypeComment)
		return lexEOF
	}

	for unicode.IsSpace(rune(l.input[l.pos+i-1])) {
		i -= 1
	}
	l.pos += i
	l.emit(tokenTypeComment)
	return l.lastState
}

// lexDataItemSize scans a data item's size, e.g. [2] or [2..7].
// The left square bracket is known to be present.
func lexDataItemSize(l *lexer) stateFn {
	numberFound := false
	l.accept("[")
	l.acceptRun(" \t\r\n")
	if l.accept("0123456789") {
		numberFound = true
		l.acceptRun("0123456789")
		l.acceptRun(" \t\r\n")
	}
	if strings.HasPrefix(l.input[l.pos:], "..") {
		l.pos += 2
		l.acceptRun(" \t\r\n")
		if l.accept("0123456789") {
			numberFound = true
			l.acceptRun("0123456789")
			l.acceptRun(" \t\r\n")
		}
	}
	if !(l.accept("]") && numberFound) {
		return l.errorf("invalid data item size")
	}
	l.emitSpaceRemoved(tokenTypeItemSize)
	return lexMessageText
}

// lexDoubleQuotedString scans a string inside double quotes.
// The left double quote is known to be present.
func lexDoubleQuotedString(l *lexer) stateFn {
	if !l.accept(`"`) {
		return l.errorf("invalid double quoted string")
	}

	i := strings.Index(l.input[l.pos:], `"`)
	j := strings.IndexAny(l.input[l.pos:], "\r\n")

	if i < 0 || (j > 0 && j < i) {
		return l.errorf("unclosed double quoted string")
	}
	// exclude the double quote
	l.start += 1
	l.pos += i
	l.emit(tokenTypeQuotedString)
	l.skip(1)

	return lexMessageText
}

// lexSingleQuotedString scans a string inside single quotes.
// The left single quote is known to be present.
func lexSingleQuotedString(l *lexer) stateFn {
	if !l.accept(`'`) {
		return l.errorf("invalid single quoted string")
	}

	i := strings.Index(l.input[l.pos:], `'`)
	j := strings.IndexAny(l.input[l.pos:], "\r\n")
	if i < 0 || (j > 0 && j < i) {
		return l.errorf("unclosed single quoted string")
	}
	// exclude the single quote
	l.start += 1
	l.pos += i
	l.emit(tokenTypeQuotedString)
	l.skip(1)

	return lexMessageText
}

// lexNumber scans a number, which is known to be present.
func lexNumber(l *lexer) stateFn {
	// Optional number sign
	l.accept("+-")

	// Handle decimal, hexadecimal, binary number
	digits := "0123456789" // default is decimal
	if l.accept("0") {
		if l.accept("xX") {
			digits = "0123456789abcdefABCDEF"
		} else if l.accept("bB") {
			digits = "01"
		} else if l.accept("oO") {
			digits = "01234567"
		}
	}
	l.acceptRun(digits)

	// Handle floating-point number
	if l.accept(".") {
		l.acceptRun(digits)
	}

	// Handle scientific notation
	if l.accept("eE") {
		l.accept("+-")
		l.acceptRun("0123456789")
	}

	// Next thing must not be alphanumeric
	if isAlphaNumeric(l.peek()) {
		l.next()
		return l.errorf("invalid number syntax: %q", l.input[l.start:l.pos])
	}

	l.emit(tokenTypeNumber)
	return lexMessageText
}

// Helper functions

// isAlphaNumeric reports whether r is an alphabetic, digit, or underscore.
func isAlphaNumeric(r rune) bool {
	return r == '_' || unicode.IsLetter(r) || unicode.IsDigit(r)
}

// isDigit reports whether r is a digit.
func isDigit(r rune) bool {
	return ('0' <= r && r <= '9')
}
