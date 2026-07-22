package server

import (
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
	"time"
)

func TestWebhookValidationHandshake(t *testing.T) {
	srv := httptest.NewServer(New("secret", nil).Handler())
	defer srv.Close()

	resp, err := http.Post(srv.URL+"/sync/webhook?validationToken=abc123", "text/plain", nil)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("status = %d, want 200", resp.StatusCode)
	}
	if ct := resp.Header.Get("Content-Type"); !strings.HasPrefix(ct, "text/plain") {
		t.Errorf("content-type = %q, want text/plain", ct)
	}
	b, _ := io.ReadAll(resp.Body)
	if string(b) != "abc123" {
		t.Errorf("echoed %q, want abc123", b)
	}
}

func TestWebhookRejectsBadClientState(t *testing.T) {
	var called int32
	srv := httptest.NewServer(New("secret", func(int) { atomic.AddInt32(&called, 1) }).Handler())
	defer srv.Close()

	body := `{"value":[{"clientState":"WRONG","resource":"drives/x/root"}]}`
	resp, err := http.Post(srv.URL+"/sync/webhook", "application/json", strings.NewReader(body))
	if err != nil {
		t.Fatal(err)
	}
	if resp.StatusCode != http.StatusUnauthorized {
		t.Errorf("status = %d, want 401", resp.StatusCode)
	}
	time.Sleep(50 * time.Millisecond)
	if atomic.LoadInt32(&called) != 0 {
		t.Error("onNotify fired for a spoofed clientState")
	}
}

func TestWebhookAcceptsValidNotification(t *testing.T) {
	done := make(chan int, 1)
	srv := httptest.NewServer(New("secret", func(c int) { done <- c }).Handler())
	defer srv.Close()

	body := `{"value":[{"clientState":"secret","resource":"drives/x/root","changeType":"updated"}]}`
	resp, err := http.Post(srv.URL+"/sync/webhook", "application/json", strings.NewReader(body))
	if err != nil {
		t.Fatal(err)
	}
	if resp.StatusCode != http.StatusAccepted {
		t.Errorf("status = %d, want 202", resp.StatusCode)
	}
	select {
	case c := <-done:
		if c != 1 {
			t.Errorf("notify count = %d, want 1", c)
		}
	case <-time.After(2 * time.Second):
		t.Error("onNotify was not called for a valid notification")
	}
}
