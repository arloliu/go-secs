package hsms

import (
	"sync"

	"github.com/arloliu/go-secs/internal/util"
	"github.com/arloliu/go-secs/secs2"
)

var dataMsgPool = sync.Pool{New: func() interface{} { return new(DataMessage) }}

// getDataMessage retrieves a DataMessage from the pool if enabled, or creates a new one otherwise.
//
// It initializes the DataMessage with the provided stream, function, replyExpected flag, session ID,
// system bytes, and data item. If the data item is nil, it sets it to an empty item.
//
// The usePool variable determines whether to use the DataMessage pool. If enabled, the function will try to
// retrieve a DataMessage from the pool. If the pool is disabled or no DataMessage is available in the pool,
// a new DataMessage is created.
func getDataMessage(stream byte, function byte, replyExpected bool, sessionID uint16, systemBytes []byte, dataItem secs2.Item) *DataMessage {
	var msg *DataMessage
	if usePool {
		msg, _ = dataMsgPool.Get().(*DataMessage)
	} else {
		msg = &DataMessage{}
	}

	if msg == nil {
		msg = &DataMessage{}
	}

	msg.stream = stream
	msg.function = function
	msg.waitBit = WaitBitFalse
	msg.dataItem = dataItem
	msg.sessionID = sessionID
	msg.systemBytes = util.CloneSlice(systemBytes, 4)

	if msg.dataItem == nil {
		msg.dataItem = secs2.NewEmptyItem()
	}

	if replyExpected {
		msg.waitBit = WaitBitTrue
	}

	return msg
}

// putDataMessage returns a DataMessage to the pool if pooling is enabled.
// It resets the dataItem field to nil before putting the message back into the pool.
func putDataMessage(msg *DataMessage) {
	if usePool {
		msg.dataItem = nil
		dataMsgPool.Put(msg)
	}
}

var usePool = true

// IsUsePool returns true if HSMS data message and SECS-II item pooling is enabled, false otherwise.
func IsUsePool() bool {
	return usePool
}

// UsePool enables or disables the use of pools for HSMS data messages and SECS-II items.
// Pooling can help reduce memory allocations by reusing objects.
//
// By default, pooling is enabled (usePool = true).
//
// This function also controls the pooling behavior of SECS-II items using secs2.UsePool(val).
func UsePool(val bool) {
	usePool = val
	secs2.UsePool(val)
}
