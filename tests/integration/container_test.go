//go:build integration

package integration

import (
	"bytes"
	"context"
	"crypto/rand"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"net"
	"net/http"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/emersion/go-sasl"
	"github.com/emersion/go-smtp"
	"github.com/google/uuid"
	"github.com/moby/moby/api/types/container"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"github.com/testcontainers/testcontainers-go"
	"github.com/testcontainers/testcontainers-go/modules/mailpit"
	"github.com/testcontainers/testcontainers-go/modules/postgres"
	"github.com/testcontainers/testcontainers-go/network"
	"github.com/testcontainers/testcontainers-go/wait"
)

func randomHex(t *testing.T) string {
	t.Helper()
	value := make([]byte, 16)
	_, err := rand.Read(value)
	require.NoError(t, err)
	return hex.EncodeToString(value)
}

func gatewayEndpoint(t *testing.T, container testcontainers.Container, port string) string {
	t.Helper()
	host, err := container.Host(t.Context())
	require.NoError(t, err)
	mapped, err := container.MappedPort(t.Context(), port)
	require.NoError(t, err)
	return net.JoinHostPort(host, mapped.Port())
}

func submitSMTP(t *testing.T, endpoint, password, subject string) {
	t.Helper()
	client, err := smtp.Dial(endpoint)
	require.NoError(t, err)
	defer func() { _ = client.Close() }()
	require.NoError(t, client.Auth(sasl.NewPlainClient("", "client", password)))
	require.NoError(t, client.Mail("original@example.com", nil))
	require.NoError(t, client.Rcpt("a@example.com", nil))
	require.NoError(t, client.Rcpt("b@example.com", nil))
	data, err := client.Data()
	require.NoError(t, err)
	_, err = data.Write([]byte("From: original@example.com\r\nSubject: " + subject + "\r\n\r\nbody\r\n"))
	require.NoError(t, err)
	require.NoError(t, data.Close())
}

