package sml

import (
	"errors"
	"fmt"
	"math"
	"strconv"
	"strings"
	"unicode"

	"github.com/arloliu/go-secs/v2/hsms"
	"github.com/arloliu/go-secs/v2/secs2"
)

const eof rune = -1

// Parser is a parser for HSMS data messages in SML (SECS Message Language) format.
// It provides methods for parsing SML strings to HSMS data messages.
// A Parser's configuration is immutable after construction; create a new Parser
// via NewParser to change options.
type Parser struct {
	pos      int
	len      int
	input    string
	data     string
	stream   uint8
	function uint8
	wbit     bool
	strict   bool // immutable after NewParser
}

// NewParser returns a Parser configured by opts (default: non-strict).
func NewParser(opts ...ParserOption) *Parser {
	p := &Parser{}
	for _, opt := range opts {
		opt(p)
	}

	return p
}

// Parse parses one or more SML messages from input, fail-fast (first error wins).
// Returns all successfully parsed messages, or nil and an error on the first failure.
// An empty or whitespace-only input is not an error: it returns an empty slice and a nil error.
func Parse(input string) ([]*hsms.DataMessage, error) {
	return NewParser().Parse(input)
}

// ParseStrict is Parse with strict ASCII parsing enabled.
func ParseStrict(input string) ([]*hsms.DataMessage, error) {
	return NewParser(WithParserStrictMode(true)).Parse(input)
}

// Parse parses the input SML string and returns a slice of parsed HSMS data messages
// and an error if any occurred during parsing.
//
// This method is similar to ParseMessage, but it parses multiple HSMS data messages from the input string.
//
// The parser will attempt to extract and validate individual HSMS data messages from the input string.
// If any errors are encountered during parsing, an error will be returned, and no messages will be
// returned to ensure data integrity.
func (p *Parser) Parse(input string) ([]*hsms.DataMessage, error) {
	p.initInput(input)

	messages := make([]*hsms.DataMessage, 0, 1)

	for {
		msg, err := p.parseMsg(false)
		if err != nil {
			return nil, err
		}

		// no more messages
		if msg == nil {
			break
		}

		messages = append(messages, msg)
	}

	return messages, nil
}

// ParseMessage parses a single HSMS data message from the input string and returns the parsed
// message and an error if any occurred during parsing.
//
// If the input is empty or contains only whitespace and comments (no message), ParseMessage
// returns nil and [ErrNoMessage]. Syntax errors are returned as *[ParseError].
func (p *Parser) ParseMessage(input string) (*hsms.DataMessage, error) {
	p.initInput(input)

	msg, err := p.parseMsg(false)
	if err != nil {
		return nil, err
	}

	if msg == nil {
		return nil, ErrNoMessage
	}

	return msg, nil
}

// ParseHeader parses the header of a single HSMS data message from the input string and returns
// the parsed message with header information but without the message body.
//
// If the input is empty or contains only whitespace and comments (no message), ParseHeader
// returns nil and [ErrNoMessage]. Syntax errors are returned as *[ParseError].
func (p *Parser) ParseHeader(input string) (*hsms.DataMessage, error) {
	p.initInput(input)

	msg, err := p.parseMsg(true)
	if err != nil {
		return nil, err
	}

	if msg == nil {
		return nil, ErrNoMessage
	}

	return msg, nil
}

func (p *Parser) initInput(input string) {
	p.input = input
	p.data = input
	p.len = len(input)
	p.pos = 0
}

// errf builds a *ParseError at the parser's current offset.
func (p *Parser) errf(format string, args ...any) *ParseError {
	return p.errfAt(p.pos, format, args...)
}

// errfAt builds a *ParseError at a specific byte offset. Value parsers use this
// because getItemValueStrings advances p.pos PAST the item before a value error
// fires, so p.pos would otherwise point after (not at) the offending item.
func (p *Parser) errfAt(offset int, format string, args ...any) *ParseError {
	return newParseError(p.input, offset, fmt.Sprintf(format, args...))
}

