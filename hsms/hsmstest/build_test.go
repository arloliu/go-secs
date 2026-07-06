package hsmstest_test

import (
	"testing"

	"github.com/arloliu/go-secs/v2/hsms"
	"github.com/arloliu/go-secs/v2/hsms/hsmstest"
	"github.com/stretchr/testify/require"
)

func TestRequireBuild_ReturnsBuiltMessage(t *testing.T) {
	msg := hsms.NewEmptyDataMessage()
	built := hsmstest.RequireBuild(t, msg.Derive().WithStream(1).WithFunction(1))
	require.Equal(t, uint8(1), built.Stream())
	require.Equal(t, uint8(1), built.Function())
}
