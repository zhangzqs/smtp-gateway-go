//go:build integration

package store

import (
	"crypto/rand"
	"encoding/hex"
	"testing"

	"github.com/stretchr/testify/require"
	"github.com/testcontainers/testcontainers-go"
	"github.com/testcontainers/testcontainers-go/modules/postgres"

	"github.com/zhangzqs/smtp-gateway-go/internal/config"
)

func TestPostgresQueueContract(t *testing.T) {
	passwordBytes := make([]byte, 16)
	_, err := rand.Read(passwordBytes)
	require.NoError(t, err)
	container, err := postgres.Run(t.Context(), "postgres:16.6-alpine3.20",
		postgres.WithDatabase("gateway"),
		postgres.WithUsername("gateway"),
		postgres.WithPassword(hex.EncodeToString(passwordBytes)),
		postgres.BasicWaitStrategies(),
	)
	require.NoError(t, err)
	t.Cleanup(func() { require.NoError(t, testcontainers.TerminateContainer(container)) })
	dsn, err := container.ConnectionString(t.Context(), "sslmode=disable")
	require.NoError(t, err)
	key := make([]byte, 32)
	_, err = rand.Read(key)
	require.NoError(t, err)
	cfg := config.Config{Database: config.Database{Driver: "postgres", DSN: dsn}}
	s := openStore(t, cfg, key)
	t.Cleanup(func() { require.NoError(t, s.Close()) })
	runQueueContract(t, s, cfg, key)
}