func TestContainerRestartRecovery(t *testing.T) {
	nw, err := network.New(t.Context())
	require.NoError(t, err)
	t.Cleanup(func() { require.NoError(t, nw.Remove(context.WithoutCancel(t.Context()))) })
	pgPassword := randomHex(t)
	pg, err := postgres.Run(t.Context(), "postgres:16.6-alpine3.20",
		postgres.WithDatabase("gateway"), postgres.WithUsername("gateway"), postgres.WithPassword(pgPassword),
		postgres.BasicWaitStrategies(), network.WithNetwork([]string{"postgres"}, nw),
	)
	require.NoError(t, err)
	t.Cleanup(func() { require.NoError(t, testcontainers.TerminateContainer(pg)) })
	mp, err := mailpit.Run(t.Context(), "axllent/mailpit:v1.27.9", network.WithNetwork([]string{"mailpit"}, nw))
	require.NoError(t, err)
	t.Cleanup(func() { require.NoError(t, testcontainers.TerminateContainer(mp)) })

	key := make([]byte, 32)
	_, err = rand.Read(key)
	require.NoError(t, err)
	httpPassword := randomHex(t)
	smtpPassword := randomHex(t)
	configPath := filepath.Join(t.TempDir(), "config.yaml")
	configText := fmt.Sprintf(`http:
  listen: "0.0.0.0:8080"
  basic_auth: {username: manager, password: %q}
smtp:
  listen: "0.0.0.0:2525"
  hostname: gateway
  auth: {username: client, password: %q}
  allow_insecure_auth: true
database:
  driver: postgres
  dsn: "host=postgres port=5432 user=gateway password=%s dbname=gateway sslmode=disable"
security:
  encryption_key: %q
worker:
  concurrency: 1
  attempt_timeout: 5s
  lease_duration: 10s
  retry_initial: 1s
  retry_max: 2s
  max_age: 1m
  history_retention: 1h
`, httpPassword, smtpPassword, pgPassword, base64.StdEncoding.EncodeToString(key))
	require.NoError(t, os.WriteFile(configPath, []byte(configText), 0o600))
	root, err := filepath.Abs(filepath.Join("..", ".."))
	require.NoError(t, err)
	tag := uuid.NewString()
	dockerfile := testcontainers.FromDockerfile{Context: root, Repo: "smtp-gateway-go-test", Tag: tag}
	request := testcontainers.ContainerRequest{
		FromDockerfile: dockerfile,
		ExposedPorts:   []string{"8080/tcp", "2525/tcp"},
		WaitingFor:     wait.ForHTTP("/readyz").WithPort("8080/tcp").WithStartupTimeout(2 * time.Minute),
		Networks:       []string{nw.Name},
		Files:          []testcontainers.ContainerFile{{HostFilePath: configPath, ContainerFilePath: "/etc/smtp-gateway/config.yaml", FileMode: 0o644}},
	}
	first, err := testcontainers.GenericContainer(t.Context(), testcontainers.GenericContainerRequest{ContainerRequest: request, Started: true})
	require.NoError(t, err)
	t.Cleanup(func() { require.NoError(t, testcontainers.TerminateContainer(first)) })
	baseURL := "http://" + gatewayEndpoint(t, first, "8080/tcp")
	payload := []byte(`{"host":"mailpit","port":1025,"tls":"none","username":"","from_address":"sender@example.com","from_name":"测试通知"}`)
	req, err := http.NewRequestWithContext(t.Context(), http.MethodPut, baseURL+"/v1/relay", bytes.NewReader(payload))
	require.NoError(t, err)
	req.SetBasicAuth("manager", httpPassword)
	resp, err := http.DefaultClient.Do(req)
	require.NoError(t, err)
	require.Equal(t, http.StatusOK, resp.StatusCode)
	require.NoError(t, resp.Body.Close())

	zero := time.Duration(0)
	require.NoError(t, mp.Stop(t.Context(), &zero))
	submitSMTP(t, gatewayEndpoint(t, first, "2525/tcp"), smtpPassword, "restart recovery")

	require.NoError(t, first.Stop(t.Context(), &zero))
	request.FromDockerfile = testcontainers.FromDockerfile{}
	request.Image = "smtp-gateway-go-test:" + tag
	second, err := testcontainers.GenericContainer(t.Context(), testcontainers.GenericContainerRequest{ContainerRequest: request, Started: true})
	require.NoError(t, err)
	t.Cleanup(func() { require.NoError(t, testcontainers.TerminateContainer(second)) })
	require.NoError(t, mp.Start(t.Context()))
	mailpitURL, err := mp.HTTPURL(t.Context())
	require.NoError(t, err)
	require.Eventually(t, func() bool {
		request, err := http.NewRequestWithContext(t.Context(), http.MethodGet, mailpitURL+"/api/v1/messages", nil)
		if err != nil {
			return false
		}
		response, err := http.DefaultClient.Do(request)
		if err != nil {
			return false
		}
		defer func() { _ = response.Body.Close() }()
		if response.StatusCode != http.StatusOK {
			return false
		}
		var result struct {
			Total int `json:"total"`
		}
		return json.NewDecoder(io.LimitReader(response.Body, 1<<20)).Decode(&result) == nil && result.Total == 1
	}, 30*time.Second, 200*time.Millisecond)
	messageRequest, err := http.NewRequestWithContext(t.Context(), http.MethodGet, mailpitURL+"/api/v1/messages", nil)
	require.NoError(t, err)
	messageResponse, err := http.DefaultClient.Do(messageRequest)
	require.NoError(t, err)
	var captured struct {
		Messages []struct {
			ID      string `json:"ID"`
			Subject string `json:"Subject"`
			From    struct {
				Address string `json:"Address"`
			} `json:"From"`
		} `json:"messages"`
	}
	require.NoError(t, json.NewDecoder(messageResponse.Body).Decode(&captured))
	require.NoError(t, messageResponse.Body.Close())
	require.Len(t, captured.Messages, 1)
	require.Equal(t, "restart recovery", captured.Messages[0].Subject)
	require.Equal(t, "sender@example.com", captured.Messages[0].From.Address)
	messageRequest, err = http.NewRequestWithContext(t.Context(), http.MethodGet, mailpitURL+"/api/v1/message/"+captured.Messages[0].ID, nil)
	require.NoError(t, err)
	messageResponse, err = http.DefaultClient.Do(messageRequest)
	require.NoError(t, err)
	var detail struct {
		Text string `json:"Text"`
	}
	require.NoError(t, json.NewDecoder(messageResponse.Body).Decode(&detail))
	require.NoError(t, messageResponse.Body.Close())
	require.Contains(t, detail.Text, "body")

	statusURL := "http://" + gatewayEndpoint(t, second, "8080/tcp") + "/v1/messages?status=accepted_by_upstream"
	require.Eventually(t, func() bool {
		request, err := http.NewRequestWithContext(t.Context(), http.MethodGet, statusURL, nil)
		if err != nil {
			return false
		}
		request.SetBasicAuth("manager", httpPassword)
		response, err := http.DefaultClient.Do(request)
		if err != nil {
			return false
		}
		defer func() { _ = response.Body.Close() }()
		var listing struct {
			Messages []struct {
				Recipients []struct {
					Status string `json:"status"`
				} `json:"recipients"`
			} `json:"messages"`
		}
		if response.StatusCode != http.StatusOK || json.NewDecoder(response.Body).Decode(&listing) != nil || len(listing.Messages) != 1 || len(listing.Messages[0].Recipients) != 2 {
			return false
		}
		for _, recipient := range listing.Messages[0].Recipients {
			if recipient.Status != "accepted_by_upstream" {
				return false
			}
		}
		return true
	}, 30*time.Second, 200*time.Millisecond)

	dataDir := filepath.Join(t.TempDir(), "data")
	require.NoError(t, os.Mkdir(dataDir, 0o700))
	require.NoError(t, os.Chmod(dataDir, 0o777))
	sqliteConfigPath := filepath.Join(t.TempDir(), "sqlite.yaml")
	sqliteConfig := fmt.Sprintf(`http:
  listen: "0.0.0.0:8080"
  basic_auth: {username: manager, password: %q}
smtp:
  listen: "0.0.0.0:2525"
  hostname: gateway
  auth: {username: client, password: %q}
  allow_insecure_auth: true
database:
  driver: sqlite
  dsn: /data/gateway.db
security:
  encryption_key: %q
worker:
  concurrency: 1
  attempt_timeout: 5s
  lease_duration: 10s
  retry_initial: 1s
  retry_max: 2s
  max_age: 1m
  history_retention: 1h
relay:
  host: mailpit
  port: 1025
  tls: none
  from_address: sqlite@example.com
`, httpPassword, smtpPassword, base64.StdEncoding.EncodeToString(key))
	require.NoError(t, os.WriteFile(sqliteConfigPath, []byte(sqliteConfig), 0o600))
	require.NoError(t, mp.Stop(t.Context(), &zero))
	sqliteRequest := request
	sqliteRequest.Files = []testcontainers.ContainerFile{{HostFilePath: sqliteConfigPath, ContainerFilePath: "/etc/smtp-gateway/config.yaml", FileMode: 0o644}}
	sqliteRequest.HostConfigModifier = func(hostConfig *container.HostConfig) {
		hostConfig.Binds = append(hostConfig.Binds, dataDir+":/data")
	}
	sqliteFirst, err := testcontainers.GenericContainer(t.Context(), testcontainers.GenericContainerRequest{ContainerRequest: sqliteRequest, Started: true})
	require.NoError(t, err)
	t.Cleanup(func() { require.NoError(t, testcontainers.TerminateContainer(sqliteFirst)) })
	sqliteURL := "http://" + gatewayEndpoint(t, sqliteFirst, "8080/tcp") + "/v1/relay"
	req, err = http.NewRequestWithContext(t.Context(), http.MethodGet, sqliteURL, nil)
	require.NoError(t, err)
	req.SetBasicAuth("manager", httpPassword)
	resp, err = http.DefaultClient.Do(req)
	require.NoError(t, err)
	require.Equal(t, http.StatusOK, resp.StatusCode)
	var fileRelay struct {
		ReadOnly    bool   `json:"read_only"`
		FromAddress string `json:"from_address"`
	}
	require.NoError(t, json.NewDecoder(resp.Body).Decode(&fileRelay))
	require.NoError(t, resp.Body.Close())
	require.True(t, fileRelay.ReadOnly)
	require.Equal(t, "sqlite@example.com", fileRelay.FromAddress)
	req, err = http.NewRequestWithContext(t.Context(), http.MethodPut, sqliteURL, bytes.NewReader(payload))
	require.NoError(t, err)
	req.SetBasicAuth("manager", httpPassword)
	resp, err = http.DefaultClient.Do(req)
	require.NoError(t, err)
	require.Equal(t, http.StatusForbidden, resp.StatusCode)
	require.NoError(t, resp.Body.Close())
	submitSMTP(t, gatewayEndpoint(t, sqliteFirst, "2525/tcp"), smtpPassword, "sqlite recovery")
	require.NoError(t, sqliteFirst.Stop(t.Context(), &zero))
	sqliteSecond, err := testcontainers.GenericContainer(t.Context(), testcontainers.GenericContainerRequest{ContainerRequest: sqliteRequest, Started: true})
	require.NoError(t, err)
	t.Cleanup(func() { require.NoError(t, testcontainers.TerminateContainer(sqliteSecond)) })
	require.NoError(t, mp.Start(t.Context()))
	mailpitURL, err = mp.HTTPURL(t.Context())
	require.NoError(t, err)
	delivered := assert.Eventually(t, func() bool {
		request, err := http.NewRequestWithContext(t.Context(), http.MethodGet, mailpitURL+"/api/v1/messages", nil)
		if err != nil {
			return false
		}
		response, err := http.DefaultClient.Do(request)
		if err != nil {
			return false
		}
		defer func() { _ = response.Body.Close() }()
		var messages struct {
			Messages []struct {
				Subject string `json:"Subject"`
			} `json:"messages"`
		}
		if response.StatusCode != http.StatusOK || json.NewDecoder(io.LimitReader(response.Body, 1<<20)).Decode(&messages) != nil {
			return false
		}
		for _, message := range messages.Messages {
			if message.Subject == "sqlite recovery" {
				return true
			}
		}
		return false
	}, 30*time.Second, 200*time.Millisecond)
	if !delivered {
		url := "http://" + gatewayEndpoint(t, sqliteSecond, "8080/tcp") + "/v1/messages"
		req, err := http.NewRequestWithContext(t.Context(), http.MethodGet, url, nil)
		require.NoError(t, err)
		req.SetBasicAuth("manager", httpPassword)
		resp, err := http.DefaultClient.Do(req)
		require.NoError(t, err)
		body, err := io.ReadAll(resp.Body)
		require.NoError(t, err)
		require.NoError(t, resp.Body.Close())
		t.Logf("SQLite queue status %d: %s", resp.StatusCode, body)
		logs, err := sqliteSecond.Logs(context.WithoutCancel(t.Context()))
		require.NoError(t, err)
		output, err := io.ReadAll(logs)
		require.NoError(t, err)
		require.NoError(t, logs.Close())
		t.Logf("SQLite gateway logs: %s", output)
	}
}