func (p *Parser) parseMsg(headerOnly bool) (*hsms.DataMessage, error) {
	p.stream = 0
	p.function = 0
	p.wbit = false

	p.skipComment()

	if p.peekNonSpaceRune() == eof {
		return nil, nil //nolint:nilnil
	}

	// parse header
	err := p.parseHSMSHeader()
	if err != nil {
		return nil, err
	}

	if headerOnly {
		msg, err := hsms.NewDataMessage(p.stream, p.function, p.wbit, 0, [4]byte{}, secs2.NewEmptyItem())
		if err != nil {
			return nil, fmt.Errorf("sml: %w", err)
		}

		return msg, nil
	}

	// parse text
	item, err := p.parseText()
	if err != nil {
		return nil, err
	}

	if ch := p.nextNonSpaceRune(); ch != '.' {
		return nil, p.errf("expect dot in the end of message, got %c", ch)
	}

	msg, err := hsms.NewDataMessage(p.stream, p.function, p.wbit, 0, [4]byte{}, item)
	if err != nil {
		return nil, fmt.Errorf("sml: %w", err)
	}

	return msg, nil
}

func (p *Parser) parseHSMSHeader() error {
	// find if the first dot or newline character is present in the data.
	//
	// ths header SML message should be terminated by a dot or newline character.
	//
	// there are several cases:
	// 1. the first dot is present, it means the message is terminated by a dot.
	// 2. the first newline is present, it means the header message is terminated by a newline.
	// 3. the output does not contain any dot symbol, it means the firstTerm is not accurate,
	firstTerm := strings.IndexAny(p.data, "\n.")
	if firstTerm < 0 {
		return p.errf("invalid SML message without dot symbol or newline")
	}

	// try to find if the first item bracket '<' is present in the data, and choose the maximum index
	// of the firstTerm and firstItemBracket.
	//
	// if firstTerm is not found or < firstItemBracket, we needs to use the firstItemBracket index
	// as the end of search range for searching the message name.
	//
	// if firstItemBracket is not found or < firstTerm, we needs to use the firstTerm index
	// as the end of search range for searching the message name.
	firstItemBracket := strings.IndexByte(p.data, byte('<'))
	i := max(firstTerm, firstItemBracket)
	if i < 0 {
		return p.errf("invalid SML message without item bracket '<' and dot symbol")
	}

	// get optional message name
	midx := strings.IndexByte(p.data[:i], byte(':'))

	// if colon found, it means there is a message name regardless it is empty or not,
	// so we can skip the message name part by forwarding the index to the next character after the colon.
	if midx >= 0 {
		// ignore message name
		p.forward(midx + 1)
	}

	// skip single or double quote
	ch := p.peekNonSpaceRune()
	if ch == '\'' || ch == '"' {
		p.forward(1)
	}

	// parse stream code for stream-function
	if p.nextRune() != 'S' {
		return p.errf("failed to parse stream code")
	}

	streamVal, err := p.nextCode()
	if err != nil {
		return err
	}

	if streamVal > 127 {
		return p.errf("stream code range overflow, should be in range of [0, 128)")
	}

	p.stream = streamVal

	// parse function code
	if p.nextRune() != 'F' {
		return p.errf("failed to parse function code")
	}

	funcVal, err := p.nextCode()
	if err != nil {
		return err
	}

	p.function = funcVal

	// skip single or double quote for stream-function
	ch = p.peekNonSpaceRune()
	if ch == '\'' || ch == '"' {
		p.forward(1)
	}

	// find optional wbit
	if p.peekNonSpaceRune() == 'W' {
		p.wbit = true
		p.forward(1)
	}

	return nil
}

func (p *Parser) parseText() (secs2.Item, error) {
	p.skipComment()

	ch := p.peekNonSpaceRune()
	if ch == '.' {
		return secs2.NewEmptyItem(), nil
	}

	item, err := p.parseItem()
	if err != nil {
		return nil, err
	}

	if item == nil {
		return secs2.NewEmptyItem(), nil
	}

	return item, nil
}

