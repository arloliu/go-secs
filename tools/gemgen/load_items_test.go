package main

import (
	"testing"

	"github.com/stretchr/testify/require"
)

const goodItems = `
MDLN:
  formats: [A]
  binding: fixed
  goType: string
  description: Equipment model type.
  source: e5
CEID:
  formats: [A, U1, U2, U4, U8, I1, I2, I4, I8]
  binding: open
  description: Collected event ID.
  source: e5
`

func TestLoadItemsValid(t *testing.T) {
	items, err := LoadItems([]byte(goodItems))
	require.NoError(t, err)
	require.NoError(t, ValidateItems(items))
	require.Equal(t, BindingFixed, items["MDLN"].Binding)
	require.Equal(t, "string", items["MDLN"].GoType)
	require.Equal(t, BindingOpen, items["CEID"].Binding)
}

func TestValidateItemsRejectsBadFixed(t *testing.T) {
	cases := map[string]string{
		"fixed multi format":  "X:\n  formats: [A, B]\n  binding: fixed\n  goType: string\n  source: e5\n",
		"fixed no goType":     "X:\n  formats: [A]\n  binding: fixed\n  source: e5\n",
		"binding mismatch":    "X:\n  formats: [A]\n  binding: open\n  source: e5\n",
		"goType mismatch":     "X:\n  formats: [A]\n  binding: fixed\n  goType: int8\n  source: e5\n",
		"values on open item": "X:\n  formats: [A, B]\n  binding: open\n  source: e5\n  values:\n    - {name: Foo, value: 0}\n",
	}
	for name, yml := range cases {
		t.Run(name, func(t *testing.T) {
			items, err := LoadItems([]byte(yml))
			require.NoError(t, err)
			require.Error(t, ValidateItems(items))
		})
	}
}
