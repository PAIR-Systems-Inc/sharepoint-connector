// Command connector syncs a content source (a SharePoint site or a Google Drive
// Shared Drive, selected by SOURCE / --source) to a Goodmem space.
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
	"path/filepath"
	"strconv"
	"strings"
	"syscall"
	"time"

	"fury.io/pairsys/goodmem"

	"github.com/PAIR-Systems-Inc/goodmem-connectors/internal/core/config"
	"github.com/PAIR-Systems-Inc/goodmem-connectors/internal/core/gm"
	"github.com/PAIR-Systems-Inc/goodmem-connectors/internal/core/server"
	"github.com/PAIR-Systems-Inc/goodmem-connectors/internal/core/source"
	"github.com/PAIR-Systems-Inc/goodmem-connectors/internal/core/syncer"
	"github.com/PAIR-Systems-Inc/goodmem-connectors/internal/providers/googledrive"
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
	srcFlag := fs.String("source", "", "content source: sharepoint|google-drive (overrides SOURCE)")
	dryRun := fs.Bool("dry-run", false, "compute the sync plan without changing Goodmem")
	_ = fs.Parse(args)
	if *srcFlag != "" {
		os.Setenv("SOURCE", *srcFlag)
	}

	cfg, err := loadConfig(*envFile)
	if err != nil {
		return err
	}
	gmc, err := buildGoodmem(cfg)
	if err != nil {
		return err
	}
	ctx := context.Background()
	src, err := buildSource(ctx, cfg, cfg.SharePointFolderPath, "") // one-shot: no durable channel state
	if err != nil {
		return err
	}
	spaceID, err := syncer.ResolveSpaceID(ctx, gmc, cfg.GoodmemSpaceID, spaceName(cfg), cfg.GoodmemEmbedderID, cfg.OpenAIAPIKey)
	if err != nil {
		return err
	}
	fmt.Printf("Space: %s\n", spaceID)

	res, err := syncer.RunFull(ctx, src, gmc, spaceID, syncer.Options{
		ExtractPageImages: cfg.ExtractPageImages,
		DryRun:            *dryRun,
		MaxFileBytes:      int64(atoiOr(os.Getenv("SHAREPOINT_MAX_FILE_MB"), 100)) * 1024 * 1024,
		MaxDeleteRatio:    floatOr(os.Getenv("GRAPH_MAX_DELETE_RATIO"), 0.5),
	})
	if err != nil {
		return err
	}
	fmt.Printf("Source files: %d   Goodmem memories: %d\n", res.SourceFiles, res.GoodmemMemories)
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
	srcFlag := fs.String("source", "", "content source: sharepoint|google-drive (overrides SOURCE)")
	_ = fs.Parse(args)
	if *srcFlag != "" {
		os.Setenv("SOURCE", *srcFlag)
	}

	cfg, err := loadConfig(*envFile)
	if err != nil {
		return err
	}
	configureLogging() // structured logs to stderr (Fly logs / shippers)

	// Poll vs push. Google Drive's changes.watch requires a domain-verified HTTPS
	// webhook, so Google Drive defaults to POLL mode (periodic delta — no public URL
	// needed); SharePoint defaults to push (Graph webhooks are easy to stand up).
	// Override either way with SYNC_POLL_MINUTES (>0 → poll; 0 → push).
	defaultPoll := 0
	if cfg.Source == config.SourceGoogleDrive {
		defaultPoll = 2
	}
	pollMin := atoiOr(os.Getenv("SYNC_POLL_MINUTES"), defaultPoll)

	// Push mode needs the webhook secret + public URL; poll mode needs neither.
	if pollMin <= 0 {
		if strings.TrimSpace(cfg.GraphClientState) == "" {
			return errors.New("GRAPH_CLIENT_STATE (webhook secret) is required for push mode; set SYNC_POLL_MINUTES>0 for poll mode")
		}
		if strings.TrimSpace(cfg.GraphNotificationURL) == "" {
			return errors.New("GRAPH_NOTIFICATION_URL (public webhook URL) is required for push mode; set SYNC_POLL_MINUTES>0 for poll mode")
		}
	}
	gmc, err := buildGoodmem(cfg)
	if err != nil {
		return err
	}
	port := firstNonEmpty(os.Getenv("PORT"), cfg.GraphPort, "5000")
	deltaPath := firstNonEmpty(os.Getenv("GRAPH_DELTA_TOKEN_FILE"), ".graph_delta_link")
	// The listener always syncs the whole drive; its durable-state dir (delta
	// cursor + Google Drive channel state) is the delta file's directory.
	src, err := buildSource(context.Background(), cfg, "", filepath.Dir(deltaPath))
	if err != nil {
		return err
	}
	spaceID, err := syncer.ResolveSpaceID(context.Background(), gmc, cfg.GoodmemSpaceID, spaceName(cfg), cfg.GoodmemEmbedderID, cfg.OpenAIAPIKey)
	if err != nil {
		return err
	}
	subMin := atoiOr(cfg.GraphSubscriptionMinutes, sharepoint.SubMinutesDefault)
	// Periodic safety full-sync: defaults to the subscription-renewal cadence
	// (~half the subscription lifetime). Set GRAPH_FULL_SYNC_MINUTES=0 to disable.
	fullSyncMin := atoiOr(os.Getenv("GRAPH_FULL_SYNC_MINUTES"), max(subMin/2, 20))

	l := &server.Listener{
		Src:               src,
		GM:                gmc,
		SpaceID:           spaceID,
		NotificationURL:   cfg.GraphNotificationURL,
		SubMinutes:        subMin,
		FullSyncMinutes:   fullSyncMin,
		PollMinutes:       pollMin,
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
	if pollMin > 0 {
		fmt.Printf("Listener on :%s   space=%s   mode=poll(%dm)\n", port, spaceID, pollMin)
	} else {
		fmt.Printf("Listener on :%s   space=%s   mode=push   webhook=%s\n", port, spaceID, cfg.GraphNotificationURL)
	}
	return l.Run(ctx)
}

