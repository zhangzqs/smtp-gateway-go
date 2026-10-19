package service

import (
	"context"
	"errors"
	"time"

	"github.com/zhangzqs/smtp-gateway-go/internal/config"
	"github.com/zhangzqs/smtp-gateway-go/internal/store"
)

var (
	ErrReadOnly      = store.ErrReadOnly
	ErrValidation    = store.ErrValidation
	ErrNotFound      = store.ErrNotFound
	ErrConflict      = store.ErrConflict
	ErrInvalidStatus = errors.New("invalid message status")
)

type Service struct {
	store         *store.Store
	worker        config.Worker
	relayReadOnly bool
	ready         func() bool
}

func New(db *store.Store, cfg config.Config, ready func() bool) *Service {
	return &Service{store: db, worker: cfg.Worker, relayReadOnly: cfg.Relay != nil, ready: ready}
}

func (s *Service) Ready(ctx context.Context) error {
	if s.ready == nil || !s.ready() {
		return errors.New("gateway is not ready")
	}
	return s.store.Ready(ctx)
}

func (s *Service) Relay(ctx context.Context) (store.RelayView, error) {
	_, view, err := s.store.Relay(ctx)
	return view, err
}

func (s *Service) CheckRelayWritable() error {
	if s.relayReadOnly {
		return ErrReadOnly
	}
	return nil
}

func (s *Service) UpdateRelay(ctx context.Context, relay config.Relay, password *string) (store.RelayView, error) {
	if err := s.CheckRelayWritable(); err != nil {
		return store.RelayView{}, err
	}
	return s.store.SaveRelay(ctx, relay, password)
}

func (s *Service) ListMessages(ctx context.Context, status string, limit, offset int) ([]store.MessageStatus, error) {
	if !store.ValidStatus(status) {
		return nil, ErrInvalidStatus
	}
	return s.store.List(ctx, store.Status(status), limit, offset)
}

func (s *Service) RetryMessage(ctx context.Context, id string) error {
	return s.store.Retry(ctx, id, s.worker.MaxAge, s.worker.HistoryRetention)
}

type Metrics struct {
	store.Metrics

	OldestPendingSeconds int64
}

func (s *Service) Metrics(ctx context.Context) (Metrics, error) {
	snapshot, err := s.store.Metrics(ctx)
	if err != nil {
		return Metrics{}, err
	}
	result := Metrics{Metrics: snapshot}
	if snapshot.Oldest > 0 {
		result.OldestPendingSeconds = max(0, time.Now().UnixMilli()-snapshot.Oldest) / 1000
	}
	return result, nil
}
