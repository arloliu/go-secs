package tracepack

import "math"

// periodHour returns the UTC hour of the period [start, end),
// and false when the period is empty or not inside one UTC hour.
func periodHour(start, end int64) (int64, bool) {
	// start < end is checked first, so end-1 cannot overflow.
	if start >= end || hourOf(start) != hourOf(end-1) {
		return 0, false
	}

	return hourOf(start), true
}

// hourBounds returns the first and last nanosecond of UTC hour h, as hourOf numbers it, clamped to the int64 range.
func hourBounds(h int64) (first, last int64) {
	first, last = math.MinInt64, math.MaxInt64
	// Go's division truncates toward zero, so these are the extreme hours whose bounds do not overflow.
	if h >= math.MinInt64/hourNs {
		first = h * hourNs
	}
	if h < math.MaxInt64/hourNs {
		last = (h+1)*hourNs - 1
	}

	return first, last
}