//nolint:cyclop
func (p *Parser) parseItem() (secs2.Item, error) {
	ch := p.nextNonSpaceRune()
	if ch != '<' {
		return nil, p.errf("expected '<'")
	}

	itemType, ok := p.parseItemType()
	if !ok {
		return nil, p.errf("failed to parse item type")
	}
	_, maxSize, err := p.parseItemSize()
	if err != nil {
		return nil, err
	}

	p.skipComment()

	var item secs2.Item
	// parse data item body
	switch itemType {
	case secs2.ListFormatCode:
		item, err = p.parseList(maxSize)
	case secs2.ASCIIFormatCode:
		if p.strict {
			item, err = p.parseASCIIStrict(maxSize)
		} else {
			item, err = p.parseASCIIFast(maxSize)
		}
	case secs2.JIS8FormatCode:
		item, err = p.parseJIS8()
	case secs2.LocalizedStrFormatCode:
		item, err = p.parseLocalizedStr()
	case secs2.BooleanFormatCode:
		item, err = p.parseBoolean(maxSize)
	case secs2.BinaryFormatCode:
		item, err = p.parseBinary(maxSize)
	case secs2.Float32FormatCode:
		item, err = p.parseFloat(4, maxSize)
	case secs2.Float64FormatCode:
		item, err = p.parseFloat(8, maxSize)
	case secs2.Int8FormatCode:
		item, err = p.parseInt(1, maxSize)
	case secs2.Int16FormatCode:
		item, err = p.parseInt(2, maxSize)
	case secs2.Int32FormatCode:
		item, err = p.parseInt(4, maxSize)
	case secs2.Int64FormatCode:
		item, err = p.parseInt(8, maxSize)
	case secs2.Uint8FormatCode:
		item, err = p.parseUint(1, maxSize)
	case secs2.Uint16FormatCode:
		item, err = p.parseUint(2, maxSize)
	case secs2.Uint32FormatCode:
		item, err = p.parseUint(4, maxSize)
	case secs2.Uint64FormatCode:
		item, err = p.parseUint(8, maxSize)
	default:
	}

	if err != nil {
		return nil, err
	}

	p.skipComment()

	return item, nil
}

func (p *Parser) parseList(size int) (secs2.Item, error) {
	childItems := make([]secs2.Item, 0, size)

	for {
		switch ch := p.peekNonSpaceRune(); ch {
		case '<':
			item, err := p.parseItem()
			if err != nil {
				return nil, err
			}
			childItems = append(childItems, item)

		case '>':
			p.forward(1)
			item := secs2.NewListItem(childItems...)
			childItems = nil //nolint:ineffassign,wastedassign

			return item, nil

		case eof:
			return nil, p.errf("should not got eof")

		default:
			return nil, p.errf("expected child data item or '<', '>', found %q", ch)
		}
	}
}

