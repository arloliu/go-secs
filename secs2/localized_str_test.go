package secs2

import (
	"reflect"
	"testing"
)

func TestLocalizedStrItem_Size(t *testing.T) {
	tests := []struct {
		name     string
		item     Item
		expected int
	}{
		{
			name:     "empty localized string",
			item:     NewUTF8StrItem(""),
			expected: 2, // 2 bytes for LSH
		},
		{
			name:     "short localized string",
			item:     NewUTF8StrItem("hello"),
			expected: 7, // 5 (length of "hello") + 2 (LSH)
		},
		{
			name:     "unicode localized string",
			item:     NewUTF8StrItem("你好"),
			expected: 8, // 6 (length of UTF-8 "你好") + 2 (LSH)
		},
		{
			name:     "custom LSH localized string",
			item:     NewLocalizedStrItem(LSHShiftJIS, "test"),
			expected: 6, // 4 + 2
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if size := tt.item.Size(); size != tt.expected {
				t.Errorf("LocalizedStrItem.Size() = %d, want %d", size, tt.expected)
			}
		})
	}
}

func TestLocalizedStrItem_ToBytes(t *testing.T) {
	tests := []struct {
		name     string
		item     Item
		expected []byte
	}{
		{
			name:     "empty localized string",
			item:     NewUTF8StrItem(""),
			expected: []byte{0111, 2, 0, 2}, // 0111 is octal format code 22 (bin: 01001001), 2 bytes length, LSH=2 (0x0002)
		},
		{
			name:     "short localized string",
			item:     NewUTF8StrItem("hi"),
			expected: []byte{0111, 4, 0, 2, 'h', 'i'}, // 4 bytes length, LSH=2, 'h', 'i'
		},
		{
			name:     "custom LSH localized string",
			item:     NewLocalizedStrItem(LSHASCII, "test"),
			expected: []byte{0111, 6, 0, 3, 't', 'e', 's', 't'}, // 6 bytes length, LSH=3 (ASCII), "test"
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := tt.item.ToBytes(); !reflect.DeepEqual(got, tt.expected) {
				t.Errorf("LocalizedStrItem.ToBytes() = %v, want %v", got, tt.expected)
			}
		})
	}
}

func TestLocalizedStrItem_ToSML(t *testing.T) {
	tests := []struct {
		name     string
		item     Item
		expected string
	}{
		{
			name:     "empty localized string",
			item:     NewUTF8StrItem(""),
			expected: `<W "">`,
		},
		{
			name:     "short localized string",
			item:     NewUTF8StrItem("hello world"),
			expected: `<W "hello world">`,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := tt.item.ToSML(); got != tt.expected {
				t.Errorf("LocalizedStrItem.ToSML() = %q, want %q", got, tt.expected)
			}
		})
	}
}

func TestLocalizedStrItem_ValuesAndType(t *testing.T) {
	item := NewUTF8StrItem("test_values")

	if val := item.Values(); !reflect.DeepEqual(val, []string{"test_values"}) {
		t.Errorf("LocalizedStrItem.Values() = %v, want %v", val, []string{"test_values"})
	}

	if item.Type() != LocalizedStrType {
		t.Errorf("LocalizedStrItem.Type() = %q, want %q", item.Type(), LocalizedStrType)
	}

	if !item.IsLocalizedStr() {
		t.Errorf("LocalizedStrItem.IsLocalizedStr() should be true")
	}

	val, err := item.ToLocalizedStr()
	if err != nil || val != "test_values" {
		t.Errorf("LocalizedStrItem.ToLocalizedStr() = %v, %v, want 'test_values', nil", val, err)
	}

	lsh, err := item.ToLocalizedStrHeader()
	if err != nil || lsh != LSHUTF8 {
		t.Errorf("LocalizedStrItem.ToLocalizedStrHeader() = %v, %v, want LSHUTF8 (2), nil", lsh, err)
	}
}

func TestLocalizedStrItem_SetValues(t *testing.T) {
	item := NewUTF8StrItem("initial")

	err := item.SetValues("updated")
	if err != nil {
		t.Fatalf("SetValues failed: %v", err)
	}

	if val, _ := item.ToLocalizedStr(); val != "updated" {
		t.Errorf("SetValues did not update the value, got %q", val)
	}

	err = item.SetValues(123)
	if err == nil {
		t.Errorf("SetValues should fail for non-string types")
	}

	err = item.SetValues("1", "2")
	if err == nil {
		t.Errorf("SetValues should fail for multiple values")
	}
}
