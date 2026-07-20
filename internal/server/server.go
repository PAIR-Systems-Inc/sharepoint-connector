// Package server implements the event-triggered listener: an HTTP server that
// receives Microsoft Graph change notifications and drives a delta sync. It is
// the Go port of listener.py's web surface. The listener must be deployed
// somewhere publicly reachable over HTTPS — Graph POSTs notifications to it.
package server

import (
	"encoding/json"
	"io"
	"net/http"
	"sync"
	"time"
)

// Notifier is invoked (asynchronously) when a validated change notification
// arrives, carrying the number of changes reported.
type Notifier func(count int)

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
	return &Server{clientState: clientState, onNotify: onNotify, maxLog: 500, Metrics: NewMetrics()}
}

// Handler returns the HTTP routes: the Graph webhook, health, activity, metrics.
func (s *Server) Handler() http.Handler {
	mux := http.NewServeMux()
	mux.HandleFunc("/sync/webhook", s.handleWebhook)
	mux.HandleFunc("/activity", s.handleActivity)
	mux.HandleFunc("/healthz", func(w http.ResponseWriter, _ *http.Request) { w.WriteHeader(http.StatusOK) })
	mux.HandleFunc("/metrics", func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "text/plain; version=0.0.4; charset=utf-8")
		s.Metrics.WritePrometheus(w)
	})
	return mux
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
	defer s.mu.Unlock()
	s.activity = append(s.activity, Event{TS: time.Now().UTC(), Type: typ, Message: msg})
	if len(s.activity) > s.maxLog {
		s.activity = s.activity[len(s.activity)-s.maxLog:]
	}
}
