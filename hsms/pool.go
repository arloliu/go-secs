package hsms

import (
	"sync"

	"github.com/arloliu/go-secs/internal/util"
	"github.com/arloliu/go-secs/secs2"
)

var dataMsgPool = sync.Pool{New: func() interface{} { return new(DataMessage) }}

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

func putDataMessage(msg *DataMessage) {
	if usePool {
		msg.dataItem = nil
		dataMsgPool.Put(msg)
	}
}

var usePool = true

// IsUsePool returns if using SECS-II item pool
func IsUsePool() bool {
	return usePool
}

func UsePool(val bool) {
	usePool = val
	secs2.UsePool(val)
}
