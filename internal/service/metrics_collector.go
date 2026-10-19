package service

import (
	"context"
	"errors"
	"time"

	"github.com/prometheus/client_golang/prometheus"
	"github.com/rs/zerolog"
)

type MetricsCollector struct {
	service  *Service
	logger   zerolog.Logger
	pending  *prometheus.Desc
	bytes    *prometheus.Desc
	oldest   *prometheus.Desc
	inFlight *prometheus.Desc
	accepted *prometheus.Desc
	failed   *prometheus.Desc
	expired  *prometheus.Desc
	retries  *prometheus.Desc
	writes   *prometheus.Desc
}

func NewMetricsCollector(service *Service, logger zerolog.Logger) *MetricsCollector {
	return &MetricsCollector{
		service:  service,
		logger:   logger,
		pending:  prometheus.NewDesc("smtp_gateway_pending_recipients", "Pending recipients in the queue.", nil, nil),
		bytes:    prometheus.NewDesc("smtp_gateway_pending_bytes", "Bytes in messages with pending recipients.", nil, nil),
		oldest:   prometheus.NewDesc("smtp_gateway_oldest_pending_seconds", "Age of the oldest pending message in seconds.", nil, nil),
		inFlight: prometheus.NewDesc("smtp_gateway_in_flight_messages", "Messages currently leased for delivery.", nil, nil),
		accepted: prometheus.NewDesc("smtp_gateway_accepted_recipients", "Recipients accepted by the upstream relay.", nil, nil),
		failed:   prometheus.NewDesc("smtp_gateway_failed_recipients", "Recipients permanently rejected by the upstream relay.", nil, nil),
		expired:  prometheus.NewDesc("smtp_gateway_expired_recipients", "Recipients whose delivery window expired.", nil, nil),
		retries:  prometheus.NewDesc("smtp_gateway_retries", "Retry attempts retained in the queue.", nil, nil),
		writes:   prometheus.NewDesc("smtp_gateway_database_write_failures_total", "Database write failures while accepting messages.", nil, nil),
	}
}

func (c *MetricsCollector) Describe(ch chan<- *prometheus.Desc) {
	ch <- c.pending
	ch <- c.bytes
	ch <- c.oldest
	ch <- c.inFlight
	ch <- c.accepted
	ch <- c.failed
	ch <- c.expired
	ch <- c.retries
	ch <- c.writes
}

func (c *MetricsCollector) Collect(ch chan<- prometheus.Metric) {
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	snapshot, err := c.service.Metrics(ctx)
	if err != nil {
		c.logger.Error().Err(err).Msg("collect metrics")
		ch <- prometheus.NewInvalidMetric(c.pending, errors.New("database unavailable"))
		return
	}
	ch <- prometheus.MustNewConstMetric(c.pending, prometheus.GaugeValue, float64(snapshot.Pending))
	ch <- prometheus.MustNewConstMetric(c.bytes, prometheus.GaugeValue, float64(snapshot.Bytes))
	ch <- prometheus.MustNewConstMetric(c.oldest, prometheus.GaugeValue, float64(snapshot.OldestPendingSeconds))
	ch <- prometheus.MustNewConstMetric(c.inFlight, prometheus.GaugeValue, float64(snapshot.InFlight))
	ch <- prometheus.MustNewConstMetric(c.accepted, prometheus.GaugeValue, float64(snapshot.Accepted))
	ch <- prometheus.MustNewConstMetric(c.failed, prometheus.GaugeValue, float64(snapshot.Failed))
	ch <- prometheus.MustNewConstMetric(c.expired, prometheus.GaugeValue, float64(snapshot.Expired))
	ch <- prometheus.MustNewConstMetric(c.retries, prometheus.GaugeValue, float64(snapshot.Retries))
	ch <- prometheus.MustNewConstMetric(c.writes, prometheus.CounterValue, float64(snapshot.DBWriteFailures))
}
