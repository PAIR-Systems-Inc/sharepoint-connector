// Command connector syncs a SharePoint site to a Goodmem space.
//
// It replaces the Python proof-of-concept (listener.py, sync_once.py,
// watch_listener.py) with a single Go binary exposing subcommands. This is the
// distributed artifact — shipping as a compiled binary keeps the source closed.
package main

import (
	"context"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"log/slog"
	"net/http"
	"os"
	"os/signal"
	"strconv"
	"strings"
	"syscall"
	"time"

	"fury.io/pairsys/goodmem"

	"github.com/PAIR-Systems-Inc/goodmem-connectors/internal/core/config"
	"github.com/PAIR-Systems-Inc/goodmem-connectors/internal/core/gm"
	"github.com/PAIR-Systems-Inc/goodmem-connectors/internal/core/server"
	"github.com/PAIR-Systems-Inc/goodmem-connectors/internal/core/syncer"
	"github.com/PAIR-Systems-Inc/goodmem-connectors/internal/providers/sharepoint"
)

func main() {
	if len(os.Args) < 2 {
		usage(os.Stderr)
		os.Exit(2)
	}
	var err error
	switch os.Args[1] {
	case "sync-once":
		err = runSyncOnce(os.Args[2:])
	case "serve":
		err = runServe(os.Args[2:])
	case "create-subscription":
		err = runCreateSubscription(os.Args[2:])
	case "watch":
		err = runWatch(os.Args[2:])
	case "-h", "--help", "help":
		usage(os.Stdout)
	default:
		fmt.Fprintf(os.Stderr, "unknown command: %s\n\n", os.Args[1])
		usage(os.Stderr)
		os.Exit(2)
	}
	if err != nil {
		fmt.Fprintln(os.Stderr, "Error:", err)
		os.Exit(1)
	}
}

// --- sync-once ---

func runSyncOnce(args []string) error {
	fs := flag.NewFlagSet("sync-once", flag.ExitOnError)
	envFile := fs.String("env-file", "", "env file to load (default: process env, plus .env if present)")
	dryRun := fs.Bool("dry-run", false, "compute the sync plan without changing Goodmem")
	_ = fs.Parse(args)

	cfg, err := loadConfig(*envFile)
	if err != nil {
		return err
	}
	gc, gmc, err := buildClients(cfg)
	if err != nil {
		return err
	}
	ctx := context.Background()
	spaceID, err := syncer.ResolveSpaceID(ctx, gmc, cfg.GoodmemSpaceID, cfg.SharePointSiteURL, cfg.GoodmemEmbedderID, cfg.OpenAIAPIKey)
	if err != nil {
		return err
	}
	fmt.Printf("Space: %s\n", spaceID)

	res, err := syncer.RunFull(ctx, gc, gmc, spaceID, syncer.Options{
		FolderPath:        cfg.SharePointFolderPath,
		ExtractPageImages: cfg.ExtractPageImages,
		DryRun:            *dryRun,
		MaxFileBytes:      int64(atoiOr(os.Getenv("SHAREPOINT_MAX_FILE_MB"), 100)) * 1024 * 1024,
		MaxDeleteRatio:    floatOr(os.Getenv("GRAPH_MAX_DELETE_RATIO"), 0.5),
	})
	if err != nil {
		return err
	}
	fmt.Printf("SharePoint files: %d   Goodmem memories: %d\n", res.SharePointFiles, res.GoodmemMemories)
	fmt.Printf("Plan: +%d add   ~%d update   -%d delete\n", len(res.Plan.Add), len(res.Plan.Update), len(res.Plan.Delete))
	if n := len(res.Plan.UnexpectedNewer); n > 0 {
		fmt.Printf("Warning: %d file(s) have a Goodmem timestamp ≥ SharePoint (skipped): %v\n", n, res.Plan.UnexpectedNewer)
	}
	if *dryRun {
		fmt.Println("(dry run — no changes applied)")
		return nil
	}
	fmt.Printf("Applied: %d added, %d updated, %d deleted, %d skipped\n", res.Added, res.Updated, res.Deleted, res.Skipped)
	for _, e := range res.Errors {
		fmt.Fprintln(os.Stderr, "  ! "+e)
	}
	if len(res.Errors) > 0 {
		return fmt.Errorf("%d item(s) failed", len(res.Errors))
	}
	fmt.Println("Sync complete.")
	return nil
}

