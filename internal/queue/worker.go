package queue

import (
	"context"
	"crypto/rand"
	"errors"
	"math/big"
	"sync"
	"sync/atomic"
	"time"

	"github.com/rs/zerolog"

	"github.com/zhangzqs/smtp-gateway-go/internal/config"
	"github.com/zhangzqs/smtp-gateway-go/internal/relay"
	"github.com/zhangzqs/smtp-gateway-go/internal/store"
)

type Worker struct {
	Store         *store.Store
	Config        config.Worker
	wg            sync.WaitGroup
	attempts      atomic.Int64
	durationNanos atomic.Int64
	stopClaims    context.CancelFunc
}

func New(s *store.Store, cfg config.Worker) *Worker {
	return &Worker{Store: s, Config: cfg}
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

func (w *Worker) Stats() (int64, float64) {
	return w.attempts.Load(), float64(w.durationNanos.Load()) / float64(time.Second)
}

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
	defer func() {
		w.attempts.Add(1)
		w.durationNanos.Add(time.Since(started).Nanoseconds())
	}()
	attemptCtx, cancel := context.WithTimeout(ctx, w.Config.AttemptTimeout)
	defer cancel()
	settings, _, err := w.Store.Relay(attemptCtx)
	var outcomes []relay.Outcome
	if err != nil || settings.Host == "" {
		outcomes = make([]relay.Outcome, 0, len(claim.Recipients))
		for _, recipient := range claim.Recipients {
			outcomes = append(outcomes, relay.Outcome{ID: recipient.ID, Status: store.Pending, Error: "relay_unavailable"})
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
			if status == store.Pending {
				next = now.Add(backoff(w.Config, recipient.Attempts)).UnixMilli()
				if next >= claim.ExpiresAt {
					status = store.Expired
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
	delay := cfg.RetryInitial
	for range min(attempts, 30) {
		if delay >= cfg.RetryMax/2 {
			delay = cfg.RetryMax
			break
		}
		delay *= 2
	}
	if delay > cfg.RetryMax {
		delay = cfg.RetryMax
	}
	jitter, err := rand.Int(rand.Reader, big.NewInt(int64(delay/2)+1))
	if err != nil {
		return delay
	}
	return delay/2 + time.Duration(jitter.Int64())
}
