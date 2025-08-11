package secs2

import (
	"sync"
)

var (
	asciiItemPool  = sync.Pool{New: func() any { return &ASCIIItem{} }}
	jis8ItemPool   = sync.Pool{New: func() any { return &JIS8Item{} }}
	boolItemPool   = sync.Pool{New: func() any { return &BooleanItem{values: []bool{}} }}
	binaryItemPool = sync.Pool{New: func() any { return &BinaryItem{values: []byte{}} }}
	intItemPool    = sync.Pool{New: func() any { return &IntItem{values: []int64{}} }}
	uintItemPool   = sync.Pool{New: func() any { return &UintItem{values: []uint64{}} }}
	floatItemPool  = sync.Pool{New: func() any { return &FloatItem{values: []float64{}} }}
	listItemPool   = sync.Pool{New: func() any { return &ListItem{values: []Item{}} }}
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
		item.rawBytes = nil
		asciiItemPool.Put(item)
	}
}

func getJIS8Item() *JIS8Item {
	if usePool {
		item, _ := jis8ItemPool.Get().(*JIS8Item)
		if item == nil {
			return &JIS8Item{}
		}

		return item
	}

	return &JIS8Item{}
}

func putJIS8Item(item *JIS8Item) {
	if usePool {
		jis8ItemPool.Put(item)
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
		item.rawBytes = nil
		boolItemPool.Put(item)
	}
}

func getBinaryItem() *BinaryItem {
	if usePool {
		item, _ := binaryItemPool.Get().(*BinaryItem)
		if item == nil {
			return &BinaryItem{values: []byte{}}
		}

		return item
	}

	return &BinaryItem{values: []byte{}}
}

func putBinaryItem(item *BinaryItem) {
	if usePool {
		item.rawBytes = nil
		binaryItemPool.Put(item)
	}
}

func getIntItem() *IntItem {
	if usePool {
		item, _ := intItemPool.Get().(*IntItem)
		if item == nil {
			return &IntItem{values: []int64{}}
		}

		return item
	}

	return &IntItem{values: []int64{}}
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
			return &UintItem{values: []uint64{}}
		}

		return item
	}

	return &UintItem{values: []uint64{}}
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
			return &FloatItem{values: []float64{}}
		}

		return item
	}

	return &FloatItem{values: []float64{}}
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
			return &ListItem{values: []Item{}}
		}

		return item
	}

	return &ListItem{values: []Item{}}
}

func putListItem(item *ListItem) {
	if usePool {
		item.values = item.values[:0]
		item.itemErr = nil
		item.rawBytes = nil
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
