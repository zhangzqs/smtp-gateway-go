package main

import (
	"context"
	"crypto/tls"
	"errors"
	"io"
	"log/slog"
	"net"
	"net/http"
	"os"
	"os/signal"
	"sync/atomic"
	"syscall"
	"time"

	"github.com/emersion/go-smtp"
	"github.com/spf13/cobra"

	"github.com/zhangzqs/smtp-gateway-go/internal/api"
	"github.com/zhangzqs/smtp-gateway-go/internal/config"
	"github.com/zhangzqs/smtp-gateway-go/internal/queue"
	"github.com/zhangzqs/smtp-gateway-go/internal/smtpd"
	"github.com/zhangzqs/smtp-gateway-go/internal/store"
)

// version is supplied by the build task through linker flags.
var version = "dev"

func main() {
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	err := run(ctx, os.Args[1:], os.Stdout)
	stop()
	if err != nil {
		slog.Error("Command failed", "error", err)
		os.Exit(1)
	}
}

func run(ctx context.Context, args []string, output io.Writer) error {
	command := &cobra.Command{
		Use:           "smtp-gateway",
		Short:         "A persistent SMTP relay gateway",
		Version:       version,
		Args:          cobra.NoArgs,
		SilenceUsage:  true,
		SilenceErrors: true,
		RunE: func(command *cobra.Command, _ []string) error {
			return command.Help()
		},
	}
	command.SetVersionTemplate("{{.Name}} {{.Version}}\n")
	command.SetOut(output)
	command.SetErr(output)
	command.SetArgs(args)
	serve := &cobra.Command{
		Use:   "serve",
		Short: "Start the SMTP gateway",
		Args:  cobra.NoArgs,
	}
	var configPath string
	serve.Flags().StringVar(&configPath, "config", "config.yaml", "configuration file")
	serve.RunE = func(command *cobra.Command, _ []string) error {
		return serveGateway(ctx, configPath)
	}
	command.AddCommand(serve)
	return command.ExecuteContext(ctx)
}

func serveGateway(ctx context.Context, path string) error {
	cfg, key, err := config.Load(path)
	if err != nil {
		return err
	}
	db, err := store.Open(ctx, cfg, key)
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
	smtpBackend := &smtpd.Backend{Store: db, Config: cfg}
	smtpServer := smtp.NewServer(smtpBackend)
	smtpServer.Addr = cfg.SMTP.Listen
	smtpServer.Domain = cfg.SMTP.Hostname
	smtpServer.MaxRecipients = cfg.Limits.MaxRecipients
	smtpServer.MaxMessageBytes = cfg.Limits.MaxMessageBytes
	smtpServer.AllowInsecureAuth = cfg.SMTP.AllowInsecureAuth
	smtpServer.ReadTimeout = 5 * time.Minute
	smtpServer.WriteTimeout = 5 * time.Minute
	if cfg.SMTP.TLS.Mode != "" && cfg.SMTP.TLS.Mode != "none" {
		cert, err := tls.LoadX509KeyPair(cfg.SMTP.TLS.CertFile, cfg.SMTP.TLS.KeyFile)
		if err != nil {
			return err
		}
		smtpServer.TLSConfig = &tls.Config{Certificates: []tls.Certificate{cert}, MinVersion: tls.VersionTLS12}
	}
	httpListener, err := (&net.ListenConfig{}).Listen(ctx, "tcp", cfg.HTTP.Listen)
	if err != nil {
		return err
	}
	defer func() { _ = httpListener.Close() }()
	smtpListener, err := (&net.ListenConfig{}).Listen(ctx, "tcp", cfg.SMTP.Listen)
	if err != nil {
		return err
	}
	defer func() { _ = smtpListener.Close() }()
	if cfg.HTTP.TLS.Mode == "implicit" {
		cert, err := tls.LoadX509KeyPair(cfg.HTTP.TLS.CertFile, cfg.HTTP.TLS.KeyFile)
		if err != nil {
			return err
		}
		httpListener = tls.NewListener(httpListener, &tls.Config{Certificates: []tls.Certificate{cert}, MinVersion: tls.VersionTLS12})
	}
	if cfg.SMTP.TLS.Mode == "implicit" {
		smtpListener = tls.NewListener(smtpListener, smtpServer.TLSConfig)
	}
	workerCtx, cancelWorkers := context.WithCancel(context.WithoutCancel(ctx))
	worker := queue.New(db, cfg.Worker, slog.Default())
	apiServer.DeliveryStats = worker.Stats
	worker.Start(workerCtx)
	defer func() {
		cancelWorkers()
		worker.Wait()
	}()
	errorsCh := make(chan error, 2)
	go func() { errorsCh <- httpServer.Serve(httpListener) }()
	go func() { errorsCh <- smtpServer.Serve(smtpListener) }()
	ready.Store(true)
	slog.Info("gateway ready", "http", cfg.HTTP.Listen, "smtp", cfg.SMTP.Listen)
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
		slog.Warn("SMTP shutdown", "error", err)
	}
	if err := httpServer.Shutdown(shutdownCtx); err != nil {
		slog.Warn("HTTP shutdown", "error", err)
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
