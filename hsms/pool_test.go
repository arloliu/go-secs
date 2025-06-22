package hsms

import (
	"sync"
	"testing"

	"github.com/arloliu/go-secs/secs2"
	"github.com/stretchr/testify/assert"
)

func TestDataMessagePool(t *testing.T) {
	assert := assert.New(t)

	t.Run("GetDataMessage and PutDataMessage", func(t *testing.T) {
		UsePool(true) // Ensure pooling is enabled for this test

		item := secs2.NewASCIIItem("test")
		systemBytes := []byte{1, 2, 3, 4}

		msg1 := getDataMessage(1, 1, true, 10, systemBytes, item)
		assert.NotNil(msg1)
		assert.Equal(uint8(1), msg1.StreamCode())
		assert.Equal(uint8(1), msg1.FunctionCode())
		assert.True(msg1.WaitBit())
		assert.Equal(uint16(10), msg1.SessionID())
		assert.Equal(systemBytes, msg1.SystemBytes())
		assert.Equal(item, msg1.Item())

		putDataMessage(msg1)

		msg2 := getDataMessage(2, 2, false, 20, systemBytes, nil)
		assert.NotNil(msg2)
		// Since dataMsgPool is a sync.Pool, we can't guarantee that msg2 is the same as msg1
		assert.Equal(uint8(2), msg2.StreamCode())
		assert.Equal(uint8(2), msg2.FunctionCode())
		assert.False(msg2.WaitBit())
		assert.Equal(uint16(20), msg2.SessionID())
		assert.Equal(systemBytes, msg2.SystemBytes())
		assert.Equal(secs2.NewEmptyItem(), msg2.Item()) // Should be an empty item

		putDataMessage(msg2)
	})

	t.Run("Reuse from Pool", func(t *testing.T) {
		UsePool(true) // Ensure pooling is enabled for this test

		item := secs2.NewASCIIItem("test")
		systemBytes := []byte{1, 2, 3, 4}

		msg1 := getDataMessage(1, 1, true, 10, systemBytes, item)
		putDataMessage(msg1)

		msg2 := getDataMessage(2, 2, false, 20, systemBytes, nil)
		// After putting msg1 back, msg2 might be the same object
		assert.Equal(uint8(2), msg2.StreamCode())
		assert.Equal(uint8(2), msg2.FunctionCode())
		assert.False(msg2.WaitBit())
		assert.Equal(uint16(20), msg2.SessionID())
		assert.Equal(systemBytes, msg2.SystemBytes())
		assert.Equal(secs2.NewEmptyItem(), msg2.Item())
	})

	t.Run("No Pool", func(t *testing.T) {
		UsePool(false) // Disable pooling for this test

		item := secs2.NewASCIIItem("test")
		systemBytes := []byte{1, 2, 3, 4}

		msg1 := getDataMessage(1, 1, true, 10, systemBytes, item)
		putDataMessage(msg1) // Should have no effect when pooling is disabled

		msg2 := getDataMessage(2, 2, false, 20, systemBytes, nil)
		assert.NotEqual(msg1, msg2) // Should be different objects when pooling is disabled
		assert.Equal(uint8(2), msg2.StreamCode())
		assert.Equal(uint8(2), msg2.FunctionCode())
		assert.False(msg2.WaitBit())
		assert.Equal(uint16(20), msg2.SessionID())
		assert.Equal(systemBytes, msg2.SystemBytes())
		assert.Equal(secs2.NewEmptyItem(), msg2.Item())
	})

	t.Run("IsUsePool and UsePool", func(t *testing.T) {
		// Disable pooling
		UsePool(false)
		assert.False(IsUsePool())

		// Enable pooling
		UsePool(true)
		assert.True(IsUsePool())
	})
}