// --- serve ---

func runServe(args []string) error {
	fs := flag.NewFlagSet("serve", flag.ExitOnError)
	envFile := fs.String("env-file", "", "env file to load (default: process env, plus .env if present)")
	_ = fs.Parse(args)

	cfg, err := loadConfig(*envFile)
	if err != nil {
		return err
	}
	configureLogging() // structured logs to stderr (Fly logs / shippers)
	if strings.TrimSpace(cfg.GraphClientState) == "" {
		return errors.New("GRAPH_CLIENT_STATE is required for serve")
	}
	if strings.TrimSpace(cfg.GraphNotificationURL) == "" {
		return errors.New("GRAPH_NOTIFICATION_URL is required for serve")
	}
	gc, gmc, err := buildClients(cfg)
	if err != nil {
		return err
	}
	spaceID, err := syncer.ResolveSpaceID(context.Background(), gmc, cfg.GoodmemSpaceID, cfg.SharePointSiteURL, cfg.GoodmemEmbedderID, cfg.OpenAIAPIKey)
	if err != nil {
		return err
	}

	port := firstNonEmpty(os.Getenv("PORT"), cfg.GraphPort, "5000")
	deltaPath := firstNonEmpty(os.Getenv("GRAPH_DELTA_TOKEN_FILE"), ".graph_delta_link")
	subMin := atoiOr(cfg.GraphSubscriptionMinutes, sharepoint.SubMinutesDefault)
	// Periodic safety full-sync: defaults to the subscription-renewal cadence
	// (~half the subscription lifetime). Set GRAPH_FULL_SYNC_MINUTES=0 to disable.
	fullSyncMin := atoiOr(os.Getenv("GRAPH_FULL_SYNC_MINUTES"), max(subMin/2, 20))

	l := &server.Listener{
		GC:                gc,
		GM:                gmc,
		SpaceID:           spaceID,
		ClientState:       cfg.GraphClientState,
		NotificationURL:   cfg.GraphNotificationURL,
		SubMinutes:        subMin,
		FullSyncMinutes:   fullSyncMin,
		Port:              port,
		DeltaPath:         deltaPath,
		ExtractPageImages: cfg.ExtractPageImages,
		MaxItemAttempts:   atoiOr(os.Getenv("GRAPH_MAX_ITEM_ATTEMPTS"), 10),
		MaxDeleteRatio:    floatOr(os.Getenv("GRAPH_MAX_DELETE_RATIO"), 0.5),
		MaxFileBytes:      int64(atoiOr(os.Getenv("SHAREPOINT_MAX_FILE_MB"), 100)) * 1024 * 1024,
		RetentionDays:     atoiOr(os.Getenv("SYNC_HISTORY_RETENTION_DAYS"), 90),
		IgnoredFolderPath: strings.TrimSpace(cfg.SharePointFolderPath),
	}
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()
	fmt.Printf("Listener on :%s   space=%s   webhook=%s\n", port, spaceID, cfg.GraphNotificationURL)
	return l.Run(ctx)
}

// --- create-subscription ---

func runCreateSubscription(args []string) error {
	fs := flag.NewFlagSet("create-subscription", flag.ExitOnError)
	envFile := fs.String("env-file", "", "env file to load (default: .env if present)")
	_ = fs.Parse(args)

	cfg, err := loadConfig(*envFile)
	if err != nil {
		return err
	}
	if cfg.GraphClientState == "" || cfg.GraphNotificationURL == "" {
		return errors.New("GRAPH_CLIENT_STATE and GRAPH_NOTIFICATION_URL are required")
	}
	gc := sharepoint.NewClient(cfg.AzureClientID, cfg.AzureTenantID, cfg.AzureClientSecret, cfg.SharePointSiteURL)
	sub, err := gc.EnsureSubscription(cfg.GraphNotificationURL, cfg.GraphClientState, atoiOr(cfg.GraphSubscriptionMinutes, sharepoint.SubMinutesDefault))
	if err != nil {
		return err
	}
	fmt.Printf("Subscription ready: id=%s\n  resource=%s\n  expires=%s\n", sub.ID, sub.Resource, sub.ExpirationDateTime)
	return nil
}

// --- watch ---

