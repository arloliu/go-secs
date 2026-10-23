package gem

import "github.com/arloliu/go-secs/v2/secs2"

// Present exposes present to the external tests, which measure it directly,
// as a function for the same reason as [PresentEach].
func Present(item secs2.Item, where string) secs2.Item {
	return present(item, where)
}

// PresentEach exposes presentEach to the external tests, which measure it directly.
// It is a function rather than a variable holding presentEach,
// so escape analysis still sees the direct call and keeps at on the stack.
func PresentEach(items []secs2.Item, where string, at ...int) []secs2.Item {
	return presentEach(items, where, at...)
}
