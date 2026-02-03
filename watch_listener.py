#!/usr/bin/env -S uv run --script
# /// script
# requires-python = ">=3.10"
# dependencies = [
#     "requests>=2.31.0",
# ]
# ///
"""
Watch the listener's activity log from your local machine.

Polls the listener's /activity endpoint and prints new events (notifications
received, files synced to Goodmem, deleted, skipped, errors).

Usage:
  python watch_listener.py [OPTIONS] [LISTENER_BASE_URL]
  python watch_listener.py -n 0.5 https://your-app.fly.dev

If LISTENER_BASE_URL is omitted, uses LISTENER_ACTIVITY_URL or GRAPH_NOTIFICATION_URL
from the env file (strip /sync/webhook or /webhook from GRAPH_NOTIFICATION_URL if set).

Use --env-file to load a different env file (default: .env) so the
watcher uses that file's GRAPH_NOTIFICATION_URL when you omit the URL.
"""

import argparse
import os
import sys
import time
from datetime import datetime, timezone

import requests
from dotenv import load_dotenv

# Default poll interval (seconds)
DEFAULT_POLL_INTERVAL = 2


def _ts_to_local(ts_iso: str) -> str:
    """Convert activity log UTC ISO timestamp to local YYYY-MM-DD HH:MM:SS."""
    if not ts_iso:
        return ""
    try:
        s = ts_iso.strip().replace("Z", "+00:00")
        dt = datetime.fromisoformat(s)
        if dt.tzinfo is None:
            dt = dt.replace(tzinfo=timezone.utc)
        return dt.astimezone().strftime("%Y-%m-%d %H:%M:%S")
    except (ValueError, TypeError):
        return ts_iso[:19].replace("T", " ")  # fallback


def _hierarchical_tag(typ: str, msg: str) -> tuple[str, str] | None:
    """Map event type (and message) to [category] [action] for display. Returns (category, action) or None for passthrough."""
    if typ == "oauth2_start":
        return ("oauth2", "reauth")
    if typ == "oauth2_reauth":
        return ("oauth2", "reauth")
    if typ == "subscription_creating":
        return ("subscription", "created")
    if typ == "subscription_created":
        return ("subscription", "created")
    if typ == "subscription_renewing":
        return ("subscription", "renewed")
    if typ == "subscription_renewed":
        return ("subscription", "renewed")
    if typ == "subscription_info":
        return ("subscription", "info")
    if typ == "notification_received":
        return ("notification", "received")
    if typ == "coalesced":
        return ("notification", "coalesced")
    if typ == "delta":
        return ("sync", "delta")
    if typ == "diff":
        return ("sync", "diff")
    if typ == "info":
        if msg and "started" in msg.lower():
            return ("info", "started")
        if msg and "finished" in msg.lower():
            return ("info", "finished")
        if msg and ("subscription length" in msg.lower() or "GRAPH_SUBSCRIPTION" in msg):
            return ("info", "config")
        return ("info", "info")
    if typ == "skipped":
        return ("skipped", "skipped")
    if typ == "error":
        return ("error", "error")
    if typ == "remove":
        return None  # handled by _parse_sync_message
    return None


def _parse_sync_message(typ: str, msg: str) -> tuple[str, str] | tuple[str, str, str] | None:
    """Parse listener sync messages. Returns ('start', op, path) or ('end', op, path, 'DONE'|'FAILED') or None."""
    if not msg or not isinstance(msg, str):
        return None
    msg = msg.strip()
    # [Syncing] Add: path / [Syncing] Update: path / [Syncing] Remove: path
    if typ == "syncing" and msg.startswith("[Syncing] "):
        rest = msg[len("[Syncing] ") :].strip()
        if ": " in rest:
            op, path = rest.split(": ", 1)
            return ("start", op.strip(), path.strip())
    # [Done] Add: path / [Failed] Add: path
    if typ == "done":
        if msg.startswith("[Done] "):
            rest = msg[len("[Done] ") :].strip()
            if ": " in rest:
                op, path = rest.split(": ", 1)
                return ("end", op.strip(), path.strip(), "DONE")
        if msg.startswith("[Failed] "):
            rest = msg[len("[Failed] ") :].strip()
            if ": " in rest:
                op, path = rest.split(": ", 1)
                return ("end", op.strip(), path.strip(), "FAILED")
    # [Synced] Remove: path (type is "remove")
    if typ == "remove" and msg.startswith("[Synced] "):
        rest = msg[len("[Synced] ") :].strip()
        if ": " in rest:
            op, path = rest.split(": ", 1)
            return ("end", op.strip(), path.strip(), "DONE")
    return None


def get_listener_base_url(args: argparse.Namespace) -> str | None:
    """Get listener base URL from env or remaining positional arg."""
    url = os.getenv("LISTENER_ACTIVITY_URL") or os.getenv("GRAPH_NOTIFICATION_URL") or ""
    url = url.strip().rstrip("/")
    # Strip path so we have base only (e.g. https://app.fly.dev)
    for path in ["/sync/webhook", "/webhook", "/activity"]:
        if url.endswith(path):
            url = url[: -len(path)].rstrip("/")
    if url:
        return url
    if args.url:
        return args.url.strip().rstrip("/")
    return None


