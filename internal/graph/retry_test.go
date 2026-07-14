package graph

import (
	"net/http"
	"net/http/httptest"
	"sync/atomic"
	"testing"
	"time"
)

func TestShouldRetryStatus(t *testing.T) {
	retry := []int{429, 500, 502, 503, 504}
	noRetry := []int{200, 201, 204, 301, 400, 401, 403, 404, 410}
	for _, s := range retry {
		if !shouldRetryStatus(s) {
			t.Errorf("shouldRetryStatus(%d) = false, want true", s)
		}
	}
	for _, s := range noRetry {
		if shouldRetryStatus(s) {
			t.Errorf("shouldRetryStatus(%d) = true, want false", s)
		}
	}
}

func TestParseRetryAfter(t *testing.T) {
	// delta-seconds
	if got := parseRetryAfter("5"); got != 5*time.Second {
		t.Errorf("parseRetryAfter(\"5\") = %v, want 5s", got)
	}
	// empty / unparseable / non-positive -> 0
	for _, in := range []string{"", "   ", "abc", "0", "-3"} {
		if got := parseRetryAfter(in); got != 0 {
			t.Errorf("parseRetryAfter(%q) = %v, want 0", in, got)
		}
	}
	// a past HTTP-date -> 0 (already elapsed)
	if got := parseRetryAfter("Mon, 02 Jan 2006 15:04:05 GMT"); got != 0 {
		t.Errorf("parseRetryAfter(past date) = %v, want 0", got)
	}
	// a future HTTP-date -> positive
	future := time.Now().Add(30 * time.Second).UTC().Format(http.TimeFormat)
	if got := parseRetryAfter(future); got <= 0 {
		t.Errorf("parseRetryAfter(future date) = %v, want > 0", got)
	}
}

func TestBackoff(t *testing.T) {
	c := &Client{baseBackoff: 100 * time.Millisecond, maxBackoff: 30 * time.Second}

	// Retry-After is honored verbatim, capped at maxRetryAfter.
	if got := c.backoff(0, 7*time.Second); got != 7*time.Second {
		t.Errorf("backoff honoring Retry-After = %v, want 7s", got)
	}
	if got := c.backoff(3, 10*time.Minute); got != maxRetryAfter {
		t.Errorf("backoff Retry-After cap = %v, want %v", got, maxRetryAfter)
	}

	// Full-jitter exponential: result ∈ [d/2, d] with d = base·2^attempt (capped).
	for attempt := 0; attempt < 6; attempt++ {
		d := c.baseBackoff
		for i := 0; i < attempt && d < c.maxBackoff; i++ {
			d *= 2
		}
		if d > c.maxBackoff {
			d = c.maxBackoff
		}
		for i := 0; i < 50; i++ {
			got := c.backoff(attempt, 0)
			if got < d/2 || got > d {
				t.Fatalf("backoff(%d,0) = %v, want in [%v, %v]", attempt, got, d/2, d)
			}
		}
	}
}

// TestSend_RetriesThenSucceeds verifies an authenticated call retries through
// 429 and 503 and then returns the 200 body, firing OnThrottle each retry.
func TestSend_RetriesThenSucceeds(t *testing.T) {
	var calls int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch atomic.AddInt32(&calls, 1) {
		case 1:
			w.Header().Set("Retry-After", "0") // 0 => fall back to jittered backoff
			w.WriteHeader(http.StatusTooManyRequests)
		case 2:
			w.WriteHeader(http.StatusServiceUnavailable)
		default:
			w.WriteHeader(http.StatusOK)
			_, _ = w.Write([]byte("ok"))
		}
	}))
	defer srv.Close()

	var throttles int32
	c := newTestClient(srv.URL)
	c.OnThrottle = func(status, attempt int, ra time.Duration) { atomic.AddInt32(&throttles, 1) }

	body, status, err := c.send(http.MethodGet, srv.URL+"/x", "tok", nil)
	if err != nil {
		t.Fatalf("send: %v", err)
	}
	if status != http.StatusOK || string(body) != "ok" {
		t.Fatalf("send = (%d, %q), want (200, \"ok\")", status, body)
	}
	if got := atomic.LoadInt32(&calls); got != 3 {
		t.Errorf("server calls = %d, want 3", got)
	}
	if got := atomic.LoadInt32(&throttles); got != 2 {
		t.Errorf("OnThrottle fired %d times, want 2", got)
	}
}

// TestSend_ExhaustsRetries verifies it stops after maxRetries and returns the
// last (still-throttled) response rather than retrying forever.
func TestSend_ExhaustsRetries(t *testing.T) {
	var calls int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		atomic.AddInt32(&calls, 1)
		w.WriteHeader(http.StatusTooManyRequests)
	}))
	defer srv.Close()

	c := newTestClient(srv.URL)
	c.maxRetries = 2
	_, status, err := c.send(http.MethodGet, srv.URL+"/x", "tok", nil)
	if err != nil {
		t.Fatalf("send: %v", err)
	}
	if status != http.StatusTooManyRequests {
		t.Errorf("status = %d, want 429", status)
	}
	if got := atomic.LoadInt32(&calls); got != 3 { // 1 initial + 2 retries
		t.Errorf("server calls = %d, want 3", got)
	}
}

// TestSend_HonorsRetryAfterSeconds verifies a numeric Retry-After drives the
// wait (observed via the stubbed sleepFn), not the jittered fallback.
func TestSend_HonorsRetryAfterSeconds(t *testing.T) {
	var calls int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if atomic.AddInt32(&calls, 1) == 1 {
			w.Header().Set("Retry-After", "2")
			w.WriteHeader(http.StatusServiceUnavailable)
			return
		}
		w.WriteHeader(http.StatusOK)
	}))
	defer srv.Close()

	var slept []time.Duration
	c := newTestClient(srv.URL)
	c.sleepFn = func(d time.Duration) { slept = append(slept, d) } // don't actually sleep

	if _, status, err := c.send(http.MethodGet, srv.URL+"/x", "tok", nil); err != nil || status != 200 {
		t.Fatalf("send = (%d, %v), want (200, nil)", status, err)
	}
	if len(slept) != 1 || slept[0] != 2*time.Second {
		t.Errorf("slept = %v, want [2s]", slept)
	}
}

// newTestClient builds a client whose sleepFn is a no-op (so retries don't add
// real wall-clock) pointed at a test server.
func newTestClient(base string) *Client {
	c := NewClient("cid", "tid", "sec", "https://contoso.sharepoint.com/sites/Test")
	c.graphBase = base
	c.loginBase = base
	c.sleepFn = func(time.Duration) {}
	return c
}
