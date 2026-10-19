package smtpserver

import (
	"context"
	"io"
	"testing"
	"time"

	"github.com/emersion/go-smtp"
	"github.com/rs/zerolog"
	"github.com/stretchr/testify/require"
)

func TestRunCancelledBeforeServe(t *testing.T) {
	server, err := New(t.Context(), Options{
		Addr:    "127.0.0.1:0",
		Backend: smtp.BackendFunc(func(*smtp.Conn) (smtp.Session, error) { return nil, io.EOF }),
		Logger:  zerolog.Nop(),
	})
	require.NoError(t, err)
	t.Cleanup(func() { _ = server.Close() })

	ctx, cancel := context.WithCancel(t.Context())
	cancel()
	done := make(chan error, 1)
	go func() { done <- server.Run(ctx) }()
	select {
	case err := <-done:
		require.NoError(t, err)
	case <-time.After(time.Second):
		t.Fatal("SMTP server did not stop after cancellation")
	}
}
