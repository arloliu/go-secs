package hsms

import (
	"encoding/binary"
	"errors"
	"fmt"
	"math"
	"sync"

	"github.com/arloliu/go-secs/secs2"
)

// HSMS decoder pool
var decoderPool = sync.Pool{New: func() any { return new(hsmsDecoder) }}

// DecodeMessage decodes an HSMS message from the given byte slice.
//
// msgLen specifies the total length of the message in bytes, including the header and data.
// input is the byte array containing the encoded HSMS message.
//
// It returns the decoded HSMSMessage and an error if any occurred during decoding.
// This function uses a sync.Pool to reuse hsmsDecoder objects for efficiency.
func DecodeMessage(msgLen uint32, input []byte) (HSMSMessage, error) {
	decoder, _ := decoderPool.Get().(*hsmsDecoder)
	decoder.msgLen = msgLen
	decoder.input = input
	decoder.pos = 0

	msg, err := decoder.decodeMessage()
	decoderPool.Put(decoder)

	return msg, err
}

// hsmsDecoder is a helper struct for decoding HSMS messages.
// It maintains the current position in the input byte array and provides methods for
// decoding various data types.
type hsmsDecoder struct {
	input    []byte
	boolBuf  []bool
	intBuf   []int64
	uintBuf  []uint64
	floatBuf []float64
	pos      int
	msgLen   uint32
}

// read reads a specified number of bytes from the input and advances the current position.
func (d *hsmsDecoder) read(length int) []byte {
	result := d.input[d.pos : d.pos+length]
	d.pos += length
	return result
}

// readByte reads a single byte from the input and advances the current position.
func (d *hsmsDecoder) readByte() byte {
	result := d.input[d.pos]
	d.pos++
	return result
}

// readString reads a string of the specified length from the input and advances the current position.
func (d *hsmsDecoder) readString(length int) string {
	result := d.input[d.pos : d.pos+length]
	d.pos += length
	return string(result)
}

// decodeMessage decodes the HSMS message from the input byte array.
// It first decodes the header to determine the message type and then decodes the
// corresponding data item.
func (d *hsmsDecoder) decodeMessage() (HSMSMessage, error) {
	if len(d.input) != int(d.msgLen) {
		return nil, fmt.Errorf("hsms message length mismatch, expected: %d, actual: %d", int(d.msgLen), len(d.input))
	}

	header := d.read(10)

	if header[4] != 0 { // PType is not a SECS-II message
		return nil, fmt.Errorf("invalid PType: %d", header[4])
	}

	switch header[5] { // SType
	case DataMsgType:
		sessionID := binary.BigEndian.Uint16(header[:2])
		stream := header[2] & 0x7F
		function := header[3]
		systemBytes := header[6:10]
		replyExpected := false
		if (header[2] >> 7) != WaitBitFalse {
			replyExpected = true
		}

		dataItem, err := d.decodeMessageText()
		if err != nil {
			return nil, err
		}

		msg, err := NewDataMessage(stream, function, replyExpected, sessionID, systemBytes, dataItem)
		if err != nil {
			return nil, err
		}

		return msg, nil

	case SelectReqType, DeselectReqType, LinkTestReqType:
		return NewControlMessage(header, false), nil

	case SelectRspType, DeselectRspType, LinkTestRspType, RejectReqType, SeparateReqType:
		return NewControlMessage(header, false), nil

	default:
		// undefined SType
		return nil, fmt.Errorf("undefined SType: %d", header[5])
	}
}

