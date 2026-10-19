package config

import (
	"encoding/base64"
	"errors"
	"fmt"
	"math"
	"net/mail"
	"os"
	"strings"
	"time"

	"go.yaml.in/yaml/v3"
)

type Credentials struct {
	Username string `yaml:"username"`
	Password string `yaml:"password"`
}

type TLS struct {
	Mode     string `yaml:"mode"`
	CertFile string `yaml:"cert_file"`
	KeyFile  string `yaml:"key_file"`
}

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

type Database struct {
	Driver string `yaml:"driver"`
	DSN    string `yaml:"dsn"`
}

type Limits struct {
	MaxMessageBytes  int64 `yaml:"max_message_bytes"`
	MaxRecipients    int   `yaml:"max_recipients"`
	MaxConnections   int   `yaml:"max_connections"`
	MaxQueueMessages int64 `yaml:"max_queue_messages"`
	MaxQueueBytes    int64 `yaml:"max_queue_bytes"`
}

type Worker struct {
	Concurrency      int           `yaml:"concurrency"`
	AttemptTimeout   time.Duration `yaml:"-"`
	LeaseDuration    time.Duration `yaml:"-"`
	RetryInitial     time.Duration `yaml:"-"`
	RetryMax         time.Duration `yaml:"-"`
	MaxAge           time.Duration `yaml:"-"`
	HistoryRetention time.Duration `yaml:"-"`
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
	Security struct {
		EncryptionKey string `yaml:"encryption_key"`
	} `yaml:"security"`
	Limits Limits `yaml:"limits"`
	Worker Worker `yaml:"worker"`
	Relay  *Relay `yaml:"relay"`
}

type workerYAML struct {
	Concurrency      *int   `yaml:"concurrency"`
	AttemptTimeout   string `yaml:"attempt_timeout"`
	LeaseDuration    string `yaml:"lease_duration"`
	RetryInitial     string `yaml:"retry_initial"`
	RetryMax         string `yaml:"retry_max"`
	MaxAge           string `yaml:"max_age"`
	HistoryRetention string `yaml:"history_retention"`
}

func (w *Worker) UnmarshalYAML(node *yaml.Node) error {
	for i := 0; i+1 < len(node.Content); i += 2 {
		switch node.Content[i].Value {
		case "concurrency", "attempt_timeout", "lease_duration", "retry_initial", "retry_max", "max_age", "history_retention":
		default:
			return fmt.Errorf("unknown worker setting %q", node.Content[i].Value)
		}
	}
	var raw workerYAML
	if err := node.Decode(&raw); err != nil {
		return err
	}
	if raw.Concurrency != nil {
		w.Concurrency = *raw.Concurrency
	}
	for _, item := range []struct {
		value string
		dest  *time.Duration
	}{
		{raw.AttemptTimeout, &w.AttemptTimeout},
		{raw.LeaseDuration, &w.LeaseDuration},
		{raw.RetryInitial, &w.RetryInitial},
		{raw.RetryMax, &w.RetryMax},
		{raw.MaxAge, &w.MaxAge},
		{raw.HistoryRetention, &w.HistoryRetention},
	} {
		if item.value == "" {
			continue
		}
		value, err := time.ParseDuration(item.value)
		if err != nil {
			return fmt.Errorf("invalid worker duration %q: %w", item.value, err)
		}
		*item.dest = value
	}
	return nil
}

func Load(path string) (Config, []byte, error) {
	var cfg Config
	// #nosec G304 -- the CLI caller explicitly chooses the configuration path.
	data, err := os.ReadFile(path)
	if err != nil {
		return cfg, nil, err
	}
	cfg.Limits = Limits{10 << 20, 100, 64, 10000, 1 << 30}
	cfg.Worker = Worker{4, time.Minute, 2 * time.Minute, 30 * time.Second, 15 * time.Minute, 24 * time.Hour, 7 * 24 * time.Hour}
	decoder := yaml.NewDecoder(strings.NewReader(string(data)))
	decoder.KnownFields(true)
	if err := decoder.Decode(&cfg); err != nil {
		return cfg, nil, err
	}
	key, err := base64.StdEncoding.DecodeString(cfg.Security.EncryptionKey)
	if err != nil || len(key) != 32 {
		return cfg, nil, errors.New("security.encryption_key must be base64-encoded 32 bytes")
	}
	if err := cfg.Validate(); err != nil {
		return cfg, nil, err
	}
	return cfg, key, nil
}

func (c Config) Validate() error {
	if c.HTTP.Listen == "" || c.SMTP.Listen == "" || c.SMTP.Hostname == "" || c.Database.DSN == "" {
		return errors.New("HTTP, SMTP, and database settings are required")
	}
	if c.Database.Driver != "sqlite" && c.Database.Driver != "postgres" {
		return errors.New("database.driver must be sqlite or postgres")
	}
	if c.HTTP.BasicAuth.Username == "" || c.HTTP.BasicAuth.Password == "" || c.SMTP.Auth.Username == "" || c.SMTP.Auth.Password == "" {
		return errors.New("HTTP Basic Auth and SMTP AUTH credentials are required")
	}
	if c.Limits.MaxMessageBytes <= 0 || c.Limits.MaxMessageBytes == math.MaxInt64 || c.Limits.MaxRecipients <= 0 || c.Limits.MaxConnections <= 0 || c.Limits.MaxQueueMessages <= 0 || c.Limits.MaxQueueBytes <= 0 {
		return errors.New("all limits must be positive")
	}
	if c.Worker.Concurrency <= 0 || c.Worker.AttemptTimeout <= 0 || c.Worker.LeaseDuration <= c.Worker.AttemptTimeout || c.Worker.RetryInitial <= 0 || c.Worker.RetryMax < c.Worker.RetryInitial || c.Worker.MaxAge <= 0 || c.Worker.HistoryRetention <= 0 {
		return errors.New("invalid worker settings")
	}
	if err := validateTLS(c.SMTP.TLS, true); err != nil {
		return err
	}
	if (c.SMTP.TLS.Mode == "" || c.SMTP.TLS.Mode == "none") && !c.SMTP.AllowInsecureAuth {
		return errors.New("SMTP TLS is required unless allow_insecure_auth is explicitly enabled")
	}
	if err := validateTLS(c.HTTP.TLS, false); err != nil {
		return err
	}
	if c.Relay != nil {
		return c.Relay.Validate()
	}
	return nil
}

func validateTLS(t TLS, smtp bool) error {
	if t.Mode == "" || t.Mode == "none" {
		return nil
	}
	if (smtp && t.Mode != "starttls" && t.Mode != "implicit") || (!smtp && t.Mode != "implicit") {
		return errors.New("invalid listener TLS mode")
	}
	if t.CertFile == "" || t.KeyFile == "" {
		return errors.New("TLS certificate and key files are required")
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
