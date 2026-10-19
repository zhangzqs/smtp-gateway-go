package smtpserver

import (
	"context"
	"errors"
	stdlog "log"
	"time"

	"github.com/emersion/go-smtp"
	"github.com/rs/zerolog"

	"github.com/zhangzqs/smtp-gateway-go/internal/infra/listener"
)

type Options struct {
	Addr              string
	Domain            string
	TLS               listener.TLS
	Backend           smtp.Backend
	Logger            zerolog.Logger
	MaxRecipients     int
	MaxMessageBytes   int64
	AllowInsecureAuth bool
}

type Server struct {
	smtp     *smtp.Server
	listener *listener.Listener
}

func New(ctx context.Context, opts Options) (*Server, error) {
	connection, err := listener.New(ctx, listener.Config{Addr: opts.Addr, TLS: opts.TLS})
	if err != nil {
		return nil, err
	}
	server := smtp.NewServer(opts.Backend)
	server.ErrorLog = stdlog.New(opts.Logger.Level(zerolog.ErrorLevel), "", 0)
	server.Addr = opts.Addr
	server.Domain = opts.Domain
	server.MaxRecipients = opts.MaxRecipients
	server.MaxMessageBytes = opts.MaxMessageBytes
	server.AllowInsecureAuth = opts.AllowInsecureAuth
	server.ReadTimeout = 5 * time.Minute
	server.WriteTimeout = 5 * time.Minute
	server.TLSConfig = connection.TLSConfig()
	return &Server{smtp: server, listener: connection}, nil
}

func (s *Server) Run(ctx context.Context) error {
	done := make(chan error, 1)
	go func() { done <- s.smtp.Serve(s.listener) }()
	select {
	case err := <-done:
		if ctx.Err() != nil {
			return errors.Join(s.shutdown(ctx), err)
		}
		_ = s.smtp.Close()
		if err == nil {
			return errors.New("SMTP server stopped unexpectedly")
		}
		return err
	case <-ctx.Done():
		return errors.Join(s.shutdown(ctx), <-done)
	}
}

func (s *Server) shutdown(ctx context.Context) error {
	shutdownCtx, cancel := context.WithTimeout(context.WithoutCancel(ctx), 30*time.Second)
	defer cancel()
	err := s.smtp.Shutdown(shutdownCtx)
	_ = s.listener.Close()
	return err
}

func (s *Server) Close() error { return s.listener.Close() }
