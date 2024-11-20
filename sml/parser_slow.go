package sml

import (
	"errors"
	"fmt"
	"strconv"
	"strings"
	"unicode"

	"github.com/arloliu/go-secs/hsms"
	"github.com/arloliu/go-secs/internal/queue"
	"github.com/arloliu/go-secs/secs2"
)

// ParseHSMSSlow parses the input string and returns a slice of parsed HSMS data messages
// and a slice of any errors encountered during parsing.
//
// The input string should be a valid UTF-8 encoded representation of one or more HSMS data messages.
//
// If any errors are encountered during parsing, no messages will be returned to ensure data integrity.
func ParseHSMSSlow(input string) (messages []*hsms.DataMessage, errs []error) {
	queueSize := len(input) / 4
	if queueSize < 10 {
		queueSize = 10
	}

	p := &parser{
		lexer:      getLexer(input),
		tokenQueue: queue.NewSliceQueue(queueSize),
		messages:   make([]*hsms.DataMessage, 0, 1),
		errors:     make([]error, 0),
	}
	defer putLexer(p.lexer)

	for p.peek().typ != tokenTypeEOF {
		if ok := p.parseHSMSDataMessage(); !ok {
			break
		}
	}

	if len(p.errors) > 0 {
		return []*hsms.DataMessage{}, p.errors
	}

	return p.messages, p.errors
}

type parser struct {
	lexer      *lexer              // lexer to tokenize the input string
	tokenQueue queue.Queue         // token queue that the lexer tokenized
	messages   []*hsms.DataMessage // parsed messages
	errors     []error             // parsing errors
}

// peek returns the next token.
func (p *parser) peek() *token {
	if p.tokenQueue.IsEmpty() {
		var t *token
		for {
			// ignore comment token
			if t = p.lexer.nextToken(); t.typ != tokenTypeComment {
				break
			}
		}
		p.tokenQueue.Enqueue(t)
	}

	token, ok := p.tokenQueue.Peek().(*token)
	if !ok {
		return nil
	}

	return token
}

// dequeueToken returns the next token and removes it from the token queue.
func (p *parser) dequeueToken() *token {
	data := p.tokenQueue.Dequeue()
	if data == nil {
		return nil
	}

	token, ok := data.(*token)
	if !ok {
		return nil
	}

	return token
}

// accept returns the next token and removes the token if the token type matches/
// The ok is true if token type matches.
func (p *parser) accept(typ tokenType) (t *token, ok bool) {
	t = p.peek()
	if t.typ != typ {
		return t, false
	}

	return p.dequeueToken(), true
}

// errorf create parse error and append it to parser.errors slice.
func (p *parser) errorf(format string, args ...any) {
	p.errors = append(p.errors, fmt.Errorf(format, args...))
}

// parseHSMSDataMessage parses an HSMS data message from the input byte stream.
//
// It attempts to extract and validate the various components of an HSMS data message,
// including the header, system bytes, and the SECS-II data item.
func (p *parser) parseHSMSDataMessage() (ok bool) {
	var (
		msgName  string
		stream   uint8
		function uint8
		waitBit  bool
		dataItem secs2.Item
	)

	// parse optional message name at front
	if t, ok := p.accept(tokenTypeMsgName); ok {
		msgName = strings.Trim(t.val, " ")
		putToken(t)
	}

	stream, function, ok = p.parseStreamFunctionCode()
	if !ok {
		return false
	}

	if t, ok := p.accept(tokenTypeWaitBit); ok {
		if t.val == "W" {
			waitBit = true
			if function%2 == 0 {
				p.errorf("wait bit cannot be true on reply message (function code is even)")
			}
		}
		putToken(t)
	}

	// parse optional message name after stream-function and optional waitbit
	if t, ok := p.accept(tokenTypeMsgName); ok {
		msgName = t.val
		putToken(t)
	}

	dataItem, ok = p.parseHSMSDataMessageText()
	if !ok {
		return false
	}

	t, ok := p.accept(tokenTypeMsgEnd)
	if !ok {
		p.errorf("expected message end character '.', found %q", t.val)
		putToken(t)
		return false
	}
	putToken(t)

	message, err := hsms.NewDataMessage(stream, function, waitBit, 0, nil, dataItem)
	if err != nil {
		p.errorf("error:%s", err.Error())
		return false
	}
	if msgName != "" {
		message.SetName(msgName)
	}

	p.messages = append(p.messages, message)

	return true
}

