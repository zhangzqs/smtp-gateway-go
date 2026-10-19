package smtpd

import (
	"context"
	"crypto/sha256"
	"crypto/subtle"
	"errors"
	"io"
	"sync/atomic"
	"time"

	"github.com/emersion/go-sasl"
	"github.com/emersion/go-smtp"

	"github.com/zhangzqs/smtp-gateway-go/internal/config"
	"github.com/zhangzqs/smtp-gateway-go/internal/store"
)

type Backend struct {
	Store  *store.Store
	Config config.Config
	active atomic.Int64
}

func (b *Backend) NewSession(_ *smtp.Conn) (smtp.Session, error) {
	if b.active.Add(1) > int64(b.Config.Limits.MaxConnections) {
		b.active.Add(-1)
		return nil, &smtp.SMTPError{Code: 421, Message: "too many connections"}
	}
	return &session{backend: b}, nil
}

type session struct {
	backend    *Backend
	authorized bool
	mailSet    bool
	sender     string
	recipients []string
	closed     atomic.Bool
}

func (s *session) AuthMechanisms() []string { return []string{sasl.Plain} }

func (s *session) Auth(mechanism string) (sasl.Server, error) {
	if mechanism != sasl.Plain {
		return nil, &smtp.SMTPError{Code: 504, Message: "unsupported authentication mechanism"}
	}
	return sasl.NewPlainServer(func(_, username, password string) error {
		wantUser := sha256.Sum256([]byte(s.backend.Config.SMTP.Auth.Username))
		wantPass := sha256.Sum256([]byte(s.backend.Config.SMTP.Auth.Password))
		gotUser := sha256.Sum256([]byte(username))
		gotPass := sha256.Sum256([]byte(password))
		if subtle.ConstantTimeCompare(gotUser[:], wantUser[:]) != 1 || subtle.ConstantTimeCompare(gotPass[:], wantPass[:]) != 1 {
			return &smtp.SMTPError{Code: 535, Message: "authentication failed"}
		}
		s.authorized = true
		return nil
	}), nil
}

func (s *session) Mail(from string, _ *smtp.MailOptions) error {
	if !s.authorized {
		return &smtp.SMTPError{Code: 530, Message: "authentication required"}
	}
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	relay, _, err := s.backend.Store.Relay(ctx)
	if err != nil || relay.Host == "" {
		return &smtp.SMTPError{Code: 451, Message: "relay unavailable"}
	}
	s.sender = from
	s.mailSet = true
	s.recipients = nil
	return nil
}

func (s *session) Rcpt(to string, _ *smtp.RcptOptions) error {
	if !s.authorized || !s.mailSet {
		return &smtp.SMTPError{Code: 503, Message: "MAIL command required"}
	}
	if len(s.recipients) >= s.backend.Config.Limits.MaxRecipients {
		return &smtp.SMTPError{Code: 452, Message: "too many recipients"}
	}
	s.recipients = append(s.recipients, to)
	return nil
}

func (s *session) Data(r io.Reader) error {
	if len(s.recipients) == 0 {
		return &smtp.SMTPError{Code: 503, Message: "recipient required"}
	}
	maxBytes := s.backend.Config.Limits.MaxMessageBytes
	raw, err := io.ReadAll(io.LimitReader(r, maxBytes+1))
	if err != nil {
		return &smtp.SMTPError{Code: 451, Message: "failed to read message"}
	}
	if int64(len(raw)) > maxBytes {
		return &smtp.SMTPError{Code: 552, Message: "message too large"}
	}
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	_, err = s.backend.Store.Enqueue(ctx, s.sender, s.recipients, raw, s.backend.Config.Limits, s.backend.Config.Worker.MaxAge)
	if err != nil {
		if errors.Is(err, store.ErrQueueFull) {
			return &smtp.SMTPError{Code: 452, Message: "queue full"}
		}
		return &smtp.SMTPError{Code: 451, Message: "storage unavailable"}
	}
	s.Reset()
	return nil
}

func (s *session) Reset() {
	s.sender = ""
	s.mailSet = false
	s.recipients = nil
}

func (s *session) Logout() error {
	if s.closed.CompareAndSwap(false, true) {
		s.backend.active.Add(-1)
	}
	return nil
}
