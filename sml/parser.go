package sml

import (
	"errors"
	"fmt"
	"math"
	"strconv"
	"strings"
	"sync/atomic"
	"unicode"
	"unicode/utf8"

	"github.com/arloliu/go-secs/hsms"
	"github.com/arloliu/go-secs/secs2"
)

const eof rune = -1

// HSMSParser is a parser for HSMS data messages in SML (SECS Message Language) format.
// It provides methods for parsing SML strings to HSMS data messages.
type HSMSParser struct {
	pos        int
	len        int
	input      string
	data       string
	stream     uint8
	function   uint8
	wbit       bool
	strictMode atomic.Bool
}

// NewHSMSParser creates a new SML HSMS parser.
func NewHSMSParser() *HSMSParser {
	return &HSMSParser{}
}

// ParseHSMS parses the input string using a new HSMSParser instanace with the default strict mode setting.
// It returns a slice of parsed HSMS data messages and an error if any occurred during parsing.
//
// The input string should be a valid UTF-8 encoded representation of one or more HSMS data messages.
//
// If any errors are encountered during parsing, no messages will be returned to ensure data integrity.
func ParseHSMS(input string) ([]*hsms.DataMessage, error) {
	if len(input) == 0 {
		return []*hsms.DataMessage{}, nil
	}

	p := NewHSMSParser()
	p.WithStrictMode(defStrictMode.Load())

	return p.Parse(input)
}

var defStrictMode atomic.Bool

// WithStrictMode configures the default setting to use strict mode for parsing ASCII characters.
// It affects when calling ParseHSMS function.
//
// In strict mode, the parser adheres to the ASCII printable characters (character codes 32 to 126) and
// supports parsing non-printable ASCII characters represented by their decimal values
// (e.g., 0x0A for newline).
//
// In non-strict mode, the parser optimizes for performance by making certain assumptions about the input:
//   - It assumes that the ASCII string does not contain the same quote character as the one used
//     to enclose the ASCII item.
//   - It does not handle escape sequences.
//
// The strict mode setting of SECS-II ASCII items can be configured by secs2.WithStrictMode.
func WithStrictMode(enable bool) {
	defStrictMode.Store(enable)
}

// WithStrictMode configures the parser to use strict mode for parsing ASCII characters.
//
// In strict mode, the parser adheres to the ASCII printable characters (character codes 32 to 126) and
// supports parsing non-printable ASCII characters represented by their decimal values
// (e.g., 0x0A for newline).
//
// In non-strict mode, the parser optimizes for performance by making certain assumptions about the input:
//   - It assumes that the ASCII string does not contain the same quote character as the one used
//     to enclose the ASCII item.
//   - It does not handle escape sequences.
//
// The strict mode setting of SECS-II ASCII items can be configured by secs2.WithStrictMode.
func (p *HSMSParser) WithStrictMode(enable bool) {
	p.strictMode.Store(enable)
	secs2.WithASCIIStrictMode(enable)
}