// parseASCIIStrict parses an ASCII data item from the input string in strict mode.
//
// In strict mode, the parser adheres to the ASCII printable characters (character codes 32 to 126) and
// supports parsing non-printable ASCII characters represented by their decimal values
// (e.g., 0x0A for newline).
//
// This method is typically used when parsing SML generated with strict mode for SECS-II ASCII items.
//
// It returns the parsed ASCII item as a secs2.Item and an error if any occurred during parsing.
//
//nolint:cyclop
func (p *Parser) parseASCIIStrict(size int) (secs2.Item, error) {
	var numStr string

	// Determine quoteChar from input: scan for the first ' or " before the closing >.
	// Default to '"' when the item contains only numeric tokens (no quoted run).
	quoteChar := '"'
	for _, ch := range p.data {
		if ch == '>' {
			break
		}
		if ch == '\'' || ch == '"' {
			quoteChar = ch
			break
		}
	}

	isQuoteStr := false
	isNumStr := false
	isEscapedCh := false
	var sb strings.Builder
	sb.Grow(size)

	for i, ch := range p.data {
		switch {
		// is a quoted string
		case isQuoteStr:
			switch ch {
			case '\\':
				if !isEscapedCh { // escaped char starts
					isEscapedCh = true
				} else { // write `\`
					sb.WriteRune(ch)
					isEscapedCh = false
				}

			// quote char found in quoted string
			case quoteChar:
				if isEscapedCh { // write quote char
					sb.WriteRune(ch)
					isEscapedCh = false
				} else { // quoted string end
					isQuoteStr = false
				}

			// found >
			case '>':
				if !isEscapedCh {
					return nil, p.errf("unclosed quote string, no closing quote found")
				}
				sb.WriteRune(ch)
				isEscapedCh = false

			default:
				sb.WriteRune(ch)
				isEscapedCh = false
			}

		// is a number string
		case isNumStr:
			switch ch {
			case ' ':
				isNumStr = false
				val, err := strconv.ParseUint(numStr, 0, 0)
				if err != nil {
					return nil, p.errf("invalid ASCII numeric byte: %v", err)
				}
				if val > unicode.MaxLatin1 {
					return nil, p.errf("non-printable char out of latin-1 range, got %d", val)
				}
				sb.WriteByte(byte(val))
				numStr = ""
			case '>':
				// trailing numeric token: append the byte, forward past '>', and return.
				val, err := strconv.ParseUint(numStr, 0, 0)
				if err != nil {
					return nil, p.errf("invalid ASCII numeric byte: %v", err)
				}
				if val > unicode.MaxLatin1 {
					return nil, p.errf("non-printable char out of latin-1 range, got %d", val)
				}
				sb.WriteByte(byte(val))
				p.forward(i + 1)

				return secs2.NewASCIIItem(sb.String()), nil
			default:
				numStr += string(ch)
			}

		// not quoted string and number string
		default:
			switch ch {
			case quoteChar:
				isQuoteStr = true
			case ' ':
				// skip
			case '>':
				p.forward(i + 1)
				return secs2.NewASCIIItem(sb.String()), nil
			default:
				if !isNumStr {
					numStr = string(ch)
					isNumStr = true
				} else {
					sb.WriteRune(ch)
				}
			}
		}
	}

	return nil, p.errf("invalid ASCII item, got EOF before item end")
}

// parseASCIIFast parses an ASCII data item from the input string in fast mode.
//
// In fast mode, the parser optimizes for performance by making certain assumptions about the input:
//   - It only detects the first quote + right angle bracket ("'>") pattern to identify the end of the ASCII item.
//   - It does not handle escape sequences.
//
// Note: The detection of the same quote character in fast mode is not exhaustive. There might be cases where
// the fast mode fails to identify these characters correctly, leading to inaccurate parsing. In such scenarios,
// it's recommended to use strict mode (WithParserStrictMode(true)) for more reliable parsing.
//
// It returns the parsed ASCII item as a secs2.Item and an error if any occurred during parsing.
func (p *Parser) parseASCIIFast(maxSize int) (secs2.Item, error) {
	// consume first quote
	ch := p.nextNonSpaceRune()

	if ch == '>' { // empty ASCII
		return secs2.NewASCIIItem(""), nil
	}

	// check if the first quote is valid
	if ch != '\'' && ch != '"' {
		return nil, p.errf("invalid quote for ASCII string")
	}

	quoteCh := byte(ch)

	// use size hint to parse ASCII item.
	// this is the optimized method when the size hint is provided.
	if maxSize > 0 {
		// the data length should be >= maxSize + 2 (quote + right angle bracket)
		if len(p.data) < maxSize+2 {
			return nil, p.errf("ASCII item size overflow, expect (%d+2), got %d", maxSize, len(p.data))
		}

		// check if the pattern is "'>" at the end, if so, return the ASCII item without further parsing.
		if ok, nidx := p.checkASCIICloseQuote(maxSize, quoteCh); ok {
			data := p.data[:maxSize]
			p.forward(nidx)
			return secs2.NewASCIIItem(data), nil
		}
	}

	// parse ASCII item byte-by-byte until the end of the item.
	// this is the fallback method when the size hint is not provided or the pattern "'>" is not found at the end.
	for i := 0; i < len(p.data); i++ {
		// check if the pattern is "'>" at the end and ensure the access index is within the data length.
		if ok, nidx := p.checkASCIICloseQuote(i, quoteCh); ok {
			data := p.data[:i]
			p.forward(nidx)
			return secs2.NewASCIIItem(data), nil
		}
	}

	return nil, p.errf("unclosed quote string for ASCII item")
}

