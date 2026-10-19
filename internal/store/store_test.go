package store

import (
	"crypto/rand"
	"encoding/hex"
	"path/filepath"
	"sync"
	"testing"
	"time"

	"github.com/stretchr/testify/require"

	"github.com/zhangzqs/smtp-gateway-go/internal/config"
	"github.com/zhangzqs/smtp-gateway-go/internal/infra/database"
)

func openStore(t *testing.T, cfg config.Config, key []byte) *Store {
	t.Helper()
	resource, err := database.New(t.Context(), cfg.Database)
	require.NoError(t, err)
	s, err := New(t.Context(), resource, cfg, key)
	require.NoError(t, err)
	return s
}

func testStore(t *testing.T) (*Store, config.Config, []byte) {
	t.Helper()
	key := make([]byte, 32)
	_, err := rand.Read(key)
	require.NoError(t, err)
	cfg := config.Config{Database: config.Database{Driver: "sqlite", DSN: filepath.Join(t.TempDir(), "gateway.db")}}
	s := openStore(t, cfg, key)
	t.Cleanup(func() { require.NoError(t, s.Close()) })
	return s, cfg, key
}

func TestRelayPasswordRules(t *testing.T) {
	s, _, _ := testStore(t)
	relay := config.Relay{Host: "localhost", Port: 2525, TLS: "none", Username: "upstream", FromAddress: "sender@example.com"}
	_, err := s.SaveRelay(t.Context(), relay, nil)
	require.ErrorIs(t, err, ErrValidation)
	passwordBytes := make([]byte, 16)
	_, err = rand.Read(passwordBytes)
	require.NoError(t, err)
	password := hex.EncodeToString(passwordBytes)
	view, err := s.SaveRelay(t.Context(), relay, &password)
	require.NoError(t, err)
	require.True(t, view.HasPassword)
	relay.FromName = "测试"
	_, err = s.SaveRelay(t.Context(), relay, nil)
	require.NoError(t, err)
	actual, _, err := s.Relay(t.Context())
	require.NoError(t, err)
	require.Equal(t, password, actual.Password)
	relay.Host = "other.example.com"
	_, err = s.SaveRelay(t.Context(), relay, nil)
	require.ErrorIs(t, err, ErrValidation)
	relay.Host = "localhost"
	relay.Username = ""
	_, err = s.SaveRelay(t.Context(), relay, nil)
	require.NoError(t, err)
	actual, view, err = s.Relay(t.Context())
	require.NoError(t, err)
	require.Empty(t, actual.Password)
	require.False(t, view.HasPassword)
}

func TestFileRelayOverridesDatabaseWithoutDeletingIt(t *testing.T) {
	s, cfg, key := testStore(t)
	databaseRelay := config.Relay{Host: "database.example.com", Port: 25, TLS: "none", FromAddress: "db@example.com"}
	_, err := s.SaveRelay(t.Context(), databaseRelay, nil)
	require.NoError(t, err)
	require.NoError(t, s.Close())
	fileRelay := config.Relay{Host: "file.example.com", Port: 25, TLS: "none", FromAddress: "file@example.com"}
	cfg.Relay = &fileRelay
	fileManaged := openStore(t, cfg, key)
	actual, view, err := fileManaged.Relay(t.Context())
	require.NoError(t, err)
	require.Equal(t, fileRelay, actual)
	require.True(t, view.ReadOnly)
	_, err = fileManaged.SaveRelay(t.Context(), databaseRelay, nil)
	require.ErrorIs(t, err, ErrReadOnly)
	require.NoError(t, fileManaged.Close())
	cfg.Relay = nil
	restored := openStore(t, cfg, key)
	t.Cleanup(func() { _ = restored.Close() })
	actual, view, err = restored.Relay(t.Context())
	require.NoError(t, err)
	require.Equal(t, databaseRelay, actual)
	require.False(t, view.ReadOnly)
}

func TestQueuePersistenceAndLease(t *testing.T) {
	s, cfg, key := testStore(t)
	runQueueContract(t, s, cfg, key)
}

func runQueueContract(t *testing.T, s *Store, cfg config.Config, key []byte) {
	t.Helper()
	limits := config.Limits{MaxMessageBytes: 1024, MaxQueueMessages: 1, MaxQueueBytes: 1024}
	raw := []byte("From: old@example.com\r\nSubject: Hi\r\n\r\nbody")
	type enqueueResult struct {
		id  string
		err error
	}
	resultsCh := make(chan enqueueResult, 10)
	var wg sync.WaitGroup
	for range 10 {
		wg.Go(func() {
			id, err := s.Enqueue(t.Context(), "old@example.com", []string{"a@example.com", "b@example.com"}, raw, limits, time.Hour)
			resultsCh <- enqueueResult{id, err}
		})
	}
	wg.Wait()
	close(resultsCh)
	var id string
	for result := range resultsCh {
		if result.err == nil {
			require.Empty(t, id)
			id = result.id
		} else {
			require.ErrorIs(t, result.err, ErrQueueFull)
		}
	}
	require.NotEmpty(t, id)
	var count int64
	require.NoError(t, s.DB.Model(&Message{}).Count(&count).Error)
	require.EqualValues(t, 1, count)
	claim, err := s.Claim(t.Context(), time.Minute)
	require.NoError(t, err)
	require.NotNil(t, claim)
	require.Equal(t, id, claim.ID)
	require.Len(t, claim.Recipients, 2)
	second, err := s.Claim(t.Context(), time.Minute)
	require.ErrorIs(t, err, ErrNoClaim)
	require.Nil(t, second)
	stale := *claim
	stale.ClaimToken = "stale"
	err = s.Complete(t.Context(), &stale, nil)
	require.ErrorIs(t, err, ErrConflict)
	results := []Result{{ID: claim.Recipients[0].ID, Status: Accepted}, {ID: claim.Recipients[1].ID, Status: Failed, Error: "rcpt_rejected"}}
	require.NoError(t, s.Complete(t.Context(), claim, results))
	require.ErrorIs(t, s.Complete(t.Context(), claim, results), ErrConflict)
	require.NoError(t, s.Retry(t.Context(), id, time.Hour, 7*24*time.Hour))
	items, err := s.List(t.Context(), Pending, 10, 0)
	require.NoError(t, err)
	require.Len(t, items, 1)
	require.Equal(t, Accepted, items[0].Recipients[0].Status)
	require.Equal(t, Pending, items[0].Recipients[1].Status)
	// A second process can open the same durable queue after a restart.
	require.NoError(t, s.Close())
	reopened := openStore(t, cfg, key)
	t.Cleanup(func() { _ = reopened.Close() })
	items, err = reopened.List(t.Context(), "", 10, 0)
	require.NoError(t, err)
	require.Len(t, items, 1)
	require.Equal(t, id, items[0].ID)
	_, err = reopened.Claim(t.Context(), time.Minute)
	require.NotErrorIs(t, err, ErrConflict)
}
