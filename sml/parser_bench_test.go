package sml

import (
	"fmt"
	"math"
	"strings"
	"testing"

	"github.com/arloliu/go-secs/v2/secs2"
)

func BenchmarkParse_Int_Small(b *testing.B) {
	benchParse(b, genIntSML(100))
}

func BenchmarkParse_Int_Medium(b *testing.B) {
	benchParse(b, genIntSML(1000))
}

func BenchmarkParse_Int_Large(b *testing.B) {
	benchParse(b, genIntSML(10000))
}

func BenchmarkParse_ASCII_Small(b *testing.B) {
	benchParse(b, genASCIISML(100))
}

func BenchmarkParse_ASCII_Medium(b *testing.B) {
	benchParse(b, genASCIISML(1000))
}

func BenchmarkParse_ASCII_Large(b *testing.B) {
	benchParse(b, genASCIISML(10000))
}

func BenchmarkParse_AllTypes(b *testing.B) {
	items := []secs2.Item{}

	for i := range 39 {
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
		default:
		}
	}
	listItems := make([]secs2.Item, 0, 100)
	for range 100 {
		listItems = append(listItems, secs2.L(items...))
	}

	smlStr := itemToMsgSML(1, 1, true, secs2.L(listItems...))
	benchParse(b, smlStr)
}

func benchParse(b *testing.B, sml string) {
	b.Helper()

	msgs, err := Parse(sml)
	_ = msgs
	if err != nil {
		b.Log(err)
		b.FailNow()
	}

	b.ResetTimer()
	for range b.N {
		msgs, err := Parse(sml)
		_ = msgs
		if err != nil {
			b.Log(err)
			b.FailNow()
		}
	}
	b.StopTimer()
}

// itemToMsgSML constructs a minimal SML message string from item.ToSML() output.
func itemToMsgSML(stream, function uint8, wbit bool, item secs2.Item) string {
	wbitStr := ""
	if wbit {
		wbitStr = " W"
	}
	itemSML := item.ToSML()
	if itemSML == "" {
		return fmt.Sprintf("S%dF%d%s\n.", stream, function, wbitStr)
	}

	return fmt.Sprintf("S%dF%d%s\n%s\n.", stream, function, wbitStr, itemSML)
}

func genIntSML(count int) string {
	intItems := make([]secs2.Item, count/100)
	for i := 0; i < count/100; i++ {
		items := make([]any, 100)
		for j := range 100 {
			items[j] = int64(j)
		}
		intItems[i] = secs2.I8(items...)
	}
	listItem := secs2.L(intItems...)

	return itemToMsgSML(0, 0, false, listItem)
}

func genASCIISML(count int) string {
	strItem := make([]secs2.Item, count/100)
	for i := 0; i < count/100; i++ {
		var str strings.Builder
		for j := range 100 {
			fmt.Fprint(&str, j)
		}
		strItem[i] = secs2.A(str.String())
	}
	listItem := secs2.L(strItem...)

	return itemToMsgSML(0, 0, false, listItem)
}
