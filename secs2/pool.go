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
	} else {
		return &ASCIIItem{}
	}
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
	} else {
		return &BooleanItem{}
	}
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
	} else {
		return &BinaryItem{}
	}
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
	} else {
		return &IntItem{}
	}
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
	} else {
		return &UintItem{}
	}
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
	} else {
		return &FloatItem{}
	}
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
	} else {
		return &ListItem{}
	}
}

func putListItem(item *ListItem) {
	if usePool {
		listItemPool.Put(item)
	}
}

var usePool = true

func IsUsePool() bool {
	return usePool
}

func UsePool(val bool) {
	usePool = val
}
