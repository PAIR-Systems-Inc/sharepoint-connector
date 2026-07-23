// Package server implements the event-triggered listener: an HTTP server that
// receives Microsoft Graph change notifications and drives a delta sync. It is
// the Go port of listener.py's web surface. The listener must be deployed
// somewhere publicly reachable over HTTPS — Graph POSTs notifications to it.
package server

import (
	"context"
	"encoding/json"
	"io"
	"log/slog"
	"net/http"
	"strconv"
	"sync"
	"time"

	"github.com/PAIR-Systems-Inc/goodmem-connectors/internal/core/syncer"
)

// Notifier is invoked (asynchronously) when a validated change notification
// arrives, carrying the number of changes reported.
type Notifier func(count int)

// SyncHistory provides recent sync records for the GET /syncs endpoint (the
// SQLite store implements it). nil = the endpoint reports "not enabled".
type SyncHistory interface {
	Recent(limit int, status string) ([]syncer.SyncRecord, error)
}

// Server holds the webhook HTTP surface and an in-memory activity log.
// (Durable state — delta cursor, pending sets, persisted activity — is a
// follow-up; today the log is in-memory like the Python PoC.)
type Server struct {
	// clientState is the shared secret Graph echoes back in each notification;
	// it authenticates that a notification genuinely came from our subscription.
	clientState string
	onNotify    Notifier

	// Metrics is exposed at GET /metrics (Prometheus text format).
	Metrics *Metrics

	// History, if set, backs GET /syncs (durable sync-history query).
	History SyncHistory

	// readyFn reports readiness for GET /readyz (nil = always ready).
	readyFn func() bool

	// logger emits structured logs (in addition to the in-memory /activity ring).
	logger *slog.Logger

	mu       sync.Mutex
	activity []Event
	maxLog   int
}

// Event is one entry in the activity log.
type Event struct {
	TS      time.Time `json:"ts"`
	Type    string    `json:"type"`
	Message string    `json:"message"`
}

// New returns a Server. onNotify is called (in a goroutine) for each validated
// notification; pass nil to only record activity.
func New(clientState string, onNotify Notifier) *Server {
	return &Server{clientState: clientState, onNotify: onNotify, maxLog: 500, Metrics: NewMetrics(), logger: slog.Default()}
}

// SetReadyFn registers a readiness predicate for GET /readyz. Until it returns
// true (e.g. the startup sync completed and the subscription is ensured),
// /readyz answers 503 so a load balancer won't route to a not-yet-ready listener.
func (s *Server) SetReadyFn(fn func() bool) { s.readyFn = fn }

// Handler returns the HTTP routes: the Graph webhook, health, activity, metrics.
func (s *Server) Handler() http.Handler {
	mux := http.NewServeMux()
	mux.HandleFunc("/sync/webhook", s.handleWebhook)
	mux.HandleFunc("/activity", s.handleActivity)
	mux.HandleFunc("/healthz", func(w http.ResponseWriter, _ *http.Request) { w.WriteHeader(http.StatusOK) })
	mux.HandleFunc("/readyz", func(w http.ResponseWriter, _ *http.Request) {
		if s.readyFn == nil || s.readyFn() {
			w.WriteHeader(http.StatusOK)
			return
		}
		http.Error(w, "not ready: startup sync/subscription not complete", http.StatusServiceUnavailable)
	})
	mux.HandleFunc("/metrics", func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "text/plain; version=0.0.4; charset=utf-8")
		s.Metrics.WritePrometheus(w)
	})
	mux.HandleFunc("/syncs", s.handleSyncs)
	return mux
}

// handleSyncs serves the durable sync history as JSON:
//
//	GET /syncs?limit=100&status=failure
func (s *Server) handleSyncs(w http.ResponseWriter, r *http.Request) {
	if s.History == nil {
		http.Error(w, "sync history not enabled", http.StatusNotFound)
		return
	}
	limit := 100
	if v, err := strconv.Atoi(r.URL.Query().Get("limit")); err == nil && v > 0 {
		limit = v
	}
	recs, err := s.History.Recent(limit, r.URL.Query().Get("status"))
	if err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}
	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(map[string]any{"syncs": recs})
}

// graphNotification is the subset of a Graph change-notification payload we read.
type graphNotification struct {
	Value []struct {
		ClientState string `json:"clientState"`
		Resource    string `json:"resource"`
		ChangeType  string `json:"changeType"`
	} `json:"value"`
}

// handleWebhook implements the Graph webhook contract:
//   - Subscription validation: a GET/POST carrying ?validationToken=... must
//     echo the token back verbatim as text/plain within 10s.
//   - Change notification: a POST whose body carries our clientState; any value
//     with a mismatched clientState is rejected (spoof protection).
func (s *Server) handleWebhook(w http.ResponseWriter, r *http.Request) {
	if token := r.URL.Query().Get("validationToken"); token != "" {
		w.Header().Set("Content-Type", "text/plain")
		w.WriteHeader(http.StatusOK)
		_, _ = io.WriteString(w, token)
		return
	}
	if r.Method != http.MethodPost {
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}
	body, _ := io.ReadAll(io.LimitReader(r.Body, 1<<20)) // cap at 1 MiB
	var n graphNotification
	if err := json.Unmarshal(body, &n); err != nil {
		http.Error(w, "bad request", http.StatusBadRequest)
		return
	}
	// Reject if any notification carries a clientState that isn't ours.
	for _, v := range n.Value {
		if v.ClientState != s.clientState {
			s.log("rejected", "notification with invalid clientState")
			http.Error(w, "invalid clientState", http.StatusUnauthorized)
			return
		}
	}
	// Ack immediately (Graph requires a fast 2xx); process out of band.
	w.WriteHeader(http.StatusAccepted)
	count := len(n.Value)
	s.log("notification_received", "received change notification")
	if s.onNotify != nil && count > 0 {
		go s.onNotify(count)
	}
}

func (s *Server) handleActivity(w http.ResponseWriter, _ *http.Request) {
	s.mu.Lock()
	events := append([]Event(nil), s.activity...)
	s.mu.Unlock()
	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(map[string]any{"events": events})
}

// Log records an activity event (also used by the Listener orchestration).
func (s *Server) Log(typ, msg string) { s.log(typ, msg) }

func (s *Server) log(typ, msg string) {
	s.mu.Lock()
	s.activity = append(s.activity, Event{TS: time.Now().UTC(), Type: typ, Message: msg})
	if len(s.activity) > s.maxLog {
		s.activity = s.activity[len(s.activity)-s.maxLog:]
	}
	s.mu.Unlock()
	// Also emit a structured log so operational events reach stdout/stderr (Fly
	// logs, log shippers), not only the in-memory /activity ring.
	if s.logger != nil {
		s.logger.Log(context.Background(), levelForType(typ), msg, "event", typ)
	}
}

// levelForType maps an activity event type to an slog level.
func levelForType(typ string) slog.Level {
	switch typ {
	case "error":
		return slog.LevelError
	case "warn", "rejected":
		return slog.LevelWarn
	default:
		return slog.LevelInfo
	}
}
