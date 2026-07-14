// Command connector syncs a SharePoint site to a Goodmem space.
//
// It replaces the Python proof-of-concept (listener.py, sync_once.py,
// watch_listener.py) with a single Go binary exposing subcommands. This is the
// distributed artifact — shipping as a compiled binary keeps the source closed.
package main

import (
	"fmt"
	"os"
)

func main() {
	if len(os.Args) < 2 {
		usage(os.Stderr)
		os.Exit(2)
	}
	switch os.Args[1] {
	case "serve":
		notImplemented("serve")
	case "sync-once":
		notImplemented("sync-once")
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

func notImplemented(cmd string) {
	fmt.Fprintf(os.Stderr, "%s: not yet implemented (port in progress)\n", cmd)
	os.Exit(1)
}

func usage(w *os.File) {
	fmt.Fprint(w, `connector — SharePoint → Goodmem sync

Usage: connector <command> [flags]

Commands:
  serve                Run the Microsoft Graph webhook listener + sync engine
  sync-once            One-time full sync from SharePoint to Goodmem
  watch                Monitor a deployed listener's activity log
  create-subscription  Create or renew the Graph change subscription
  help                 Show this help
`)
}
