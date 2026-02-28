package secs1integration

import (
	"sync"
	"testing"
	"time"

	"github.com/arloliu/go-secs/logger"
	"github.com/arloliu/go-secs/secs1"
	"github.com/stretchr/testify/require"
)

const testSessionID = 1

// TestConcurrentClose ensures that calling Close() from multiple goroutines
// does not cause a panic or a data race, thereby validating the CAS and loopCtx logic.
func TestConcurrentClose(t *testing.T) {
	require := require.New(t)
	port := getFreePort(t)

	ctx := t.Context()

	cfg, err := secs1.NewConnectionConfig("127.0.0.1", port,
		secs1.WithActive(),
		secs1.WithHostRole(),
		secs1.WithLogger(logger.GetLogger().With("test", "ConcurrentClose")),
	)
	require.NoError(err)

	secs1Conn, err := secs1.NewConnection(ctx, cfg)
	require.NoError(err)
	secs1Conn.AddSession(testSessionID)

	// Open connection in background, which will start the connectLoop
	go func() {
		_ = secs1Conn.Open(false)
	}()

	// Let openActive run
	time.Sleep(50 * time.Millisecond)

	var wg sync.WaitGroup
	// Fire 10 concurrent Close() calls
	for i := 0; i < 10; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			err := secs1Conn.Close()
			require.NoError(err, "Close() should not return an error, or at least handle it cleanly")
		}()
	}

	wg.Wait()
}
