package sml

import (
	"fmt"
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
	WithStrictMode(true)
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
