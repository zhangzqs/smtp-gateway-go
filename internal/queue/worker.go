package queue

import (
	"context"
	"errors"
	"sync"
	"time"

	retrybackoff "github.com/cenkalti/backoff/v4"
	"github.com/prometheus/client_golang/prometheus"
	"github.com/rs/zerolog"

	"github.com/zhangzqs/smtp-gateway-go/internal/config"
	"github.com/zhangzqs/smtp-gateway-go/internal/relay"
	"github.com/zhangzqs/smtp-gateway-go/internal/store"
)

type Worker struct {
	Store      *store.Store
	Config     config.Worker
	wg         sync.WaitGroup
	duration   prometheus.Summary
	stopClaims context.CancelFunc
}

func New(s *store.Store, cfg config.Worker) *Worker {
	return &Worker{
		Store:  s,
		Config: cfg,
		duration: prometheus.NewSummary(prometheus.SummaryOpts{
			Name: "smtp_gateway_delivery_attempt_seconds",
			Help: "Duration of upstream delivery attempts in seconds.",
		}),
	}
}

func (w *Worker) Start(ctx context.Context) {
	claimCtx, stopClaims := context.WithCancel(ctx)
	w.stopClaims = stopClaims
	w.wg.Go(func() {
		w.maintain(claimCtx)
	})
	for range w.Config.Concurrency {
		w.wg.Go(func() {
			w.loop(ctx, claimCtx)
		})
	}
}

func (w *Worker) maintain(ctx context.Context) {
	ticker := time.NewTicker(time.Minute)
	defer ticker.Stop()
	for {
		if err := w.Store.Expire(ctx); err != nil && !errors.Is(err, context.Canceled) {
			zerolog.Ctx(ctx).Error().Err(err).Msg("expire queue")
		}
		if err := w.Store.Cleanup(ctx, w.Config.HistoryRetention); err != nil && !errors.Is(err, context.Canceled) {
			zerolog.Ctx(ctx).Error().Err(err).Msg("cleanup queue")
		}
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
		}
	}
}

func (w *Worker) Wait() { w.wg.Wait() }

func (w *Worker) StopClaiming() { w.stopClaims() }

func (w *Worker) DeliveryDuration() prometheus.Summary { return w.duration }

func (w *Worker) loop(ctx, claimCtx context.Context) {
	ticker := time.NewTicker(500 * time.Millisecond)
	defer ticker.Stop()
	for {
		if claimCtx.Err() != nil {
			return
		}
		claim, err := w.Store.Claim(claimCtx, w.Config.LeaseDuration)
		if err != nil && !errors.Is(err, context.Canceled) && !errors.Is(err, store.ErrNoClaim) {
			zerolog.Ctx(ctx).Error().Err(err).Msg("claim queue")
		}
		if claim != nil {
			w.deliver(ctx, claim)
			continue
		}
		select {
		case <-claimCtx.Done():
			return
		case <-ticker.C:
		}
	}
}

func (w *Worker) deliver(ctx context.Context, claim *store.Claimed) {
	started := time.Now()
	defer func() { w.duration.Observe(time.Since(started).Seconds()) }()
	attemptCtx, cancel := context.WithTimeout(ctx, w.Config.AttemptTimeout)
	defer cancel()
	settings, _, err := w.Store.Relay(attemptCtx)
	var outcomes []relay.Outcome
	if err != nil || settings.Host == "" {
		outcomes = make([]relay.Outcome, 0, len(claim.Recipients))
		for _, recipient := range claim.Recipients {
			outcomes = append(outcomes, relay.Outcome{ID: recipient.ID, Status: store.PendingStatus, Error: "relay_unavailable"})
		}
	} else {
		outcomes = relay.Deliver(attemptCtx, settings, claim)
	}
	now := time.Now()
	results := make([]store.Result, 0, len(outcomes))
	for _, outcome := range outcomes {
		for _, recipient := range claim.Recipients {
			if recipient.ID != outcome.ID {
				continue
			}
			status := outcome.Status
			next := int64(0)
			if status == store.PendingStatus {
				next = now.Add(backoff(w.Config, recipient.Attempts)).UnixMilli()
				if next >= claim.ExpiresAt {
					status = store.ExpiredStatus
				}
			}
			results = append(results, store.Result{ID: outcome.ID, Status: status, Error: outcome.Error, NextAt: next})
			break
		}
	}
	// A delivery deadline may have passed, but a successful DATA response still
	// needs to be recorded before the lease expires.
	commitCtx, cancel := context.WithTimeout(context.WithoutCancel(ctx), 10*time.Second)
	defer cancel()
	if err := w.Store.Complete(commitCtx, claim, results); err != nil {
		zerolog.Ctx(ctx).Error().Err(err).Str("message_id", claim.ID).Msg("complete delivery")
		return
	}
	zerolog.Ctx(ctx).Info().Str("message_id", claim.ID).Int("recipients", len(results)).Msg("delivery attempt")
}

func backoff(cfg config.Worker, attempts int) time.Duration {
	policy := retrybackoff.NewExponentialBackOff(
		retrybackoff.WithInitialInterval(cfg.RetryInitial),
		retrybackoff.WithMultiplier(2),
		retrybackoff.WithMaxInterval(cfg.RetryMax),
		retrybackoff.WithMaxElapsedTime(0),
	)
	for range min(attempts, 30) {
		policy.NextBackOff()
	}
	return min(policy.NextBackOff(), cfg.RetryMax)
}