func (p *Parser) checkASCIICloseQuote(idx int, quoteCh byte) (bool, int) {
	if idx+1 >= p.len || idx >= p.len || p.data[idx] != quoteCh {
		return false, 0
	}

	// skip space characters
	for nidx := idx + 1; nidx < p.len; nidx++ {
		switch p.data[nidx] {
		case ' ', '\t', '\r', '\n':
			continue
		case '>':
			return true, nidx + 1
		default:
			return false, 0
		}
	}

	return false, 0
}

// parseJIS8 parses a JIS-8 data item from the input string.
//
// It returns the parsed JIS-8 item as a secs2.Item and an error if any occurred during parsing.
func (p *Parser) parseJIS8() (secs2.Item, error) {
	// consume first quote
	ch := p.nextNonSpaceRune()

	if ch == '>' { // empty string
		return secs2.NewJIS8Item(""), nil
	}

	if ch != '\'' && ch != '"' {
		return nil, p.errf("invalid quote for JIS-8 string")
	}

	quoteCh := ch
	lastQuotePos := 0

	for i, ch := range p.data {
		switch ch {
		case quoteCh:
			lastQuotePos = i

		case '>':
			// check if the pattern is "'>"
			if lastQuotePos < i-1 {
				continue
			}

			data := p.data[:lastQuotePos]
			p.forward(i + 1)

			return secs2.NewJIS8Item(data), nil

		default:
			// collect all characters between the quotes; no utf8 validity check
		}
	}

	return nil, p.errf("unclosed quote string for JIS-8 item")
}

// parseLocalizedStr parses a Localized Character String data item from the input string.
//
// It returns the parsed LocalizedStr item as a secs2.Item and an error if any occurred during parsing.
func (p *Parser) parseLocalizedStr() (secs2.Item, error) {
	// consume first quote
	ch := p.nextNonSpaceRune()

	if ch == '>' { // empty string
		return secs2.NewUTF8StrItem(""), nil
	}

	if ch != '\'' && ch != '"' {
		return nil, p.errf("invalid quote for Localized string")
	}

	quoteCh := ch
	lastQuotePos := 0

	for i, ch := range p.data {
		switch ch {
		case quoteCh:
			lastQuotePos = i

		case '>':
			// check if the pattern is "'>"
			if lastQuotePos < i-1 {
				continue
			}

			data := p.data[:lastQuotePos]
			p.forward(i + 1)

			return secs2.NewUTF8StrItem(data), nil
		default:
		}
	}

	return nil, p.errf("unclosed quote string for Localized string item")
}

func (p *Parser) parseBoolean(size int) (secs2.Item, error) {
	items := make([]bool, 0, size)
	start := p.pos
	values := p.getItemValueStrings()

	for _, val := range values {
		switch strings.ToUpper(val) {
		case "TRUE", "T":
			items = append(items, true)
		case "FALSE", "F":
			items = append(items, false)
		default:
			return nil, p.errfAt(start, "expect boolean, found %s", val)
		}
	}

	return secs2.NewBooleanItem(items), nil
}

func (p *Parser) parseBinary(size int) (secs2.Item, error) {
	items := make([]byte, 0, size)
	start := p.pos
	values := p.getItemValueStrings()

	for _, val := range values {
		item, err := strconv.ParseInt(val, 0, 0)
		if err != nil {
			return nil, p.errfAt(start, "expect binary value, found %s", val)
		}

		if item < 0 || item >= 256 {
			return nil, p.errfAt(start, "binary value overflow, should be in range of [0, 256)")
		}

		items = append(items, byte(item))
	}

	return secs2.NewBinaryItem(items), nil
}

func (p *Parser) parseFloat(byteSize int, size int) (secs2.Item, error) {
	items := make([]float64, 0, size)
	start := p.pos
	values := p.getItemValueStrings()

	for _, val := range values {
		item, err := strconv.ParseFloat(val, byteSize*8)
		if err != nil {
			if errors.Is(err, strconv.ErrRange) {
				return nil, p.errfAt(start, "f%d overflow", byteSize)
			}

			return nil, p.errfAt(start, "expect float, found %s", val)
		}

		items = append(items, item)
	}

	return secs2.NewFloatItem(byteSize, items), nil
}

