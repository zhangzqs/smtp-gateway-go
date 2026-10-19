package httpserver

import (
	"context"
	"errors"
	"net/http"
	"time"

	"github.com/zhangzqs/smtp-gateway-go/internal/infra/listener"
)

type Options struct {
	Addr    string
	TLS     listener.TLS
	Handler http.Handler
}

type Server struct {
	http     *http.Server
	listener *listener.Listener
}

func New(ctx context.Context, opts Options) (*Server, error) {
	connection, err := listener.New(ctx, listener.Config{Addr: opts.Addr, TLS: opts.TLS})
	if err != nil {
		return nil, err
	}
	return &Server{
		http:     &http.Server{Addr: opts.Addr, Handler: opts.Handler, ReadHeaderTimeout: 10 * time.Second},
		listener: connection,
	}, nil
}

func (s *Server) Run(ctx context.Context) error {
	done := make(chan error, 1)
	go func() { done <- s.http.Serve(s.listener) }()
	select {
	case err := <-done:
		if ctx.Err() != nil {
			return errors.Join(s.shutdown(ctx), serverError(err))
		}
		_ = s.http.Close()
		return serverError(err)
	case <-ctx.Done():
		return errors.Join(s.shutdown(ctx), serverError(<-done))
	}
}

func (s *Server) shutdown(ctx context.Context) error {
	shutdownCtx, cancel := context.WithTimeout(context.WithoutCancel(ctx), 30*time.Second)
	defer cancel()
	if err := s.http.Shutdown(shutdownCtx); err != nil {
		return errors.Join(err, s.http.Close())
	}
	return nil
}

func serverError(err error) error {
	if errors.Is(err, http.ErrServerClosed) {
		return nil
	}
	if err == nil {
		return errors.New("HTTP server stopped unexpectedly")
	}
	return err
}

func (s *Server) Close() error { return s.listener.Close() }
