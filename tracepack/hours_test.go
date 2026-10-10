package tracepack

import (
	"math"
	"testing"

	"github.com/stretchr/testify/assert"
)

func TestPeriodHour(t *testing.T) {
	t.Parallel()

	h := blockTestHour
	tests := []struct {
		name       string
		start, end int64
		hour       int64
		ok         bool
	}{
		{"a whole hour", h, h + hourNs, hourOf(h), true},
		{"one nanosecond", h + 10, h + 11, hourOf(h), true},
		{"ending exactly on the hour", h + hourNs - 1, h + hourNs, hourOf(h), true},
		{"the hour before 1970", -hourNs, 0, -1, true},
		{"one nanosecond before 1970", -1, 0, -1, true},
		{"the lowest time", math.MinInt64, math.MinInt64 + 1, hourOf(math.MinInt64), true},
		{"the highest time", math.MaxInt64 - 1, math.MaxInt64, hourOf(math.MaxInt64 - 1), true},
		{"an empty period", h + 10, h + 10, 0, false},
		{"a reversed period", h + 10, h + 5, 0, false},
		{"ending one nanosecond into the next hour", h, h + hourNs + 1, 0, false},
		{"across two hours", h + 10, h + hourNs + 1, 0, false},
		{"across 1970", -1, 1, 0, false},
		{"the whole int64 range", math.MinInt64, math.MaxInt64, 0, false},
		{"empty at the int64 extremes", math.MaxInt64, math.MinInt64, 0, false},
	}
	for _, tt := range tests {
		hour, ok := periodHour(tt.start, tt.end)
		assert.Equal(t, tt.ok, ok, tt.name)
		assert.Equal(t, tt.hour, hour, tt.name)
	}
}

func TestHourBounds(t *testing.T) {
	t.Parallel()

	first, last := hourBounds(hourOf(blockTestHour))
	assert.Equal(t, [2]int64{blockTestHour, blockTestHour + hourNs - 1}, [2]int64{first, last})

	first, last = hourBounds(-1)
	assert.Equal(t, [2]int64{-hourNs, -1}, [2]int64{first, last}, "the hour before 1970")

	first, last = hourBounds(hourOf(math.MinInt64))
	assert.Equal(t, int64(math.MinInt64), first, "clamped")
	assert.Equal(t, hourOf(math.MinInt64), hourOf(last))
	assert.NotEqual(t, hourOf(math.MinInt64), hourOf(last+1))

	first, last = hourBounds(hourOf(math.MaxInt64))
	assert.Equal(t, int64(math.MaxInt64), last, "clamped")
	assert.Equal(t, hourOf(math.MaxInt64), hourOf(first))
	assert.NotEqual(t, hourOf(math.MaxInt64), hourOf(first-1))
}
