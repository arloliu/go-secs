package hsms

import (
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