def main() -> int:
    parser = argparse.ArgumentParser(
        description="Watch the listener's activity log (polls /activity).",
    )
    parser.add_argument(
        "-n",
        "--interval",
        type=float,
        default=DEFAULT_POLL_INTERVAL,
        metavar="SECS",
        help=f"Poll interval in seconds (default: {DEFAULT_POLL_INTERVAL})",
    )
    parser.add_argument(
        "--env-file",
        metavar="PATH",
        default=None,
        help="Load this env file for GRAPH_NOTIFICATION_URL (e.g. .env.sharepoint-joint). Default: .env",
    )
    parser.add_argument(
        "url",
        nargs="?",
        default=None,
        help="Listener base URL (e.g. https://your-app.fly.dev). Overrides URL from env file.",
    )
    args = parser.parse_args()

    # Load .env or the given env file so GRAPH_NOTIFICATION_URL / LISTENER_ACTIVITY_URL are set
    if args.env_file and os.path.isfile(args.env_file):
        load_dotenv(args.env_file, override=True)
    else:
        load_dotenv()

    if args.interval <= 0:
        print("Error: interval must be positive.", file=sys.stderr)
        return 1

    base = get_listener_base_url(args)
    if not base:
        print(
            "Usage: python watch_listener.py [OPTIONS] [LISTENER_BASE_URL]",
            file=sys.stderr,
        )
        print(
            "  Or set LISTENER_ACTIVITY_URL or GRAPH_NOTIFICATION_URL in .env (or use --env-file PATH)",
            file=sys.stderr,
        )
        return 1

    activity_url = f"{base}/activity"
    last_id: int | None = None
    interval = args.interval
    connected = False
    idle_line_shown = False  # at most one "no new activity" line between activities

    print(f"Watching listener activity at {activity_url} (interval: {interval}s)")
    print("(Ctrl+C to stop)\n")

    try:
        while True:
            try:
                params = {"since": last_id} if last_id is not None else {}
                resp = requests.get(activity_url, params=params, timeout=10)
                resp.raise_for_status()
                data = resp.json()
                events = data.get("events") or []
                latest_id = data.get("latest_id")
                if latest_id is not None:
                    last_id = latest_id

                if not connected:
                    print("Connected to listener. Waiting for activity...\n")
                    connected = True

                if events:
                    if idle_line_shown:
                        print()  # end the idle line so events appear below
                        idle_line_shown = False
                    for e in events:
                        ts = _ts_to_local(e.get("ts", ""))
                        typ = e.get("type", "?")
                        msg = e.get("message", "")
                        parsed = _parse_sync_message(typ, msg)
                        # Sync lines: [Sync] [Op]: path  [Started] / — [Done] / [FAILED]
                        if parsed is not None:
                            if parsed[0] == "start":
                                _, op, path = parsed
                                print(f"  {ts}  [Sync] [{op}]: {path}  [Started]")
                            else:
                                _, op, path, status = parsed
                                status_label = "Done" if status == "DONE" else "FAILED"
                                print(f"  {ts}  [Sync] [{op}]: {path}  —  [{status_label}]")
                            continue
                        # OAuth2 / subscription: two lines — [Started] then — [Done]
                        if typ in ("oauth2_start", "subscription_creating", "subscription_renewing"):
                            tag = _hierarchical_tag(typ, msg)
                            if tag:
                                cat, act = tag
                                print(f"  {ts}  [{cat}] [{act}]  [Started]")
                            continue
                        if typ in ("oauth2_reauth", "subscription_created", "subscription_renewed"):
                            tag = _hierarchical_tag(typ, msg)
                            if tag:
                                cat, act = tag
                                print(f"  {ts}  [{cat}] [{act}]  —  [Done]")
                            continue
                        # Delta: [sync] [delta] then multi-line tree
                        if typ == "delta":
                            print(f"  {ts}  [sync] [delta]")
                            for line in msg.split("\n"):
                                print(f"  {ts}  {line}")
                            continue
                        # Other: hierarchical [category] [action]  message
                        tag = _hierarchical_tag(typ, msg)
                        if tag is not None:
                            cat, act = tag
                            err = e.get("error")
                            if err:
                                msg = f"{msg} — {err}"
                            name = e.get("file_name") or e.get("file_id") or ""
                            if name and name not in msg:
                                msg = f"{msg} ({name})"
                            print(f"  {ts}  [{cat}] [{act}]  {msg}")
                            continue
                        # Fallback: raw type and message
                        err = e.get("error")
                        if err:
                            msg = f"{msg} — {err}"
                        name = e.get("file_name") or e.get("file_id") or ""
                        if name and name not in msg:
                            msg = f"{msg} ({name})"
                        print(f"  {ts}  [{typ}]  {msg}")
                    idle_line_shown = False  # after activity, allow one idle line again
                else:
                    # No new events: at most one idle line (with timestamp) until next activity
                    if not idle_line_shown:
                        ts = time.strftime("%Y-%m-%d %H:%M:%S", time.localtime())
                        print(f"\r  {ts}  — no new activity (listener reachable)\033[K", end="", flush=True)
                        idle_line_shown = True

            except requests.RequestException as e:
                connected = False
                if idle_line_shown:
                    print()
                    idle_line_shown = False
                print(f"  (poll error: {e})", file=sys.stderr)
            except KeyboardInterrupt:
                break

            time.sleep(interval)
    except KeyboardInterrupt:
        pass

    if idle_line_shown:
        print()
    print("Stopped.")
    return 0


if __name__ == "__main__":
    sys.exit(main())