func runWatch(args []string) error {
	fs := flag.NewFlagSet("watch", flag.ExitOnError)
	interval := fs.Float64("n", 2, "poll interval in seconds")
	_ = fs.Parse(args)
	if fs.NArg() < 1 {
		return errors.New("usage: connector watch [-n SECS] <listener-base-url>")
	}
	base := strings.TrimRight(fs.Arg(0), "/")
	if strings.HasSuffix(base, "/sync/webhook") {
		base = strings.TrimSuffix(base, "/sync/webhook")
	}
	fmt.Printf("Watching %s/activity every %.1fs (Ctrl+C to stop)\n", base, *interval)

	seen := 0
	for {
		resp, err := http.Get(base + "/activity")
		if err != nil {
			fmt.Fprintln(os.Stderr, "  poll error:", err)
		} else {
			var data struct {
				Events []server.Event `json:"events"`
			}
			_ = json.NewDecoder(resp.Body).Decode(&data)
			resp.Body.Close()
			if seen > len(data.Events) {
				seen = 0 // log rotated/truncated
			}
			for _, e := range data.Events[seen:] {
				fmt.Printf("  %s  [%s]  %s\n", e.TS.Local().Format("2006-01-02 15:04:05"), e.Type, e.Message)
			}
			seen = len(data.Events)
		}
		time.Sleep(time.Duration(*interval * float64(time.Second)))
	}
}

// --- shared helpers ---

// loadConfig loads from envFile (or .env when present) and validates the fields
// common to all syncing commands.
func loadConfig(envFile string) (*config.Config, error) {
	if envFile == "" {
		if _, err := os.Stat(".env"); err == nil {
			envFile = ".env"
		}
	}
	cfg, err := config.Load(envFile)
	if err != nil {
		return nil, err
	}
	if err := cfg.ValidateSync(); err != nil {
		return nil, err
	}
	if err := sharepoint.ValidateTokenRefreshBuffer(); err != nil {
		return nil, err
	}
	return cfg, nil
}

func buildClients(cfg *config.Config) (*sharepoint.Client, *goodmem.Client, error) {
	gc := sharepoint.NewClient(cfg.AzureClientID, cfg.AzureTenantID, cfg.AzureClientSecret, cfg.SharePointSiteURL)
	gmc, err := gm.New(cfg.GoodmemBaseURL, cfg.GoodmemAPIKey)
	if err != nil {
		return nil, nil, fmt.Errorf("goodmem client: %w", err)
	}
	return gc, gmc, nil
}

func firstNonEmpty(vals ...string) string {
	for _, v := range vals {
		if strings.TrimSpace(v) != "" {
			return v
		}
	}
	return ""
}

func atoiOr(s string, def int) int {
	if n, err := strconv.Atoi(strings.TrimSpace(s)); err == nil {
		return n
	}
	return def
}

func floatOr(s string, def float64) float64 {
	if f, err := strconv.ParseFloat(strings.TrimSpace(s), 64); err == nil {
		return f
	}
	return def
}

// configureLogging installs the process-wide structured logger for the listener,
// honoring LOG_LEVEL (debug|info|warn|error, default info) and LOG_FORMAT
// (json|text, default json). JSON suits log shippers; text is friendlier locally.
func configureLogging() {
	level := slog.LevelInfo
	switch strings.ToLower(strings.TrimSpace(os.Getenv("LOG_LEVEL"))) {
	case "debug":
		level = slog.LevelDebug
	case "warn":
		level = slog.LevelWarn
	case "error":
		level = slog.LevelError
	}
	opts := &slog.HandlerOptions{Level: level}
	var h slog.Handler
	if strings.EqualFold(strings.TrimSpace(os.Getenv("LOG_FORMAT")), "text") {
		h = slog.NewTextHandler(os.Stderr, opts)
	} else {
		h = slog.NewJSONHandler(os.Stderr, opts)
	}
	slog.SetDefault(slog.New(h))
}

func usage(w *os.File) {
	fmt.Fprint(w, `connector — SharePoint → Goodmem sync

Usage: connector <command> [flags]

Commands:
  sync-once            One-time full sync (flags: --env-file PATH, --dry-run)
  serve                Run the Graph webhook listener + sync engine (--env-file PATH)
  create-subscription  Create or renew the Graph change subscription (--env-file PATH)
  watch                Monitor a listener's activity log (watch [-n SECS] <base-url>)
  help                 Show this help
`)
}