// decodeMessageText decodes the SECS-II data item from the input byte array.
// It handles various data types (list, ASCII, binary, boolean, integer, float) and
// recursively decodes nested items if necessary.
func (d *hsmsDecoder) decodeMessageText() (secs2.Item, error) { //nolint: cyclop
	if d.msgLen == 10 {
		return secs2.NewEmptyItem(), nil
	}

	// decode format code and no. of length bytes
	formatByte := d.readByte()
	formatCode := formatByte >> 2

	lenBytesCount := int(formatByte & 0x3)
	if lenBytesCount == 0 {
		return secs2.NewEmptyItem(), fmt.Errorf("length bytes count is zero")
	}

	// decode length bytes to length
	length := 0
	lenBytes := d.read(lenBytesCount)

	switch lenBytesCount {
	case 1:
		length = int(lenBytes[0])
	case 2:
		length = int(lenBytes[1]) | int(lenBytes[0])<<8
	case 3:
		length = int(lenBytes[2]) | int(lenBytes[1])<<8 | int(lenBytes[0])<<16
	}

	// decode data to SECS-II item
	switch secs2.FormatCode(formatCode) {
	case secs2.ListFormatCode:
		values := make([]secs2.Item, length) // the length indicates the number of items in the list
		for i := 0; i < length; i++ {
			var err error
			values[i], err = d.decodeMessageText()
			if err != nil {
				return secs2.NewEmptyItem(), err
			}
		}

		return secs2.NewListItem(values...), nil

	case secs2.ASCIIFormatCode:
		item := secs2.NewASCIIItem(d.readString(length))
		return item, nil

	case secs2.BinaryFormatCode:
		item := secs2.NewBinaryItem(d.read(length))
		d.pos += length
		return item, nil

	case secs2.BooleanFormatCode:
		if cap(d.boolBuf) < length {
			d.boolBuf = make([]bool, 0, length)
		} else {
			d.boolBuf = d.boolBuf[:0]
		}

		for _, v := range d.read(length) {
			if v == 0 {
				d.boolBuf = append(d.boolBuf, false)
			} else {
				d.boolBuf = append(d.boolBuf, true)
			}
		}

		return secs2.NewBooleanItem(d.boolBuf), nil

	case secs2.Int8FormatCode:
		return d.decodeIntItem(1, length)
	case secs2.Int16FormatCode:
		return d.decodeIntItem(2, length)
	case secs2.Int32FormatCode:
		return d.decodeIntItem(4, length)
	case secs2.Int64FormatCode:
		return d.decodeIntItem(8, length)

	case secs2.Uint8FormatCode:
		return d.decodeUintItem(1, length)
	case secs2.Uint16FormatCode:
		return d.decodeUintItem(2, length)
	case secs2.Uint32FormatCode:
		return d.decodeUintItem(4, length)
	case secs2.Uint64FormatCode:
		return d.decodeUintItem(8, length)

	case secs2.Float32FormatCode:
		return d.decodeFloatItem(4, length)
	case secs2.Float64FormatCode:
		return d.decodeFloatItem(8, length)

	default:
		return nil, errors.New("invalid format")
	}
}

func (d *hsmsDecoder) decodeIntItem(byteSize int, length int) (secs2.Item, error) {
	if length%byteSize != 0 {
		return secs2.NewEmptyItem(), fmt.Errorf("invalid message length:%d for I%d item", length, byteSize)
	}

	if cap(d.intBuf) < length {
		d.intBuf = make([]int64, 0, length)
	} else {
		d.intBuf = d.intBuf[:0]
	}

	count := length / byteSize
	for i := 0; i < count; i++ {
		start := d.pos + byteSize*i
		switch byteSize {
		case 1:
			d.intBuf = append(d.intBuf, int64(int8(d.input[d.pos+i])))
		case 2:
			d.intBuf = append(d.intBuf, int64(int16(binary.BigEndian.Uint16(d.input[start:])))) //nolint:gosec
		case 4:
			d.intBuf = append(d.intBuf, int64(int32(binary.BigEndian.Uint32(d.input[start:])))) //nolint:gosec
		case 8:
			d.intBuf = append(d.intBuf, int64(binary.BigEndian.Uint64(d.input[start:]))) //nolint:gosec
		}
	}
	d.pos += length

	return secs2.NewIntItem(byteSize, d.intBuf), nil
}

func (d *hsmsDecoder) decodeUintItem(byteSize int, length int) (secs2.Item, error) {
	if length%byteSize != 0 {
		return secs2.NewEmptyItem(), fmt.Errorf("invalid message length:%d for I%d item", length, byteSize)
	}

	if cap(d.uintBuf) < length {
		d.uintBuf = make([]uint64, 0, length)
	} else {
		d.uintBuf = d.uintBuf[:0]
	}

	count := length / byteSize
	for i := 0; i < count; i++ {
		start := d.pos + byteSize*i
		switch byteSize {
		case 1:
			d.uintBuf = append(d.uintBuf, uint64(d.input[d.pos+i]))
		case 2:
			d.uintBuf = append(d.uintBuf, uint64(binary.BigEndian.Uint16(d.input[start:])))
		case 4:
			d.uintBuf = append(d.uintBuf, uint64(binary.BigEndian.Uint32(d.input[start:])))
		case 8:
			d.uintBuf = append(d.uintBuf, binary.BigEndian.Uint64(d.input[start:]))
		}
	}
	d.pos += length

	return secs2.NewUintItem(byteSize, d.uintBuf), nil
}

func (d *hsmsDecoder) decodeFloatItem(byteSize int, length int) (secs2.Item, error) {
	if length%byteSize != 0 {
		return secs2.NewEmptyItem(), fmt.Errorf("invalid message length:%d for I%d item", length, byteSize)
	}

	if cap(d.floatBuf) < length {
		d.floatBuf = make([]float64, 0, length)
	} else {
		d.floatBuf = d.floatBuf[:0]
	}

	count := length / byteSize
	for i := 0; i < count; i++ {
		start := d.pos + byteSize*i
		if byteSize == 4 {
			value := binary.BigEndian.Uint32(d.input[start:])
			d.floatBuf = append(d.floatBuf, float64(math.Float32frombits(value)))
		} else {
			value := binary.BigEndian.Uint64(d.input[start:])
			d.floatBuf = append(d.floatBuf, math.Float64frombits(value))
		}
	}
	d.pos += length

	return secs2.NewFloatItem(byteSize, d.floatBuf), nil
}
