package infra

import (
	"context"
	"crypto/tls"
	"net"
	"os"

	"github.com/rs/zerolog"

	"github.com/zhangzqs/smtp-gateway-go/internal/config"
)

func NewLogger(cfg config.Logger) (zerolog.Logger, error) {
	level, err := zerolog.ParseLevel(cfg.Level)
	if err != nil {
		return zerolog.Logger{}, err
	}
	return zerolog.New(os.Stderr).Level(level).With().Timestamp().Logger(), nil
}

func TLSConfig(cfg config.TLS) (*tls.Config, error) {
	cert, err := tls.LoadX509KeyPair(cfg.CertFile, cfg.KeyFile)
	if err != nil {
		return nil, err
	}
	return &tls.Config{Certificates: []tls.Certificate{cert}, MinVersion: tls.VersionTLS12}, nil
}

func Listen(ctx context.Context, addr string, tlsConfig *tls.Config, implicitTLS bool) (net.Listener, error) {
	listener, err := (&net.ListenConfig{}).Listen(ctx, "tcp", addr)
	if err != nil {
		return nil, err
	}
	if implicitTLS {
		return tls.NewListener(listener, tlsConfig), nil
	}
	return listener, nil
}
