package util

// CloneSlice clones slice with cloneSize.
// This function will use src length as the clons size if cloneSize is 0.
func CloneSlice[T any](src []T, cloneSize int) []T {
	if cloneSize == 0 {
		cloneSize = len(src)
	}
	clone := make([]T, cloneSize)
	copy(clone, src)

	return clone
}

// AppendInt64Slice converts and appends a value to int64 slice if its underlying type is a supported integer type.
//
// Supported types:
//   - Signed integers: int, int8, int16, int32, int64
//   - Unsigned integers: uint, uint8, uint16, uint32
//
// It's important to note that this function assumes the unsigned integer values are within int64 range. It does not perform validation
// to check the overflow case.
//
// If the input slice's underlying type is not one of the supported types, the function will panic.
func AppendInt64Slice[T int | int8 | int16 | int32 | int64 | uint | uint8 | uint16 | uint32](target []int64, values []T) []int64 {
	target = append(target, make([]int64, len(values))...)
	varLen := len(values)
	targetLen := len(target)
	for i, v := range values {
		target[targetLen-varLen+i] = int64(v)
	}
	return target
}

// AppendUint64Slice appends the values of a slice to a uint64 slice after converting them to uint64.
//
// The function supports appending slices of the following types:
//   - Unsigned integers: uint, uint8, uint16, uint32, uint64
//   - Signed integers (>= 0): int, int8, int16, int32, int64
//
// It's important to note that this function assumes the signed integer values are non-negative. It does not perform validation
// to check for negative values. If a negative value is encountered in a signed integer slice, the behavior is undefined.
//
// If the input slice's underlying type is not one of the supported types, the function will panic.
func AppendUint64Slice[T int | int8 | int16 | int32 | int64 | uint | uint8 | uint16 | uint32 | uint64](target []uint64, values []T) []uint64 {
	target = append(target, make([]uint64, len(values))...)
	varLen := len(values)
	targetLen := len(target)
	for i, v := range values {
		target[targetLen-varLen+i] = uint64(v)
	}
	return target
}

// AppendFloat64Slice appends the values of a slice to a float64 slice, converting them to float64 if necessary.
//
// The function supports the following types for the input slice `values`:
//
//   - Floating-point numbers: `float32`, `float64`
//   - Integers: `int`, `int8`, `int16`, `int32`, `int64`, `uint`, `uint8`, `uint16`, `uint32`, `uint64`
//
// The function performs implicit type conversions, potentially resulting in precision loss for very large integer values.
//
// It's important to note that this function does not perform explicit range checks. If an input value exceeds the representable
// range of `float64`, the behavior is implementation-defined (likely resulting in +/- `math.Inf`).
//
// If any of the input values' underlying types are not supported, the function will panic.
func AppendFloat64Slice[T float32 | float64 | int | int8 | int16 | int32 | int64 | uint | uint8 | uint16 | uint32 | uint64](target []float64, values []T) []float64 {
	target = append(target, make([]float64, len(values))...)
	varLen := len(values)
	targetLen := len(target)
	for i, v := range values {
		target[targetLen-varLen+i] = float64(v)
	}
	return target
}