// parseStreamFunctionCode parses the stream function token.
func (p *parser) parseStreamFunctionCode() (stream uint8, function uint8, ok bool) {
	t, accepted := p.accept(tokenTypeStreamFunc)
	defer putToken(t)

	if !accepted {
		p.errorf("expected stream function, found %q", t.val)
		return 0, 0, false
	}

	s := 0
	f := 0
	e := len(t.val)
	for i, ch := range t.val {
		if ch == 'S' {
			s = i + 1
		} else if ch == 'F' {
			f = i
		} else if ch == '\'' || ch == '"' {
			if s > 0 {
				e = i
			}
		}
	}

	ok = true

	streamVal, _ := strconv.Atoi(t.val[s:f])
	functionVal, _ := strconv.Atoi(t.val[f+1 : e])
	if !(0 <= streamVal && streamVal < 128) {
		p.errorf("stream code range overflow, should be in range of [0, 128)")
		stream = 0
		ok = false
	} else {
		stream = uint8(streamVal)
	}

	if !(0 <= functionVal && functionVal < 256) {
		p.errorf("function code range overflow, should be in range of [0, 256)")
		function = 0
		ok = false
	} else {
		function = uint8(functionVal) //nolint:gosec
	}

	return stream, function, ok
}

// parseHSMSDataMessageText parses the message text.
func (p *parser) parseHSMSDataMessageText() (item secs2.Item, ok bool) {
	switch t := p.peek(); t.typ { //nolint:exhaustive
	case tokenTypeMsgEnd:
		return secs2.NewEmptyItem(), true

	case tokenTypeLeftAngleBracket:
		return p.parseItem()

	default:
		p.errorf("expected '<' or '.', found %q", t.val)
		return secs2.NewEmptyItem(), false
	}
	// should not reach here
}

// parseItem parses a data item.
func (p *parser) parseItem() (item secs2.Item, ok bool) { //nolint:cyclop
	t, ok := p.accept(tokenTypeLeftAngleBracket)
	if !ok {
		p.errorf("expected '<', found %q", t.val)
		putToken(t)
		return secs2.NewEmptyItem(), false
	}
	putToken(t)

	defer func() {
		if r := recover(); r != nil {
			p.errorf("%v", r)
			// override return value
			item, ok = secs2.NewEmptyItem(), false
		}
	}()

	var dataItemType string
	t, ok = p.accept(tokenTypeItemType)
	if !ok {
		p.errorf("invalid data item type: %q", t.val)
		putToken(t)
		return secs2.NewEmptyItem(), false
	}

	dataItemType = t.val

	putToken(t)

	sizeStart := 0
	sizeEnd := -1
	if t := p.peek(); t.typ == tokenTypeItemSize {
		sizeStart, sizeEnd = p.parseItemSize()
	} else if t.typ == tokenTypeError {
		p.errorf("syntax error: %s", t.val)
		return secs2.NewEmptyItem(), false
	}

	size := sizeEnd
	if size == -1 {
		size = 0
	}

	switch dataItemType {
	case "L":
		item, ok = p.parseList(size)
	case "A":
		item, ok = p.parseASCII()
	case "B":
		item, ok = p.parseBinary(size)
	case "BOOLEAN":
		item, ok = p.parseBoolean(size)
	case "F4":
		item, ok = p.parseFloat(4, size)
	case "F8":
		item, ok = p.parseFloat(8, size)
	case "I1":
		item, ok = p.parseInt(1, size)
	case "I2":
		item, ok = p.parseInt(2, size)
	case "I4":
		item, ok = p.parseInt(4, size)
	case "I8":
		item, ok = p.parseInt(8, size)
	case "U1":
		item, ok = p.parseUint(1, size)
	case "U2":
		item, ok = p.parseUint(2, size)
	case "U4":
		item, ok = p.parseUint(4, size)
	case "U8":
		item, ok = p.parseUint(8, size)
	}
	if !ok {
		return secs2.NewEmptyItem(), false
	}

	if item.Size() >= 0 {
		p.checkItemSizeError(item.Size(), sizeStart, sizeEnd)
	}

	t, ok = p.accept(tokenTypeRightAngleBracket)
	if !ok {
		p.errorf("expected '>', found %q", t.val)
		putToken(t)
		return secs2.NewEmptyItem(), false
	}
	putToken(t)

	return item, ok
}

