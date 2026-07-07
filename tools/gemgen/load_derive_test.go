package main

import (
	"testing"

	"github.com/stretchr/testify/require"
)

func TestDeriveBindingAndGoType(t *testing.T) {
	cases := []struct {
		name     string
		formats  []string
		binding  Binding
		goType   string
		hasGoTyp bool
	}{
		{"fixed ascii", []string{"A"}, BindingFixed, "string", true},
		{"fixed binary", []string{"B"}, BindingFixed, "byte", true},
		{"fixed bool", []string{"BOOLEAN"}, BindingFixed, "bool", true},
		{"list always open", []string{"L"}, BindingOpen, "", false},
		{"multi open", []string{"A", "U1", "U2", "U4", "U8"}, BindingOpen, "", false},
		{"signed wildcard open", []string{"I1", "I2", "I4", "I8"}, BindingOpen, "", false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			require.Equal(t, tc.binding, deriveBinding(tc.formats))
			gt, ok := deriveGoType(tc.formats)
			require.Equal(t, tc.hasGoTyp, ok)
			require.Equal(t, tc.goType, gt)
		})
	}
}
