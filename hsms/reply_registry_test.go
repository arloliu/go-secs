package hsms

import (
	"testing"
	"time"

	"github.com/stretchr/testify/require"
)

func TestReplyRegistry_RouteHit(t *testing.T) {
	r := newReplyRegistry()
	key := [4]byte{1, 2, 3, 4}
	ch := r.register(key)
	defer r.deregister(key)
	require.True(t, r.route(key, replyResult{msg: nil, err: nil}))
	select {
	case <-ch:
	default:
		t.Fatal("routed result not delivered to sender channel")
	}
}

func TestReplyRegistry_RouteMiss(t *testing.T) {
	r := newReplyRegistry()
	require.False(t, r.route([4]byte{9, 9, 9, 9}, replyResult{}), "unknown key must miss")
}

func TestReplyRegistry_LateReplyAfterDeregisterIsDropped(t *testing.T) {
	r := newReplyRegistry()
	key := [4]byte{5, 5, 5, 5}
	_ = r.register(key)
	r.deregister(key) // sender gave up (ctx/T3)
	require.False(t, r.route(key, replyResult{}), "late reply after deregister must drop, never panic")
	require.Equal(t, 0, r.len())
}

func TestReplyRegistry_RouteNeverBlocks(t *testing.T) {
	r := newReplyRegistry()
	key := [4]byte{7, 7, 7, 7}
	_ = r.register(key)                          // cap-1 channel, nobody draining
	require.True(t, r.route(key, replyResult{})) // fills buffer
	done := make(chan struct{})
	go func() { defer close(done); r.route(key, replyResult{}) }() // 2nd hits default, never blocks
	select {
	case <-done:
	case <-time.After(time.Second):
		t.Fatal("route must never block (non-blocking send)")
	}
}
