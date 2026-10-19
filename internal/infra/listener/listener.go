package listener

import (
	"context"
	"crypto/tls"
	"errors"
	"net"
	"slices"
)

type Mode string

const (
	NoneMode     Mode = "none"
	StartTLSMode Mode = "starttls"
	ImplicitMode Mode = "implicit"
)

var ModeValues = []Mode{NoneMode, StartTLSMode, ImplicitMode}

type TLS struct {
	Mode     Mode   `yaml:"mode"`
	CertFile string `yaml:"cert_file"`
	KeyFile  string `yaml:"key_file"`
}

func (c TLS) Validate() error {
	if c.Mode == "" || c.Mode == NoneMode {
		return nil
	}
	if !slices.Contains(ModeValues, c.Mode) {
		return errors.New("invalid listener TLS mode")
	}
	if c.CertFile == "" || c.KeyFile == "" {
		return errors.New("TLS certificate and key files are required")
	}
	return nil
}

type Config struct {
	Addr string
	TLS  TLS
}

type Listener struct {
	net.Listener

	tlsConfig *tls.Config
}

func New(ctx context.Context, cfg Config) (*Listener, error) {
	if err := cfg.TLS.Validate(); err != nil {
		return nil, err
	}
	var tlsConfig *tls.Config
	if cfg.TLS.Mode != "" && cfg.TLS.Mode != NoneMode {
		cert, err := tls.LoadX509KeyPair(cfg.TLS.CertFile, cfg.TLS.KeyFile)
		if err != nil {
			return nil, err
		}
		tlsConfig = &tls.Config{Certificates: []tls.Certificate{cert}, MinVersion: tls.VersionTLS12}
	}
	connection, err := (&net.ListenConfig{}).Listen(ctx, "tcp", cfg.Addr)
	if err != nil {
		return nil, err
	}
	if cfg.TLS.Mode == ImplicitMode {
		connection = tls.NewListener(connection, tlsConfig)
	}
	return &Listener{Listener: connection, tlsConfig: tlsConfig}, nil
}

func (l *Listener) TLSConfig() *tls.Config { return l.tlsConfig }
