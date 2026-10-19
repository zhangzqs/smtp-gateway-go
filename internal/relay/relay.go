package relay

import (
	"bufio"
	"bytes"
	"context"
	"crypto/tls"
	"errors"
	"io"
	"net"
	"net/mail"
	"strconv"
	"time"

	"github.com/emersion/go-message/textproto"
	"github.com/emersion/go-sasl"
	"github.com/emersion/go-smtp"

	"github.com/zhangzqs/smtp-gateway-go/internal/config"
	"github.com/zhangzqs/smtp-gateway-go/internal/store"
)

func Rewrite(raw []byte, sender config.Relay) ([]byte, error) {
	if err := sender.Validate(); err != nil {
		return nil, err
	}
	reader := bufio.NewReader(bytes.NewReader(raw))
	header, err := textproto.ReadHeader(reader)
	if err != nil {
		return nil, err
	}
	header.Del("From")
	header.Del("Sender")
	header.Del("Return-Path")
	header.Set("From", (&mail.Address{Name: sender.FromName, Address: sender.FromAddress}).String())
	var out bytes.Buffer
	if err := textproto.WriteHeader(&out, header); err != nil {
		return nil, err
	}
	if _, err := io.Copy(&out, reader); err != nil {
		return nil, err
	}
	return out.Bytes(), nil
}

type Outcome struct {
	ID     string
	Status string
	Error  string
}

func all(recipients []store.Recipient, status, category string) []Outcome {
	results := make([]Outcome, 0, len(recipients))
	for _, recipient := range recipients {
		results = append(results, Outcome{recipient.ID, status, category})
	}
	return results
}

func Deliver(ctx context.Context, settings config.Relay, claim *store.Claimed) []Outcome {
	body, err := Rewrite(claim.Raw, settings)
	if err != nil {
		return all(claim.Recipients, store.Failed, "invalid_message")
	}
	client, conn, stop, err := dial(ctx, settings)
	if err != nil {
		return all(claim.Recipients, store.Pending, "connection_or_tls")
	}
	defer func() { _ = stop() }()
	defer func() { _ = conn.Close() }()
	if settings.Username != "" {
		if err := client.Auth(sasl.NewPlainClient("", settings.Username, settings.Password)); err != nil {
			return all(claim.Recipients, store.Pending, "upstream_auth")
		}
	}
	if err := client.Mail(settings.FromAddress, nil); err != nil {
		return all(claim.Recipients, store.Pending, "mail_from")
	}
	results := make([]Outcome, 0, len(claim.Recipients))
	accepted := make([]store.Recipient, 0, len(claim.Recipients))
	for _, recipient := range claim.Recipients {
		err := client.Rcpt(recipient.Address, nil)
		if err == nil {
			accepted = append(accepted, recipient)
			continue
		}
		status := store.Pending
		if permanent(err) {
			status = store.Failed
		}
		results = append(results, Outcome{recipient.ID, status, "rcpt_rejected"})
	}
	if len(accepted) == 0 {
		return results
	}
	command, err := client.Data()
	if err != nil {
		status := store.Pending
		if permanent(err) {
			status = store.Failed
		}
		return append(results, all(accepted, status, "data_rejected")...)
	}
	if _, err := command.Write(body); err != nil {
		_ = command.Close()
		return append(results, all(accepted, store.Pending, "data_uncertain")...)
	}
	if err := command.Close(); err != nil {
		status := store.Pending
		if permanent(err) {
			status = store.Failed
		}
		return append(results, all(accepted, status, "data_rejected_or_uncertain")...)
	}
	// DATA's final 2xx response is the acceptance boundary. A later QUIT error
	// must not cause a resend.
	return append(results, all(accepted, store.Accepted, "")...)
}

func permanent(err error) bool {
	var smtpErr *smtp.SMTPError
	return errors.As(err, &smtpErr) && smtpErr.Code >= 500 && smtpErr.Code < 600
}

func dial(ctx context.Context, settings config.Relay) (*smtp.Client, net.Conn, func() bool, error) {
	address := net.JoinHostPort(settings.Host, strconv.Itoa(settings.Port))
	dialer := &net.Dialer{}
	conn, err := dialer.DialContext(ctx, "tcp", address)
	if err != nil {
		return nil, nil, nil, err
	}
	if deadline, ok := ctx.Deadline(); ok {
		if err := conn.SetDeadline(deadline); err != nil {
			_ = conn.Close()
			return nil, nil, nil, err
		}
	}
	stop := context.AfterFunc(ctx, func() { _ = conn.Close() })
	tlsConfig := &tls.Config{ServerName: settings.Host, MinVersion: tls.VersionTLS12}
	var client *smtp.Client
	switch settings.TLS {
	case "none":
		client = smtp.NewClient(conn)
	case "starttls":
		client, err = smtp.NewClientStartTLS(conn, tlsConfig)
	case "implicit":
		tlsConn := tls.Client(conn, tlsConfig)
		err = tlsConn.HandshakeContext(ctx)
		if err == nil {
			client = smtp.NewClient(tlsConn)
		}
	default:
		err = errors.New("unsupported relay TLS mode")
	}
	if err != nil {
		_ = stop()
		_ = conn.Close()
		return nil, nil, nil, err
	}
	client.CommandTimeout = time.Until(deadlineOr(ctx, time.Now().Add(time.Minute)))
	client.SubmissionTimeout = client.CommandTimeout
	return client, conn, stop, nil
}

func deadlineOr(ctx context.Context, fallback time.Time) time.Time {
	if deadline, ok := ctx.Deadline(); ok {
		return deadline
	}
	return fallback
}
