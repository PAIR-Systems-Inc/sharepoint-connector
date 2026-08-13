package googledrive

import (
	"context"
	"errors"
	"io"
	"net/http"
	"strings"
	"sync/atomic"
	"testing"
	"time"
)

// stubRT is a scripted http.RoundTripper: one entry per expected attempt.
type stubRT struct {
	responses []func() (*http.Response, error)
	calls     atomic.Int32
	bodies    []string // body observed on each attempt, to prove replay
}

func (s *stubRT) RoundTrip(req *http.Request) (*http.Response, error) {
	n := int(s.calls.Add(1)) - 1
	body := ""
	if req.Body != nil {
		b, _ := io.ReadAll(req.Body)
		body = string(b)
	}
	s.bodies = append(s.bodies, body)
	if n >= len(s.responses) {
		return nil, errors.New("unexpected extra attempt")
	}
	return s.responses[n]()
}

// resp builds a response with a JSON body, as Drive returns for errors.
func resp(code int, body string) func() (*http.Response, error) {
	return func() (*http.Response, error) {
		return &http.Response{
			StatusCode: code,
			Header:     http.Header{},
			Body:       io.NopCloser(strings.NewReader(body)),
		}, nil
	}
}

func respWithHeader(code int, body string, h http.Header) func() (*http.Response, error) {
	return func() (*http.Response, error) {
		return &http.Response{StatusCode: code, Header: h, Body: io.NopCloser(strings.NewReader(body))}, nil
	}
}

func netErr() func() (*http.Response, error) {
	return func() (*http.Response, error) { return nil, errors.New("connection reset") }
}

// newTestTransport wraps stub with sleeping and jitter disabled, so tests assert
// on the retry decisions rather than on wall-clock behavior.
func newTestTransport(stub http.RoundTripper) (*retryTransport, *[]time.Duration) {
	var slept []time.Duration
	t := newRetryTransport(stub)
	t.sleepFn = func(d time.Duration) { slept = append(slept, d) }
	t.randFn = func() float64 { return 0 } // jitter floor: backoff == d/2
	t.nowFn = func() time.Time { return time.Unix(1700000000, 0) }
	return t, &slept
}

func get(t *testing.T) *http.Request {
	t.Helper()
	r, err := http.NewRequestWithContext(context.Background(), http.MethodGet, "https://www.googleapis.com/drive/v3/files", nil)
	if err != nil {
		t.Fatalf("NewRequest: %v", err)
	}
	return r
}

const rateLimit403 = `{"error":{"code":403,"errors":[{"reason":"userRateLimitExceeded","message":"Rate Limit Exceeded"}]}}`
const exportTooLarge403 = `{"error":{"code":403,"errors":[{"reason":"exportSizeLimitExceeded","message":"too big"}]}}`
const forbidden403 = `{"error":{"code":403,"errors":[{"reason":"insufficientFilePermissions","message":"nope"}]}}`

func TestRetryTransport_RetriesRateLimits(t *testing.T) {
	cases := []struct {
		name  string
		first func() (*http.Response, error)
	}{
		{"429", resp(http.StatusTooManyRequests, `{}`)},
		{"500", resp(http.StatusInternalServerError, `{}`)},
		{"503", resp(http.StatusServiceUnavailable, `{}`)},
		{"403 rate limit", resp(http.StatusForbidden, rateLimit403)},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			stub := &stubRT{responses: []func() (*http.Response, error){tc.first, resp(200, `{"ok":true}`)}}
			tr, slept := newTestTransport(stub)

			r, err := tr.RoundTrip(get(t))
			if err != nil {
				t.Fatalf("RoundTrip: %v", err)
			}
			if r.StatusCode != 200 {
				t.Errorf("status = %d, want 200 after retry", r.StatusCode)
			}
			if got := stub.calls.Load(); got != 2 {
				t.Errorf("attempts = %d, want 2", got)
			}
			if len(*slept) != 1 {
				t.Fatalf("backoffs = %d, want 1", len(*slept))
			}
			// randFn=0 → first backoff is baseDelay/2.
			if want := defaultBaseDelay / 2; (*slept)[0] != want {
				t.Errorf("backoff = %v, want %v", (*slept)[0], want)
			}
		})
	}
}

