package app

import (
	"context"
	"errors"
	stdlog "log"
	"net/http"
	"sync/atomic"
	"time"

	"github.com/emersion/go-smtp"
	"github.com/rs/zerolog"

	"github.com/zhangzqs/smtp-gateway-go/internal/api"
	"github.com/zhangzqs/smtp-gateway-go/internal/config"
	"github.com/zhangzqs/smtp-gateway-go/internal/infra/database"
	"github.com/zhangzqs/smtp-gateway-go/internal/infra/listener"
	loggerinfra "github.com/zhangzqs/smtp-gateway-go/internal/infra/logger"
	"github.com/zhangzqs/smtp-gateway-go/internal/queue"
	"github.com/zhangzqs/smtp-gateway-go/internal/service"
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
	logger, err := loggerinfra.New(cfg.Logger)
	if err != nil {
		return err
	}
	ctx = logger.WithContext(ctx)
	databaseResource, err := database.New(ctx, cfg.Database)
	if err != nil {
		return err
	}
	db, err := store.New(ctx, databaseResource, cfg, g.key)
	if err != nil {
		return err
	}
	defer func() { _ = db.Close() }()
	if _, _, err := db.Relay(ctx); err != nil {
		return err
	}
	var ready atomic.Bool
	apiServer := &api.API{Service: service.New(db, cfg, ready.Load), Auth: cfg.HTTP.BasicAuth}
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
	httpListener, err := listener.New(ctx, listener.Config{Addr: cfg.HTTP.Listen, TLS: cfg.HTTP.TLS})
	if err != nil {
		return err
	}
	defer func() { _ = httpListener.Close() }()
	smtpListener, err := listener.New(ctx, listener.Config{Addr: cfg.SMTP.Listen, TLS: cfg.SMTP.TLS})
	if err != nil {
		return err
	}
	defer func() { _ = smtpListener.Close() }()
	smtpServer.TLSConfig = smtpListener.TLSConfig()
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
