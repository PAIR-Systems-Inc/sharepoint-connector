package googledrive

import (
	"bytes"
	"context"
	"encoding/json"
	"io"
	"math/rand"
	"net/http"
	"strconv"
	"sync/atomic"
	"time"
)

// Retry/backoff for Drive rate limiting (429, and the rate-limit flavours of
// 403) and transient failures (5xx, network errors).
//
// This exists because the Drive SDK does not retry: every generated call site in
// drive-gen.go uses the non-retrying gensupport.SendRequest, and nothing in it
// honors Retry-After. Without this, a rate-limited sync surfaces the 429 as a
// plain error — the core Retrier would re-queue the file for the next cycle, so
// nothing is lost, but the connector would neither back off politely nor report
// that it is being throttled. Putting the retry in the transport (rather than
// around each call) also means paginated calls retry per HTTP request instead of
// restarting the whole page walk.
//
// The SharePoint provider gets the same behavior from its hand-rolled Graph
// client (see sharepoint/graph.go); both feed the same connector_throttle_events_total metric.
const (
	defaultMaxRetries = 4
	defaultBaseDelay  = 500 * time.Millisecond
	defaultMaxDelay   = 30 * time.Second
	// Cap on honoring a server-provided Retry-After, so an absurd or hostile
	// value cannot wedge a sync for hours.
	defaultMaxRetryAfter = 2 * time.Minute
	// Error bodies are small JSON; cap the peek so a surprise never buffers a
	// large response into memory.
	peekLimit = 32 << 10
)

// throttleHook is called before each backoff sleep: the HTTP status (0 for a
// network error), the upcoming retry number (1-based), and the server-provided
// Retry-After (0 if absent).
type throttleHook func(status, attempt int, retryAfter time.Duration)

// retryTransport wraps an http.RoundTripper with Drive-aware retry and backoff.
// It sits *underneath* Google's auth transport, so requests it replays already
// carry an Authorization header (tokens are hour-lived, and a 401 is never
// retried, so there is nothing to refresh mid-loop).
type retryTransport struct {
	base          http.RoundTripper
	maxRetries    int
	baseDelay     time.Duration
	maxDelay      time.Duration
	maxRetryAfter time.Duration

	hook atomic.Pointer[throttleHook]

	// Overridable in tests.
	sleepFn func(time.Duration)
	randFn  func() float64
	nowFn   func() time.Time
}

func newRetryTransport(base http.RoundTripper) *retryTransport {
	return &retryTransport{
		base:          base,
		maxRetries:    defaultMaxRetries,
		baseDelay:     defaultBaseDelay,
		maxDelay:      defaultMaxDelay,
		maxRetryAfter: defaultMaxRetryAfter,
	}
}

func (t *retryTransport) setHook(fn throttleHook) { t.hook.Store(&fn) }

func (t *retryTransport) RoundTrip(req *http.Request) (*http.Response, error) {
	for attempt := 1; ; attempt++ {
		resp, err := t.base.RoundTrip(req)

		status, retryAfter, retryable := t.classify(req, resp, err)
		if !retryable || attempt > t.maxRetries {
			return resp, err
		}
		// Replay needs a rewindable body; if we cannot rebuild it, hand the
		// caller what we got rather than silently sending a bodiless request.
		next, ok := rewind(req)
		if !ok {
			return resp, err
		}
		if resp != nil {
			drainAndClose(resp.Body)
		}
		if h := t.hook.Load(); h != nil && *h != nil {
			(*h)(status, attempt, retryAfter)
		}
		if !t.sleep(req.Context(), t.backoff(attempt, retryAfter)) {
			return nil, req.Context().Err()
		}
		req = next
	}
}

// classify decides whether this outcome is worth another attempt, and extracts
// the status and any Retry-After for the caller's log/metric.
func (t *retryTransport) classify(req *http.Request, resp *http.Response, err error) (status int, retryAfter time.Duration, retryable bool) {
	if err != nil {
		// Ambiguous — the request may well have reached Drive. Only replay reads;
		// re-sending a watch/stop could create a duplicate channel.
		return 0, 0, isIdempotent(req)
	}
	switch resp.StatusCode {
	case http.StatusTooManyRequests:
	case http.StatusInternalServerError, http.StatusBadGateway,
		http.StatusServiceUnavailable, http.StatusGatewayTimeout:
	case http.StatusForbidden:
		// 403 is overloaded: rate limiting, but also permission denial and
		// permanent policy failures such as exportSizeLimitExceeded, which must
		// never be retried. Only the rate-limit reasons qualify.
		if !isRateLimited403(resp) {
			return resp.StatusCode, 0, false
		}
	default:
		return resp.StatusCode, 0, false
	}
	return resp.StatusCode, parseRetryAfter(resp.Header.Get("Retry-After"), t.now()), true
}

