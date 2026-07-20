package server

import (
	"context"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/PAIR-Systems-Inc/sharepoint-connector/internal/fakes"
	"github.com/PAIR-Systems-Inc/sharepoint-connector/internal/gm"
	"github.com/PAIR-Systems-Inc/sharepoint-connector/internal/graph"
)

// TestIntegration_ListenerMetrics runs the REAL Listener (its real HTTP server)
// against fake SharePoint + Goodmem servers, then scrapes /metrics, /healthz, and
// /activity over real HTTP — verifying the startup full sync is reflected in the
// Prometheus metrics end-to-end.
func TestIntegration_ListenerMetrics(t *testing.T) {
	fg := fakes.NewGraph()
	gsrv := httptest.NewServer(fg.Handler())
	defer gsrv.Close()
	fg.SetBase(gsrv.URL)
	fg.Put(fakes.File{ID: "a", Name: "a.pdf", Mime: "application/pdf", Modified: "2026-01-01T00:00:00Z", Content: "A"})
	fg.Put(fakes.File{ID: "b", Name: "b.pdf", Mime: "application/pdf", Modified: "2026-01-01T00:00:00Z", Content: "B"})

	fm := fakes.NewGoodmem()
	msrv := httptest.NewServer(fm.Handler())
	defer msrv.Close()

	gc := graph.NewClient("cid", "tid", "sec", "https://contoso.sharepoint.com/sites/Test",
		graph.WithBaseURLs(gsrv.URL, gsrv.URL))
	gmc, err := gm.New(msrv.URL, "key")
	if err != nil {
		t.Fatal(err)
	}

	port := freePort(t)
	l := &Listener{
		GC:              gc,
		GM:              gmc,
		SpaceID:         "space-1",
		ClientState:     "cs",
		NotificationURL: "https://example.test/sync/webhook",
		SubMinutes:      4320,
		FullSyncMinutes: 0, // disable the periodic loop so counts stay deterministic
		Port:            port,
		DeltaPath:       filepath.Join(t.TempDir(), "graph_delta_link"),
	}
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	go func() { _ = l.Run(ctx) }()

	base := "http://127.0.0.1:" + port
	// Poll /metrics over real HTTP until the startup full sync (2 adds) lands.
	var body string
	deadline := time.Now().Add(10 * time.Second)
	for time.Now().Before(deadline) {
		resp, err := http.Get(base + "/metrics")
		if err == nil {
			b, _ := io.ReadAll(resp.Body)
			resp.Body.Close()
			body = string(b)
			if strings.Contains(body, "\nsharepoint_files_added_total 2\n") {
				break
			}
		}
		time.Sleep(50 * time.Millisecond)
	}

	for _, want := range []string{
		"\nsharepoint_files_added_total 2\n",
		"\nsharepoint_full_syncs_total 1\n",
		"\nsharepoint_up 1\n",
		"# TYPE sharepoint_files_added_total counter",
		"# TYPE sharepoint_up gauge",
	} {
		if !strings.Contains(body, want) {
			t.Errorf("missing %q in /metrics output:\n%s", want, body)
		}
	}

	// The same real server answers /healthz and /activity.
	if resp, err := http.Get(base + "/healthz"); err != nil || resp.StatusCode != http.StatusOK {
		t.Errorf("healthz: err=%v status=%v", err, statusOf(resp))
	}
	if resp, err := http.Get(base + "/activity"); err != nil || resp.StatusCode != http.StatusOK {
		t.Errorf("activity: err=%v status=%v", err, statusOf(resp))
	} else {
		b, _ := io.ReadAll(resp.Body)
		resp.Body.Close()
		if !strings.Contains(string(b), "full sync done") {
			t.Errorf("activity log missing the startup sync: %s", b)
		}
	}
}

func statusOf(r *http.Response) int {
	if r == nil {
		return 0
	}
	return r.StatusCode
}

// freePort returns a currently-free TCP port (as a string) for the test server.
func freePort(t *testing.T) string {
	t.Helper()
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer ln.Close()
	_, port, _ := net.SplitHostPort(ln.Addr().String())
	return port
}
