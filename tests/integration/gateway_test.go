package integration

import (
	"bytes"
	"context"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strconv"
	"sync"
	"testing"
	"time"

	"github.com/emersion/go-sasl"
	"github.com/emersion/go-smtp"
	"github.com/stretchr/testify/require"

	"github.com/zhangzqs/smtp-gateway-go/internal/api"
	"github.com/zhangzqs/smtp-gateway-go/internal/config"
	"github.com/zhangzqs/smtp-gateway-go/internal/infra/database"
	"github.com/zhangzqs/smtp-gateway-go/internal/queue"
	"github.com/zhangzqs/smtp-gateway-go/internal/service"
	"github.com/zhangzqs/smtp-gateway-go/internal/smtpd"
	"github.com/zhangzqs/smtp-gateway-go/internal/store"
)

type upstream struct {
	mu       sync.Mutex
	attempts int
	message  []byte
}

func (u *upstream) NewSession(*smtp.Conn) (smtp.Session, error) {
	return &upstreamSession{upstream: u}, nil
}

type upstreamSession struct{ upstream *upstream }

func (*upstreamSession) Reset()                               {}
func (*upstreamSession) Logout() error                        { return nil }
func (*upstreamSession) Mail(string, *smtp.MailOptions) error { return nil }
func (*upstreamSession) Rcpt(string, *smtp.RcptOptions) error { return nil }
func (s *upstreamSession) Data(r io.Reader) error {
	body, err := io.ReadAll(r)
	if err != nil {
		return err
	}
	s.upstream.mu.Lock()
	defer s.upstream.mu.Unlock()
	s.upstream.attempts++
	if s.upstream.attempts == 1 {
		return &smtp.SMTPError{Code: 451, Message: "temporarily unavailable"}
	}
	s.upstream.message = body
	return nil
}

