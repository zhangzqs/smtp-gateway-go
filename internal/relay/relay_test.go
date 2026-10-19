package relay

import (
	"bufio"
	"bytes"
	"context"
	"io"
	"net"
	"net/mail"
	"strconv"
	"testing"
	"time"

	"github.com/emersion/go-message/textproto"
	"github.com/emersion/go-smtp"
	"github.com/stretchr/testify/require"

	"github.com/zhangzqs/smtp-gateway-go/internal/config"
	"github.com/zhangzqs/smtp-gateway-go/internal/store"
)

func TestRewritePreservesBody(t *testing.T) {
	originalBody := []byte("--boundary\r\nContent-Type: text/plain\r\n\r\nHello\r\n--boundary--\r\n")
	raw := append([]byte("From: old@example.com\r\nFrom: duplicate@example.com\r\nSender: other@example.com\r\nReturn-Path: <old@example.com>\r\nSubject: Keep me\r\nContent-Type: multipart/mixed; boundary=boundary\r\n\r\n"), originalBody...)
	settings := config.Relay{Host: "localhost", Port: 2525, TLS: "none", FromAddress: "new@example.com", FromName: "中文名称"}
	out, err := Rewrite(raw, settings)
	require.NoError(t, err)
	parts := bytes.SplitN(out, []byte("\r\n\r\n"), 2)
	require.Len(t, parts, 2)
	require.Equal(t, originalBody, parts[1])
	require.Equal(t, 1, bytes.Count(parts[0], []byte("From:")))
	require.NotContains(t, string(parts[0]), "Sender:")
	require.NotContains(t, string(parts[0]), "Return-Path:")
	message, err := mail.ReadMessage(bytes.NewReader(out))
	require.NoError(t, err)
	addresses, err := message.Header.AddressList("From")
	require.NoError(t, err)
	require.Len(t, addresses, 1)
	require.Equal(t, "中文名称", addresses[0].Name)
	require.Equal(t, "new@example.com", addresses[0].Address)
}

func FuzzRewrite(f *testing.F) {
	f.Add([]byte("From: old@example.com\r\nSubject: test\r\n\r\nbody"))
	f.Add([]byte("invalid message"))
	f.Fuzz(func(t *testing.T, raw []byte) {
		if len(raw) > 64<<10 {
			t.Skip()
		}
		settings := config.Relay{Host: "localhost", Port: 2525, TLS: "none", FromAddress: "sender@example.com"}
		out, err := Rewrite(raw, settings)
		if err != nil {
			return
		}
		originalReader := bufio.NewReader(bytes.NewReader(raw))
		_, err = textproto.ReadHeader(originalReader)
		require.NoError(t, err)
		updatedReader := bufio.NewReader(bytes.NewReader(out))
		updatedHeader, err := textproto.ReadHeader(updatedReader)
		require.NoError(t, err)
		address, err := mail.ParseAddress(updatedHeader.Get("From"))
		require.NoError(t, err)
		require.Equal(t, "sender@example.com", address.Address)
		originalBody, err := io.ReadAll(originalReader)
		require.NoError(t, err)
		updatedBody, err := io.ReadAll(updatedReader)
		require.NoError(t, err)
		require.Equal(t, originalBody, updatedBody)
	})
}

type partialBackend struct{ received chan []byte }

func (b *partialBackend) NewSession(*smtp.Conn) (smtp.Session, error) {
	return &partialSession{backend: b}, nil
}

type partialSession struct{ backend *partialBackend }

func (*partialSession) Reset()                               {}
func (*partialSession) Logout() error                        { return nil }
func (*partialSession) Mail(string, *smtp.MailOptions) error { return nil }
func (*partialSession) Rcpt(to string, _ *smtp.RcptOptions) error {
	switch to {
	case "later@example.com":
		return &smtp.SMTPError{Code: 450, Message: "try later"}
	case "bad@example.com":
		return &smtp.SMTPError{Code: 550, Message: "unknown user"}
	default:
		return nil
	}
}

func (s *partialSession) Data(r io.Reader) error {
	data, err := io.ReadAll(r)
	if err == nil {
		s.backend.received <- data
	}
	return err
}

func TestDeliverPartialRecipients(t *testing.T) {
	listener, err := (&net.ListenConfig{}).Listen(t.Context(), "tcp", "127.0.0.1:0")
	require.NoError(t, err)
	backend := &partialBackend{received: make(chan []byte, 1)}
	server := smtp.NewServer(backend)
	go func() { _ = server.Serve(listener) }()
	t.Cleanup(func() { require.NoError(t, server.Close()) })
	_, portText, err := net.SplitHostPort(listener.Addr().String())
	require.NoError(t, err)
	port, err := strconv.Atoi(portText)
	require.NoError(t, err)
	settings := config.Relay{Host: "127.0.0.1", Port: port, TLS: "none", FromAddress: "sender@example.com"}
	claim := &store.Claimed{Recipients: []store.Recipient{
		{ID: "a", Address: "ok@example.com"},
		{ID: "b", Address: "later@example.com"},
		{ID: "c", Address: "bad@example.com"},
	}}
	claim.Raw = []byte("From: original@example.com\r\nSubject: mixed\r\n\r\nbody")
	ctx, cancel := context.WithTimeout(t.Context(), 5*time.Second)
	defer cancel()
	outcomes := Deliver(ctx, settings, claim)
	statuses := map[string]store.Status{}
	for _, outcome := range outcomes {
		statuses[outcome.ID] = outcome.Status
	}
	require.Equal(t, map[string]store.Status{"a": store.AcceptedByUpstreamStatus, "b": store.PendingStatus, "c": store.FailedStatus}, statuses)
	select {
	case body := <-backend.received:
		require.Contains(t, string(body), "sender@example.com")
	case <-ctx.Done():
		t.Fatal("upstream did not receive the accepted recipient")
	}
}
