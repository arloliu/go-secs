package hsms

import (
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
