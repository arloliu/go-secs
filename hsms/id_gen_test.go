package hsms

import (
	"math"
	"sync"
	"testing"
)

func TestGenerateMsgID(t *testing.T) {
	id1 := GenerateMsgID()
	id2 := GenerateMsgID()

	if id1 == id2 {
		t.Errorf("expected different IDs, got %d and %d", id1, id2)
	}
	if id2 != id1+1 {
		t.Errorf("expected consecutive IDs, got %d then %d", id1, id2)
	}
}

func TestGenerateMsgIDConcurrent(t *testing.T) {
	const goroutines = 50
	const perGoroutine = 100

	var wg sync.WaitGroup
	var mu sync.Mutex
	seen := make(map[uint32]struct{}, goroutines*perGoroutine)

	for range goroutines {
		wg.Go(func() {
			for range perGoroutine {
				id := GenerateMsgID()
				mu.Lock()
				seen[id] = struct{}{}
				mu.Unlock()
			}
		})
	}
	wg.Wait()

	if len(seen) != goroutines*perGoroutine {
		t.Errorf("expected %d unique IDs, got %d (collisions occurred)",
			goroutines*perGoroutine, len(seen))
	}
}

func TestToSystemBytes(t *testing.T) {
	id := uint32(123456)
	want := [4]byte{0x00, 0x01, 0xe2, 0x40}

	got := ToSystemBytes(id)
	if got != want {
		t.Errorf("ToSystemBytes(%d) = %v, want %v", id, got, want)
	}
}

func TestToSystemBytesRoundTrip(t *testing.T) {
	id := GenerateMsgID()

	msg, err := NewDataMessage(1, 1, false, 0, ToSystemBytes(id), nil)
	if err != nil {
		t.Fatalf("NewDataMessage: %v", err)
	}
	if got := msg.SystemBytes(); got != ToSystemBytes(id) {
		t.Errorf("message System Bytes = %v, want %v", got, ToSystemBytes(id))
	}
}

func TestFromSystemBytes(t *testing.T) {
	tests := []struct {
		name string
		b    [4]byte
		want uint32
	}{
		{name: "zero", b: [4]byte{0, 0, 0, 0}, want: 0},
		{name: "one, big-endian", b: [4]byte{0, 0, 0, 1}, want: 1},
		{name: "max", b: [4]byte{0xFF, 0xFF, 0xFF, 0xFF}, want: math.MaxUint32},
		{name: "123456", b: [4]byte{0x00, 0x01, 0xe2, 0x40}, want: 123456},
	}

	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			if got := FromSystemBytes(test.b); got != test.want {
				t.Errorf("FromSystemBytes(%v) = %d, want %d", test.b, got, test.want)
			}
		})
	}
}

func TestFromSystemBytesRoundTrip(t *testing.T) {
	ids := []uint32{0, 1, math.MaxUint32, GenerateMsgID(), GenerateMsgID()}

	for _, id := range ids {
		if got := FromSystemBytes(ToSystemBytes(id)); got != id {
			t.Errorf("FromSystemBytes(ToSystemBytes(%d)) = %d, want %d", id, got, id)
		}
	}
}