// The permanent 403s must pass straight through — retrying exportSizeLimitExceeded
// would turn a permanent skip into a stall, which is exactly the bug this guards.
func TestRetryTransport_PermanentForbiddenNotRetried(t *testing.T) {
	for name, body := range map[string]string{
		"export too large": exportTooLarge403,
		"no permission":    forbidden403,
	} {
		t.Run(name, func(t *testing.T) {
			stub := &stubRT{responses: []func() (*http.Response, error){resp(http.StatusForbidden, body)}}
			tr, slept := newTestTransport(stub)

			r, err := tr.RoundTrip(get(t))
			if err != nil {
				t.Fatalf("RoundTrip: %v", err)
			}
			if got := stub.calls.Load(); got != 1 {
				t.Errorf("attempts = %d, want 1 (no retry)", got)
			}
			if len(*slept) != 0 {
				t.Errorf("slept %v, want none", *slept)
			}
			// The body was consumed to read the reason; the caller must still see
			// all of it, or googleapi.Error loses the reason IsExportTooLarge needs.
			got, err := io.ReadAll(r.Body)
			if err != nil {
				t.Fatalf("reading restored body: %v", err)
			}
			if string(got) != body {
				t.Errorf("restored body = %q, want %q", got, body)
			}
		})
	}
}

func TestRetryTransport_HonorsRetryAfter(t *testing.T) {
	h := http.Header{}
	h.Set("Retry-After", "7")
	stub := &stubRT{responses: []func() (*http.Response, error){
		respWithHeader(http.StatusTooManyRequests, `{}`, h),
		resp(200, `{}`),
	}}
	tr, slept := newTestTransport(stub)

	if _, err := tr.RoundTrip(get(t)); err != nil {
		t.Fatalf("RoundTrip: %v", err)
	}
	if len(*slept) != 1 || (*slept)[0] != 7*time.Second {
		t.Errorf("slept %v, want [7s] from Retry-After", *slept)
	}
}

func TestRetryTransport_CapsAbsurdRetryAfter(t *testing.T) {
	h := http.Header{}
	h.Set("Retry-After", "86400") // a day
	stub := &stubRT{responses: []func() (*http.Response, error){
		respWithHeader(http.StatusServiceUnavailable, `{}`, h),
		resp(200, `{}`),
	}}
	tr, slept := newTestTransport(stub)

	if _, err := tr.RoundTrip(get(t)); err != nil {
		t.Fatalf("RoundTrip: %v", err)
	}
	if (*slept)[0] != defaultMaxRetryAfter {
		t.Errorf("slept %v, want the %v cap", (*slept)[0], defaultMaxRetryAfter)
	}
}

func TestRetryTransport_GivesUpAfterMaxRetries(t *testing.T) {
	var script []func() (*http.Response, error)
	for i := 0; i < defaultMaxRetries+1; i++ {
		script = append(script, resp(http.StatusTooManyRequests, `{}`))
	}
	stub := &stubRT{responses: script}
	tr, slept := newTestTransport(stub)

	r, err := tr.RoundTrip(get(t))
	if err != nil {
		t.Fatalf("RoundTrip: %v", err)
	}
	if r.StatusCode != http.StatusTooManyRequests {
		t.Errorf("status = %d, want the final 429 surfaced", r.StatusCode)
	}
	if got, want := int(stub.calls.Load()), defaultMaxRetries+1; got != want {
		t.Errorf("attempts = %d, want %d (1 try + %d retries)", got, want, defaultMaxRetries)
	}
	if len(*slept) != defaultMaxRetries {
		t.Errorf("backoffs = %d, want %d", len(*slept), defaultMaxRetries)
	}
	// Exponential growth, capped: 0.25s, 0.5s, 1s, 2s with randFn=0.
	for i := 1; i < len(*slept); i++ {
		if (*slept)[i] <= (*slept)[i-1] {
			t.Errorf("backoff %v did not grow: %v", i, *slept)
			break
		}
	}
}

// A network error is ambiguous — the request may have been applied. Replaying a
// read is safe; replaying changes.watch could leave a duplicate channel behind.
func TestRetryTransport_NetworkErrorRetriedOnlyForReads(t *testing.T) {
	t.Run("GET retries", func(t *testing.T) {
		stub := &stubRT{responses: []func() (*http.Response, error){netErr(), resp(200, `{}`)}}
		tr, _ := newTestTransport(stub)
		r, err := tr.RoundTrip(get(t))
		if err != nil {
			t.Fatalf("RoundTrip: %v", err)
		}
		if r.StatusCode != 200 || stub.calls.Load() != 2 {
			t.Errorf("status=%d attempts=%d, want 200 after 2", r.StatusCode, stub.calls.Load())
		}
	})

	t.Run("POST does not", func(t *testing.T) {
		stub := &stubRT{responses: []func() (*http.Response, error){netErr(), resp(200, `{}`)}}
		tr, _ := newTestTransport(stub)
		req, err := http.NewRequestWithContext(context.Background(), http.MethodPost,
			"https://www.googleapis.com/drive/v3/changes/watch", strings.NewReader(`{"id":"ch1"}`))
		if err != nil {
			t.Fatalf("NewRequest: %v", err)
		}
		if _, err := tr.RoundTrip(req); err == nil {
			t.Fatal("want the network error surfaced, got nil")
		}
		if stub.calls.Load() != 1 {
			t.Errorf("attempts = %d, want 1 (no replay of a write)", stub.calls.Load())
		}
	})
}

