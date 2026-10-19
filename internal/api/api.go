package api

import (
	"crypto/sha256"
	"crypto/subtle"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"strconv"
	"time"

	"github.com/go-chi/chi/v5"
	"github.com/go-chi/chi/v5/middleware"
	"github.com/prometheus/client_golang/prometheus"
	"github.com/prometheus/client_golang/prometheus/promhttp"

	"github.com/zhangzqs/smtp-gateway-go/internal/config"
	"github.com/zhangzqs/smtp-gateway-go/internal/store"
)

type API struct {
	Store            *store.Store
	Cfg              config.Config
	Ready            func() bool
	DeliveryDuration prometheus.Summary
}

func (a *API) Handler() http.Handler {
	router := chi.NewRouter()
	router.Use(middleware.GetHead)
	router.Get("/healthz", func(w http.ResponseWriter, _ *http.Request) { w.WriteHeader(http.StatusOK) })
	router.Get("/readyz", a.ready)
	router.Group(func(r chi.Router) {
		r.Use(a.auth)
		r.Get("/metrics", a.metrics)
		r.Get("/v1/relay", a.getRelay)
		r.Put("/v1/relay", a.putRelay)
		r.Get("/v1/messages", a.messages)
		r.Post("/v1/messages/{id}/retry", a.retry)
	})
	return router
}

func (a *API) auth(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		username, password, ok := r.BasicAuth()
		wantUser := sha256.Sum256([]byte(a.Cfg.HTTP.BasicAuth.Username))
		wantPass := sha256.Sum256([]byte(a.Cfg.HTTP.BasicAuth.Password))
		gotUser := sha256.Sum256([]byte(username))
		gotPass := sha256.Sum256([]byte(password))
		if !ok || subtle.ConstantTimeCompare(gotUser[:], wantUser[:]) != 1 || subtle.ConstantTimeCompare(gotPass[:], wantPass[:]) != 1 {
			w.Header().Set("WWW-Authenticate", `Basic realm="smtp-gateway"`)
			http.Error(w, "unauthorized", http.StatusUnauthorized)
			return
		}
		next.ServeHTTP(w, r)
	})
}

func (a *API) ready(w http.ResponseWriter, r *http.Request) {
	if a.Ready == nil || !a.Ready() || a.Store.Ready(r.Context()) != nil {
		http.Error(w, "unavailable", http.StatusServiceUnavailable)
		return
	}
	w.WriteHeader(http.StatusOK)
}

func writeJSON(w http.ResponseWriter, value any) {
	w.Header().Set("Content-Type", "application/json")
	if err := json.NewEncoder(w).Encode(value); err != nil {
		return
	}
}

func (a *API) getRelay(w http.ResponseWriter, r *http.Request) {
	_, view, err := a.Store.Relay(r.Context())
	if err != nil {
		http.Error(w, "database unavailable", http.StatusServiceUnavailable)
		return
	}
	writeJSON(w, view)
}

type relayInput struct {
	Host        string          `json:"host"`
	Port        int             `json:"port"`
	TLS         string          `json:"tls"`
	Username    string          `json:"username"`
	Password    json.RawMessage `json:"password"`
	FromAddress string          `json:"from_address"`
	FromName    string          `json:"from_name"`
}

func (a *API) putRelay(w http.ResponseWriter, r *http.Request) {
	if a.Cfg.Relay != nil {
		http.Error(w, "relay is read only", http.StatusForbidden)
		return
	}
	relay, password, err := decodeRelayInput(http.MaxBytesReader(w, r.Body, 64<<10))
	if err != nil {
		http.Error(w, err.Error(), http.StatusUnprocessableEntity)
		return
	}
	view, err := a.Store.SaveRelay(r.Context(), relay, password)
	if err != nil {
		switch {
		case errors.Is(err, store.ErrReadOnly):
			http.Error(w, "relay is read only", http.StatusForbidden)
		case errors.Is(err, store.ErrValidation):
			http.Error(w, err.Error(), http.StatusUnprocessableEntity)
		default:
			http.Error(w, "database unavailable", http.StatusServiceUnavailable)
		}
		return
	}
	writeJSON(w, view)
}

