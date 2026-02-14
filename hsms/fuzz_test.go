package hsms

import (
	"encoding/binary"
	"testing"
)

// FuzzDecodeMessage fuzzes the HSMS message decoder with arbitrary payloads.
//
// This exercises the full SECS-II parsing path: header validation, format code
// decoding, recursive list unpacking, and all numeric/string/boolean item types.
// The invariant is: DecodeMessage must never panic.
func FuzzDecodeMessage(f *testing.F) {
	// Seed: valid linktest.req (10-byte control message, SType=5)
	f.Add(uint32(10), []byte{
		0xFF, 0xFF, 0x00, 0x00, 0x00, LinkTestReqType, 0x00, 0x00, 0x00, 0x01,
	})

	// Seed: valid select.req (10-byte control message, SType=1)
	f.Add(uint32(10), []byte{
		0xFF, 0xFF, 0x00, 0x00, 0x00, SelectReqType, 0x00, 0x00, 0x00, 0x02,
	})

	// Seed: valid S1F1 data message with ASCII item <A[5] "hello">
	// Header: session=1, stream=1|W-bit, func=1, PType=0, SType=0
	// Body: format=ASCII(0x41), len=5, "hello"
	s1f1 := []byte{
		0x00, 0x01, 0x81, 0x01, 0x00, 0x00, 0x00, 0x00, 0x00, 0x03,
		0x41, 0x05, 'h', 'e', 'l', 'l', 'o',
	}
	f.Add(uint32(len(s1f1)), s1f1)

	// Seed: header-only data message (no SECS-II body, e.g. S1F1 with no item)
	f.Add(uint32(10), []byte{
		0x00, 0x01, 0x81, 0x01, 0x00, 0x00, 0x00, 0x00, 0x00, 0x04,
	})

	// Seed: payload too short for HSMS header (< 10 bytes)
	f.Add(uint32(5), []byte{0x01, 0x02, 0x03, 0x04, 0x05})

	// Seed: empty payload
	f.Add(uint32(0), []byte{})

	// Seed: length/input mismatch (msgLen=10, only 5 bytes)
	f.Add(uint32(10), []byte{0x01, 0x02, 0x03, 0x04, 0x05})

	// Seed: undefined SType (0xFF)
	f.Add(uint32(10), []byte{
		0xFF, 0xFF, 0x00, 0x00, 0x00, 0xFF, 0x00, 0x00, 0x00, 0x01,
	})

	// Seed: invalid PType (must be 0 for SECS-II)
	f.Add(uint32(10), []byte{
		0xFF, 0xFF, 0x00, 0x00, 0x01, LinkTestReqType, 0x00, 0x00, 0x00, 0x01,
	})

	// Seed: nested list L[1, L[1, <A[1] "x">]]
	nestedList := []byte{
		0x00, 0x01, 0x81, 0x01, 0x00, 0x00, 0x00, 0x00, 0x00, 0x05, // header
		0x01, 0x01, 0x01, // L,1
		0x01, 0x01, 0x01, // L,1
		0x41, 0x01, 'x', // A,1 "x"
	}
	f.Add(uint32(len(nestedList)), nestedList)

	// Seed: deeply nested empty lists (stress recursion depth check)
	deepList := []byte{0x00, 0x01, 0x81, 0x01, 0x00, 0x00, 0x00, 0x00, 0x00, 0x06}
	for range 70 { // 70 levels of L[1] exceeds MaxListDepth(64)
		deepList = append(deepList, 0x01, 0x01, 0x01)
	}
	f.Add(uint32(len(deepList)), deepList)

	// Seed: boolean item
	boolItem := []byte{
		0x00, 0x01, 0x81, 0x01, 0x00, 0x00, 0x00, 0x00, 0x00, 0x07,
		0x25, 0x02, 0x01, 0x00, // Boolean format (0x09<<2|0x01=0x25), len=2
	}
	f.Add(uint32(len(boolItem)), boolItem)

	// Seed: I4 integer item
	i4Item := []byte{
		0x00, 0x01, 0x81, 0x01, 0x00, 0x00, 0x00, 0x00, 0x00, 0x08,
		0x71, 0x04, 0x00, 0x00, 0x00, 0x2A, // I4 format (0x1C<<2|0x01=0x71), len=4
	}
	f.Add(uint32(len(i4Item)), i4Item)

	f.Fuzz(func(t *testing.T, msgLen uint32, input []byte) {
		// DecodeMessage must never panic regardless of input.
		msg, err := DecodeMessage(msgLen, input)
		if err == nil && msg == nil {
			t.Fatal("DecodeMessage returned nil msg and nil err")
		}
	})
}

// FuzzDecodeHSMSMessage fuzzes the full HSMS message decoder including
// the 4-byte big-endian length prefix.
func FuzzDecodeHSMSMessage(f *testing.F) {
	// Seed: valid linktest.req with length prefix
	linktest := make([]byte, 14)
	binary.BigEndian.PutUint32(linktest[:4], 10)
	copy(linktest[4:], []byte{
		0xFF, 0xFF, 0x00, 0x00, 0x00, LinkTestReqType, 0x00, 0x00, 0x00, 0x01,
	})
	f.Add(linktest)

	// Seed: too short for MinHSMSSize
	f.Add([]byte{0x00, 0x01})

	// Seed: empty
	f.Add([]byte{})

	// Seed: length header claims 10 but only 5 bytes follow
	short := make([]byte, 9)
	binary.BigEndian.PutUint32(short[:4], 10)
	copy(short[4:], []byte{0x01, 0x02, 0x03, 0x04, 0x05})
	f.Add(short)

	// Seed: valid S1F1 data message with complete frame
	s1f1Frame := make([]byte, 4+17)
	binary.BigEndian.PutUint32(s1f1Frame[:4], 17)
	copy(s1f1Frame[4:], []byte{
		0x00, 0x01, 0x81, 0x01, 0x00, 0x00, 0x00, 0x00, 0x00, 0x03,
		0x41, 0x05, 'h', 'e', 'l', 'l', 'o',
	})
	f.Add(s1f1Frame)

	// Seed: oversized length
	oversized := make([]byte, 14)
	binary.BigEndian.PutUint32(oversized[:4], 0x7FFFFFFF)
	copy(oversized[4:], []byte{0xFF, 0xFF, 0x00, 0x00, 0x00, 0x05, 0x00, 0x00, 0x00, 0x01})
	f.Add(oversized)

	f.Fuzz(func(t *testing.T, data []byte) {
		// DecodeHSMSMessage must never panic regardless of input.
		msg, err := DecodeHSMSMessage(data)
		if err == nil && msg == nil {
			t.Fatal("DecodeHSMSMessage returned nil msg and nil err")
		}
	})
}
