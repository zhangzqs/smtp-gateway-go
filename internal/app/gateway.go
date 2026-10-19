package app

import (
	"context"
	"crypto/tls"
	"errors"
	stdlog "log"
	"net/http"
	"sync/atomic"
	"time"

	"github.com/emersion/go-smtp"
	"github.com/rs/zerolog"

	"github.com/zhangzqs/smtp-gateway-go/internal/api"
	"github.com/zhangzqs/smtp-gateway-go/internal/config"
	"github.com/zhangzqs/smtp-gateway-go/internal/infra"
	"github.com/zhangzqs/smtp-gateway-go/internal/queue"
	"github.com/zhangzqs/smtp-gateway-go/internal/smtpd"
	"github.com/zhangzqs/smtp-gateway-go/internal/store"
)

type Gateway struct {
	config config.Config
	key    []byte
}

func New(cfg config.Config, key []byte) *Gateway {
	return &Gateway{config: cfg, key: key}
}

func (g *Gateway) Run(ctx context.Context) error {
	cfg := g.config
	logger, err := infra.NewLogger(cfg.Logger)
	if err != nil {
		return err
	}
	ctx = logger.WithContext(ctx)
	db, err := store.Open(ctx, cfg, g.key)
	if err != nil {
		return err
	}
	defer func() { _ = db.Close() }()
	if _, _, err := db.Relay(ctx); err != nil {
		return err
	}
	var ready atomic.Bool
	apiServer := &api.API{Store: db, Cfg: cfg, Ready: ready.Load}
	httpServer := &http.Server{Addr: cfg.HTTP.Listen, Handler: apiServer.Handler(), ReadHeaderTimeout: 10 * time.Second}
	smtpServer := smtp.NewServer(&smtpd.Backend{Store: db, Config: cfg})
	smtpServer.ErrorLog = stdlog.New(logger.Level(zerolog.ErrorLevel), "", 0)
	smtpServer.Addr = cfg.SMTP.Listen
	smtpServer.Domain = cfg.SMTP.Hostname
	smtpServer.MaxRecipients = cfg.Limits.MaxRecipients
	smtpServer.MaxMessageBytes = cfg.Limits.MaxMessageBytes
	smtpServer.AllowInsecureAuth = cfg.SMTP.AllowInsecureAuth
	smtpServer.ReadTimeout = 5 * time.Minute
	smtpServer.WriteTimeout = 5 * time.Minute
	var smtpTLS *tls.Config
	if cfg.SMTP.TLS.Mode != "" && cfg.SMTP.TLS.Mode != "none" {
		smtpTLS, err = infra.TLSConfig(cfg.SMTP.TLS)
		if err != nil {
			return err
		}
	}
	smtpServer.TLSConfig = smtpTLS
	var httpTLS *tls.Config
	if cfg.HTTP.TLS.Mode == "implicit" {
		httpTLS, err = infra.TLSConfig(cfg.HTTP.TLS)
		if err != nil {
			return err
		}
	}
	httpListener, err := infra.Listen(ctx, cfg.HTTP.Listen, httpTLS, cfg.HTTP.TLS.Mode == "implicit")
	if err != nil {
		return err
	}
	defer func() { _ = httpListener.Close() }()
	smtpListener, err := infra.Listen(ctx, cfg.SMTP.Listen, smtpTLS, cfg.SMTP.TLS.Mode == "implicit")
	if err != nil {
		return err
	}
	defer func() { _ = smtpListener.Close() }()
	workerCtx, cancelWorkers := context.WithCancel(context.WithoutCancel(ctx))
	worker := queue.New(db, cfg.Worker)
	apiServer.DeliveryDuration = worker.DeliveryDuration()
	worker.Start(workerCtx)
	defer func() {
		cancelWorkers()
		worker.Wait()
	}()
	errorsCh := make(chan error, 2)
	go func() { errorsCh <- httpServer.Serve(httpListener) }()
	go func() { errorsCh <- smtpServer.Serve(smtpListener) }()
	ready.Store(true)
	logger.Info().Str("http", cfg.HTTP.Listen).Str("smtp", cfg.SMTP.Listen).Msg("gateway ready")
	var runErr error
	select {
	case <-ctx.Done():
	case err := <-errorsCh:
		if !errors.Is(err, http.ErrServerClosed) && !errors.Is(err, smtp.ErrServerClosed) {
			runErr = err
		}
	}
	ready.Store(false)
	worker.StopClaiming()
	shutdownCtx, cancel := context.WithTimeout(context.WithoutCancel(ctx), 30*time.Second)
	defer cancel()
	if err := smtpServer.Shutdown(shutdownCtx); err != nil {
		logger.Warn().Err(err).Msg("SMTP shutdown")
	}
	if err := httpServer.Shutdown(shutdownCtx); err != nil {
		logger.Warn().Err(err).Msg("HTTP shutdown")
	}
	workerDone := make(chan struct{})
	go func() { worker.Wait(); close(workerDone) }()
	select {
	case <-workerDone:
	case <-shutdownCtx.Done():
		cancelWorkers()
		<-workerDone
	}
	return runErr
}
