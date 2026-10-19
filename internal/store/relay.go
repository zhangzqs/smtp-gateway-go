package store

import (
	"context"
	"crypto/aes"
	"crypto/cipher"
	"crypto/rand"
	"errors"
	"fmt"

	"gorm.io/gorm"

	"github.com/zhangzqs/smtp-gateway-go/internal/config"
)

var (
	ErrReadOnly   = errors.New("relay is managed by the configuration file")
	ErrValidation = errors.New("invalid relay configuration")
)

type relayRow struct {
	ID          int `gorm:"primaryKey"`
	Version     int64
	Host        string
	Port        int
	TLS         string
	Username    string
	Password    []byte
	FromAddress string
	FromName    string
}

func (relayRow) TableName() string { return "relay_config" }

type RelayView struct {
	Configured  bool   `json:"configured"`
	ReadOnly    bool   `json:"read_only"`
	HasPassword bool   `json:"has_password"`
	Host        string `json:"host"`
	Port        int    `json:"port"`
	TLS         string `json:"tls"`
	Username    string `json:"username"`
	FromAddress string `json:"from_address"`
	FromName    string `json:"from_name"`
}

func view(relay config.Relay, readOnly bool) RelayView {
	return RelayView{relay.Host != "", readOnly, relay.Password != "", relay.Host, relay.Port, relay.TLS, relay.Username, relay.FromAddress, relay.FromName}
}

func (s *Store) Relay(ctx context.Context) (config.Relay, RelayView, error) {
	var row relayRow
	if err := s.DB.WithContext(ctx).First(&row, 1).Error; err != nil {
		return config.Relay{}, RelayView{}, err
	}
	relay := config.Relay{Host: row.Host, Port: row.Port, TLS: row.TLS, Username: row.Username, FromAddress: row.FromAddress, FromName: row.FromName}
	if len(row.Password) > 0 {
		password, err := s.decrypt(row.Password)
		if err != nil {
			return config.Relay{}, RelayView{}, fmt.Errorf("decrypt relay password: %w", err)
		}
		relay.Password = password
	}
	if s.fileRelay != nil {
		return *s.fileRelay, view(*s.fileRelay, true), nil
	}
	return relay, view(relay, false), nil
}

func (s *Store) SaveRelay(ctx context.Context, relay config.Relay, password *string) (RelayView, error) {
	if s.fileRelay != nil {
		return RelayView{}, ErrReadOnly
	}
	var result RelayView
	err := s.DB.WithContext(ctx).Transaction(func(tx *gorm.DB) error {
		// The no-op update obtains the SQLite write lock before reading the row.
		if err := tx.Exec("UPDATE relay_config SET version = version WHERE id = 1").Error; err != nil {
			return err
		}
		var old relayRow
		if err := tx.First(&old, 1).Error; err != nil {
			return err
		}
		switch {
		case relay.Username == "":
			if password != nil && *password != "" {
				return fmt.Errorf("%w: password requires username", ErrValidation)
			}
			relay.Password = ""
		case password != nil:
			relay.Password = *password
		default:
			if old.Host != relay.Host || old.Port != relay.Port || old.TLS != relay.TLS || old.Username != relay.Username || len(old.Password) == 0 {
				return fmt.Errorf("%w: password is required when the relay authentication context changes", ErrValidation)
			}
			var err error
			relay.Password, err = s.decrypt(old.Password)
			if err != nil {
				return fmt.Errorf("decrypt relay password: %w", err)
			}
		}
		if err := relay.Validate(); err != nil {
			return fmt.Errorf("%w: %w", ErrValidation, err)
		}
		secret, err := s.encrypt(relay.Password)
		if err != nil {
			return err
		}
		changes := map[string]any{"version": old.Version + 1, "host": relay.Host, "port": relay.Port, "tls": relay.TLS, "username": relay.Username, "password": secret, "from_address": relay.FromAddress, "from_name": relay.FromName}
		if err := tx.Model(&relayRow{}).Where("id = 1").Updates(changes).Error; err != nil {
			return err
		}
		result = view(relay, false)
		return nil
	})
	return result, err
}

func (s *Store) encrypt(value string) ([]byte, error) {
	if value == "" {
		return []byte{}, nil
	}
	block, err := aes.NewCipher(s.key)
	if err != nil {
		return nil, err
	}
	aead, err := cipher.NewGCM(block)
	if err != nil {
		return nil, err
	}
	nonce := make([]byte, aead.NonceSize())
	if _, err := rand.Read(nonce); err != nil {
		return nil, err
	}
	return aead.Seal(nonce, nonce, []byte(value), nil), nil
}

func (s *Store) decrypt(value []byte) (string, error) {
	block, err := aes.NewCipher(s.key)
	if err != nil {
		return "", err
	}
	aead, err := cipher.NewGCM(block)
	if err != nil {
		return "", err
	}
	if len(value) < aead.NonceSize() {
		return "", errors.New("invalid encrypted password")
	}
	plain, err := aead.Open(nil, value[:aead.NonceSize()], value[aead.NonceSize():], nil)
	return string(plain), err
}
