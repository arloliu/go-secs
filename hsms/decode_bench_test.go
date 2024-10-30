package hsms

import (
	"math"
	"testing"

	"github.com/arloliu/go-secs/secs2"
)

func BenchmarkDecodeMessage_DataMessage_10_UsePool(b *testing.B) {
	UsePool(true)
	benchmarkDecodeDataMessage(b, 10)
}

func BenchmarkDecodeMessage_DataMessage_10_NoPool(b *testing.B) {
	UsePool(false)
	benchmarkDecodeDataMessage(b, 10)
}

func BenchmarkDecodeMessage_DataMessage_100_UsePool(b *testing.B) {
	UsePool(true)
	benchmarkDecodeDataMessage(b, 100)
}

func BenchmarkDecodeMessage_DataMessage_100_NoPool(b *testing.B) {
	UsePool(false)
	benchmarkDecodeDataMessage(b, 100)
}

func BenchmarkDecodeMessage_DataMessage_1000_UsePool(b *testing.B) {
	UsePool(true)
	benchmarkDecodeDataMessage(b, 1000)
}

func BenchmarkDecodeMessage_DataMessage_1000_NoPool(b *testing.B) {
	UsePool(false)
	benchmarkDecodeDataMessage(b, 1000)
}

func BenchmarkDecodeMessage_DataMessage_AllTypes(b *testing.B) {
	items := []secs2.Item{}

	for i := 0; i < 39; i++ {
		switch i % 13 {
		case 0:
			items = append(items, secs2.B(127))
		case 1:
			items = append(items, secs2.BOOLEAN(true))
		case 2:
			items = append(items, secs2.A("test message"))
		case 3:
			items = append(items, secs2.I1(math.MaxInt8))
		case 4:
			items = append(items, secs2.I2(math.MaxInt16))
		case 5:
			items = append(items, secs2.I4(math.MaxInt32))
		case 6:
			items = append(items, secs2.I8(math.MaxInt64))
		case 7:
			items = append(items, secs2.U1(math.MaxUint8))
		case 8:
			items = append(items, secs2.U2(math.MaxUint16))
		case 9:
			items = append(items, secs2.U4(math.MaxUint32))
		case 10:
			items = append(items, secs2.U8(uint64(math.MaxUint64)))
		case 11:
			items = append(items, secs2.F4(1.2345678))
		case 12:
			items = append(items, secs2.F8(1.2345678))
		}
	}
	listItems := []secs2.Item{}
	for i := 0; i < 100; i++ {
		listItems = append(listItems, secs2.L(items...))
	}

	item := secs2.L(listItems...)

	msg, err := NewDataMessage(1, 1, true, 1234, GenerateMsgSystemBytes(), item)
	if err != nil {
		b.Logf("error:%v", err)
		b.FailNow()
	}

	input := msg.ToBytes()[4:]

	decodedMsg, err := DecodeMessage(uint32(len(input)), input)
	if err != nil {
		b.FailNow()
	}
	decodedMsg.Free()

	b.ResetTimer()
	for i := 0; i <= b.N; i++ {
		msg, err := DecodeMessage(uint32(len(input)), input)
		if err != nil {
			b.FailNow()
		}
		_ = msg
		msg.Free()
	}
	b.StopTimer()
}

func benchmarkDecodeDataMessage(b *testing.B, testSize int) {
	intItems := make([]secs2.Item, 0, testSize)
	for i := 0; i < testSize; i++ {
		intItems = append(intItems, secs2.I8(int64(i)))
	}

	floatItems := make([]secs2.Item, 0, testSize)
	for i := 0; i < testSize; i++ {
		floatItems = append(floatItems, secs2.F8(i))
	}

	item := secs2.L(
		secs2.L(intItems...),
		secs2.L(floatItems...),
	)
	msg, err := NewDataMessage(1, 1, true, 1234, GenerateMsgSystemBytes(), item)
	if err != nil {
		b.Logf("error:%v", err)
		b.FailNow()
	}

	input := msg.ToBytes()[4:]

	decodedMsg, err := DecodeMessage(uint32(len(input)), input)
	if err != nil {
		b.FailNow()
	}
	decodedMsg.Free()

	b.ResetTimer()
	for i := 0; i <= b.N; i++ {
		msg, err := DecodeMessage(uint32(len(input)), input)
		if err != nil {
			b.FailNow()
		}
		_ = msg
		msg.Free()
	}
	b.StopTimer()
}