// parseItemSize parses data item and returns lower and upper bound of data item size.
func (p *parser) parseItemSize() (sizeStart int, sizeEnd int) {
	sizeStart = 0
	sizeEnd = -1
	t, ok := p.accept(tokenTypeItemSize)
	if ok {
		// possible token value: [x], [x..], [..y], [x..y], where x and y are integers
		i := strings.Index(t.val, "..")
		if i == -1 {
			// '..' is not found
			sizeStart, _ = strconv.Atoi(t.val[1 : len(t.val)-1])
			sizeEnd = sizeStart
		} else {
			sizeStart, _ = strconv.Atoi(t.val[1:i]) // atoi return 0 when syntax error
			end, err := strconv.Atoi(t.val[i+2 : len(t.val)-1])
			if err == nil || errors.Is(err, strconv.ErrRange) {
				sizeEnd = end
			}
		}
	}
	putToken(t)

	return sizeStart, sizeEnd
}

func (p *parser) checkItemSizeError(size, lowerLimit, upperLimit int) {
	if upperLimit == -1 {
		if lowerLimit > size {
			p.errorf("data item size overflow, got size of %d", size)
		}
	} else if !(lowerLimit <= size && size <= upperLimit) {
		p.errorf("data item size overflow, got size of %d", size)
	}
}

func (p *parser) parseList(size int) (item secs2.Item, ok bool) {
	values := make([]secs2.Item, 0, size)

	for {
		switch t := p.peek(); t.typ { //nolint:exhaustive
		case tokenTypeLeftAngleBracket:
			childItem, ok := p.parseItem()
			if !ok {
				return secs2.NewEmptyItem(), false
			}
			values = append(values, childItem)

		case tokenTypeRightAngleBracket:
			return secs2.NewListItem(values...), true

		case tokenTypeError:
			p.errorf("syntax error: %s", t.val)
			return secs2.NewEmptyItem(), false

		default:
			p.errorf("expected child data item or '<', '>', found %q", t.val)
			return secs2.NewEmptyItem(), false
		}
	}
	// should not reach here
}

func (p *parser) getItemValueTokens(size int) []*token {
	tokens := make([]*token, 0, size)
	for {
		switch p.peek().typ { //nolint:exhaustive
		case tokenTypeNumber, tokenTypeBool, tokenTypeQuotedString:
			tokens = append(tokens, p.dequeueToken())
		case tokenTypeRightAngleBracket:
			return tokens
		default:
			tokens = append(tokens, p.dequeueToken())
			return tokens
		}
	}
	// should not reach here
}

func (p *parser) getItemValueStrings(size int) []string {
	rabIdx := strings.IndexRune(p.lexer.input[p.lexer.pos:], '>')
	if rabIdx == -1 {
		return []string{""}
	}

	tokens := make([]string, 0, size)
	for {
		token := p.dequeueToken()
		if token == nil {
			break
		}
		tokens = append(tokens, token.val)
	}

	tokens = append(tokens, strings.Fields(p.lexer.input[p.lexer.pos:p.lexer.pos+rabIdx])...)

	p.lexer.skip(rabIdx)

	return tokens
}

func (p *parser) parseASCII() (item secs2.Item, ok bool) {
	var literal string

	tokens := p.getItemValueTokens(0)
	defer func() {
		for _, t := range tokens {
			putToken(t)
		}
	}()

	for _, t := range tokens {
		switch t.typ { //nolint:exhaustive
		case tokenTypeQuotedString:
			// val, _ := unquoteString(t.val)
			val := t.val
			for _, r := range val {
				if r > unicode.MaxASCII {
					val = ""
					p.errorf("expected ASCII characters, found %q", r)
					break
				}
			}
			literal += val

		case tokenTypeNumber:
			val, err := strconv.ParseUint(t.val, 0, 8)
			if err != nil {
				if errors.Is(err, strconv.ErrSyntax) {
					p.errorf("expected ASCII number code, found %q", t.val)
				}
			}
			if val > unicode.MaxASCII {
				val = 0
				p.errorf("overflows ASCII range, found %q", t.val)
			}
			literal += string(byte(val))

		case tokenTypeError:
			p.errorf("syntax error: %s", t.val)
			return secs2.NewEmptyItem(), false

		default:
			p.errorf("expected quoted string, ASCII number code or variable, found %q", t.val)
			return secs2.NewEmptyItem(), false
		}
	}

	return secs2.NewASCIIItem(literal), true
}

