package secs2_test

import (
	"fmt"

	"github.com/arloliu/go-secs/v2/secs2"
)

// ExampleItem_deferredError shows the safe pattern: check the accessor's error (or
// Error()) rather than relying on a type predicate.
func ExampleItem_deferredError() {
	item := secs2.NewIntItem(1, 42)

	// Correct: the accessor's error is checked, not discarded.
	if v, err := item.ToInt(); err == nil {
		fmt.Println(v[0])
	}

	// For a list, Error() aggregates child errors even when the list's own error is nil.
	_ = item.Error()
	// Output: 42
}
