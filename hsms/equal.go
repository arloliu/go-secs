package hsms

import "github.com/arloliu/go-secs/v2/secs2"

// Equal reports whether msg and other are the same HSMS data message: identical 10-byte header
// (session ID, stream, function, wait bit, PType/SType, System Bytes) AND semantically equal
// SECS-II body.
//
// Equal forces the lazy body decode on both messages and compares the decoded items with
// secs2.Equal; it never inspects the internal decode cache, so two messages with identical wire
// bytes compare equal regardless of whether Item() has previously been called on either. A decode
// error on either side makes the messages unequal (Equal never panics).
//
// Two nil messages are equal; a nil message is not equal to a non-nil message.
func (msg *DataMessage) Equal(other *DataMessage) bool {
	if msg == nil || other == nil {
		return msg == nil && other == nil
	}
	if msg.HeaderBytes() != other.HeaderBytes() { // [10]byte value compare
		return false
	}

	itemA, errA := msg.Item()
	itemB, errB := other.Item()
	if errA != nil || errB != nil {
		return false
	}

	return secs2.Equal(itemA, itemB)
}