func (p *Parser) parseInt(byteSize int, size int) (secs2.Item, error) {
	items := make([]int64, 0, size)
	start := p.pos
	values := p.getItemValueStrings()

	for _, val := range values {
		item, err := strconv.ParseInt(val, 0, byteSize*8)
		if err != nil {
			if errors.Is(err, strconv.ErrRange) {
				return nil, p.errfAt(start, "i%d range overflow", byteSize)
			}

			return nil, p.errfAt(start, "expect signed integer, found %s", val)
		}

		items = append(items, item)
	}

	return secs2.NewIntItem(byteSize, items), nil
}

func (p *Parser) parseUint(byteSize int, size int) (secs2.Item, error) {
	items := make([]uint64, 0, size)
	start := p.pos
	values := p.getItemValueStrings()

	for _, val := range values {
		item, err := strconv.ParseUint(val, 0, byteSize*8)
		if err != nil {
			if errors.Is(err, strconv.ErrRange) {
				return nil, p.errfAt(start, "u%d range overflow", byteSize)
			}

			return nil, p.errfAt(start, "expect unsigned integer, found %s", val)
		}

		items = append(items, item)
	}

	return secs2.NewUintItem(byteSize, items), nil
}

func (p *Parser) getItemValueStrings() []string {
	rabIdx := strings.IndexByte(p.data, '>')
	if rabIdx == -1 {
		return []string{""}
	}

	items := strings.Fields(p.data[:rabIdx])
	p.forward(rabIdx + 1)

	return items
}

func (p *Parser) parseItemSize() (minSize, maxSize int, err error) {
	if p.nextNonSpaceRune() != '[' {
		p.backward(1)
		return 0, 0, nil
	}

	if p.peekNonSpaceRune() == '.' { // no minSize, only maxSize
		minSize = 0
		p.forward(2)
		maxSize, err = p.nextItemSize()
		if err != nil {
			return 0, 0, err
		}
	} else { // has minSize
		minSize, err = p.nextItemSize()
		if err != nil {
			return 0, 0, err
		}

		if p.peekNonSpaceRune() == '.' { // might has maxSize
			p.forward(2) // skip ".."
			if p.peekRune() == ']' {
				maxSize = minSize
			} else {
				maxSize, err = p.nextItemSize()
				if err != nil {
					return 0, 0, err
				}
			}
		} else { // no maxSize
			maxSize = minSize
		}
	}

	if p.nextNonSpaceRune() != ']' {
		return 0, 0, p.errf("invalid item size")
	}

	if minSize > maxSize {
		return minSize, maxSize, p.errf("minSize:%d > maxSize:%d", minSize, maxSize)
	}

	return minSize, maxSize, nil
}

//nolint:cyclop
func (p *Parser) parseItemType() (secs2.FormatCode, bool) {
	p.skipSpace()
	if len(p.data) < 1 {
		return 0, false
	}

	firstChar := toUpperRune(p.peekRune())

	var secondChar rune
	var hasSecondChar bool
	if len(p.data) >= 2 {
		secondChar = toUpperRune(rune(p.data[1]))
		hasSecondChar = true
	}

	switch firstChar {
	case 'L':
		p.forward(1)
		return secs2.ListFormatCode, true

	case 'A':
		p.forward(1)
		return secs2.ASCIIFormatCode, true

	case 'J':
		p.forward(1)
		return secs2.JIS8FormatCode, true

	case 'W':
		p.forward(1)
		return secs2.LocalizedStrFormatCode, true

	case 'B':
		if hasSecondChar {
			switch secondChar {
			case 'O':
				if len(p.data) >= 7 && strings.ToUpper(p.data[:7]) == "BOOLEAN" {
					p.forward(7)
					return secs2.BooleanFormatCode, true
				}
			case ' ', '[':
				p.forward(1)
				return secs2.BinaryFormatCode, true
			default:
				return 0, false
			}
		}
		p.forward(1)

		return secs2.BinaryFormatCode, true

	case 'F':
		if !hasSecondChar {
			return 0, false
		}

		switch secondChar {
		case '4':
			p.forward(2)
			return secs2.Float32FormatCode, true
		case '8':
			p.forward(2)
			return secs2.Float64FormatCode, true
		default:
			return 0, false
		}
	case 'I', 'U':
		if !hasSecondChar {
			return 0, false
		}

		formatCode, ok := getIntFormatCode(firstChar, secondChar)
		if !ok {
			return 0, false
		}
		p.forward(2)

		return formatCode, true
	default:
		return 0, false
	}
}

