package gem_test

import (
	"testing"

	"github.com/arloliu/go-secs/v2/gem"
	"github.com/arloliu/go-secs/v2/secs2"
)

func TestReport(t *testing.T) {
	rptid := secs2.U4(7)
	v1 := secs2.A("alpha")
	v2 := secs2.A("beta")

	item := gem.Report(rptid, v1, v2)

	decoded, err := secs2.Decode(item.ToBytes())
	if err != nil {
		t.Fatalf("secs2.Decode: %v", err)
	}

	// L[2]{ U4[7] L[2]{ A[alpha] A[beta] } }
	outer, err := decoded.ToList()
	if err != nil {
		t.Fatalf("outer ToList: %v", err)
	}

	if len(outer) != 2 {
		t.Fatalf("outer list length: got %d, want 2", len(outer))
	}

	rid, err := outer[0].ToUint()
	if err != nil {
		t.Fatalf("outer[0].ToUint: %v", err)
	}

	if len(rid) != 1 || rid[0] != 7 {
		t.Errorf("rptid: got %v, want [7]", rid)
	}

	inner, err := outer[1].ToList()
	if err != nil {
		t.Fatalf("outer[1].ToList: %v", err)
	}

	if len(inner) != 2 {
		t.Fatalf("inner list length: got %d, want 2", len(inner))
	}

	s0, err := inner[0].ToASCII()
	if err != nil {
		t.Fatalf("inner[0].ToASCII: %v", err)
	}

	if s0 != "alpha" {
		t.Errorf("inner[0]: got %q, want %q", s0, "alpha")
	}

	s1, err := inner[1].ToASCII()
	if err != nil {
		t.Fatalf("inner[1].ToASCII: %v", err)
	}

	if s1 != "beta" {
		t.Errorf("inner[1]: got %q, want %q", s1, "beta")
	}
}

func TestReportEmpty(t *testing.T) {
	rptid := secs2.U4(99)

	item := gem.Report(rptid)

	decoded, err := secs2.Decode(item.ToBytes())
	if err != nil {
		t.Fatalf("secs2.Decode: %v", err)
	}

	// L[2]{ U4[99] L[0] }
	outer, err := decoded.ToList()
	if err != nil {
		t.Fatalf("outer ToList: %v", err)
	}

	if len(outer) != 2 {
		t.Fatalf("outer list length: got %d, want 2", len(outer))
	}

	inner, err := outer[1].ToList()
	if err != nil {
		t.Fatalf("outer[1].ToList: %v", err)
	}

	if len(inner) != 0 {
		t.Fatalf("inner list length: got %d, want 0", len(inner))
	}
}
