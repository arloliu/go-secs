package sml

import (
	"fmt"
	"math"
	"os"
	"testing"

	"github.com/arloliu/go-secs/hsms"
	"github.com/arloliu/go-secs/secs2"
)

func BenchmarkParseHSMS_Int_Small(b *testing.B) {
	if os.Getenv("PARSER") == "fast" {
		benchParseHSMSFast(b, genIntSML(100))
	} else {
		benchParseHSMSSlow(b, genIntSML(100))
	}
}

func BenchmarkParseHSMS_Int_Medium(b *testing.B) {
	if os.Getenv("PARSER") == "fast" {
		benchParseHSMSFast(b, genIntSML(1000))
	} else {
		benchParseHSMSSlow(b, genIntSML(1000))
	}
}

func BenchmarkParseHSMS_Int_Large(b *testing.B) {
	if os.Getenv("PARSER") == "fast" {
		benchParseHSMSFast(b, genIntSML(10000))
	} else {
		benchParseHSMSSlow(b, genIntSML(10000))
	}
}

func BenchmarkParseHSMS_ASCII_Small(b *testing.B) {
	if os.Getenv("PARSER") == "fast" {
		benchParseHSMSFast(b, genASCIISML(100))
	} else {
		benchParseHSMSSlow(b, genASCIISML(100))
	}
}

func BenchmarkParseHSMS_ASCII_Medium(b *testing.B) {
	if os.Getenv("PARSER") == "fast" {
		benchParseHSMSFast(b, genASCIISML(1000))
	} else {
		benchParseHSMSSlow(b, genASCIISML(1000))
	}
}

func BenchmarkParseHSMS_ASCII_Large(b *testing.B) {
	if os.Getenv("PARSER") == "fast" {
		benchParseHSMSFast(b, genASCIISML(10000))
	} else {
		benchParseHSMSSlow(b, genASCIISML(10000))
	}
}

func BenchmarkParseHSMS_AllTypes(b *testing.B) {
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

	msg, _ := hsms.NewDataMessage(1, 1, true, 0, nil, secs2.L(listItems...))
	sml := msg.ToSML()

	if os.Getenv("PARSER") == "fast" {
		benchParseHSMSFast(b, sml)
	} else {
		benchParseHSMSSlow(b, sml)
	}
}

func benchParseHSMSSlow(b *testing.B, sml string) {
	msgs, errs := ParseHSMSSlow(sml)
	_ = msgs
	if len(errs) > 0 {
		b.Log(errs)
		b.FailNow()
	}

	b.ResetTimer()
	for i := 0; i <= b.N; i++ {
		msgs, errs := ParseHSMSSlow(sml)
		_ = msgs
		if len(errs) > 0 {
			b.Log(errs)
			b.FailNow()
		}
		for _, msg := range msgs {
			msg.Free()
		}
	}
	b.StopTimer()
}

func benchParseHSMSFast(b *testing.B, sml string) {
	WithStrictMode(false)
	msgs, err := ParseHSMS(sml)
	_ = msgs
	if err != nil {
		b.Log(err)
		b.FailNow()
	}

	b.ResetTimer()
	for i := 0; i <= b.N; i++ {
		msgs, err := ParseHSMS(sml)
		_ = msgs
		if err != nil {
			b.Log(err)
			b.FailNow()
		}
		for _, msg := range msgs {
			msg.Free()
		}
	}
	b.StopTimer()
}

func genIntSML(count int) string {
	intItems := make([]secs2.Item, count/100)
	for i := 0; i < count/100; i++ {
		items := make([]any, 100)
		for j := 0; j < 100; j++ {
			items[j] = int64(j)
		}
		intItems[i] = secs2.I8(items...)
	}
	listItem := secs2.L(intItems...)
	msg, err := hsms.NewDataMessage(0, 0, false, 1234, nil, listItem)
	if err != nil {
		return ""
	}

	return msg.ToSML()
}

func genASCIISML(count int) string {
	strItem := make([]secs2.Item, count/100)
	for i := 0; i < count/100; i++ {
		str := ""
		for j := 0; j < 100; j++ {
			str += fmt.Sprint(j)
		}
		strItem[i] = secs2.A(str)
	}
	listItem := secs2.L(strItem...)
	msg, err := hsms.NewDataMessage(0, 0, false, 1234, nil, listItem)
	if err != nil {
		return ""
	}

	return msg.ToSML()
}
