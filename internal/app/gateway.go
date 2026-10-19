package app

import (
	"context"
	"errors"
	"sync/atomic"
	"time"

	"github.com/prometheus/client_golang/prometheus"
	"github.com/prometheus/client_golang/prometheus/promhttp"

	"github.com/zhangzqs/smtp-gateway-go/internal/api"
	"github.com/zhangzqs/smtp-gateway-go/internal/config"
	"github.com/zhangzqs/smtp-gateway-go/internal/infra/database"
	"github.com/zhangzqs/smtp-gateway-go/internal/infra/httpserver"
	loggerinfra "github.com/zhangzqs/smtp-gateway-go/internal/infra/logger"
	"github.com/zhangzqs/smtp-gateway-go/internal/infra/smtpserver"
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
	management := service.New(db, cfg, ready.Load)
	worker := queue.New(db, cfg.Worker)
	registry := prometheus.NewRegistry()
	registry.MustRegister(service.NewMetricsCollector(management, logger), worker.DeliveryDuration())
	apiServer := &api.API{
		Service:        management,
		Auth:           cfg.HTTP.BasicAuth,
		MetricsHandler: promhttp.HandlerFor(registry, promhttp.HandlerOpts{}),
	}
	httpServer, err := httpserver.New(ctx, httpserver.Options{Addr: cfg.HTTP.Listen, TLS: cfg.HTTP.TLS, Handler: apiServer.Handler()})
	if err != nil {
		return err
	}
	defer func() { _ = httpServer.Close() }()
	smtpServer, err := smtpserver.New(ctx, smtpserver.Options{
		Addr:              cfg.SMTP.Listen,
		Domain:            cfg.SMTP.Hostname,
		TLS:               cfg.SMTP.TLS,
		Backend:           &smtpd.Backend{Store: db, Config: cfg},
		Logger:            logger,
		MaxRecipients:     cfg.Limits.MaxRecipients,
		MaxMessageBytes:   cfg.Limits.MaxMessageBytes,
		AllowInsecureAuth: cfg.SMTP.AllowInsecureAuth,
	})
	if err != nil {
		return err
	}
	defer func() { _ = smtpServer.Close() }()
	workerCtx, cancelWorkers := context.WithCancel(context.WithoutCancel(ctx))
	worker.Start(workerCtx)
	defer func() {
		cancelWorkers()
		worker.Wait()
	}()
	serversCtx, stopServers := context.WithCancel(context.WithoutCancel(ctx))
	defer stopServers()
	errorsCh := make(chan error, 2)
	go func() { errorsCh <- httpServer.Run(serversCtx) }()
	go func() { errorsCh <- smtpServer.Run(serversCtx) }()
	ready.Store(true)
	logger.Info().Str("http", cfg.HTTP.Listen).Str("smtp", cfg.SMTP.Listen).Msg("gateway ready")
	var runErr error
	remaining := 2
	select {
	case <-ctx.Done():
	case err := <-errorsCh:
		runErr = err
		remaining--
	}
	ready.Store(false)
	worker.StopClaiming()
	stopServers()
	for range remaining {
		runErr = errors.Join(runErr, <-errorsCh)
	}
	shutdownCtx, cancel := context.WithTimeout(context.WithoutCancel(ctx), 30*time.Second)
	defer cancel()
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