// func unquoteString(str string) (string, bool) {
// 	if len(str) >= 2 && (str[0] == '\'' || str[0] == '"') && str[0] == str[len(str)-1] {
// 		return str[1 : len(str)-1], true
// 	}
// 	return str, false
// }

func (p *parser) parseBinary(size int) (item secs2.Item, ok bool) {
	values := make([]byte, 0, size)

	valStrs := p.getItemValueStrings(size)
	for _, str := range valStrs {
		if str == "" {
			p.errorf("syntax error")
			return secs2.NewEmptyItem(), false
		}

		val, err := strconv.ParseInt(str, 0, 0)
		if err != nil {
			p.errorf("expected binary byte, found %q", str)
			return secs2.NewEmptyItem(), false
		}

		if !(0 <= val && val < 256) {
			p.errorf("binary value overflow, should be in range of [0, 256)")
			return secs2.NewEmptyItem(), false
		}
		values = append(values, byte(val))
	}

	return secs2.NewBinaryItem(values), true
}

func (p *parser) parseBoolean(size int) (item secs2.Item, ok bool) {
	values := make([]bool, 0, size)

	valStrs := p.getItemValueStrings(size)
	for _, str := range valStrs {
		switch str {
		case "":
			p.errorf("syntax error")
			return secs2.NewEmptyItem(), false
		case "T":
			values = append(values, true)
		case "F":
			values = append(values, false)
		default:
			p.errorf("expect boolean, found %q", str)
			return secs2.NewEmptyItem(), false
		}
	}

	return secs2.NewBooleanItem(values), true
}

func (p *parser) parseFloat(byteSize int, size int) (item secs2.Item, ok bool) {
	values := make([]float64, 0, size)

	valStrs := p.getItemValueStrings(size)

	for _, str := range valStrs {
		if str == "" {
			p.errorf("syntax error")
			return secs2.NewEmptyItem(), false
		}

		val, err := strconv.ParseFloat(str, byteSize*8)
		if err != nil {
			if errors.Is(err, strconv.ErrRange) {
				p.errorf("F%d range overflow", byteSize)
			} else {
				p.errorf("expect float, found %q", str)
			}

			return secs2.NewEmptyItem(), false
		}

		values = append(values, val)
	}

	return secs2.NewFloatItem(byteSize, values), true
}

func (p *parser) parseInt(byteSize int, size int) (item secs2.Item, ok bool) {
	values := make([]int64, 0, size)

	valStrs := p.getItemValueStrings(size)

	for _, str := range valStrs {
		if str == "" {
			p.errorf("syntax error")
			return secs2.NewEmptyItem(), false
		}

		val, err := strconv.ParseInt(str, 0, byteSize*8)
		if err != nil {
			if errors.Is(err, strconv.ErrRange) {
				p.errorf("I%d range overflow", byteSize)
			} else {
				p.errorf("expected integer, found %q", str)
			}

			return secs2.NewEmptyItem(), false
		}

		values = append(values, val)
	}

	return secs2.NewIntItem(byteSize, values), true
}

func (p *parser) parseUint(byteSize int, size int) (item secs2.Item, ok bool) {
	values := make([]uint64, 0, size)

	valStrs := p.getItemValueStrings(size)

	for _, str := range valStrs {
		if str == "" {
			p.errorf("syntax error")
			return secs2.NewEmptyItem(), false
		}

		val, err := strconv.ParseUint(str, 0, byteSize*8)
		if err != nil {
			if errors.Is(err, strconv.ErrRange) {
				p.errorf("U%d range overflow", byteSize)
			} else {
				p.errorf("expected unsigned integer, found %q", str)
			}

			return secs2.NewEmptyItem(), false
		}

		values = append(values, val)
	}

	return secs2.NewUintItem(byteSize, values), true
}