func decodeRelayInput(body io.Reader) (config.Relay, *string, error) {
	decoder := json.NewDecoder(body)
	decoder.DisallowUnknownFields()
	var input relayInput
	if err := decoder.Decode(&input); err != nil {
		return config.Relay{}, nil, errors.New("invalid JSON")
	}
	var extra any
	if err := decoder.Decode(&extra); !errors.Is(err, io.EOF) {
		return config.Relay{}, nil, errors.New("invalid trailing JSON")
	}
	var password *string
	if input.Password != nil {
		if string(input.Password) == "null" || json.Unmarshal(input.Password, &password) != nil || password == nil {
			return config.Relay{}, nil, errors.New("invalid password")
		}
	}
	relay := config.Relay{Host: input.Host, Port: input.Port, TLS: input.TLS, Username: input.Username, FromAddress: input.FromAddress, FromName: input.FromName}
	return relay, password, nil
}

func (a *API) messages(w http.ResponseWriter, r *http.Request) {
	status := r.URL.Query().Get("status")
	if !store.ValidStatus(status) {
		http.Error(w, "invalid status", http.StatusUnprocessableEntity)
		return
	}
	limit := 50
	if value := r.URL.Query().Get("limit"); value != "" {
		var err error
		limit, err = strconv.Atoi(value)
		if err != nil || limit < 1 || limit > 100 {
			http.Error(w, "invalid limit", http.StatusUnprocessableEntity)
			return
		}
	}
	offset := 0
	if value := r.URL.Query().Get("offset"); value != "" {
		var err error
		offset, err = strconv.Atoi(value)
		if err != nil || offset < 0 {
			http.Error(w, "invalid offset", http.StatusUnprocessableEntity)
			return
		}
	}
	items, err := a.Store.List(r.Context(), status, limit, offset)
	if err != nil {
		http.Error(w, "database unavailable", http.StatusServiceUnavailable)
		return
	}
	writeJSON(w, map[string]any{"messages": items, "limit": limit, "offset": offset})
}

func (a *API) retry(w http.ResponseWriter, r *http.Request) {
	err := a.Store.Retry(r.Context(), chi.URLParam(r, "id"), a.Cfg.Worker.MaxAge, a.Cfg.Worker.HistoryRetention)
	switch {
	case err == nil:
		w.WriteHeader(http.StatusNoContent)
	case errors.Is(err, store.ErrNotFound):
		http.Error(w, "message not found", http.StatusNotFound)
	case errors.Is(err, store.ErrConflict):
		http.Error(w, "message cannot be retried", http.StatusConflict)
	default:
		http.Error(w, "database unavailable", http.StatusServiceUnavailable)
	}
}

func (a *API) metrics(w http.ResponseWriter, r *http.Request) {
	snapshot, err := a.Store.Metrics(r.Context())
	if err != nil {
		http.Error(w, "database unavailable", http.StatusServiceUnavailable)
		return
	}
	oldest := int64(0)
	if snapshot.Oldest > 0 {
		oldest = max(0, time.Now().UnixMilli()-snapshot.Oldest) / 1000
	}
	registry := prometheus.NewRegistry()
	for _, item := range []struct {
		name  string
		help  string
		value int64
	}{
		{"smtp_gateway_pending_recipients", "Pending recipients in the queue.", snapshot.Pending},
		{"smtp_gateway_pending_bytes", "Bytes in messages with pending recipients.", snapshot.Bytes},
		{"smtp_gateway_oldest_pending_seconds", "Age of the oldest pending message in seconds.", oldest},
		{"smtp_gateway_in_flight_messages", "Messages currently leased for delivery.", snapshot.InFlight},
		{"smtp_gateway_accepted_recipients", "Recipients accepted by the upstream relay.", snapshot.Accepted},
		{"smtp_gateway_failed_recipients", "Recipients permanently rejected by the upstream relay.", snapshot.Failed},
		{"smtp_gateway_expired_recipients", "Recipients whose delivery window expired.", snapshot.Expired},
		{"smtp_gateway_retries", "Retry attempts retained in the queue.", snapshot.Retries},
	} {
		metric := prometheus.NewGauge(prometheus.GaugeOpts{Name: item.name, Help: item.help})
		metric.Set(float64(item.value))
		registry.MustRegister(metric)
	}
	failures := prometheus.NewCounter(prometheus.CounterOpts{
		Name: "smtp_gateway_database_write_failures_total",
		Help: "Database write failures while accepting messages.",
	})
	failures.Add(float64(snapshot.DBWriteFailures))
	registry.MustRegister(failures)
	if a.DeliveryDuration != nil {
		registry.MustRegister(a.DeliveryDuration)
	}
	promhttp.HandlerFor(registry, promhttp.HandlerOpts{}).ServeHTTP(w, r)
}
