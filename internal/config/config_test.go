package config

import (
	"bytes"
	"crypto/rand"
	"encoding/base64"
	"encoding/hex"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
	"go.yaml.in/yaml/v3"
)

func TestLoad(t *testing.T) {
	key := make([]byte, 32)
	_, err := rand.Read(key)
	require.NoError(t, err)
	password := make([]byte, 16)
	_, err = rand.Read(password)
	require.NoError(t, err)
	path := filepath.Join(t.TempDir(), "config.yaml")
	data := fmt.Sprintf(`http:
  listen: "127.0.0.1:8080"
  basic_auth:
    username: manager
    password: %s
smtp:
  listen: "127.0.0.1:2525"
  hostname: localhost
  auth:
    username: client
    password: %s
  allow_insecure_auth: true
database:
  driver: sqlite
  dsn: %s
security:
  encryption_key: %s
worker:
  concurrency: 2
  retry_initial: 1s
`, hex.EncodeToString(password), hex.EncodeToString(password), filepath.Join(t.TempDir(), "gateway.db"), base64.StdEncoding.EncodeToString(key))
	require.NoError(t, os.WriteFile(path, []byte(data), 0o600))
	cfg, actualKey, err := Load(path)
	require.NoError(t, err)
	require.Equal(t, key, actualKey)
	require.Equal(t, 2, cfg.Worker.Concurrency)
	require.Equal(t, time.Second, cfg.Worker.RetryInitial)
	require.Equal(t, 2*time.Minute, cfg.Worker.LeaseDuration)
	require.Equal(t, int64(10<<20), cfg.Limits.MaxMessageBytes)
	require.Equal(t, "info", cfg.Logger.Level)
	require.NoError(t, os.WriteFile(path, []byte(data+"---\nlogger:\n  level: debug\n"), 0o600))
	_, _, err = Load(path)
	require.ErrorContains(t, err, "exactly one YAML document")
	require.NoError(t, os.WriteFile(path, []byte(data+"logger:\n  level: invalid\n"), 0o600))
	_, _, err = Load(path)
	require.ErrorContains(t, err, "logger.level")
	require.NoError(t, os.WriteFile(path, []byte(strings.Replace(data, "concurrency: 2", "concurrency: 0", 1)), 0o600))
	_, _, err = Load(path)
	require.ErrorContains(t, err, "invalid worker settings")
	require.NoError(t, os.WriteFile(path, []byte(strings.Replace(data, "concurrency: 2", "unknown_setting: 1", 1)), 0o600))
	_, _, err = Load(path)
	require.ErrorContains(t, err, "unknown worker setting")
}

func FuzzConfigDecode(f *testing.F) {
	f.Add([]byte("worker:\n  retry_initial: 1s\n  concurrency: 2\n"))
	f.Add([]byte("smtp:\n  tls:\n    mode: starttls\n"))
	f.Fuzz(func(t *testing.T, input []byte) {
		if len(input) > 64<<10 {
			t.Skip()
		}
		var cfg Config
		decoder := yaml.NewDecoder(bytes.NewReader(input))
		decoder.KnownFields(true)
		if decoder.Decode(&cfg) == nil {
			_ = cfg.Validate()
		}
	})
}