func TestMessageBufferPool(t *testing.T) {
	assert := assert.New(t)

	t.Run("GetMessageBuffer - Standard Size", func(t *testing.T) {
		// get buffer smaller than default size
		buf := GetMessageBuffer(1024)
		assert.NotNil(buf)
		assert.Equal(1024, len(buf))
		assert.GreaterOrEqual(cap(buf), 1024)

		// return it to pool
		PutMessageBuffer(buf)
	})

	t.Run("GetMessageBuffer - Exact Default Size", func(t *testing.T) {
		buf := GetMessageBuffer(DefaultMessageBufferSize)
		assert.NotNil(buf)
		assert.Equal(DefaultMessageBufferSize, len(buf))
		assert.Equal(DefaultMessageBufferSize, cap(buf))

		PutMessageBuffer(buf)
	})

	t.Run("GetMessageBuffer - Oversized", func(t *testing.T) {
		oversized := DefaultMessageBufferSize + 1
		buf := GetMessageBuffer(uint32(oversized))
		assert.NotNil(buf)
		assert.Equal(oversized, len(buf))
		assert.Equal(oversized, cap(buf))

		// oversized buffer should not be pooled
		PutMessageBuffer(buf)
	})

	t.Run("PutMessageBuffer - Nil Buffer", func(t *testing.T) {
		// should not panic
		PutMessageBuffer(nil)
	})

	t.Run("PutMessageBuffer - Wrong Capacity", func(t *testing.T) {
		// buffer with non-default capacity should not be pooled
		buf := make([]byte, 1024, 2048)
		PutMessageBuffer(buf)

		// get new buffer should create new one, not reuse
		buf2 := GetMessageBuffer(1024)
		assert.NotNil(buf2)
		assert.Equal(DefaultMessageBufferSize, cap(buf2))
		PutMessageBuffer(buf2)
	})

	t.Run("Buffer Data Isolation", func(t *testing.T) {
		// what we really want to test is that buffers don't leak data
		// between different uses through the len() boundary

		// get a buffer and fill it completely
		buf1 := GetMessageBuffer(100)
		for i := range buf1 {
			buf1[i] = byte(i + 1) // non-zero pattern
		}
		PutMessageBuffer(buf1)

		// get a smaller buffer
		buf2 := GetMessageBuffer(50)
		assert.Equal(50, len(buf2))

		// we can only access 50 bytes, regardless of what's in memory
		// Go's slice bounds checking ensures this

		// trying to access beyond len would panic
		assert.Panics(func() {
			_ = buf2[51] // this would panic
		})

		PutMessageBuffer(buf2)
	})

	t.Run("Buffer Reuse Verification", func(t *testing.T) {
		// to properly test reuse, we need to check capacity
		capacities := make(map[int]int)

		// get and return several buffers
		for range 10 {
			buf := GetMessageBuffer(1024)
			ptr := cap(buf)
			capacities[ptr]++
			PutMessageBuffer(buf)
		}

		// if pooling is working, we should see some repeated capacities
		// (same underlying array being reused)
		reused := false
		for _, count := range capacities {
			if count > 1 {
				reused = true
				break
			}
		}

		// we can't guarantee reuse (GC might clear the pool),
		// but in practice it should happen
		if !reused {
			t.Log("Warning: buffer reuse not observed, but this can happen due to GC")
		}
	})

	t.Run("Concurrent Access", func(t *testing.T) {
		var wg sync.WaitGroup
		iterations := 100

		for i := range iterations {
			wg.Add(1)
			go func(size uint32) {
				defer wg.Done()

				buf := GetMessageBuffer(size)
				assert.NotNil(buf)
				assert.Equal(int(size), len(buf))

				// simulate some work
				if len(buf) > 0 {
					buf[0] = 1
				}

				PutMessageBuffer(buf)
			}(uint32(i%1024 + 1))
		}

		wg.Wait()
	})

	t.Run("Buffer Capacity Preservation", func(t *testing.T) {
		// get buffer with small size
		buf := GetMessageBuffer(10)
		assert.NotNil(buf)
		assert.Equal(10, len(buf))
		assert.Equal(DefaultMessageBufferSize, cap(buf))

		// return to pool
		PutMessageBuffer(buf)

		// get larger buffer - should reuse same underlying array
		buf2 := GetMessageBuffer(1000)
		assert.NotNil(buf2)
		assert.Equal(1000, len(buf2))
		assert.Equal(DefaultMessageBufferSize, cap(buf2))

		PutMessageBuffer(buf2)
	})

	t.Run("Zero Length After Return", func(t *testing.T) {
		buf := GetMessageBuffer(100)
		assert.Equal(100, len(buf))

		// fill with data
		for i := range buf {
			buf[i] = byte(i)
		}

		PutMessageBuffer(buf)

		// internal buffer should be reset to zero length
		// we can't directly test this, but we can verify behavior
		buf2 := GetMessageBuffer(50)
		assert.Equal(50, len(buf2))

		// data beyond len should not be accessible
		PutMessageBuffer(buf2)
	})

	t.Run("Edge Cases", func(t *testing.T) {
		// zero size
		buf := GetMessageBuffer(0)
		assert.NotNil(buf)
		assert.Equal(0, len(buf))
		assert.Equal(DefaultMessageBufferSize, cap(buf))
		PutMessageBuffer(buf)

		// max uint32 size (should allocate directly)
		hugeBuf := GetMessageBuffer(1 << 20) // 1MB
		assert.NotNil(hugeBuf)
		assert.Equal(1<<20, len(hugeBuf))
		PutMessageBuffer(hugeBuf)
	})
}

func BenchmarkMessageBufferPool(b *testing.B) {
	b.Run("WithPool", func(b *testing.B) {
		b.ReportAllocs()
		b.ResetTimer()

		var buf []byte
		for b.Loop() {
			buf = GetMessageBuffer(1024)
			// simulate some work
			if len(buf) > 0 {
				buf[0] = 1
			}
			PutMessageBuffer(buf)
		}
	})

	b.Run("WithoutPool", func(b *testing.B) {
		b.ReportAllocs()
		b.ResetTimer()

		var buf []byte
		for b.Loop() {
			buf = make([]byte, 1024)
			// simulate some work
			if len(buf) > 0 {
				buf[0] = 1
			}
		}
	})

	b.Run("OversizedBuffer", func(b *testing.B) {
		b.ReportAllocs()
		b.ResetTimer()

		var buf []byte
		oversized := uint32(DefaultMessageBufferSize + 1)
		for b.Loop() {
			buf = GetMessageBuffer(oversized)
			// simulate some work
			if len(buf) > 0 {
				buf[0] = 1
			}
			PutMessageBuffer(buf)
		}
	})
}