// backoff is the wait before the next attempt: a server-provided Retry-After
// when present (capped), else jittered exponential backoff. attempt is 1-based.
func (t *retryTransport) backoff(attempt int, retryAfter time.Duration) time.Duration {
	if retryAfter > 0 {
		if retryAfter > t.maxRetryAfter {
			return t.maxRetryAfter
		}
		return retryAfter
	}
	d := t.baseDelay << (attempt - 1)
	if d > t.maxDelay || d <= 0 { // d <= 0 guards the shift overflowing
		d = t.maxDelay
	}
	// Jitter across [d/2, d) so a fleet of concurrent syncs doesn't resynchronize
	// onto the same retry instant.
	half := d / 2
	return half + time.Duration(t.rand()*float64(half))
}

func (t *retryTransport) sleep(ctx context.Context, d time.Duration) bool {
	if t.sleepFn != nil {
		t.sleepFn(d)
		return true
	}
	timer := time.NewTimer(d)
	defer timer.Stop()
	select {
	case <-ctx.Done():
		return false
	case <-timer.C:
		return true
	}
}

func (t *retryTransport) rand() float64 {
	if t.randFn != nil {
		return t.randFn()
	}
	return rand.Float64()
}

func (t *retryTransport) now() time.Time {
	if t.nowFn != nil {
		return t.nowFn()
	}
	return time.Now()
}

// isIdempotent reports whether replaying req after an ambiguous failure is safe.
func isIdempotent(req *http.Request) bool {
	return req.Method == http.MethodGet || req.Method == http.MethodHead
}

// rewind returns a request whose body can be sent again, or ok=false when the
// body is not replayable.
func rewind(req *http.Request) (*http.Request, bool) {
	if req.Body == nil || req.Body == http.NoBody {
		return req, true
	}
	if req.GetBody == nil {
		return req, false
	}
	body, err := req.GetBody()
	if err != nil {
		return req, false
	}
	next := req.Clone(req.Context())
	next.Body = body
	return next, true
}

// isRateLimited403 reports whether a 403 is Drive's rate-limit flavour. It
// consumes resp.Body to read the reason and then restores it, so a caller that
// ends up receiving this response still parses a complete googleapi.Error.
func isRateLimited403(resp *http.Response) bool {
	if resp.Body == nil {
		return false
	}
	buf, err := io.ReadAll(io.LimitReader(resp.Body, peekLimit))
	rest := resp.Body
	resp.Body = &rejoinedBody{Reader: io.MultiReader(bytes.NewReader(buf), rest), closer: rest}
	if err != nil {
		return false
	}
	var payload struct {
		Error struct {
			Errors []struct {
				Reason string `json:"reason"`
			} `json:"errors"`
		} `json:"error"`
	}
	if json.Unmarshal(buf, &payload) != nil {
		return false
	}
	for _, e := range payload.Error.Errors {
		switch e.Reason {
		// dailyLimitExceeded is deliberately absent: that quota resets at
		// midnight Pacific, so retrying inside one sync cycle cannot clear it.
		case "rateLimitExceeded", "userRateLimitExceeded", "sharingRateLimitExceeded":
			return true
		}
	}
	return false
}

// rejoinedBody re-presents an already-read prefix followed by the untouched
// remainder, closing the original body underneath.
type rejoinedBody struct {
	io.Reader
	closer io.Closer
}

func (b *rejoinedBody) Close() error { return b.closer.Close() }

// parseRetryAfter reads a Retry-After header in either of its forms — delta
// seconds or an HTTP date. Returns 0 when absent or unparseable.
func parseRetryAfter(v string, now time.Time) time.Duration {
	if v == "" {
		return 0
	}
	if secs, err := strconv.Atoi(v); err == nil {
		if secs <= 0 {
			return 0
		}
		return time.Duration(secs) * time.Second
	}
	if when, err := http.ParseTime(v); err == nil {
		if d := when.Sub(now); d > 0 {
			return d
		}
	}
	return 0
}

// drainAndClose releases a discarded response so the connection can be reused.
func drainAndClose(body io.ReadCloser) {
	if body == nil {
		return
	}
	_, _ = io.Copy(io.Discard, io.LimitReader(body, peekLimit))
	_ = body.Close()
}
