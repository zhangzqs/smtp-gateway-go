package config

import (
	"bytes"
	"encoding/base64"
	"errors"
	"io"
	"math"
	"net/mail"
	"os"
	"strings"
	"time"

	"go.yaml.in/yaml/v3"

	"github.com/zhangzqs/smtp-gateway-go/internal/infra/database"
	"github.com/zhangzqs/smtp-gateway-go/internal/infra/listener"
	"github.com/zhangzqs/smtp-gateway-go/internal/infra/logger"
)

type Credentials struct {
	Username string `yaml:"username"`
	Password string `yaml:"password"`
}

type TLS = listener.TLS

type HTTP struct {
	Listen    string      `yaml:"listen"`
	TLS       TLS         `yaml:"tls"`
	BasicAuth Credentials `yaml:"basic_auth"`
}

type SMTP struct {
	Listen            string      `yaml:"listen"`
	Hostname          string      `yaml:"hostname"`
	Auth              Credentials `yaml:"auth"`
	TLS               TLS         `yaml:"tls"`
	AllowInsecureAuth bool        `yaml:"allow_insecure_auth"`
}

type Database = database.Config

type Security struct {
	EncryptionKey string `yaml:"encryption_key"`
}

type Logger = logger.Config

type Limits struct {
	MaxMessageBytes  int64 `yaml:"max_message_bytes"`
	MaxRecipients    int   `yaml:"max_recipients"`
	MaxConnections   int   `yaml:"max_connections"`
	MaxQueueMessages int64 `yaml:"max_queue_messages"`
	MaxQueueBytes    int64 `yaml:"max_queue_bytes"`
}

type Worker struct {
	Concurrency      int           `yaml:"concurrency"`
	AttemptTimeout   time.Duration `yaml:"attempt_timeout"`
	LeaseDuration    time.Duration `yaml:"lease_duration"`
	RetryInitial     time.Duration `yaml:"retry_initial"`
	RetryMax         time.Duration `yaml:"retry_max"`
	MaxAge           time.Duration `yaml:"max_age"`
	HistoryRetention time.Duration `yaml:"history_retention"`
}

type Relay struct {
	Host        string `yaml:"host" json:"host"`
	Port        int    `yaml:"port" json:"port"`
	TLS         string `yaml:"tls" json:"tls"`
	Username    string `yaml:"username" json:"username"`
	Password    string `yaml:"password" json:"-"`
	FromAddress string `yaml:"from_address" json:"from_address"`
	FromName    string `yaml:"from_name" json:"from_name"`
}

type Config struct {
	HTTP     HTTP     `yaml:"http"`
	SMTP     SMTP     `yaml:"smtp"`
	Database Database `yaml:"database"`
	Security Security `yaml:"security"`
	Logger   Logger   `yaml:"logger"`
	Limits   Limits   `yaml:"limits"`
	Worker   Worker   `yaml:"worker"`
	Relay    *Relay   `yaml:"relay"`
}

func Load(path string) (Config, []byte, error) {
	cfg := Config{
		Limits: Limits{10 << 20, 100, 64, 10000, 1 << 30},
		Worker: Worker{4, time.Minute, 2 * time.Minute, 30 * time.Second, 15 * time.Minute, 24 * time.Hour, 7 * 24 * time.Hour},
		Logger: Logger{Level: "info"},
	}
	// #nosec G304 -- the CLI caller explicitly chooses the configuration path.
	data, err := os.ReadFile(path)
	if err != nil {
		return cfg, nil, err
	}
	decoder := yaml.NewDecoder(bytes.NewReader(data))
	decoder.KnownFields(true)
	if err := decoder.Decode(&cfg); err != nil {
		return cfg, nil, err
	}
	var extra any
	if err := decoder.Decode(&extra); !errors.Is(err, io.EOF) {
		return cfg, nil, errors.New("configuration must contain exactly one YAML document")
	}
	if err := cfg.Validate(); err != nil {
		return cfg, nil, err
	}
	key, err := cfg.Security.Key()
	return cfg, key, err
}

func (c Config) Validate() error {
	for _, validate := range []func() error{
		c.HTTP.Validate, c.SMTP.Validate, c.Database.Validate, c.Security.Validate,
		c.Logger.Validate, c.Limits.Validate, c.Worker.Validate,
	} {
		if err := validate(); err != nil {
			return err
		}
	}
	if c.Relay != nil {
		return c.Relay.Validate()
	}
	return nil
}

func (h HTTP) Validate() error {
	if h.Listen == "" || h.BasicAuth.Username == "" || h.BasicAuth.Password == "" {
		return errors.New("http.listen and http.basic_auth credentials are required")
	}
	if h.TLS.Mode == listener.StartTLSMode {
		return errors.New("invalid listener TLS mode")
	}
	return h.TLS.Validate()
}

func (s SMTP) Validate() error {
	if s.Listen == "" || s.Hostname == "" || s.Auth.Username == "" || s.Auth.Password == "" {
		return errors.New("smtp.listen, smtp.hostname, and smtp.auth credentials are required")
	}
	if err := s.TLS.Validate(); err != nil {
		return err
	}
	if (s.TLS.Mode == "" || s.TLS.Mode == listener.NoneMode) && !s.AllowInsecureAuth {
		return errors.New("SMTP TLS is required unless allow_insecure_auth is explicitly enabled")
	}
	return nil
}

func (s Security) Validate() error {
	_, err := s.Key()
	return err
}

func (s Security) Key() ([]byte, error) {
	key, err := base64.StdEncoding.DecodeString(s.EncryptionKey)
	if err != nil || len(key) != 32 {
		return nil, errors.New("security.encryption_key must be base64-encoded 32 bytes")
	}
	return key, nil
}

func (l Limits) Validate() error {
	if l.MaxMessageBytes <= 0 || l.MaxMessageBytes == math.MaxInt64 || l.MaxRecipients <= 0 || l.MaxConnections <= 0 || l.MaxQueueMessages <= 0 || l.MaxQueueBytes <= 0 {
		return errors.New("all limits must be positive")
	}
	return nil
}

func (w Worker) Validate() error {
	if w.Concurrency <= 0 || w.AttemptTimeout <= 0 || w.LeaseDuration <= w.AttemptTimeout || w.RetryInitial <= 0 || w.RetryMax < w.RetryInitial || w.MaxAge <= 0 || w.HistoryRetention <= 0 {
		return errors.New("invalid worker settings")
	}
	return nil
}

func (r Relay) Validate() error {
	if r.Host == "" || r.Port < 1 || r.Port > 65535 || (r.TLS != "none" && r.TLS != "starttls" && r.TLS != "implicit") {
		return errors.New("invalid relay endpoint")
	}
	if r.FromAddress == "" || strings.ContainsAny(r.Host, " \t\r\n/\\@") || strings.ContainsAny(r.Username+r.FromAddress+r.FromName, "\r\n") {
		return errors.New("invalid relay identity")
	}
	address, err := mail.ParseAddress(r.FromAddress)
	if err != nil || address.Address != r.FromAddress {
		return errors.New("invalid from_address")
	}
	if (r.Username == "") != (r.Password == "") {
		return errors.New("relay username and password must both be set or both be empty")
	}
	if r.Password == "[REDACTED]" {
		return errors.New("invalid relay password")
	}
	return nil
}
