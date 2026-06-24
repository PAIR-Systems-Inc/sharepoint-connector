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


def _format_expiration(iso_str: str | None) -> str:
    """Format ISO expiration for display (local YYYY-MM-DD HH:MM:SS). Returns '' if missing/invalid."""
    if not iso_str:
        return ""
    return _ts_to_local(iso_str)


def _parse_iso_to_utc(iso_str: str | None) -> datetime | None:
    """Parse ISO timestamp to timezone-aware UTC datetime. Returns None if missing/invalid."""
    if not iso_str:
        return None
    try:
        s = (iso_str or "").strip().replace("Z", "+00:00")
        dt = datetime.fromisoformat(s)
        if dt.tzinfo is None:
            dt = dt.replace(tzinfo=timezone.utc)
        return dt
    except (ValueError, TypeError):
        return None


def _format_subscription_expiry(expiration_iso: str | None, event_ts_iso: str | None) -> str:
    """Format subscription expiry as ' (expires in N minutes at YYYY-MM-DD HH:MM:SS)' or ' (expires at ...)' if no event_ts. Returns '' if no expiration."""
    if not expiration_iso:
        return ""
    at_str = _format_expiration(expiration_iso)
    if not at_str:
        return ""
    exp_dt = _parse_iso_to_utc(expiration_iso)
    event_dt = _parse_iso_to_utc(event_ts_iso)
    if exp_dt is not None and event_dt is not None:
        delta_sec = (exp_dt - event_dt).total_seconds()
        if delta_sec >= 0:
            minutes = int(round(delta_sec / 60))
            return f" (expires in {minutes} minutes at {at_str})"
    return f" (expires at {at_str})"


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


# Op from listener (Add/Update/Remove) -> display label
_SYNC_OP_ACTION = {"Add": "Add", "Update": "Update", "Remove": "Remove"}


def _parse_sync_message(typ: str, msg: str) -> tuple[str, str, str] | tuple[str, str, str, str] | None:
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


def _sync_display_path(e: dict, parsed_path: str) -> str:
    """File path for sync line: prefer file_name from event, else parsed path (without trailing id=)."""
    path = e.get("file_name") or parsed_path
    # If path looks like "name (id=xxx)", use name only when we have file_id in event
    if e.get("file_id") and path.endswith(")"):
        idx = path.rfind(" (id=")
        if idx != -1:
            path = path[:idx]
    return path or "(unknown)"


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

    # Load .env or the given env file so GRAPH_NOTIFICATION_URL / LISTENER_ACTIVITY_URL are set.
    # Resolve relative --env-file against script dir (same as listener.py) so it works from any cwd.
    env_file = args.env_file
    if env_file and not os.path.isabs(env_file):
        script_dir = os.path.dirname(os.path.abspath(__file__))
        env_file = os.path.join(script_dir, env_file)
    if env_file and os.path.isfile(env_file):
        load_dotenv(env_file, override=True)
    elif args.env_file:
        print(f"Warning: env file not found: {args.env_file}", file=sys.stderr)
        load_dotenv()
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
                        # Sync lines: [Add/Update/Remove] <path> (<file_id>) : Started/Done/Failed
                        if parsed is not None:
                            action = _SYNC_OP_ACTION.get(parsed[1], parsed[1])
                            path = _sync_display_path(e, parsed[2])
                            file_id = e.get("file_id") or ""
                            item = f"{path} ({file_id})" if file_id else path
                            if parsed[0] == "start":
                                print(f"  {ts}  [{action}] {item} : Started")
                            else:
                                status = "Done" if parsed[3] == "DONE" else "Failed"
                                print(f"  {ts}  [{action}] {item} : {status}")
                            continue
                        # Graph webhook subscription: Subscribing / Subscribed, Renewing / Renewed
                        if typ == "subscription_creating":
                            print(f"  {ts}  [Graph Webhook] Subscribing")
                            continue
                        if typ == "subscription_created":
                            suffix = _format_subscription_expiry(
                                e.get("expirationDateTime"), e.get("ts")
                            )
                            print(f"  {ts}  [Graph Webhook] Subscribed{suffix}")
                            continue
                        if typ == "subscription_renewing":
                            print(f"  {ts}  [Graph Webhook] Renewing")
                            continue
                        if typ == "subscription_renewed":
                            suffix = _format_subscription_expiry(
                                e.get("expirationDateTime"), e.get("ts")
                            )
                            print(f"  {ts}  [Graph Webhook] Renewed{suffix}")
                            continue
                        # OAuth2: Obtaining token / Token obtained (expires in N minutes at ...)
                        if typ == "oauth2_start":
                            print(f"  {ts}  [oauth2] Obtaining token")
                            continue
                        if typ == "oauth2_reauth":
                            suffix = _format_subscription_expiry(
                                e.get("token_expires_at"), e.get("ts")
                            )
                            print(f"  {ts}  [oauth2] Token obtained{suffix}")
                            continue
                        # Graph webhook notification received
                        if typ == "notification_received":
                            count = e.get("count", 1)
                            print(f"  {ts}  [Graph Webhook] Received {count} change(s)")
                            continue
                        # Delta tree (To Add / To Update / To Remove): skip in watcher output
                        # Delta tree: To Add / To Update / To Remove (no header line)
                        if typ == "delta":
                            for line in msg.split("\n"):
                                print(f"  {ts}  {line}")
                            continue
                        # Info: skip Graph subscription length config line
                        if typ == "info" and msg and (
                            "subscription length" in msg.lower() or "GRAPH_SUBSCRIPTION_MINUTES" in msg
                        ):
                            continue
                        # Full/Delta sync started/finished: [info] Full/Delta Sync (SharePoint → Goodmem): Started/Done
                        if typ == "info" and msg and ("Full sync" in msg or "Delta sync" in msg):
                            kind = "Full Sync" if "Full sync" in msg else "Delta Sync"
                            if "started" in msg.lower():
                                print(f"  {ts}  [info] {kind} (SharePoint → Goodmem): Started")
                            elif "finished" in msg.lower():
                                print(f"  {ts}  [info] {kind} (SharePoint → Goodmem): Done")
                            else:
                                print(f"  {ts}  [info] {kind} (SharePoint → Goodmem): {msg}")
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
                        # If type is "done" but message already starts with [Update]/[Add]/[Failed], show message only (no [done] prefix)
                        if typ == "done" and msg.strip().startswith("["):
                            print(f"  {ts}  {msg}")
                        else:
                            print(f"  {ts}  [{typ}]  {msg}")
                    idle_line_shown = False  # after activity, allow one idle line again
                else:
                    # No new events: refresh idle line with current timestamp
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
