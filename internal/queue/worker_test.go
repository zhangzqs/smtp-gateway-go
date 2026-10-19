package queue

import (
	"testing"
	"time"

	"github.com/stretchr/testify/require"

	"github.com/zhangzqs/smtp-gateway-go/internal/config"
)

func TestRetryDelayBounds(t *testing.T) {
	t.Parallel()
	cfg := config.Worker{RetryInitial: time.Second, RetryMax: 4 * time.Second}
	for range 100 {
		first := backoff(cfg, 0)
		require.GreaterOrEqual(t, first, 500*time.Millisecond)
		require.LessOrEqual(t, first, 1500*time.Millisecond)
		later := backoff(cfg, 20)
		require.GreaterOrEqual(t, later, 2*time.Second)
		require.LessOrEqual(t, later, cfg.RetryMax)
	}
}