func TestSMTPSubmissionRetryAndAPI(t *testing.T) {
	key := make([]byte, 32)
	_, err := rand.Read(key)
	require.NoError(t, err)
	passwordBytes := make([]byte, 32)
	_, err = rand.Read(passwordBytes)
	require.NoError(t, err)
	httpPassword := hex.EncodeToString(passwordBytes[:16])
	smtpPassword := hex.EncodeToString(passwordBytes[16:])
	cfg := config.Config{}
	cfg.Database = config.Database{Driver: "sqlite", DSN: filepath.Join(t.TempDir(), "gateway.db")}
	cfg.HTTP.BasicAuth = config.Credentials{Username: "manager", Password: httpPassword}
	cfg.SMTP.Auth = config.Credentials{Username: "client", Password: smtpPassword}
	cfg.Limits = config.Limits{MaxMessageBytes: 1 << 20, MaxRecipients: 10, MaxConnections: 10, MaxQueueMessages: 10, MaxQueueBytes: 10 << 20}
	cfg.Worker = config.Worker{Concurrency: 1, AttemptTimeout: 5 * time.Second, LeaseDuration: 10 * time.Second, RetryInitial: 100 * time.Millisecond, RetryMax: time.Second, MaxAge: time.Minute, HistoryRetention: time.Hour}
	databaseResource, err := database.New(t.Context(), cfg.Database)
	require.NoError(t, err)
	db, err := store.New(t.Context(), databaseResource, cfg, key)
	require.NoError(t, err)
	t.Cleanup(func() { require.NoError(t, db.Close()) })
	managementAPI := &api.API{Service: service.New(db, cfg, func() bool { return true }), Auth: cfg.HTTP.BasicAuth}
	apiServer := httptest.NewServer(managementAPI.Handler())
	t.Cleanup(apiServer.Close)
	head, err := http.NewRequestWithContext(t.Context(), http.MethodHead, apiServer.URL+"/healthz", nil)
	require.NoError(t, err)
	headResponse, err := apiServer.Client().Do(head)
	require.NoError(t, err)
	require.Equal(t, http.StatusOK, headResponse.StatusCode)
	require.NoError(t, headResponse.Body.Close())

	request, err := http.NewRequestWithContext(t.Context(), http.MethodGet, apiServer.URL+"/v1/relay", nil)
	require.NoError(t, err)
	response, err := apiServer.Client().Do(request)
	require.NoError(t, err)
	require.Equal(t, http.StatusUnauthorized, response.StatusCode)
	require.NoError(t, response.Body.Close())
	request, err = http.NewRequestWithContext(t.Context(), http.MethodGet, apiServer.URL+"/v1/relay", nil)
	require.NoError(t, err)
	request.SetBasicAuth("client", smtpPassword)
	response, err = apiServer.Client().Do(request)
	require.NoError(t, err)
	require.Equal(t, http.StatusUnauthorized, response.StatusCode)
	require.NoError(t, response.Body.Close())
	request, err = http.NewRequestWithContext(t.Context(), http.MethodGet, apiServer.URL+"/v1/messages?status=unknown", nil)
	require.NoError(t, err)
	request.SetBasicAuth("manager", httpPassword)
	response, err = apiServer.Client().Do(request)
	require.NoError(t, err)
	require.Equal(t, http.StatusUnprocessableEntity, response.StatusCode)
	require.NoError(t, response.Body.Close())

	upstreamListener, err := (&net.ListenConfig{}).Listen(t.Context(), "tcp", "127.0.0.1:0")
	require.NoError(t, err)
	up := &upstream{}
	upServer := smtp.NewServer(up)
	go func() { _ = upServer.Serve(upstreamListener) }()
	t.Cleanup(func() { require.NoError(t, upServer.Close()) })
	_, portText, err := net.SplitHostPort(upstreamListener.Addr().String())
	require.NoError(t, err)
	port, err := strconv.Atoi(portText)
	require.NoError(t, err)
	payload, err := json.Marshal(map[string]any{"host": "127.0.0.1", "port": port, "tls": "none", "username": "", "from_address": "sender@example.com", "from_name": "测试通知"})
	require.NoError(t, err)
	request, err = http.NewRequestWithContext(t.Context(), http.MethodPut, apiServer.URL+"/v1/relay", bytes.NewReader(payload))
	require.NoError(t, err)
	request.SetBasicAuth("manager", httpPassword)
	response, err = apiServer.Client().Do(request)
	require.NoError(t, err)
	require.Equal(t, http.StatusOK, response.StatusCode)
	require.NoError(t, response.Body.Close())

	smtpListener, err := (&net.ListenConfig{}).Listen(t.Context(), "tcp", "127.0.0.1:0")
	require.NoError(t, err)
	smtpServer := smtp.NewServer(&smtpd.Backend{Store: db, Config: cfg})
	smtpServer.Domain = "localhost"
	smtpServer.AllowInsecureAuth = true
	smtpServer.MaxRecipients = cfg.Limits.MaxRecipients
	smtpServer.MaxMessageBytes = cfg.Limits.MaxMessageBytes
	go func() { _ = smtpServer.Serve(smtpListener) }()
	t.Cleanup(func() { require.NoError(t, smtpServer.Close()) })
	wrongClient, err := smtp.Dial(smtpListener.Addr().String())
	require.NoError(t, err)
	require.Error(t, wrongClient.Auth(sasl.NewPlainClient("", "manager", httpPassword)))
	require.NoError(t, wrongClient.Close())
	client, err := smtp.Dial(smtpListener.Addr().String())
	require.NoError(t, err)
	defer func() { _ = client.Close() }()
	require.NoError(t, client.Auth(sasl.NewPlainClient("", "client", smtpPassword)))
	require.NoError(t, client.Mail("original@example.com", nil))
	require.NoError(t, client.Rcpt("recipient@example.com", nil))
	data, err := client.Data()
	require.NoError(t, err)
	_, err = data.Write([]byte("From: original@example.com\r\nSubject: Hi\r\n\r\nbody\r\n"))
	require.NoError(t, err)
	require.NoError(t, data.Close())

	workerCtx, stop := context.WithCancel(context.Background())
	worker := queue.New(db, cfg.Worker)
	managementAPI.DeliveryDuration = worker.DeliveryDuration()
	worker.Start(workerCtx)
	t.Cleanup(func() { stop(); worker.Wait() })
	require.Eventually(t, func() bool {
		up.mu.Lock()
		defer up.mu.Unlock()
		return up.attempts >= 2 && len(up.message) > 0
	}, 5*time.Second, 20*time.Millisecond)
	up.mu.Lock()
	received := bytes.Clone(up.message)
	up.mu.Unlock()
	require.Contains(t, string(received), "sender@example.com")
	require.Contains(t, string(received), "body\r\n")
	items, err := db.List(t.Context(), store.Accepted, 10, 0)
	require.NoError(t, err)
	require.Len(t, items, 1)
	require.Equal(t, 2, items[0].Recipients[0].Attempts)

	request, err = http.NewRequestWithContext(t.Context(), http.MethodGet, apiServer.URL+"/metrics", nil)
	require.NoError(t, err)
	request.SetBasicAuth("manager", httpPassword)
	response, err = apiServer.Client().Do(request)
	require.NoError(t, err)
	require.Equal(t, http.StatusOK, response.StatusCode)
	metrics, err := io.ReadAll(response.Body)
	require.NoError(t, err)
	require.NoError(t, response.Body.Close())
	require.Contains(t, string(metrics), "smtp_gateway_accepted_recipients 1")
	require.Contains(t, string(metrics), "smtp_gateway_retries "+strconv.Itoa(items[0].Recipients[0].Attempts-1))
	require.Contains(t, string(metrics), "# TYPE smtp_gateway_accepted_recipients gauge")
	require.Contains(t, string(metrics), "# TYPE smtp_gateway_database_write_failures_total counter")
	require.Contains(t, string(metrics), "# TYPE smtp_gateway_delivery_attempt_seconds summary")
	require.Contains(t, string(metrics), "smtp_gateway_delivery_attempt_seconds_count 2")
	request, err = http.NewRequestWithContext(t.Context(), http.MethodPost, apiServer.URL+"/v1/messages/missing/retry", nil)
	require.NoError(t, err)
	request.SetBasicAuth("manager", httpPassword)
	response, err = apiServer.Client().Do(request)
	require.NoError(t, err)
	require.Equal(t, http.StatusNotFound, response.StatusCode)
	require.NoError(t, response.Body.Close())
}
