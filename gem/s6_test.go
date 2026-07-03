package gem_test

import (
	"testing"

	"github.com/arloliu/go-secs/v2/gem"
	"github.com/arloliu/go-secs/v2/secs2"
)

// assertS6Header checks that msg is an S6 message with the given function code and wait bit.
func assertS6Header(t *testing.T, msg secs2.SECS2Message, function uint8, wantW bool) {
	t.Helper()

	if msg.StreamCode() != 6 {
		t.Errorf("StreamCode: got %d, want 6", msg.StreamCode())
	}

	if msg.FunctionCode() != function {
		t.Errorf("FunctionCode: got %d, want %d", msg.FunctionCode(), function)
	}

	if msg.WaitBit() != wantW {
		t.Errorf("WaitBit: got %v, want %v", msg.WaitBit(), wantW)
	}
}

// checkReportElement verifies that item is L[2]{ <rptid as U4[wantRptID]> L[b]{ A[v]... } }.
func checkReportElement(t *testing.T, item secs2.Item, wantRptID uint64, wantVals []string) {
	t.Helper()

	rpt, err := item.ToList()
	if err != nil {
		t.Fatalf("report.ToList: %v", err)
	}

	if len(rpt) != 2 {
		t.Fatalf("report list length: got %d, want 2", len(rpt))
	}

	rid, err := rpt[0].ToUint()
	if err != nil {
		t.Fatalf("rpt[0].ToUint: %v", err)
	}

	if len(rid) != 1 || rid[0] != wantRptID {
		t.Errorf("rptid: got %v, want [%d]", rid, wantRptID)
	}

	vals, err := rpt[1].ToList()
	if err != nil {
		t.Fatalf("rpt[1].ToList: %v", err)
	}

	if len(vals) != len(wantVals) {
		t.Fatalf("vals length: got %d, want %d", len(vals), len(wantVals))
	}

	for i, want := range wantVals {
		got, err := vals[i].ToASCII()
		if err != nil {
			t.Fatalf("vals[%d].ToASCII: %v", i, err)
		}

		if got != want {
			t.Errorf("vals[%d]: got %q, want %q", i, got, want)
		}
	}
}

func TestS6F11(t *testing.T) {
	dataid := secs2.U4(1)
	ceid := secs2.U4(1000)
	rptid1 := secs2.U4(10)
	v1 := secs2.A("val1")
	v2 := secs2.A("val2")
	rptid2 := secs2.U4(20)
	v3 := secs2.A("val3")

	msg := gem.S6F11(dataid, ceid, gem.Report(rptid1, v1, v2), gem.Report(rptid2, v3))

	assertS6Header(t, msg, 11, true)

	decoded, err := secs2.Decode(msg.Item().ToBytes())
	if err != nil {
		t.Fatalf("secs2.Decode: %v", err)
	}

	// L[3]{ <dataid> <ceid> L[2]{ report report } }
	top, err := decoded.ToList()
	if err != nil {
		t.Fatalf("top ToList: %v", err)
	}

	if len(top) != 3 {
		t.Fatalf("top list length: got %d, want 3", len(top))
	}

	did, err := top[0].ToUint()
	if err != nil {
		t.Fatalf("top[0].ToUint: %v", err)
	}

	if len(did) != 1 || did[0] != 1 {
		t.Errorf("dataid: got %v, want [1]", did)
	}

	cev, err := top[1].ToUint()
	if err != nil {
		t.Fatalf("top[1].ToUint: %v", err)
	}

	if len(cev) != 1 || cev[0] != 1000 {
		t.Errorf("ceid: got %v, want [1000]", cev)
	}

	reports, err := top[2].ToList()
	if err != nil {
		t.Fatalf("top[2].ToList: %v", err)
	}

	if len(reports) != 2 {
		t.Fatalf("reports list length: got %d, want 2", len(reports))
	}

	// L[2]{ U4[10] L[2]{ A[val1] A[val2] } }
	checkReportElement(t, reports[0], 10, []string{"val1", "val2"})
	// L[2]{ U4[20] L[1]{ A[val3] } }
	checkReportElement(t, reports[1], 20, []string{"val3"})
}

func TestS6F12(t *testing.T) {
	msg := gem.S6F12(0)

	assertS6Header(t, msg, 12, false)

	decoded, err := secs2.Decode(msg.Item().ToBytes())
	if err != nil {
		t.Fatalf("secs2.Decode: %v", err)
	}

	bs, err := decoded.ToBinary()
	if err != nil {
		t.Fatalf("ToBinary: %v", err)
	}

	if len(bs) != 1 || bs[0] != 0 {
		t.Errorf("ackc6: got %v, want [0x00]", bs)
	}
}