// --- create-subscription ---

func runCreateSubscription(args []string) error {
	fs := flag.NewFlagSet("create-subscription", flag.ExitOnError)
	envFile := fs.String("env-file", "", "env file to load (default: .env if present)")
	srcFlag := fs.String("source", "", "content source: sharepoint|google-drive (overrides SOURCE)")
	_ = fs.Parse(args)
	if *srcFlag != "" {
		os.Setenv("SOURCE", *srcFlag)
	}

	cfg, err := loadConfig(*envFile)
	if err != nil {
		return err
	}
	if cfg.GraphClientState == "" || cfg.GraphNotificationURL == "" {
		return errors.New("GRAPH_CLIENT_STATE (webhook secret) and GRAPH_NOTIFICATION_URL (public webhook URL) are required")
	}
	ctx := context.Background()
	// Route through the configured provider so this works for Google Drive too, instead
	// of silently building a SharePoint client under SOURCE=google-drive. No durable
	// channel state for a one-off manual create.
	src, err := buildSource(ctx, cfg, "", "")
	if err != nil {
		return err
	}
	subMin := atoiOr(cfg.GraphSubscriptionMinutes, sharepoint.SubMinutesDefault)
	sub, err := src.EnsureSubscription(ctx, cfg.GraphNotificationURL, time.Duration(subMin)*time.Minute)
	if err != nil {
		return err
	}
	fmt.Printf("Subscription ready (%s): id=%s   expires=%s\n", src.Label(), sub.ID, sub.Expiration)
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

// buildSource constructs the configured provider adapter (a source.Source).
// folderPath scopes a one-time SharePoint full sync ("" = whole drive; ignored by
// Google Drive, which syncs the whole Shared Drive). stateDir, when non-empty, is the
// durable-state directory (the listener's); Google Drive persists its push-channel pair
// there so a restart stops the old channel instead of leaking it. One-shot
// commands pass "".
func buildSource(ctx context.Context, cfg *config.Config, folderPath, stateDir string) (source.Source, error) {
	switch cfg.Source {
	case config.SourceGoogleDrive:
		var (
			c   *googledrive.Client
			err error
		)
		if cfg.HasServiceAccount() {
			var sa []byte
			if sa, err = cfg.ServiceAccountJSON(); err != nil {
				return nil, err
			}
			c, err = googledrive.NewWithServiceAccount(ctx, sa, cfg.GoogleDriveID)
		} else {
			// No key configured — use Application Default Credentials.
			c, err = googledrive.NewWithADC(ctx, cfg.GoogleDriveID)
		}
		if err != nil {
			return nil, fmt.Errorf("google drive client: %w", err)
		}
		a := googledrive.NewAdapter(c, cfg.GraphClientState)
		if stateDir != "" {
			a = a.WithChannelStore(googledrive.FileChannelStore{Path: filepath.Join(stateDir, "google_drive_channel.json")})
		}
		return a, nil
	default: // sharepoint
		c := sharepoint.NewClient(cfg.AzureClientID, cfg.AzureTenantID, cfg.AzureClientSecret, cfg.SharePointSiteURL)
		return sharepoint.NewAdapter(c, folderPath, cfg.GraphClientState), nil
	}
}

// spaceName derives the default Goodmem space name for the configured source
// (used only when GOODMEM_SPACE_ID is unset).
func spaceName(cfg *config.Config) string {
	if cfg.Source == config.SourceGoogleDrive {
		return "GoogleDrive_" + cfg.GoogleDriveID
	}
	return syncer.SpaceNameFromSiteURL(cfg.SharePointSiteURL)
}

func buildGoodmem(cfg *config.Config) (*goodmem.Client, error) {
	gmc, err := gm.New(cfg.GoodmemBaseURL, cfg.GoodmemAPIKey)
	if err != nil {
		return nil, fmt.Errorf("goodmem client: %w", err)
	}
	return gmc, nil
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
	fmt.Fprint(w, `connector — SharePoint / Google Drive → Goodmem sync

Usage: connector <command> [flags]

Source: set SOURCE=sharepoint|google-drive (or --source) on any syncing command.

Commands:
  sync-once            One-time full sync (flags: --env-file PATH, --source NAME, --dry-run)
  serve                Run the webhook listener + sync engine (--env-file PATH, --source NAME)
  create-subscription  Create or renew the change subscription (--env-file PATH, --source NAME)
  watch                Monitor a listener's activity log (watch [-n SECS] <base-url>)
  help                 Show this help
`)
}