// A rate-limited write IS safe to replay — the server rejected it — but only if
// the body can be rebuilt.
func TestRetryTransport_ReplaysWriteBodyOnRateLimit(t *testing.T) {
	stub := &stubRT{responses: []func() (*http.Response, error){
		resp(http.StatusTooManyRequests, `{}`),
		resp(200, `{}`),
	}}
	tr, _ := newTestTransport(stub)
	req, err := http.NewRequestWithContext(context.Background(), http.MethodPost,
		"https://www.googleapis.com/drive/v3/changes/watch", strings.NewReader(`{"id":"ch1"}`))
	if err != nil {
		t.Fatalf("NewRequest: %v", err)
	}
	if _, err := tr.RoundTrip(req); err != nil {
		t.Fatalf("RoundTrip: %v", err)
	}
	if len(stub.bodies) != 2 {
		t.Fatalf("attempts = %d, want 2", len(stub.bodies))
	}
	if stub.bodies[0] != stub.bodies[1] {
		t.Errorf("replayed body = %q, want the original %q", stub.bodies[1], stub.bodies[0])
	}
}

func TestRetryTransport_ThrottleHook(t *testing.T) {
	h := http.Header{}
	h.Set("Retry-After", "3")
	stub := &stubRT{responses: []func() (*http.Response, error){
		respWithHeader(http.StatusTooManyRequests, `{}`, h),
		resp(http.StatusForbidden, rateLimit403),
		resp(200, `{}`),
	}}
	tr, _ := newTestTransport(stub)

	type call struct {
		status, attempt int
		retryAfter      time.Duration
	}
	var got []call
	tr.setHook(func(status, attempt int, retryAfter time.Duration) {
		got = append(got, call{status, attempt, retryAfter})
	})

	if _, err := tr.RoundTrip(get(t)); err != nil {
		t.Fatalf("RoundTrip: %v", err)
	}
	want := []call{{429, 1, 3 * time.Second}, {403, 2, 0}}
	if len(got) != len(want) {
		t.Fatalf("hook calls = %v, want %v", got, want)
	}
	for i := range want {
		if got[i] != want[i] {
			t.Errorf("hook call %d = %+v, want %+v", i, got[i], want[i])
		}
	}
}

func TestRetryTransport_NoHookOnSuccess(t *testing.T) {
	stub := &stubRT{responses: []func() (*http.Response, error){resp(200, `{}`)}}
	tr, _ := newTestTransport(stub)
	fired := false
	tr.setHook(func(int, int, time.Duration) { fired = true })
	if _, err := tr.RoundTrip(get(t)); err != nil {
		t.Fatalf("RoundTrip: %v", err)
	}
	if fired {
		t.Error("throttle hook fired on a successful request")
	}
}

// Cancellation must abort the wait rather than sleep out the full backoff.
func TestRetryTransport_ContextCancelledDuringBackoff(t *testing.T) {
	stub := &stubRT{responses: []func() (*http.Response, error){
		resp(http.StatusTooManyRequests, `{}`),
		resp(200, `{}`),
	}}
	tr := newRetryTransport(stub) // real sleep, so the cancel is what unblocks it
	tr.randFn = func() float64 { return 0 }
	tr.baseDelay = time.Hour

	ctx, cancel := context.WithCancel(context.Background())
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, "https://www.googleapis.com/drive/v3/files", nil)
	if err != nil {
		t.Fatalf("NewRequest: %v", err)
	}
	go func() {
		time.Sleep(20 * time.Millisecond)
		cancel()
	}()
	start := time.Now()
	if _, err := tr.RoundTrip(req); !errors.Is(err, context.Canceled) {
		t.Fatalf("err = %v, want context.Canceled", err)
	}
	if elapsed := time.Since(start); elapsed > 5*time.Second {
		t.Errorf("took %v — the backoff was not interrupted", elapsed)
	}
}

func TestParseRetryAfter(t *testing.T) {
	now := time.Unix(1700000000, 0).UTC()
	cases := []struct {
		in   string
		want time.Duration
	}{
		{"", 0},
		{"5", 5 * time.Second},
		{"0", 0},
		{"-3", 0},
		{"garbage", 0},
		{now.Add(30 * time.Second).Format(http.TimeFormat), 30 * time.Second},
		{now.Add(-30 * time.Second).Format(http.TimeFormat), 0}, // already past
	}
	for _, tc := range cases {
		if got := parseRetryAfter(tc.in, now); got != tc.want {
			t.Errorf("parseRetryAfter(%q) = %v, want %v", tc.in, got, tc.want)
		}
	}
}