func getIntFormatCode(signed rune, byteSize rune) (secs2.FormatCode, bool) {
	switch signed {
	case 'I':
		switch byteSize {
		case '1':
			return secs2.Int8FormatCode, true
		case '2':
			return secs2.Int16FormatCode, true
		case '4':
			return secs2.Int32FormatCode, true
		case '8':
			return secs2.Int64FormatCode, true
		default:
			return 0, false
		}
	case 'U':
		switch byteSize {
		case '1':
			return secs2.Uint8FormatCode, true
		case '2':
			return secs2.Uint16FormatCode, true
		case '4':
			return secs2.Uint32FormatCode, true
		case '8':
			return secs2.Uint64FormatCode, true
		default:
			return 0, false
		}
	default:
		return 0, false
	}
}

func (p *Parser) forward(n int) bool {
	if p.pos+n <= p.len {
		p.pos += n
		p.data = p.input[p.pos:]
		return true
	}

	return false
}

func (p *Parser) backward(n int) {
	if p.pos-n >= 0 {
		p.pos -= n
		p.data = p.input[p.pos:]
	}
}

func (p *Parser) skipSpace() bool {
	for i := 0; i < len(p.data); i++ {
		switch p.data[i] {
		case ' ', '\t', '\r', '\n':
			continue
		default:
			return p.forward(i)
		}
	}

	return false
}

func (p *Parser) skipComment() {
	if !p.skipSpace() {
		return
	}
	if strings.HasPrefix(p.data, "//") {
		i := strings.IndexByte(p.data, '\n')
		if i < 0 {
			return
		}

		p.forward(i + 1)

		return
	} else if strings.HasPrefix(p.data, "/*") {
		i := strings.Index(p.data, "*/")
		if i < 0 {
			return
		}

		p.forward(i + 2)

		return
	}
}

func (p *Parser) peekRune() rune {
	if len(p.data) == 0 {
		return eof
	}

	return rune(p.data[0])
}

func (p *Parser) peekNonSpaceRune() rune {
	if !p.skipSpace() {
		return eof
	}

	return p.peekRune()
}

func (p *Parser) nextRune() rune {
	if p.pos >= p.len {
		return eof
	}

	r := rune(p.data[0])
	if !p.forward(1) {
		return eof
	}

	return r
}

func (p *Parser) nextNonSpaceRune() rune {
	if !p.skipSpace() {
		return eof
	}

	return p.nextRune()
}

func (p *Parser) nextCode() (uint8, error) {
	if p.pos >= p.len {
		return 0, p.errf("invalid sml code")
	}

	for i, ch := range p.data {
		if ch < '0' || ch > '9' {
			code, err := strconv.ParseUint(p.data[:i], 10, 8)
			if err != nil {
				return 0, p.errf("invalid sml code: %v", err)
			}
			p.forward(i)

			return uint8(code), nil
		}
	}

	return 0, p.errf("invalid sml code")
}

func (p *Parser) nextItemSize() (int, error) {
	if p.pos >= p.len {
		return 0, p.errf("invalid item size")
	}

	for i, ch := range p.data {
		if ch < '0' || ch > '9' {
			size, err := strconv.ParseUint(p.data[:i], 10, 32)
			if err != nil {
				return 0, p.errf("invalid item size: %v", err)
			}

			if size > math.MaxInt32 {
				return 0, p.errf("parsed size exceeds maximum int value")
			}

			p.forward(i)

			return int(size), nil
		}
	}

	return 0, p.errf("invalid item size")
}

func toUpperRune(ch rune) rune {
	hasLower := false
	hasLower = hasLower || ('a' <= ch && ch <= 'z')
	if !hasLower {
		return ch
	}

	ch -= 'a' - 'A'

	return ch
}
