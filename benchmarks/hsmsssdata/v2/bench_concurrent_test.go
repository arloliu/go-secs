// Package v2 — see bench_concurrent_test.go's sibling bench_test.go for the
// single-connection round-trip benchmarks this file's harness helpers come
// from.
package v2

import (
	"context"
	"sync"
	"testing"

	"github.com/arloliu/go-secs/v2/hsms"
)

const (
	profileNumConns       = 4
	profileWorkersPerConn = 8
)

type profileConnPair struct {
	active  hsms.Connection
	passive hsms.Connection
}

// BenchmarkConnectionPool_ConcurrentRoundTrip drives profileNumConns
// active/passive HSMS-SS connection pairs, each with profileWorkersPerConn
// goroutines concurrently issuing synchronous SendDataMessage round trips on
// the SAME connection. This is safe today on three independent invariants in
// the connection implementation, not because of any locking added here:
// System Bytes are drawn from an atomic counter (hsms/sysbytes.go's
// sysBytesGen.next, doc'd safe for concurrent use), the reply registry is a
// concurrent xsync.MapOf whose route delivery is a non-blocking send
// (hsms/reply_registry.go), and the actual socket write is serialized under
// the epoch's writeMu inside writeFrame (hsms/connection_send.go) — so
// concurrent callers never interleave frames on the wire and never race on
// reply routing.
//
// This is profiling tooling (see `make profile-v2` in this module's
// Makefile), not a benchstat comparison target — there is no v1
// counterpart.
func BenchmarkConnectionPool_ConcurrentRoundTrip(b *testing.B) {
	ctx := context.Background()

	pairs := make([]profileConnPair, profileNumConns)
	for i := range pairs {
		port := freeLoopbackPort(b)

		passiveConn, err := newBenchConn(port, false)
		if err != nil {
			b.Fatal(err)
		}
		b.Cleanup(func() { _ = passiveConn.Close() })

		activeConn, err := newBenchConn(port, true)
		if err != nil {
			b.Fatal(err)
		}
		b.Cleanup(func() { _ = activeConn.Close() })

		passiveConn.AddDataMessageHandler(echoHandler)

		if err := passiveConn.Open(ctx, hsms.OpenBackground); err != nil {
			b.Fatal(err)
		}
		if err := activeConn.Open(ctx, hsms.OpenBackground); err != nil {
			b.Fatal(err)
		}

		waitSelected(b, passiveConn)
		waitSelected(b, activeConn)

		pairs[i] = profileConnPair{active: activeConn, passive: passiveConn}
	}

	const totalWorkers = profileNumConns * profileWorkersPerConn

	// b.N is expected to be an exact multiple of totalWorkers (32) — every
	// caller-supplied -benchtime=Nx in this plan is chosen that way — so this
	// division is exact and every worker performs the same number of round
	// trips. The floor-to-1 clamp only guards ad-hoc runs below 32x; it is
	// not meant to produce an exact accounting in that regime.
	iterationsPerWorker := max(b.N/totalWorkers, 1)

	errCh := make(chan error, totalWorkers)

	b.ReportAllocs()
	b.ResetTimer()

	var wg sync.WaitGroup
	for i := range totalWorkers {
		conn := pairs[i/profileWorkersPerConn].active
		wg.Add(1)
		go func(conn hsms.Connection) {
			defer wg.Done()
			for j := 0; j < iterationsPerWorker; j++ {
				item := structuredListItem()
				if _, err := conn.SendDataMessage(ctx, 1, 1, true, item); err != nil {
					select {
					case errCh <- err:
					default:
					}

					return
				}
			}
		}(conn)
	}
	wg.Wait()

	select {
	case err := <-errCh:
		b.Fatal(err)
	default:
	}
}
