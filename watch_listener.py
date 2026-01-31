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

If LISTENER_BASE_URL is omitted, uses LISTENER_ACTIVITY_URL or SYNC_NOTIFICATION_URL
from .env (strip /sync/webhook or /webhook from SYNC_NOTIFICATION_URL if set).
"""

import argparse
import os
import sys
import time

import requests
from dotenv import load_dotenv

load_dotenv()

# Default poll interval (seconds)
DEFAULT_POLL_INTERVAL = 2


def get_listener_base_url(args: argparse.Namespace) -> str | None:
    """Get listener base URL from env or remaining positional arg."""
    url = os.getenv("LISTENER_ACTIVITY_URL") or os.getenv("SYNC_NOTIFICATION_URL") or ""
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
        "url",
        nargs="?",
        default=None,
        help="Listener base URL (e.g. https://your-app.fly.dev)",
    )
    args = parser.parse_args()

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
            "  Or set LISTENER_ACTIVITY_URL or SYNC_NOTIFICATION_URL in .env",
            file=sys.stderr,
        )
        return 1

    activity_url = f"{base}/activity"
    last_id: int | None = None
    interval = args.interval
    connected = False
    last_idle_msg = 0.0  # time of last "no activity" message

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
                    for e in events:
                        ts = e.get("ts", "")[:19].replace("T", " ")
                        typ = e.get("type", "?")
                        msg = e.get("message", "")
                        # Delta: ASCII trees (multi-line), print each line indented
                        if typ == "delta":
                            for line in msg.split("\n"):
                                print(f"  {ts}  {line}")
                        # [Done] Add/Update/Remove: message already has path
                        elif typ in ("done", "remove"):
                            err = e.get("error")
                            if err:
                                msg = f"{msg} — {err}"
                            print(f"  {ts}  {msg}")
                        else:
                            name = e.get("file_name") or e.get("file_id") or ""
                            if name and name not in msg:
                                msg = f"{msg} ({name})"
                            err = e.get("error")
                            if err:
                                msg = f"{msg} — {err}"
                            print(f"  {ts}  [{typ}]  {msg}")
                else:
                    # No new events: print a brief idle line every 30s so you know we're still talking to the hook
                    now = time.monotonic()
                    if now - last_idle_msg >= 30:
                        print("  — no new activity (listener reachable)")
                        last_idle_msg = now

            except requests.RequestException as e:
                connected = False
                print(f"  (poll error: {e})", file=sys.stderr)
            except KeyboardInterrupt:
                break

            time.sleep(interval)
    except KeyboardInterrupt:
        pass

    print("\nStopped.")
    return 0


if __name__ == "__main__":
    sys.exit(main())