// Parse parses the input SML string and returns a slice of parsed HSMS data messages and an error
// if any occurred during parsing.
//
// This method is similar to ParseMessage, but it parses multiple HSMS data messages from the input string.
//
// The parser will attempt to extract and validate individual HSMS data messages from the input string.
// If any errors are encountered during parsing, an error will be returned, and no messages will be
// returned to ensure data integrity.
func (p *HSMSParser) Parse(input string) ([]*hsms.DataMessage, error) {
	p.initInput(input)

	messages := make([]*hsms.DataMessage, 0, 1)

	for {
		msg, err := p.parseMsg(false, false)
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

// ParseMessage parses a single HSMS data message from the input string, returns the parsed message
// and an error if any occurred during parsing.
//
// The parser will attempt to extract and validate individual HSMS data messages from the input string.
// If any errors are encountered during parsing, an error will be returned, and no messages will be
// returned to ensure data integrity.
//
// The lazy flag can be set to true to skip parsing the message body immediately. This is useful when
// the message body is not required for further processing.
func (p *HSMSParser) ParseMessage(input string, lazy bool) (*hsms.DataMessage, error) {
	p.initInput(input)

	msg, err := p.parseMsg(false, lazy)
	if err != nil {
		return nil, err
	}

	if msg == nil {
		return nil, errors.New("no message parsed")
	}

	return msg, nil
}

// ParseMessageHeader parses the header of a single HSMS data message from the input string and returns
// the parsed message with header information but without the message body.
func (p *HSMSParser) ParseMessageHeader(input string) (*hsms.DataMessage, error) {
	p.initInput(input)

	msg, err := p.parseMsg(true, true)
	if err != nil {
		return nil, err
	}

	if msg == nil {
		return nil, errors.New("no message parsed")
	}

	return msg, nil
}

func (p *HSMSParser) initInput(input string) {
	p.input = input
	p.data = input
	p.len = len(input)
	p.pos = 0
}

func (p *HSMSParser) parseMsg(headerOnly bool, lazy bool) (*hsms.DataMessage, error) {
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
		return hsms.NewDataMessage(p.stream, p.function, p.wbit, 0, nil, nil)
	}

	// skip body parsing if lazy flag is set
	if lazy {
		rawSMLItem := NewRawSMLItem([]byte(p.data), p.strictMode.Load())
		msg, err := hsms.NewDataMessage(p.stream, p.function, p.wbit, 0, nil, rawSMLItem)
		if err != nil {
			return nil, err
		}

		return msg, nil
	}

	// parse text
	item, err := p.parseText()
	if err != nil {
		return nil, err
	}

	if ch := p.nextNonSpaceRune(); ch != '.' {
		return nil, fmt.Errorf("expect dot in the end of message, got %c", ch)
	}

	msg, err := hsms.NewDataMessage(p.stream, p.function, p.wbit, 0, nil, item)
	if err != nil {
		return nil, err
	}

	return msg, nil
}

func (p *HSMSParser) parseHSMSHeader() error {
	i := strings.IndexAny(p.data, "\n.<")
	if i < 0 {
		return errors.New("invalid SML message without end symbol")
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
		return errors.New("failed to parse stream code")
	}

	streamVal, err := p.nextCode()
	if err != nil {
		return err
	}

	if streamVal > 127 {
		return errors.New("stream code range overflow, should be in range of [0, 128)")
	}

	p.stream = streamVal

	// parse function code
	if p.nextRune() != 'F' {
		return errors.New("failed to parse function code")
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

func (p *HSMSParser) parseText() (secs2.Item, error) {
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

func (p *HSMSParser) parseItem() (secs2.Item, error) {
	ch := p.nextNonSpaceRune()
	if ch != '<' {
		return nil, errors.New("expected '<'")
	}

	itemType, ok := p.parseItemType()
	if !ok {
		return nil, errors.New("failed to parse item type")
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
		if p.strictMode.Load() {
			item, err = p.parseASCIIStrict(maxSize)
		} else {
			item, err = p.parseASCIIFast(maxSize)
		}
	case secs2.JIS8FormatCode:
		item, err = p.parseJIS8()
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
	}

	if err != nil {
		return nil, err
	}

	p.skipComment()

	return item, nil
}

func (p *HSMSParser) parseList(size int) (secs2.Item, error) {
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
			return secs2.NewListItem(childItems...), nil

		case eof:
			return nil, errors.New("should not got eof")

		default:
			return nil, fmt.Errorf("expected child data item or '<', '>', found %q", ch)
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
func (p *HSMSParser) parseASCIIStrict(size int) (secs2.Item, error) {
	var numStr string
	quoteChar := secs2.ASCIIQuote()
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
					return nil, errors.New("unclosed quote string, no closing quote found")
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
			case ' ', '>':
				isNumStr = false
				val, err := strconv.ParseUint(numStr, 0, 0)
				if err != nil {
					return nil, err
				}
				if val > unicode.MaxLatin1 {
					return nil, fmt.Errorf("non-printable char out of latin-1 range, got %d", val)
				}
				sb.WriteByte(byte(val))
				numStr = ""
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

	return nil, errors.New("invalid ASCII item, got EOF before item end")
}

// parseASCIIFast parses an ASCII data item from the input string in fast mode.
//
// In fast mode, the parser optimizes for performance by making certain assumptions about the input:
//   - It only detects the first quote + right angle bracket ("'>") pattern to identify the end of the ASCII item.
//   - It does not handle escape sequences.
//
// Note: The detection of the same quote character in fast mode is not exhaustive. There might be cases where
// the fast mode fails to identify these characters correctly, leading to inaccurate parsing. In such scenarios,
// it's recommended to use strict mode (WithStrictMode(true)) for more reliable parsing.
//
// It returns the parsed ASCII item as a secs2.Item and an error if any occurred during parsing.
func (p *HSMSParser) parseASCIIFast(maxSize int) (secs2.Item, error) {
	// consume first quote
	ch := p.nextNonSpaceRune()

	if ch == '>' { // empty ASCII
		return secs2.NewASCIIItem(""), nil
	}

	// check if the first quote is valid
	if ch != '\'' && ch != '"' {
		return nil, errors.New("invalid quote for ASCII string")
	}

	quoteCh := byte(ch)

	// use size hint to parse ASCII item.
	// this is the optimized method when the size hint is provided.
	if maxSize > 0 {
		// the data length should be >= maxSize + 2 (quote + right angle bracket)
		if len(p.data) < maxSize+2 {
			return nil, fmt.Errorf("ASCII item size overflow, expect (%d+2), got %d", maxSize, len(p.data))
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

	return nil, errors.New("unclosed quote string for ASCII item")
}

func (p *HSMSParser) checkASCIICloseQuote(idx int, quoteCh byte) (bool, int) {
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

// paseJIS8 parses a JIS-8 data item from the input string.
//
// It returns the parsed JIS-8 item as a secs2.Item and an error if any occurred during parsing.
func (p *HSMSParser) parseJIS8() (secs2.Item, error) {
	// consume first quote
	ch := p.nextNonSpaceRune()

	if ch == '>' { // empty string
		return secs2.NewJIS8Item(""), nil
	}

	if ch != '\'' && ch != '"' {
		return nil, errors.New("invalid quote for JIS-8 string")
	}

	quoteCh := ch
	quoteCount := 0
	lastQuotePos := 0

	for i, ch := range p.data {
		switch ch {
		case quoteCh:
			quoteCount++
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
			if utf8.ValidRune(ch) {
				return nil, fmt.Errorf("out of utf-8 range, got %d", ch)
			}
		}
	}

	return nil, errors.New("unclosed quote string for JIS-8 item")
}

func (p *HSMSParser) parseBoolean(size int) (secs2.Item, error) {
	items := make([]bool, 0, size)
	values := p.getItemValueStrings()

	for _, val := range values {
		switch strings.ToUpper(val) {
		case "TRUE", "T":
			items = append(items, true)
		case "FALSE", "F":
			items = append(items, false)
		default:
			return nil, fmt.Errorf("expect boolean, found %s", val)
		}
	}

	return secs2.NewBooleanItem(items), nil
}

func (p *HSMSParser) parseBinary(size int) (secs2.Item, error) {
	items := make([]byte, 0, size)
	values := p.getItemValueStrings()

	for _, val := range values {
		item, err := strconv.ParseInt(val, 0, 0)
		if err != nil {
			return nil, fmt.Errorf("expect binary value, found %s", val)
		}

		if !(0 <= item && item < 256) {
			return nil, errors.New("binary value overflow, should be in range of [0, 256)")
		}

		items = append(items, byte(item))
	}

	return secs2.NewBinaryItem(items), nil
}

func (p *HSMSParser) parseFloat(byteSize int, size int) (secs2.Item, error) {
	items := make([]float64, 0, size)
	values := p.getItemValueStrings()

	for _, val := range values {
		item, err := strconv.ParseFloat(val, byteSize*8)
		if err != nil {
			if errors.Is(err, strconv.ErrRange) {
				return nil, fmt.Errorf("F%d overflow", byteSize)
			}

			return nil, fmt.Errorf("expect float, found %s", val)
		}

		items = append(items, item)
	}

	return secs2.NewFloatItem(byteSize, items), nil
}

func (p *HSMSParser) parseInt(byteSize int, size int) (secs2.Item, error) {
	items := make([]int64, 0, size)
	values := p.getItemValueStrings()

	for _, val := range values {
		item, err := strconv.ParseInt(val, 0, byteSize*8)
		if err != nil {
			if errors.Is(err, strconv.ErrRange) {
				return nil, fmt.Errorf("I%d range overflow", byteSize)
			}

			return nil, fmt.Errorf("expect signed integer, found %s", val)
		}

		items = append(items, item)
	}

	return secs2.NewIntItem(byteSize, items), nil
}

func (p *HSMSParser) parseUint(byteSize int, size int) (secs2.Item, error) {
	items := make([]uint64, 0, size)
	values := p.getItemValueStrings()

	for _, val := range values {
		item, err := strconv.ParseUint(val, 0, byteSize*8)
		if err != nil {
			if errors.Is(err, strconv.ErrRange) {
				return nil, fmt.Errorf("U%d range overflow", byteSize)
			}

			return nil, fmt.Errorf("expect unsigned integer, found %s", val)
		}

		items = append(items, item)
	}

	return secs2.NewUintItem(byteSize, items), nil
}

func (p *HSMSParser) getItemValueStrings() []string {
	rabIdx := strings.IndexByte(p.data, '>')
	if rabIdx == -1 {
		return []string{""}
	}

	items := strings.Fields(p.data[:rabIdx])
	p.forward(rabIdx + 1)

	return items
}

func (p *HSMSParser) parseItemSize() (minSize, maxSize int, err error) {
	if p.nextNonSpaceRune() != '[' {
		p.backward(1)
		return 0, 0, nil
	}

	if p.peekNonSpaceRune() == '.' { // no minSize, only maxSize
		minSize = 0
		p.forward(2)
		maxSize, err = p.nextItemSize()
		if err != nil {
			return 0, 0, fmt.Errorf("invalid maxSize: %w", err)
		}
	} else { // has minSize
		minSize, err = p.nextItemSize()
		if err != nil {
			return 0, 0, fmt.Errorf("invalid minSize: %w", err)
		}

		if p.peekNonSpaceRune() == '.' { // might has maxSize
			p.forward(2) // skip ".."
			if p.peekRune() == ']' {
				maxSize = minSize
			} else {
				maxSize, err = p.nextItemSize()
				if err != nil {
					return 0, 0, fmt.Errorf("invalid maxSize: %w", err)
				}
			}
		} else { // no maxSize
			maxSize = minSize
		}
	}

	if p.nextNonSpaceRune() != ']' {
		return 0, 0, errors.New("invalid item size")
	}

	if minSize > maxSize {
		return minSize, maxSize, fmt.Errorf("minSize:%d > maxSize:%d", minSize, maxSize)
	}

	return minSize, maxSize, nil
}

func (p *HSMSParser) parseItemType() (secs2.FormatCode, bool) {
	p.skipSpace()
	if len(p.data) < 1 {
		return -1, false
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
				return -1, false
			}
		}
		p.forward(1)

		return secs2.BinaryFormatCode, true

	case 'F':
		if !hasSecondChar {
			return -1, false
		}

		switch secondChar {
		case '4':
			p.forward(2)
			return secs2.Float32FormatCode, true
		case '8':
			p.forward(2)
			return secs2.Float64FormatCode, true
		}
	case 'I', 'U':
		if !hasSecondChar {
			return -1, false
		}

		formatCode := getIntFormatCode(firstChar, secondChar)
		if formatCode < 0 {
			return -1, false
		}
		p.forward(2)

		return formatCode, true
	}

	return -1, false
}

func getIntFormatCode(signed rune, byteSize rune) int {
	if signed == 'I' {
		switch byteSize {
		case '1':
			return secs2.Int8FormatCode
		case '2':
			return secs2.Int16FormatCode
		case '4':
			return secs2.Int32FormatCode
		case '8':
			return secs2.Int64FormatCode
		default:
			return -1
		}
	} else if signed == 'U' {
		switch byteSize {
		case '1':
			return secs2.Uint8FormatCode
		case '2':
			return secs2.Uint16FormatCode
		case '4':
			return secs2.Uint32FormatCode
		case '8':
			return secs2.Uint64FormatCode
		default:
			return -1
		}
	}

	return -1
}

func (p *HSMSParser) forward(n int) bool {
	if p.pos+n <= p.len {
		p.pos += n
		p.data = p.input[p.pos:]
		return true
	}

	return false
}

func (p *HSMSParser) backward(n int) {
	if p.pos-n >= 0 {
		p.pos -= n
		p.data = p.input[p.pos:]
	}
}

func (p *HSMSParser) skipSpace() bool {
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

func (p *HSMSParser) skipComment() {
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

func (p *HSMSParser) peekRune() rune {
	return rune(p.data[0])
}

func (p *HSMSParser) peekNonSpaceRune() rune {
	if !p.skipSpace() {
		return eof
	}

	return p.peekRune()
}

func (p *HSMSParser) nextRune() rune {
	if p.pos >= p.len {
		return eof
	}

	r := rune(p.data[0])
	if !p.forward(1) {
		return eof
	}

	return r
}

func (p *HSMSParser) nextNonSpaceRune() rune {
	if !p.skipSpace() {
		return eof
	}

	return p.nextRune()
}

func (p *HSMSParser) nextCode() (uint8, error) {
	if p.pos >= p.len {
		return 0, errors.New("invalid sml code")
	}

	for i, ch := range p.data {
		if ch < '0' || ch > '9' {
			code, err := strconv.ParseUint(p.data[:i], 10, 8)
			if err != nil {
				return 0, err
			}
			p.forward(i)

			return uint8(code), nil
		}
	}

	return 0, errors.New("invalid sml code")
}

func (p *HSMSParser) nextItemSize() (int, error) {
	if p.pos >= p.len {
		return 0, errors.New("invalid item size")
	}

	for i, ch := range p.data {
		if ch < '0' || ch > '9' {
			size, err := strconv.ParseUint(p.data[:i], 10, 32)
			if err != nil {
				return 0, err
			}

			if size > math.MaxInt32 {
				return 0, errors.New("parsed size exceeds maximum int value")
			}

			p.forward(i)

			return int(size), nil
		}
	}

	return 0, errors.New("invalid item size")
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
