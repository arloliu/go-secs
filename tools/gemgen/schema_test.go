package main

import (
	"testing"

	"github.com/stretchr/testify/require"
	"gopkg.in/yaml.v3"
)

func TestStructureNodeKind(t *testing.T) {
	cases := []struct {
		name string
		yml  string
		want string
	}{
		{"leaf", "{item: MDLN}", "leaf"},
		{"opaque", "{type: opaque}", "opaque"},
		{"list", "{type: list, items: []}", "list"},
		{"repeat", "{type: list, repeat: svids, of: {item: SVID}}", "list"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			var n StructureNode
			require.NoError(t, yaml.Unmarshal([]byte(tc.yml), &n))
			require.Equal(t, tc.want, n.Kind())
		})
	}
}
