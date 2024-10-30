package secs2

import (
	"sync"
)

var (
	asciiItemPool  = sync.Pool{New: func() any { return &ASCIIItem{} }}
	boolItemPool   = sync.Pool{New: func() any { return &BooleanItem{} }}
	binaryItemPool = sync.Pool{New: func() any { return &BinaryItem{} }}
	intItemPool    = sync.Pool{New: func() any { return &IntItem{} }}
	uintItemPool   = sync.Pool{New: func() any { return &UintItem{} }}
	floatItemPool  = sync.Pool{New: func() any { return &FloatItem{} }}
	listItemPool   = sync.Pool{New: func() any { return &ListItem{} }}
)

func getASCIIItem() *ASCIIItem {
	if usePool {
		item, _ := asciiItemPool.Get().(*ASCIIItem)
		if item == nil {
			return &ASCIIItem{}
		}
		return item
	}

	return &ASCIIItem{}
}

func putASCIIItem(item *ASCIIItem) {
	if usePool {
		asciiItemPool.Put(item)
	}
}

func getBooleanItem() *BooleanItem {
	if usePool {
		item, _ := boolItemPool.Get().(*BooleanItem)
		if item == nil {
			return &BooleanItem{}
		}
		return item
	}

	return &BooleanItem{}
}

func putBooleanItem(item *BooleanItem) {
	if usePool {
		boolItemPool.Put(item)
	}
}

func getBinaryItem() *BinaryItem {
	if usePool {
		item, _ := binaryItemPool.Get().(*BinaryItem)
		if item == nil {
			return &BinaryItem{}
		}
		return item
	}

	return &BinaryItem{}
}

func putBinaryItem(item *BinaryItem) {
	if usePool {
		binaryItemPool.Put(item)
	}
}

func getIntItem() *IntItem {
	if usePool {
		item, _ := intItemPool.Get().(*IntItem)
		if item == nil {
			return &IntItem{}
		}
		return item
	}

	return &IntItem{}
}

func putIntItem(item *IntItem) {
	if usePool {
		intItemPool.Put(item)
	}
}

func getUintItem() *UintItem {
	if usePool {
		item, _ := uintItemPool.Get().(*UintItem)
		if item == nil {
			return &UintItem{}
		}
		return item
	}

	return &UintItem{}
}

func putUintItem(item *UintItem) {
	if usePool {
		uintItemPool.Put(item)
	}
}

func getFloatItem() *FloatItem {
	if usePool {
		item, _ := floatItemPool.Get().(*FloatItem)
		if item == nil {
			return &FloatItem{}
		}
		return item
	}

	return &FloatItem{}
}

func putFloatItem(item *FloatItem) {
	if usePool {
		floatItemPool.Put(item)
	}
}

func getListItem() *ListItem {
	if usePool {
		item, _ := listItemPool.Get().(*ListItem)
		if item == nil {
			return &ListItem{}
		}
		return item
	}

	return &ListItem{}
}

func putListItem(item *ListItem) {
	if usePool {
		listItemPool.Put(item)
	}
}

var usePool = true

// IsUsePool returns true if object pooling for SECS-II items is enabled, false otherwise.
func IsUsePool() bool {
	return usePool
}

// UsePool enables or disables object pooling for SECS-II items.
// When enabled, the package reuses item objects from a pool to reduce memory allocations.
// This can improve performance, especially when creating and discarding many items.
//
// Pooling is enabled by default.
func UsePool(val bool) {
	usePool = val
}
