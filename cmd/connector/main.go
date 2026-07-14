// Command connector syncs a SharePoint site to a Goodmem space.
//
// It replaces the Python proof-of-concept (listener.py, sync_once.py,
// watch_listener.py) with a single Go binary exposing subcommands. This is the
// distributed artifact — shipping as a compiled binary keeps the source closed.
package main

import (
	"context"
	"flag"
	"fmt"
	"os"

	"github.com/PAIR-Systems-Inc/sharepoint-connector/internal/config"
	"github.com/PAIR-Systems-Inc/sharepoint-connector/internal/gm"
	"github.com/PAIR-Systems-Inc/sharepoint-connector/internal/graph"
	"github.com/PAIR-Systems-Inc/sharepoint-connector/internal/syncer"
)

func main() {
	if len(os.Args) < 2 {
		usage(os.Stderr)
		os.Exit(2)
	}
	switch os.Args[1] {
	case "sync-once":
		if err := runSyncOnce(os.Args[2:]); err != nil {
			fmt.Fprintln(os.Stderr, "Error:", err)
			os.Exit(1)
		}
	case "serve":
		notImplemented("serve")
	case "watch":
		notImplemented("watch")
	case "create-subscription":
		notImplemented("create-subscription")
	case "-h", "--help", "help":
		usage(os.Stdout)
	default:
		fmt.Fprintf(os.Stderr, "unknown command: %s\n\n", os.Args[1])
		usage(os.Stderr)
		os.Exit(2)
	}
}

func runSyncOnce(args []string) error {
	fs := flag.NewFlagSet("sync-once", flag.ExitOnError)
	envFile := fs.String("env-file", "", "env file to load (default: process env, plus .env if present)")
	dryRun := fs.Bool("dry-run", false, "compute the sync plan without changing Goodmem")
	_ = fs.Parse(args)

	ef := *envFile
	if ef == "" {
		if _, err := os.Stat(".env"); err == nil {
			ef = ".env"
		}
	}
	cfg, err := config.Load(ef)
	if err != nil {
		return err
	}
	if err := cfg.ValidateSync(); err != nil {
		return err
	}
	if err := graph.ValidateTokenRefreshBuffer(); err != nil {
		return err
	}

	gc := graph.NewClient(cfg.AzureClientID, cfg.AzureTenantID, cfg.AzureClientSecret, cfg.SharePointSiteURL)
	gmc, err := gm.New(cfg.GoodmemBaseURL, cfg.GoodmemAPIKey)
	if err != nil {
		return fmt.Errorf("goodmem client: %w", err)
	}

	ctx := context.Background()
	spaceID, err := syncer.ResolveSpaceID(ctx, gmc, cfg.GoodmemSpaceID, cfg.SharePointSiteURL, cfg.GoodmemEmbedderID)
	if err != nil {
		return err
	}
	fmt.Printf("Space: %s\n", spaceID)

	res, err := syncer.RunFull(ctx, gc, gmc, spaceID, *dryRun)
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

func notImplemented(cmd string) {
	fmt.Fprintf(os.Stderr, "%s: not yet implemented (port in progress)\n", cmd)
	os.Exit(1)
}

func usage(w *os.File) {
	fmt.Fprint(w, `connector — SharePoint → Goodmem sync

Usage: connector <command> [flags]

Commands:
  sync-once            One-time full sync from SharePoint to Goodmem
                         flags: --env-file PATH, --dry-run
  serve                Run the Microsoft Graph webhook listener + sync engine
  watch                Monitor a deployed listener's activity log
  create-subscription  Create or renew the Graph change subscription
  help                 Show this help
`)
}
