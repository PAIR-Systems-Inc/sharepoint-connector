#!/usr/bin/env -S uv run --script
# /// script
# requires-python = ">=3.10"
# dependencies = [
#     "requests>=2.31.0",
#     "python-dotenv>=1.0.0",
#     "flask>=3.0.0",
# ]
# ///
"""
Sync SharePoint file changes to Goodmem via Microsoft Graph API webhooks.

Uses Microsoft Graph change notifications (Graph API webhooks): creates a Graph
subscription for driveItem on the configured SharePoint drive, then receives
created/updated/deleted events at a public HTTPS endpoint and syncs each change
to Goodmem. This is the Graph API approach, not the older SharePoint REST
list webhooks.

Run the webhook server (must be reachable over HTTPS, e.g. via ngrok):
  python listener.py server

Create or renew the Graph subscription (run once, or before expiration):
  python listener.py create-subscription

Requires: GRAPH_NOTIFICATION_URL, GRAPH_CLIENT_STATE, and same SharePoint/Goodmem env as sync_once.py.
See permission.md for Azure AD / SharePoint permissions.
"""

import json
import os
import queue
import re
import sys
import threading
import time
from datetime import datetime, timezone, timedelta
from typing import Optional
from urllib.parse import unquote

import requests
from dotenv import load_dotenv
from flask import Flask, request

from goodmem_client import GoodmemClient, uuid_from_file_id
from sharepoint_client import SharePointConnector, validate_token_refresh_buffer


def load_env(env_file: Optional[str] = None) -> None:
    """Load environment from a single file. When env_file is given, use only that file (no .env fallback).
    Resolves relative paths against the script directory so the file is found regardless of cwd.
    """
    if not env_file:
        load_dotenv()
        return
    # Resolve relative path against this script's directory so cwd (e.g. uv run) doesn't matter
    if not os.path.isabs(env_file):
        script_dir = os.path.dirname(os.path.abspath(__file__))
        env_file = os.path.join(script_dir, env_file)
    if not os.path.isfile(env_file):
        print(f"Error: env file not found: {env_file}", file=sys.stderr)
        sys.exit(1)
    load_dotenv(env_file, override=True)


# --- Reused logic from sync_once.py (not importing to keep listener independent) ---

def _is_mime_type_supported(mime_type: str) -> bool:
    """True if Goodmem's text/content extraction supports this MIME type."""
    if not mime_type:
        return False
    mime_type_lower = mime_type.lower()
    if mime_type_lower.startswith("text/"):
        return True
    if mime_type_lower in (
        "application/pdf",
        "application/rtf",
        "application/msword",
        "application/vnd.openxmlformats-officedocument.wordprocessingml.document",
    ):
        return True
    if "+xml" in mime_type_lower or "json" in mime_type_lower:
        return True
    return False


def _space_name_from_site_url(site_url: str) -> str:
    """Derive Goodmem space name from SharePoint site URL (e.g. SharePoint_Org_Site)."""
    from urllib.parse import urlparse
    url = site_url.rstrip("/")
    parsed = urlparse(url)
    host = parsed.netloc or ""
    path = (parsed.path or "").strip("/")
    if ".sharepoint.com" in host:
        org = host.split(".sharepoint.com")[0]
    else:
        org = host
    site = ""
    if path.lower().startswith("sites/"):
        rest = path[6:]
        site = rest.split("/")[0] if rest else ""
    return f"SharePoint_{org}_{site}"


def _download_file(download_url: str) -> bytes:
    """Download file content from SharePoint download URL."""
    resp = requests.get(download_url, timeout=60)
    resp.raise_for_status()
    return resp.content


# --- Subscription and webhook ---

# driveItem limits per Microsoft docs (min 45 min, max under 30 days)
GRAPH_SUBSCRIPTION_MINUTES_MIN = 45
GRAPH_SUBSCRIPTION_MINUTES_MAX = 42_300
GRAPH_SUBSCRIPTION_MINUTES_DEFAULT = 3 * 24 * 60  # 3 days; override with env GRAPH_SUBSCRIPTION_MINUTES
# Auto-renewal: renew when expiration is this many minutes before expiry
RENEW_SUBSCRIPTION_THRESHOLD_MINUTES = 30
# Minimum sleep after creating a subscription so Graph list API can return it before we loop (avoids create loop)
SUBSCRIPTION_CREATE_COOLDOWN_SECONDS = 10


def _validate_subscription_minutes_env() -> None:
    """If GRAPH_SUBSCRIPTION_MINUTES is set, must be an integer in [45, 42300]. Exits with message otherwise."""
    val = os.getenv("GRAPH_SUBSCRIPTION_MINUTES")
    if not val or not val.strip():
        return
    try:
        n = int(val.strip())
    except ValueError:
        print(
            "Error: GRAPH_SUBSCRIPTION_MINUTES must be a number.",
            file=sys.stderr,
        )
        sys.exit(1)
    if n < GRAPH_SUBSCRIPTION_MINUTES_MIN:
        print(
            f"Error: GRAPH_SUBSCRIPTION_MINUTES is {n}; Microsoft Graph requires at least 45 minutes for driveItem subscriptions.",
            file=sys.stderr,
        )
        sys.exit(1)
    if n > GRAPH_SUBSCRIPTION_MINUTES_MAX:
        print(
            f"Error: GRAPH_SUBSCRIPTION_MINUTES is {n}; Microsoft Graph allows at most 42,300 minutes (~30 days) for driveItem subscriptions.",
            file=sys.stderr,
        )
        sys.exit(1)


def _get_subscription_minutes() -> int:
    """Subscription length in minutes from env (GRAPH_SUBSCRIPTION_MINUTES), clamped to [45, 42300]. Use short values (e.g. 45) to test re-subscribe. Call _validate_subscription_minutes_env() first so invalid values exit early."""
    val = os.getenv("GRAPH_SUBSCRIPTION_MINUTES")
    if val is not None:
        try:
            n = int(val.strip())
            return min(max(GRAPH_SUBSCRIPTION_MINUTES_MIN, n), GRAPH_SUBSCRIPTION_MINUTES_MAX)
        except ValueError:
            pass
    return min(GRAPH_SUBSCRIPTION_MINUTES_DEFAULT, GRAPH_SUBSCRIPTION_MINUTES_MAX)
# Cap sleep so we re-check at least this often (e.g. if sub was deleted externally)
RENEW_SUBSCRIPTION_MAX_SLEEP_SECONDS = 24 * 3600

# Goodmem processingStatus: after insert (200) we poll Get memory by ID until COMPLETED or FAILED
GOODMEM_POLL_INTERVAL_SECONDS = 10
GOODMEM_POLL_MAX_ATTEMPTS = 30  # ~5 min

# App-global state for webhook handler (set by server entrypoint)
_connector: Optional[SharePointConnector] = None
_goodmem: Optional[GoodmemClient] = None
_site_url: Optional[str] = None
_drive_id: Optional[str] = None
_space_id: Optional[str] = None
_client_state: Optional[str] = None
_notification_url: Optional[str] = None

# Activity log for watchers (last N events)
_activity_log: list[dict] = []
_activity_lock = threading.Lock()
_activity_max = 200
_activity_id = 0
# Subscription IDs we've already logged "subscription_info" for (so watcher sees current sub once per id)
_subscription_info_logged_ids: set[str] = set()

# Queue for processing notifications in background (return 200 quickly; coalesce root sync)
_sync_queue: "queue.Queue[dict]" = queue.Queue()
_root_sync_pending = False
_root_sync_lock = threading.Lock()

# Delta link persistence: file path (env GRAPH_DELTA_TOKEN_FILE or default .graph_delta_link in script dir)
_delta_link_path: Optional[str] = None
_delta_link_lock = threading.Lock()

# Pending removes: file_ids we failed to delete from Goodmem; retried on next full/delta sync. display_names stored for log readability.
_pending_remove_file_ids: set[str] = set()
_pending_remove_display_names: dict[str, str] = {}  # file_id -> display name (for real name in logs)
_pending_remove_path: Optional[str] = None
_pending_remove_lock = threading.Lock()

# Pending add/update: file_ids we failed to add/update on Goodmem; retried on next full/delta sync
_pending_add_file_ids: set[str] = set()
_pending_add_path: Optional[str] = None
_pending_add_lock = threading.Lock()
_pending_update_file_ids: set[str] = set()
_pending_update_path: Optional[str] = None
_pending_update_lock = threading.Lock()


def _log_activity(event_type: str, message: str, **details: object) -> None:
    """Append an event to the activity log (thread-safe)."""
    global _activity_id
    with _activity_lock:
        _activity_id += 1
        entry = {
            "id": _activity_id,
            "ts": datetime.now(timezone.utc).isoformat(),
            "type": event_type,
            "message": message,
            **details,
        }
        _activity_log.append(entry)
        while len(_activity_log) > _activity_max:
            _activity_log.pop(0)


def _parse_drive_and_item_from_resource(resource: str) -> Optional[tuple[str, str]]:
    """Extract (drive_id, item_id) from notification resource path.
    e.g. 'sites/xxx/drives/yyy/items/zzz' or 'drives/yyy/items/zzz' -> ('yyy','zzz').
    """
    if not resource:
        return None
    # Match drives/{id}/items/{id} (sites/.../drives/.../items/... or drives/.../items/...)
    m = re.search(r"drives/([^/]+)/items/([^/]+)", resource)
    if m:
        return m.group(1), m.group(2)
    return None


def _is_root_resource(resource: str) -> bool:
    """True if the notification resource is the drive root (e.g. .../drives/xxx/root)."""
    if not resource:
        return False
    return bool(re.search(r"drives/[^/]+/root$", resource.rstrip("/")))


def _drive_id_from_resource(resource: str) -> Optional[str]:
    """Extract drive_id from a resource path (e.g. .../drives/yyy/root or .../drives/yyy/items/zzz)."""
    if not resource:
        return None
    m = re.search(r"drives/([^/]+)", resource)
    return m.group(1) if m else None


def _goodmem_error_message(e: requests.RequestException) -> str:
    """Build a user-friendly Goodmem error message for debugging (404, credentials, connection, etc.)."""
    resp = getattr(e, "response", None)
    if resp is not None:
        code = resp.status_code
        if code == 401:
            return "Goodmem: 401 Unauthorized — check GOODMEM_API_KEY"
        if code == 403:
            return "Goodmem: 403 Forbidden — check API key or permissions"
        if code == 404:
            return "Goodmem: 404 Not Found — check GOODMEM_BASE_URL or resource path"
        if 500 <= code < 600:
            return f"Goodmem: {code} Server Error — Goodmem service issue"
        if code == 400:
            return "Goodmem: 400 Bad Request — check request body or content type (e.g. file type or size)"
        return f"Goodmem: HTTP {code} — {resp.reason or str(e)}"
    if isinstance(e, requests.exceptions.ConnectionError):
        return "Goodmem: Connection failed — check GOODMEM_BASE_URL or network (e.g. service down)"
    if isinstance(e, requests.exceptions.Timeout):
        return "Goodmem: Request timed out — service slow or unreachable"
    return f"Goodmem error: {e}"


def _log_goodmem_error(e: requests.RequestException) -> None:
    """Log an activity event when a Goodmem API call fails, with a precise message for debugging."""
    msg = _goodmem_error_message(e)
    _log_activity("error", msg, error=str(e))


def _ensure_space_id() -> Optional[str]:
    """Resolve Goodmem space ID: use GOODMEM_SPACE_ID (or SPACE_ID/DEFAULT_SPACE_ID) from env if set; else lookup by site URL or create with GOODMEM_EMBEDDER_ID (or EMBEDDER_ID/DEFAULT_EMBEDDER_ID)."""
    global _space_id, _goodmem, _site_url
    if _space_id:
        return _space_id
    if not _goodmem:
        return None
    default_space_id = os.getenv("GOODMEM_SPACE_ID") or os.getenv("SPACE_ID") or os.getenv("DEFAULT_SPACE_ID")
    if default_space_id:
        _space_id = default_space_id.strip()
        return _space_id
    if not _site_url:
        return None
    space_name = _space_name_from_site_url(_site_url)
    try:
        _space_id = _goodmem.find_space_by_name(space_name)
        if _space_id is None:
            embedders = _goodmem.list_embedders()
            embedder_id = (os.getenv("GOODMEM_EMBEDDER_ID") or os.getenv("EMBEDDER_ID") or os.getenv("DEFAULT_EMBEDDER_ID") or
                          (embedders[0].get("embedderId") if embedders else None))
            if not embedder_id:
                print("[listener] No embedder available; cannot create space.", file=sys.stderr)
                return None
            created = _goodmem.create_space(space_name=space_name, embedder_id=embedder_id)
            _space_id = created.get("spaceId")
    except requests.RequestException as e:
        _log_goodmem_error(e)
        print(f"[listener] Goodmem unreachable: {e}", file=sys.stderr)
        return None
    return _space_id


def _poll_memory_processing_status(
    memory_id: str,
    name_with_id: str,
    op_label: str,
) -> str:
    """Poll Goodmem Get memory by ID until processingStatus is COMPLETED or FAILED, or timeout.
    Logs 'Received by Goodmem. Pending processing.' while PENDING.
    Returns 'COMPLETED', 'FAILED', or 'PENDING' (timeout). Uses _goodmem."""
    global _goodmem
    if not _goodmem:
        return "PENDING"
    for attempt in range(GOODMEM_POLL_MAX_ATTEMPTS):
        try:
            mem = _goodmem.get_memory_by_id(memory_id)
        except requests.RequestException:
            time.sleep(GOODMEM_POLL_INTERVAL_SECONDS)
            continue
        status = (mem.get("processingStatus") or "").upper()
        if status == "COMPLETED":
            return "COMPLETED"
        if status == "FAILED":
            return "FAILED"
        _log_activity("done", f"[{op_label}] " + name_with_id + " : Received by Goodmem. Pending processing.")
        time.sleep(GOODMEM_POLL_INTERVAL_SECONDS)
    return "PENDING"


def _add_one_file_to_goodmem(file_info: dict, *, is_update: bool = False) -> Optional[tuple[str, str]]:
    """Download file and insert into Goodmem with memoryId=uuid_from_file_id(file_id). Returns ('synced', name), ('skipped', reason), ('error', msg), or None. When is_update=True, logs [Done]/[Failed] Update instead of Add.
    Non-200 from insert → file_id stays in pending_add (retry as add). 200 with processingStatus COMPLETED → success. 200 with FAILED → pending_update (retry as delete-then-add). 200 with PENDING → poll Get memory by ID until COMPLETED/FAILED or timeout; timeout → pending_add."""
    global _goodmem, _space_id
    if not _goodmem or not _space_id:
        return None
    file_id = file_info.get("id")
    if not file_id:
        name = file_info.get("name") or "(unknown)"
        return ("skipped", f"No file id: {name}")
    name = file_info.get("name") or "(unknown)"
    mime_type = file_info.get("mime_type")
    if not _is_mime_type_supported(mime_type):
        return ("skipped", f"Unsupported MIME: {name}")
    download_url = file_info.get("download_url")
    if not download_url:
        return ("skipped", f"No download URL: {name}")
    memory_uuid = uuid_from_file_id(file_id)
    # Idempotency: if a memory with our UUID already exists (e.g. race with full sync or backend reused ID), treat as update to avoid duplicate memories
    try:
        _goodmem.get_memory_by_id(memory_uuid)
        return _update_one_file_to_goodmem(file_info)
    except requests.RequestException as e:
        if e.response is not None and e.response.status_code == 404:
            pass
        else:
            _log_goodmem_error(e)
            print(f"[listener] Goodmem unreachable: {e}", file=sys.stderr)
            if is_update:
                _pending_update_add(file_id)
            else:
                _pending_add_add(file_id)
            return ("error", f"Goodmem unreachable: {name}")
    op_label = "Update" if is_update else "Add"
    name_with_id = name + " (id=" + (file_id or "?") + ")"
    try:
        content_bytes = _download_file(download_url)
    except Exception as e:
        print(f"[listener] Download failed for {file_info.get('name')}: {e}", file=sys.stderr)
        _log_activity("done", f"[Failed] {op_label}: " + name_with_id, file_name=name, file_id=file_id, error=str(e))
        return ("error", f"Download failed: {name}")
    metadata = {k: v for k, v in file_info.items() if v is not None}
    try:
        result = _goodmem.insert_memory_binary(
            space_id=_space_id,
            content_bytes=content_bytes,
            content_type=mime_type or "application/octet-stream",
            metadata=metadata,
            memory_id=memory_uuid,
            filename=file_info.get("name") or "upload",
        )
        # 200 response: check processingStatus. COMPLETED = success; FAILED = retry as delete-then-add; PENDING = poll Get memory by ID
        mem_id = result.get("memoryId") or memory_uuid
        status = (result.get("processingStatus") or "").upper()
        if status == "COMPLETED":
            _log_activity("done", f"[Done] {op_label}: " + name_with_id, file_name=name, file_id=file_id)
            if is_update:
                _pending_update_discard(file_id)
            else:
                _pending_add_discard(file_id)
            return ("synced", name)
        if status == "FAILED":
            _log_activity("done", f"[Failed] {op_label}: " + name_with_id, file_name=name, file_id=file_id, error="Goodmem processingStatus FAILED")
            _pending_update_add(file_id)
            return ("error", f"Goodmem ingest failed: {name}")
        # PENDING or missing: poll until COMPLETED, FAILED, or timeout
        final = _poll_memory_processing_status(mem_id, name_with_id, op_label)
        if final == "COMPLETED":
            _log_activity("done", f"[Done] {op_label}: " + name_with_id, file_name=name, file_id=file_id)
            if is_update:
                _pending_update_discard(file_id)
            else:
                _pending_add_discard(file_id)
            return ("synced", name)
        if final == "FAILED":
            _log_activity("done", f"[Failed] {op_label}: " + name_with_id, file_name=name, file_id=file_id, error="Goodmem processingStatus FAILED")
            _pending_update_add(file_id)
            return ("error", f"Goodmem ingest failed: {name}")
        # timeout still PENDING: retry add next sync
        _log_activity("done", f"[Failed] {op_label}: " + name_with_id, file_name=name, file_id=file_id, error="Goodmem processing still PENDING (timeout)")
        if is_update:
            _pending_update_add(file_id)
        else:
            _pending_add_add(file_id)
        return ("error", f"Ingest pending (timeout): {name}")
    except requests.RequestException as e:
        _log_goodmem_error(e)
        print(f"[listener] Goodmem unreachable: {e}", file=sys.stderr)
        _log_activity("done", f"[Failed] {op_label}: " + name_with_id, file_name=name, file_id=file_id, error=str(e))
        if is_update:
            _pending_update_add(file_id)
        else:
            _pending_add_add(file_id)
        return ("error", f"Ingest failed: {name}")
    except Exception as e:
        print(f"[listener] Ingest failed for {file_info.get('name')}: {e}", file=sys.stderr)
        _log_activity("done", f"[Failed] {op_label}: " + name_with_id, file_name=name, file_id=file_id, error=str(e))
        if is_update:
            _pending_update_add(file_id)
        else:
            _pending_add_add(file_id)
        return ("error", f"Ingest failed: {name}")


def _update_one_file_to_goodmem(file_info: dict) -> Optional[tuple[str, str]]:
    """Delete existing memory by UUID then insert (same memoryId). Returns ('synced', name), ('skipped', reason), ('error', msg), or None."""
    global _goodmem, _space_id
    if not _goodmem or not _space_id:
        return None
    file_id = file_info.get("id")
    name = file_info.get("name") or "(unknown)"
    memory_uuid = uuid_from_file_id(file_id)
    name_with_id = name + " (id=" + (file_id or "?") + ")"
    try:
        _goodmem.delete_memory(memory_uuid)
    except requests.RequestException as e:
        if e.response is not None and e.response.status_code == 404:
            pass  # already gone; treat as add
        else:
            _log_goodmem_error(e)
            print(f"[listener] Goodmem unreachable: {e}", file=sys.stderr)
            _pending_update_add(file_id)
            return ("error", f"Goodmem unreachable: {name}")
    return _add_one_file_to_goodmem(file_info, is_update=True)


def _delete_memory_for_file_id(file_id: str, log_name: Optional[str] = None) -> None:
    """Delete Goodmem memory by uuid_from_file_id(file_id). Logs remove activity. On failure (not 404), adds file_id to pending removes for retry on next sync."""
    global _goodmem, _space_id
    if not _goodmem or not _space_id:
        return
    display = log_name or file_id
    try:
        _goodmem.delete_memory(uuid_from_file_id(file_id))
        _pending_remove_discard(file_id)
        _log_activity("remove", "[Synced] Remove: " + display + " (id=" + file_id + ")", file_name=display, file_id=file_id)
    except requests.RequestException as e:
        if e.response is not None and e.response.status_code == 404:
            _pending_remove_discard(file_id)
            _log_activity("remove", "[Synced] Remove (already gone): " + display + " (id=" + file_id + ")", file_name=display, file_id=file_id)
        else:
            _pending_remove_add(file_id, display_name=display)
            _log_goodmem_error(e)
            print(f"[listener] Goodmem unreachable: {e}", file=sys.stderr)
            _log_activity("error", f"Remove failed (will retry next sync): {display} (id={file_id})", file_id=file_id)


def _is_root_sync_notification(value: list) -> bool:
    """True if this notification would trigger a full root sync (so we can coalesce duplicate notifications)."""
    global _connector, _drive_id
    if not _connector or not _drive_id or not isinstance(value, list):
        return False
    for entry in value:
        if not isinstance(entry, dict):
            continue
        if _client_state and entry.get("clientState") != _client_state:
            continue
        change_type = (entry.get("changeType") or "").strip().lower()
        resource = entry.get("resource", "")
        resource_data = entry.get("resourceData") or {}
        item_id = resource_data.get("id")
        parsed = _parse_drive_and_item_from_resource(resource)
        if parsed:
            drive_id, item_id = parsed
        else:
            drive_id = _drive_id_from_resource(resource) or _drive_id
            if not item_id and resource and "/" not in resource:
                item_id = resource
        if change_type == "updated" and drive_id and (not item_id or _is_root_resource(resource)):
            return True
    return False


def _meta_dict(m: dict) -> dict:
    """Normalize memory metadata to a dict (API may return metadata as JSON string or object)."""
    raw = m.get("metadata")
    if raw is None:
        return {}
    if isinstance(raw, dict):
        return raw
    if isinstance(raw, str):
        try:
            return json.loads(raw)
        except (json.JSONDecodeError, TypeError):
            return {}
    return {}


def _name_and_relative_path_from_memory(mem: dict, default_id: str = "") -> tuple[str, str]:
    """Extract display name and relative_path from a Goodmem memory dict (list or batch-get). Returns (name, relative_path)."""
    meta = _meta_dict(mem)
    file_name = (
        meta.get("name")
        or meta.get("filename")
        or meta.get("fileName")
        or meta.get("displayName")
        or mem.get("name")
        or mem.get("fileName")
    )
    relative_path = meta.get("relative_path") or file_name
    name = file_name or default_id
    path = relative_path or name
    return (name, path)


def _display_name_for_remove(r: dict) -> str:
    """Human-readable file name for a to_remove entry. Prefer actual filename over raw id."""
    name = r.get("relative_path") or r.get("name") or ""
    rid = r.get("id") or ""
    # Use name if it looks like a filename (has a dot, or is not the raw id)
    if name and name != rid:
        return name
    if name:
        return name
    # Fallback when metadata had no name: show truncated id so log isn't redundant
    if rid:
        return f"File {rid[:16]}..." if len(rid) > 16 else f"File {rid}"
    return "(unknown)"


def _format_tree_by_folder(title: str, relative_paths: list[str]) -> str:
    """Format relative paths as an ASCII tree grouped by folder (FolderA/ then indented files)."""
    lines = [title + ":"]
    if not relative_paths:
        lines.append("  (none)")
        return "\n".join(lines)
    # Group by first path component (folder); root-level (no "/") go under ""
    groups: dict[str, list[str]] = {}
    for p in relative_paths:
        if "/" in p:
            folder, rest = p.split("/", 1)
            groups.setdefault(folder, []).append(rest)
        else:
            groups.setdefault("", []).append(p)
    # Root-level first, then folders alphabetically
    if "" in groups:
        for i, name in enumerate(sorted(groups[""])):
            prefix = "└── " if i == len(groups[""]) - 1 and len(groups) == 1 else "├── "
            lines.append("  " + prefix + name)
    for folder in sorted(k for k in groups if k):
        items = groups[folder]
        lines.append("  " + folder + "/")
        for i, subpath in enumerate(sorted(items)):
            prefix = "└── " if i == len(items) - 1 else "├── "
            lines.append("    " + prefix + subpath)
    return "\n".join(lines)


def _resolve_sync_conflicts(
    to_add: list[dict],
    to_update: list[dict],
    to_remove: list[dict],
) -> None:
    """
    Resolve conflicts so each file_id has at most one action (add, update, or remove).
    For any file_id that appears in more than one of to_add, to_update, to_remove,
    re-check SharePoint and keep only the action that matches current state.
    Mutates the three lists in place. Uses _connector, _drive_id; may call
    _pending_add_discard, _pending_update_discard, _pending_remove_discard.
    """
    global _connector, _drive_id
    if not _connector or not _drive_id:
        return
    add_ids = {f.get("id") for f in to_add if f.get("id")}
    update_ids = {f.get("id") for f in to_update if f.get("id")}
    remove_ids = {r.get("id") for r in to_remove if r.get("id")}
    conflict_ids = (add_ids & update_ids) | (add_ids & remove_ids) | (update_ids & remove_ids)
    for fid in conflict_ids:
        if not fid:
            continue
        try:
            file_info = _connector.get_file_by_id(_drive_id, fid)
        except requests.RequestException:
            continue
        supported = (
            file_info
            and _is_mime_type_supported(file_info.get("mime_type"))
            and file_info.get("download_url")
        )
        if not supported:
            # File absent or unsupported: keep only in to_remove; drop from to_add and to_update
            to_add[:] = [f for f in to_add if f.get("id") != fid]
            to_update[:] = [f for f in to_update if f.get("id") != fid]
            _pending_add_discard(fid)
            _pending_update_discard(fid)
        else:
            # File exists and supported: keep only in to_add or to_update; drop from to_remove
            to_remove[:] = [r for r in to_remove if r.get("id") != fid]
            _pending_remove_discard(fid)
            if fid in add_ids and fid in update_ids:
                # In both add and update: keep only in to_update (update = delete-then-add)
                to_add[:] = [f for f in to_add if f.get("id") != fid]


def _compute_delta(
    root_files: list[dict], memories: list[dict]
) -> tuple[list[dict], list[dict], list[dict]]:
    """
    Compare SharePoint root files vs Goodmem memories using UUID set operations and batch-get.
    Returns (to_add, to_update, to_remove).
    to_add: file_info for files in SharePoint only (supported MIME).
    to_update: file_info for files in both but older modified_datetime in Goodmem (supported MIME).
    to_remove: items {id, name, relative_path} for memories no longer in SharePoint.
    """
    global _goodmem
    if not _goodmem:
        return ([], [], [])
    sp_by_id = {f["id"]: f for f in root_files}
    sharepoint_uuids = {uuid_from_file_id(fid) for fid in sp_by_id}
    goodmem_uuids = {m["memoryId"] for m in memories}
    mem_by_id = {m["memoryId"]: m for m in memories}
    uuid_to_file_id = {uuid_from_file_id(fid): fid for fid in sp_by_id}

    to_add_uuids = sharepoint_uuids - goodmem_uuids
    to_delete_uuids = goodmem_uuids - sharepoint_uuids
    both_uuids = sharepoint_uuids & goodmem_uuids

    # Batch-get both_uuids to decide which need update (Goodmem modified_datetime older than SharePoint)
    to_update_uuids: set[str] = set()
    if both_uuids:
        both_meta = _goodmem.batch_get_memories(list(both_uuids))
        for u in both_uuids:
            fid = uuid_to_file_id.get(u)
            if not fid:
                continue
            gm_modified = ((both_meta.get(u) or {}).get("metadata") or {}).get("modified_datetime")
            sp_modified = sp_by_id[fid].get("modified_datetime")
            if gm_modified is None or sp_modified is None:
                to_update_uuids.add(u)
                continue
            if gm_modified < sp_modified:
                to_update_uuids.add(u)
            elif gm_modified > sp_modified:
                print(f"[listener] Unexpected: Goodmem modified_datetime newer than SharePoint for file_id={fid}", file=sys.stderr)

    to_add = [
        sp_by_id[uuid_to_file_id[u]]
        for u in to_add_uuids
        if u in uuid_to_file_id and _is_mime_type_supported(sp_by_id[uuid_to_file_id[u]].get("mime_type"))
    ]
    to_update = [
        sp_by_id[uuid_to_file_id[u]]
        for u in to_update_uuids
        if u in uuid_to_file_id and _is_mime_type_supported(sp_by_id[uuid_to_file_id[u]].get("mime_type"))
    ]

    # Build to_remove from list data (memories) so we get metadata.name/relative_path the list returned
    debug_remove_meta = os.getenv("DEBUG_REMOVE_METADATA", "").strip().lower() in ("1", "true", "yes")
    debug_remove_meta_logged = 0
    debug_remove_meta_limit = 5

    def _meta_debug_summary(mem: dict) -> str:
        raw = mem.get("metadata")
        if isinstance(raw, str):
            return f"metadata=str len={len(raw)}"
        if isinstance(raw, dict):
            return f"metadata=dict keys={list(raw.keys())}"
        if raw is None:
            return "metadata=None"
        return f"metadata={type(raw).__name__}"

    to_remove: list[dict] = []
    for u in to_delete_uuids:
        mem = mem_by_id.get(u) or {}
        meta = _meta_dict(mem)
        file_name, relative_path = _name_and_relative_path_from_memory(mem, default_id=u)
        rid = meta.get("id") or u
        if debug_remove_meta and debug_remove_meta_logged < debug_remove_meta_limit:
            if not (meta.get("relative_path") or meta.get("name") or file_name):
                print(
                    "[listener][debug] list memory missing path/name: "
                    f"memoryId={u} keys={list(mem.keys())} {_meta_debug_summary(mem)}",
                    file=sys.stderr,
                )
                debug_remove_meta_logged += 1
        to_remove.append({
            "id": rid,
            "name": file_name or rid,
            "relative_path": relative_path or rid,
            "_uuid": u,
        })
    # If list didn't return metadata with name/relative_path, batch-get so we can show paths in diff tree and logs
    need_names = [r["_uuid"] for r in to_remove if (r.get("name") or r.get("relative_path") or "").strip() in ("", r.get("id"))]
    if need_names:
        try:
            extra = _goodmem.batch_get_memories(need_names)
            for r in to_remove:
                if r["_uuid"] not in extra:
                    continue
                mem = extra[r["_uuid"]] or {}
                file_name, relative_path = _name_and_relative_path_from_memory(mem, default_id=r.get("id", ""))
                if file_name:
                    r["name"] = file_name
                if relative_path:
                    r["relative_path"] = relative_path
                if debug_remove_meta and debug_remove_meta_logged < debug_remove_meta_limit:
                    if not (r.get("relative_path") and r.get("relative_path") != r.get("id")):
                        print(
                            "[listener][debug] batch-get memory missing path/name: "
                            f"memoryId={r['_uuid']} keys={list(mem.keys())} {_meta_debug_summary(mem)}",
                            file=sys.stderr,
                        )
                        debug_remove_meta_logged += 1
        except requests.RequestException:
            pass
    for r in to_remove:
        r.pop("_uuid", None)

    return (to_add, to_update, to_remove)


def _compute_file_diff() -> Optional[dict]:
    """Compare SharePoint drive root files vs Goodmem space (UUID set-based). Returns dict with only_in_sharepoint, only_in_goodmem, in_both, or None if not ready."""
    global _connector, _goodmem, _drive_id, _space_id
    if not _connector or not _drive_id or not _goodmem or not _space_id:
        return None
    try:
        root_files = _connector.list_files(drive_id=_drive_id, folder_path="", recursive=True)
        memories = _goodmem.list_all_memories(_space_id)
    except requests.RequestException as e:
        _log_goodmem_error(e)
        return None
    except Exception:
        return None
    sharepoint_uuids = {uuid_from_file_id(f["id"]) for f in root_files}
    goodmem_uuids = {m["memoryId"] for m in memories}
    sp_by_id = {f["id"]: {"id": f["id"], "name": f.get("name") or f["id"], "modified_datetime": f.get("modified_datetime")} for f in root_files}
    only_in_sharepoint = [sp_by_id[f["id"]] for f in root_files if uuid_from_file_id(f["id"]) not in goodmem_uuids]
    only_in_goodmem = []
    for mem in memories:
        if mem["memoryId"] not in sharepoint_uuids:
            meta = mem.get("metadata") or {}
            only_in_goodmem.append({"id": meta.get("id"), "name": meta.get("name") or meta.get("id"), "modified_datetime": meta.get("modified_datetime")})
    in_both = [sp_by_id[f["id"]] for f in root_files if uuid_from_file_id(f["id"]) in goodmem_uuids]
    return {"only_in_sharepoint": only_in_sharepoint, "only_in_goodmem": only_in_goodmem, "in_both": in_both}


def _get_delta_link_path() -> str:
    """Path to file storing the Graph delta link. Uses GRAPH_DELTA_TOKEN_FILE or default in script dir."""
    global _delta_link_path
    if _delta_link_path is not None:
        return _delta_link_path
    env_path = (os.getenv("GRAPH_DELTA_TOKEN_FILE") or "").strip()
    if env_path:
        _delta_link_path = os.path.abspath(env_path)
        return _delta_link_path
    script_dir = os.path.dirname(os.path.abspath(__file__))
    _delta_link_path = os.path.join(script_dir, ".graph_delta_link")
    return _delta_link_path


def _load_delta_link() -> Optional[str]:
    """Load persisted delta link from file. Returns None if missing or empty."""
    path = _get_delta_link_path()
    try:
        with open(path, "r", encoding="utf-8") as f:
            link = (f.read() or "").strip()
            return link if link else None
    except FileNotFoundError:
        return None
    except OSError:
        return None


def _save_delta_link(delta_link: str) -> None:
    """Persist delta link to file."""
    path = _get_delta_link_path()
    with _delta_link_lock:
        try:
            with open(path, "w", encoding="utf-8") as f:
                f.write(delta_link)
        except OSError as e:
            print(f"[listener] Failed to save delta link: {e}", file=sys.stderr)


def _clear_delta_link() -> None:
    """Remove persisted delta link file."""
    path = _get_delta_link_path()
    with _delta_link_lock:
        try:
            if os.path.isfile(path):
                os.remove(path)
        except OSError as e:
            print(f"[listener] Failed to clear delta link: {e}", file=sys.stderr)


def _get_pending_removes_path() -> str:
    """Path to file storing pending-remove file_ids (one per line). Same dir as delta link."""
    global _pending_remove_path
    if _pending_remove_path is not None:
        return _pending_remove_path
    base = _get_delta_link_path()
    _pending_remove_path = os.path.join(os.path.dirname(base), ".graph_pending_removes")
    return _pending_remove_path


def _load_pending_removes() -> list[dict]:
    """Load persisted pending-remove entries. Returns list of {id, name, relative_path} for merge; uses stored display name when present."""
    global _pending_remove_file_ids, _pending_remove_display_names
    path = _get_pending_removes_path()
    with _pending_remove_lock:
        try:
            with open(path, "r", encoding="utf-8") as f:
                _pending_remove_file_ids = set()
                _pending_remove_display_names = {}
                for line in f:
                    line = line.strip()
                    if not line:
                        continue
                    if "\t" in line:
                        fid, display = line.split("\t", 1)
                        fid, display = fid.strip(), display.strip()
                        if fid:
                            _pending_remove_file_ids.add(fid)
                            _pending_remove_display_names[fid] = display or fid
                    else:
                        if line:
                            _pending_remove_file_ids.add(line)
                            _pending_remove_display_names[line] = line
                return [
                    {"id": fid, "name": _pending_remove_display_names.get(fid, fid), "relative_path": _pending_remove_display_names.get(fid, fid)}
                    for fid in sorted(_pending_remove_file_ids)
                ]
        except FileNotFoundError:
            _pending_remove_file_ids = set()
            _pending_remove_display_names = {}
            return []
        except OSError:
            return [
                {"id": fid, "name": _pending_remove_display_names.get(fid, fid), "relative_path": _pending_remove_display_names.get(fid, fid)}
                for fid in sorted(_pending_remove_file_ids)
            ]


def _save_pending_removes() -> None:
    """Persist pending-remove file_ids and display names (one line per id, tab-separated: file_id and optional display_name)."""
    path = _get_pending_removes_path()
    with _pending_remove_lock:
        try:
            with open(path, "w", encoding="utf-8") as f:
                for fid in sorted(_pending_remove_file_ids):
                    display = _pending_remove_display_names.get(fid, fid)
                    if display != fid:
                        f.write(f"{fid}\t{display}\n")
                    else:
                        f.write(fid + "\n")
        except OSError as e:
            print(f"[listener] Failed to save pending removes: {e}", file=sys.stderr)


def _pending_remove_add(file_id: str, display_name: Optional[str] = None) -> None:
    """Add file_id to pending removes and persist. display_name used for logs on retry. Retried on next full/delta sync."""
    global _pending_remove_file_ids, _pending_remove_display_names
    _load_pending_removes()  # ensure state is loaded before we add
    with _pending_remove_lock:
        _pending_remove_file_ids.add(file_id)
        _pending_remove_display_names[file_id] = display_name or file_id
    _save_pending_removes()


def _pending_remove_discard(file_id: str) -> None:
    """Remove file_id from pending removes and persist."""
    global _pending_remove_file_ids, _pending_remove_display_names
    _load_pending_removes()  # ensure set is loaded so we don't overwrite file with stale empty set
    with _pending_remove_lock:
        _pending_remove_file_ids.discard(file_id)
        _pending_remove_display_names.pop(file_id, None)
    _save_pending_removes()


def _get_pending_add_path() -> str:
    base = _get_delta_link_path()
    global _pending_add_path
    if _pending_add_path is None:
        _pending_add_path = os.path.join(os.path.dirname(base), ".graph_pending_add")
    return _pending_add_path


def _load_pending_add() -> set[str]:
    global _pending_add_file_ids
    path = _get_pending_add_path()
    with _pending_add_lock:
        try:
            with open(path, "r", encoding="utf-8") as f:
                lines = (line.strip() for line in f if line.strip())
                _pending_add_file_ids = set(lines)
                return set(_pending_add_file_ids)
        except FileNotFoundError:
            _pending_add_file_ids = set()
            return set()
        except OSError:
            return set(_pending_add_file_ids)


def _save_pending_add() -> None:
    path = _get_pending_add_path()
    with _pending_add_lock:
        try:
            with open(path, "w", encoding="utf-8") as f:
                for fid in sorted(_pending_add_file_ids):
                    f.write(fid + "\n")
        except OSError as e:
            print(f"[listener] Failed to save pending add: {e}", file=sys.stderr)


def _pending_add_add(file_id: str) -> None:
    global _pending_add_file_ids
    _load_pending_add()
    with _pending_add_lock:
        _pending_add_file_ids.add(file_id)
    _save_pending_add()


def _pending_add_discard(file_id: str) -> None:
    global _pending_add_file_ids
    _load_pending_add()
    with _pending_add_lock:
        _pending_add_file_ids.discard(file_id)
    _save_pending_add()


def _get_pending_update_path() -> str:
    base = _get_delta_link_path()
    global _pending_update_path
    if _pending_update_path is None:
        _pending_update_path = os.path.join(os.path.dirname(base), ".graph_pending_update")
    return _pending_update_path


def _load_pending_update() -> set[str]:
    global _pending_update_file_ids
    path = _get_pending_update_path()
    with _pending_update_lock:
        try:
            with open(path, "r", encoding="utf-8") as f:
                lines = (line.strip() for line in f if line.strip())
                _pending_update_file_ids = set(lines)
                return set(_pending_update_file_ids)
        except FileNotFoundError:
            _pending_update_file_ids = set()
            return set()
        except OSError:
            return set(_pending_update_file_ids)


def _save_pending_update() -> None:
    path = _get_pending_update_path()
    with _pending_update_lock:
        try:
            with open(path, "w", encoding="utf-8") as f:
                for fid in sorted(_pending_update_file_ids):
                    f.write(fid + "\n")
        except OSError as e:
            print(f"[listener] Failed to save pending update: {e}", file=sys.stderr)


def _pending_update_add(file_id: str) -> None:
    global _pending_update_file_ids
    _load_pending_update()
    with _pending_update_lock:
        _pending_update_file_ids.add(file_id)
    _save_pending_update()


def _pending_update_discard(file_id: str) -> None:
    global _pending_update_file_ids
    _load_pending_update()
    with _pending_update_lock:
        _pending_update_file_ids.discard(file_id)
    _save_pending_update()


def _bootstrap_delta_link() -> None:
    """Get current delta link from Graph (token=latest) and persist it. Call after full sync."""
    global _connector, _drive_id
    if not _connector or not _drive_id:
        return
    try:
        items, new_link = _connector.drive_delta(_drive_id, token_latest=True)
        if new_link:
            _save_delta_link(new_link)
            _log_activity("delta_bootstrap", "Delta link saved for future syncs")
    except requests.RequestException as e:
        print(f"[listener] Bootstrap delta link failed: {e}", file=sys.stderr)
        _log_activity("error", f"Bootstrap delta link failed: {e}")


def _run_delta_sync(reason: str = "delta sync") -> None:
    """
    Sync using Graph delta API: fetch only changes since last deltaLink, apply to Goodmem.
    If no delta link or 410 Gone, run full list vs Goodmem then bootstrap delta link.
    """
    global _connector, _goodmem, _drive_id, _space_id
    if not _connector or not _drive_id:
        return

    delta_link = _load_delta_link()
    if not delta_link:
        _run_full_sync(reason=reason)
        _bootstrap_delta_link()
        return

    _log_activity("info", f"[{reason}] Delta sync (SharePoint → Goodmem) started", reason=reason)
    if not _ensure_space_id():
        _log_activity("skipped", f"No Goodmem space ({reason})", item_id=None)
        return

    try:
        items, new_delta_link = _connector.drive_delta(_drive_id, delta_link=delta_link)
    except requests.RequestException as e:
        _log_goodmem_error(e)
        print(f"[listener] Delta API failed: {e}", file=sys.stderr)
        _log_activity("error", f"Delta API failed: {e}")
        return

    if items is None and new_delta_link is None:
        _clear_delta_link()
        _log_activity("info", "[delta] Token invalid (410); running full sync", reason="410")
        _run_full_sync(reason=f"{reason} (410)")
        _bootstrap_delta_link()
        return

    to_remove: list[dict] = []
    to_sync: list[dict] = []
    for item in items or []:
        if not isinstance(item, dict):
            continue
        item_id = item.get("id")
        if not item_id:
            continue
        if item.get("deleted"):
            display = item.get("name") or item_id
            to_remove.append({"id": item_id, "name": display, "relative_path": display})
            continue
        if "file" not in item:
            continue
        file_info = _connector._format_file_info(item)
        file_info["relative_path"] = item.get("name") or file_info.get("id") or item_id
        if not file_info.get("download_url"):
            full = _connector.get_file_by_id(_drive_id, item_id)
            if full:
                full["relative_path"] = file_info["relative_path"]
                file_info = full
        if file_info and file_info.get("download_url") and _is_mime_type_supported(file_info.get("mime_type")):
            to_sync.append(file_info)

    # Distinguish to_add vs to_update: batch-get UUIDs for to_sync; found -> update, not found -> add
    to_add_or_update_uuids = [uuid_from_file_id(f["id"]) for f in to_sync if f.get("id")]
    to_add: list[dict] = []
    to_update: list[dict] = []
    try:
        batch_result = _goodmem.batch_get_memories(to_add_or_update_uuids) if to_add_or_update_uuids else {}
    except requests.RequestException as e:
        _log_goodmem_error(e)
        print(f"[listener] Goodmem unreachable during delta sync: {e}", file=sys.stderr)
        _log_activity("error", "Cannot batch-get memories for add/update classification")
        return
    for file_info in to_sync:
        fid = file_info.get("id")
        if not fid:
            continue
        u = uuid_from_file_id(fid)
        if u in batch_result:
            # Checkpoint: Goodmem modified_datetime must be older than delta file
            gm_modified = (batch_result[u].get("metadata") or {}).get("modified_datetime")
            sp_modified = file_info.get("modified_datetime")
            if gm_modified is not None and sp_modified is not None and gm_modified >= sp_modified:
                print(f"[listener] Unexpected: Goodmem modified_datetime >= SharePoint for file_id={fid}", file=sys.stderr)
            to_update.append(file_info)
        else:
            to_add.append(file_info)

    # Merge pending removes (failed in a previous run) so we retry on this sync
    pending_removes = _load_pending_removes()
    to_remove_ids = {r["id"] for r in to_remove}
    for r in pending_removes:
        if r["id"] not in to_remove_ids:
            to_remove.append(r)
            to_remove_ids.add(r["id"])
    # Batch-get to_remove memories so we can show relative_path in diff tree and logs (delta items often lack name for deleted)
    if to_remove and _goodmem:
        remove_uuids = [uuid_from_file_id(r["id"]) for r in to_remove if r.get("id")]
        try:
            remove_batch = _goodmem.batch_get_memories(remove_uuids) if remove_uuids else {}
            for r in to_remove:
                uid = uuid_from_file_id(r["id"]) if r.get("id") else None
                if not uid or uid not in remove_batch:
                    continue
                mem = remove_batch.get(uid) or {}
                name, rel_path = _name_and_relative_path_from_memory(mem, default_id=r["id"])
                if name and name != r["id"]:
                    r["name"] = name
                if rel_path and rel_path != r["id"]:
                    r["relative_path"] = rel_path
        except requests.RequestException:
            pass
    # Merge pending add (failed Goodmem add): fetch file_info from SharePoint, add to to_add
    to_add_ids = {f["id"] for f in to_add}
    for fid in _load_pending_add():
        if fid in to_add_ids:
            continue
        try:
            file_info = _connector.get_file_by_id(_drive_id, fid)
        except requests.RequestException:
            continue
        if not file_info:
            _pending_add_discard(fid)
            continue
        if not _is_mime_type_supported(file_info.get("mime_type")) or not file_info.get("download_url"):
            continue
        file_info["relative_path"] = file_info.get("relative_path") or file_info.get("name") or fid
        to_add.append(file_info)
        to_add_ids.add(fid)
    # Merge pending update (failed Goodmem update): fetch file_info from SharePoint, add to to_update
    to_update_ids = {f["id"] for f in to_update}
    for fid in _load_pending_update():
        if fid in to_update_ids:
            continue
        try:
            file_info = _connector.get_file_by_id(_drive_id, fid)
        except requests.RequestException:
            continue
        if not file_info:
            _pending_update_discard(fid)
            continue
        if not _is_mime_type_supported(file_info.get("mime_type")) or not file_info.get("download_url"):
            continue
        file_info["relative_path"] = file_info.get("relative_path") or file_info.get("name") or fid
        to_update.append(file_info)
        to_update_ids.add(fid)
    _resolve_sync_conflicts(to_add, to_update, to_remove)
    # Log delta file tree (same format as full sync) so watchers see To Add / To Update / To Remove
    add_paths = [f.get("relative_path") or f.get("name") or f["id"] for f in to_add]
    update_paths = [f.get("relative_path") or f.get("name") or f["id"] for f in to_update]
    remove_paths = [_display_name_for_remove(r) for r in to_remove]
    tree_add = _format_tree_by_folder("To Add", add_paths)
    tree_update = _format_tree_by_folder("To Update", update_paths)
    tree_remove = _format_tree_by_folder("To Remove", remove_paths)
    delta_message = tree_add + "\n" + tree_update + "\n" + tree_remove
    _log_activity("delta", delta_message, to_add=len(to_add), to_update=len(to_update), to_remove=len(to_remove))

    for r in to_remove:
        rname = _display_name_for_remove(r)
        _log_activity("syncing", "[Syncing] Remove: " + rname + " (id=" + r["id"] + ")", file_name=rname, file_id=r["id"])
        _delete_memory_for_file_id(r["id"], log_name=rname)
    for file_info in to_add:
        path = file_info.get("relative_path") or file_info.get("name") or file_info["id"]
        _log_activity("syncing", "[Syncing] Add: " + path + " (id=" + file_info["id"] + ")", file_name=path, file_id=file_info["id"])
        _add_one_file_to_goodmem(file_info)
    for file_info in to_update:
        path = file_info.get("relative_path") or file_info.get("name") or file_info["id"]
        _log_activity("syncing", "[Syncing] Update: " + path + " (id=" + file_info["id"] + ")", file_name=path, file_id=file_info["id"])
        _update_one_file_to_goodmem(file_info)

    if new_delta_link:
        _save_delta_link(new_delta_link)
    _log_activity("info", f"[{reason}] Delta sync (SharePoint → Goodmem) finished", reason=reason)


def _list_subscriptions(connector: SharePointConnector) -> list[dict]:
    """Return list of change-notification subscriptions for the app."""
    url = f"{connector.base_url}/subscriptions"
    try:
        resp = connector._request("GET", url, timeout=30)
        resp.raise_for_status()
        data = resp.json()
        return data.get("value") or []
    except requests.RequestException:
        return []


def _renew_subscription(
    connector: SharePointConnector,
    subscription_id: str,
    expiration_str: str,
) -> Optional[dict]:
    """Extend a subscription's expiration via PATCH. Returns updated sub JSON or None."""
    url = f"{connector.base_url}/subscriptions/{subscription_id}"
    try:
        resp = connector._request(
            "PATCH",
            url,
            json={"expirationDateTime": expiration_str},
            timeout=30,
        )
        resp.raise_for_status()
        return resp.json()
    except requests.RequestException as e:
        print(f"[listener] Renew subscription failed: {e}", file=sys.stderr)
        return None


def create_subscription(
    connector: SharePointConnector,
    notification_url: str,
    client_state: str,
    drive_id: Optional[str] = None,
    expiration_minutes: int = GRAPH_SUBSCRIPTION_MINUTES_DEFAULT,
) -> Optional[dict]:
    """
    Create or renew the Microsoft Graph subscription for driveItem changes on the given drive.
    If a subscription already exists for the same resource and clientState, it is renewed (PATCH).
    Otherwise a new subscription is created (POST). Returns the subscription JSON on success.
    """
    site_id = connector.get_site_id()
    if not site_id:
        print("[listener] Could not resolve site ID.", file=sys.stderr)
        return None
    if not drive_id:
        drives = connector.get_drives(site_id)
        if not drives:
            print("[listener] No drives found for site.", file=sys.stderr)
            return None
        drive_id = drives[0].get("id")
    connector.print_site_info()
    expiration_minutes = min(expiration_minutes, GRAPH_SUBSCRIPTION_MINUTES_MAX)
    expiration = datetime.now(timezone.utc) + timedelta(minutes=expiration_minutes)
    expiration_str = expiration.strftime("%Y-%m-%dT%H:%M:%S.000Z")
    resource = f"sites/{site_id}/drives/{drive_id}/root"

    print("Creating or renewing Graph subscription...", end=" ", flush=True)
    # If an existing subscription exists for this resource and clientState, renew it
    for sub in _list_subscriptions(connector):
        if sub.get("resource") == resource and sub.get("clientState") == client_state:
            _log_activity("subscription_renewing", "Renewing Graph subscription", subscription_id=sub.get("id"))
            renewed = _renew_subscription(connector, sub["id"], expiration_str)
            if renewed:
                print(f"✓ Renewed. id={renewed.get('id')}, expires={expiration_str}")
                _log_activity(
                    "subscription_renewed",
                    f"Graph subscription renewed (expires {expiration_str})",
                    subscription_id=renewed.get("id"),
                    expirationDateTime=expiration_str,
                    expiration_minutes=expiration_minutes,
                )
            return renewed  # return renewed sub or None on failure

    # No matching subscription: create new one. Drive root supports only "updated".
    _log_activity("subscription_creating", "Creating Graph subscription")
    body = {
        "changeType": "updated",
        "notificationUrl": notification_url,
        "resource": resource,
        "expirationDateTime": expiration_str,
        "clientState": client_state,
        "latestSupportedTlsVersion": "v1_2",
    }
    url = f"{connector.base_url}/subscriptions"
    try:
        resp = connector._request("POST", url, json=body, timeout=30)
        resp.raise_for_status()
        sub = resp.json()
        print(f"✓ Created. id={sub.get('id')}, expires={expiration_str}")
        _log_activity(
            "subscription_created",
            f"Graph subscription created (expires {expiration_str})",
            subscription_id=sub.get("id"),
            expirationDateTime=expiration_str,
            expiration_minutes=expiration_minutes,
        )
        return sub
    except requests.RequestException as e:
        print(f"✗ Failed. {e}", file=sys.stderr)
        if hasattr(e, "response") and e.response is not None:
            try:
                print(e.response.json(), file=sys.stderr)
            except Exception:
                print(e.response.text, file=sys.stderr)
        return None


def _parse_expiration_datetime(expiration_str: str) -> Optional[datetime]:
    """Parse Graph expirationDateTime (ISO 8601, e.g. 2025-02-02T12:00:00.000Z) to timezone-aware datetime."""
    if not expiration_str:
        return None
    try:
        # Graph uses Z suffix; fromisoformat needs +00:00
        s = expiration_str.strip().replace("Z", "+00:00")
        return datetime.fromisoformat(s)
    except (ValueError, TypeError):
        return None


def _subscription_renewal_loop() -> None:
    """
    Background loop: treat expirationDateTime as source of truth. Sleep until (expiration - threshold)
    or MAX_SLEEP, then renew (PATCH). Use new expirationDateTime from response to schedule next run.
    If no subscription exists, create one (when _notification_url is set) then continue.
    """
    global _connector, _drive_id, _client_state, _notification_url, _subscription_info_logged_ids
    threshold = timedelta(minutes=RENEW_SUBSCRIPTION_THRESHOLD_MINUTES)
    max_sleep = RENEW_SUBSCRIPTION_MAX_SLEEP_SECONDS

    while True:
        if not _connector or not _drive_id or not _client_state:
            time.sleep(max_sleep)
            continue
        site_id = _connector.get_site_id()
        if not site_id:
            time.sleep(max_sleep)
            continue
        resource = f"sites/{site_id}/drives/{_drive_id}/root"

        subs = _list_subscriptions(_connector)
        sub = None
        for s in subs:
            if s.get("resource") == resource and s.get("clientState") == _client_state:
                sub = s
                break

        if not sub:
            # No matching subscription: create if we have notification URL
            if _notification_url:
                created = create_subscription(
                    _connector,
                    _notification_url,
                    _client_state,
                    _drive_id,
                    expiration_minutes=_get_subscription_minutes(),
                )
                if created:
                    _subscription_info_logged_ids.add(created.get("id") or "")
                    exp_str = created.get("expirationDateTime")
                    exp_dt = _parse_expiration_datetime(exp_str)
                    if exp_dt:
                        renew_at = exp_dt - threshold
                        now = datetime.now(timezone.utc)
                        sleep_sec = min(
                            max(0.0, (renew_at - now).total_seconds()),
                            float(max_sleep),
                        )
                        # Enforce minimum sleep so Graph list API can return the new sub before we loop (avoids create loop)
                        sleep_sec = max(sleep_sec, float(SUBSCRIPTION_CREATE_COOLDOWN_SECONDS))
                        time.sleep(sleep_sec)
                        continue
            time.sleep(max_sleep)
            continue

        exp_str = sub.get("expirationDateTime")
        exp_dt = _parse_expiration_datetime(exp_str)
        if not exp_dt:
            time.sleep(max_sleep)
            continue

        now = datetime.now(timezone.utc)
        renew_at = exp_dt - threshold

        if renew_at <= now or exp_dt <= now:
            # Renew now: expiration is near or already past
            expiration_minutes = _get_subscription_minutes()
            new_exp = now + timedelta(minutes=expiration_minutes)
            new_exp_str = new_exp.strftime("%Y-%m-%dT%H:%M:%S.000Z")
            _log_activity("subscription_renewing", "Renewing Graph subscription", subscription_id=sub.get("id"))
            renewed = _renew_subscription(_connector, sub["id"], new_exp_str)
            if renewed:
                print(
                    f"[listener] Subscription renewed; next expires {renewed.get('expirationDateTime')}",
                    file=sys.stderr,
                )
                _log_activity(
                    "subscription_renewed",
                    f"Graph subscription renewed (expires {renewed.get('expirationDateTime')})",
                    subscription_id=renewed.get("id"),
                    expirationDateTime=renewed.get("expirationDateTime"),
                    expiration_minutes=expiration_minutes,
                )
                _subscription_info_logged_ids.add(renewed.get("id") or "")
                _sync_queue.put({"value": [], "is_root_sync": False, "force_full_sync": True, "reason": "subscription renew"})
                exp_str = renewed.get("expirationDateTime")
                exp_dt = _parse_expiration_datetime(exp_str)
                if exp_dt:
                    renew_at = exp_dt - threshold
                    now = datetime.now(timezone.utc)
                    sleep_sec = min(
                        max(0.0, (renew_at - now).total_seconds()),
                        float(max_sleep),
                    )
                    time.sleep(sleep_sec)
                    continue
            time.sleep(max_sleep)
            continue

        # Existing subscription; next renewal at renew_at. Log current sub to listener log once per id so watcher sees it.
        sub_id = sub.get("id") or ""
        if sub_id and sub_id not in _subscription_info_logged_ids:
            exp_str_sub = sub.get("expirationDateTime") or ""
            _log_activity(
                "subscription_info",
                f"Current Graph subscription: id={sub_id}, expires={exp_str_sub}",
                subscription_id=sub_id,
                expirationDateTime=exp_str_sub,
            )
            _subscription_info_logged_ids.add(sub_id)

        # Sleep until renew_at (capped)
        sleep_sec = min(
            max(0.0, (renew_at - now).total_seconds()),
            float(max_sleep),
        )
        time.sleep(sleep_sec)


def _run_full_sync(reason: str = "full sync") -> None:
    """Perform full sync from SharePoint drive root to Goodmem (same logic as sync_once.py). Uses _connector, _goodmem, _drive_id; sets _space_id."""
    global _connector, _goodmem, _drive_id, _space_id
    if not _connector or not _drive_id:
        return
    _log_activity("info", f"[{reason}] Full sync (SharePoint → Goodmem) started", reason=reason)
    if not _ensure_space_id():
        _log_activity("skipped", f"No Goodmem space ({reason})", item_id=None)
        return
    try:
        root_files = _connector.list_files(drive_id=_drive_id, folder_path="", recursive=True)
        memories = _goodmem.list_all_memories(_space_id) if (_goodmem and _space_id) else []
    except requests.RequestException as e:
        _log_goodmem_error(e)
        print(f"[listener] Goodmem unreachable during {reason}: {e}", file=sys.stderr)
        return
    # Guard: if SharePoint returned no files but we have memories, do not run delta — likely API/auth failure (e.g. expired token). Avoid deleting everything.
    if not root_files and memories:
        _log_activity(
            "error",
            "SharePoint list_files returned no files but Goodmem has memories; skipping full sync to avoid deleting everything (possible API/auth failure).",
            memory_count=len(memories),
        )
        print("[listener] Skipping full sync: SharePoint returned 0 files but Goodmem has memories (possible auth/API failure).", file=sys.stderr)
        return
    to_add, to_update, to_remove = _compute_delta(root_files, memories)
    # Merge pending removes (failed in a previous run) so we retry on this sync
    pending_removes = _load_pending_removes()
    to_remove_ids = {r["id"] for r in to_remove}
    for r in pending_removes:
        if r["id"] not in to_remove_ids:
            to_remove.append(r)
            to_remove_ids.add(r["id"])
    # Merge pending add (failed Goodmem add): fetch file_info from SharePoint, add to to_add
    to_add_ids = {f["id"] for f in to_add}
    for fid in _load_pending_add():
        if fid in to_add_ids:
            continue
        try:
            file_info = _connector.get_file_by_id(_drive_id, fid)
        except requests.RequestException:
            continue
        if not file_info:
            _pending_add_discard(fid)
            continue
        if not _is_mime_type_supported(file_info.get("mime_type")) or not file_info.get("download_url"):
            continue
        file_info["relative_path"] = file_info.get("relative_path") or file_info.get("name") or fid
        to_add.append(file_info)
        to_add_ids.add(fid)
    # Merge pending update (failed Goodmem update): fetch file_info from SharePoint, add to to_update
    to_update_ids = {f["id"] for f in to_update}
    for fid in _load_pending_update():
        if fid in to_update_ids:
            continue
        try:
            file_info = _connector.get_file_by_id(_drive_id, fid)
        except requests.RequestException:
            continue
        if not file_info:
            _pending_update_discard(fid)
            continue
        if not _is_mime_type_supported(file_info.get("mime_type")) or not file_info.get("download_url"):
            continue
        file_info["relative_path"] = file_info.get("relative_path") or file_info.get("name") or fid
        to_update.append(file_info)
        to_update_ids.add(fid)
    _resolve_sync_conflicts(to_add, to_update, to_remove)
    tree_add = _format_tree_by_folder("To Add", [f.get("relative_path") or f.get("name") or f["id"] for f in to_add])
    tree_update = _format_tree_by_folder("To Update", [f.get("relative_path") or f.get("name") or f["id"] for f in to_update])
    tree_remove = _format_tree_by_folder("To Remove", [_display_name_for_remove(r) for r in to_remove])
    delta_message = tree_add + "\n" + tree_update + "\n" + tree_remove
    _log_activity("delta", delta_message, to_add=len(to_add), to_update=len(to_update), to_remove=len(to_remove))
    for r in to_remove:
        rname = _display_name_for_remove(r)
        _log_activity("syncing", "[Syncing] Remove: " + rname + " (id=" + r["id"] + ")", file_name=rname, file_id=r["id"])
        _delete_memory_for_file_id(r["id"], log_name=rname)
    for file_info in to_add:
        path = file_info.get("relative_path") or file_info.get("name") or file_info["id"]
        _log_activity("syncing", "[Syncing] Add: " + path + " (id=" + file_info["id"] + ")", file_name=path, file_id=file_info["id"])
        _add_one_file_to_goodmem(file_info)
    for file_info in to_update:
        path = file_info.get("relative_path") or file_info.get("name") or file_info["id"]
        _log_activity("syncing", "[Syncing] Update: " + path + " (id=" + file_info["id"] + ")", file_name=path, file_id=file_info["id"])
        _update_one_file_to_goodmem(file_info)
    _log_activity("info", f"[{reason}] Full sync (SharePoint → Goodmem) finished", reason=reason)


def process_notification_value(value: list) -> None:
    """Process a notification payload (value list). Runs in background worker. Always logs 'what_happened'."""
    synced_names: list[str] = []
    deleted_names: list[str] = []
    skipped_reasons: list[str] = []
    error_msgs: list[str] = []
    processing_error: Optional[str] = None
    try:
        for entry in value:
            if not isinstance(entry, dict):
                continue
            client_state = entry.get("clientState")
            if _client_state and client_state != _client_state:
                continue
            change_type = (entry.get("changeType") or "").strip().lower()
            resource = entry.get("resource", "")
            resource_data = entry.get("resourceData") or {}
            item_id = resource_data.get("id")
            parsed = _parse_drive_and_item_from_resource(resource)
            if parsed:
                drive_id, item_id = parsed
            else:
                drive_id = _drive_id_from_resource(resource) or _drive_id
                if not item_id and resource and "/" not in resource:
                    item_id = resource
            if not _connector:
                continue
            if change_type == "updated" and drive_id and (not item_id or _is_root_resource(resource)):
                _run_delta_sync(reason="root notification")
                continue
            if not drive_id or not item_id:
                continue
            if change_type == "deleted":
                _delete_memory_for_file_id(item_id)
                deleted_names.append(item_id)
                continue
            if change_type in ("created", "updated"):
                file_info = _connector.get_file_by_id(drive_id, item_id)
                if not file_info:
                    _log_activity("skipped", "Item not found or not a file", item_id=item_id)
                    skipped_reasons.append("item not found")
                    continue
                if not _ensure_space_id():
                    _log_activity("skipped", "No Goodmem space", item_id=item_id)
                    skipped_reasons.append("no Goodmem space")
                    continue
                path = file_info.get("relative_path") or file_info.get("name") or file_info.get("id", item_id)
                # Distinguish add vs update via batch-get
                u = uuid_from_file_id(item_id)
                try:
                    found = _goodmem.batch_get_memories([u]) if _goodmem else {}
                except requests.RequestException:
                    found = {}
                is_update = u in found
                op = "Update" if is_update else "Add"
                _log_activity("syncing", "[Syncing] " + op + ": " + path + " (id=" + item_id + ")", file_name=path, file_id=item_id)
                result = _update_one_file_to_goodmem(file_info) if is_update else _add_one_file_to_goodmem(file_info)
                if result:
                    outcome, detail = result
                    if outcome == "synced":
                        synced_names.append(detail)
                    elif outcome == "skipped":
                        skipped_reasons.append(detail)
                    elif outcome == "error":
                        error_msgs.append(detail)
    except Exception as e:
        processing_error = str(e)
        print(f"[listener] Error processing notification: {e}", file=sys.stderr)
    finally:
        pass  # Only log [Start]/[Done] Add/Remove per file; no summary message


def _worker_loop() -> None:
    """Background worker: process one notification job at a time; clear root_sync_pending when root sync job finishes."""
    global _root_sync_pending
    while True:
        job = _sync_queue.get()
        try:
            if job.get("force_full_sync"):
                _run_full_sync(reason=job.get("reason", "full sync"))
            else:
                process_notification_value(job["value"])
        except Exception as e:
            print(f"[listener] Worker error: {e}", file=sys.stderr)
        finally:
            if job.get("is_root_sync"):
                with _root_sync_lock:
                    _root_sync_pending = False
        _sync_queue.task_done()


def build_app() -> Flask:
    """Build Flask app that handles Microsoft Graph validation and change notifications."""
    app = Flask(__name__)

    @app.route("/", methods=["GET", "POST"])
    @app.route("/sync/webhook", methods=["GET", "POST"])
    @app.route("/webhook", methods=["GET", "POST"])
    def webhook():
        # Validation: Microsoft Graph sends GET or POST with ?validationToken=...
        validation_token = request.args.get("validationToken")
        if validation_token:
            # Must return 200, text/plain, body = URL-decoded token only
            decoded = unquote(validation_token)
            return decoded, 200, {"Content-Type": "text/plain; charset=utf-8"}

        if request.method != "POST":
            return "Method not allowed", 405

        # Change notification from Microsoft Graph — return 200 quickly, process in background
        try:
            data = request.get_json(force=True, silent=True)
        except Exception:
            return "Bad request", 400
        if not data or "value" not in data:
            return "OK", 200

        value = data.get("value", [])
        if not isinstance(value, list):
            return "OK", 200

        is_root_sync = _is_root_sync_notification(value)
        global _root_sync_pending
        with _root_sync_lock:
            if is_root_sync and _root_sync_pending:
                _log_activity("coalesced", "Root sync already pending; skipping duplicate notification", count=len(value))
                return "", 200
            _log_activity("notification_received", f"Received {len(value)} change(s) from Graph", count=len(value))
            _sync_queue.put({"value": value, "is_root_sync": is_root_sync, "force_full_sync": False})
            if is_root_sync:
                _root_sync_pending = True

        return "", 200

    @app.route("/activity", methods=["GET"])
    def activity():
        """Return recent activity log for watchers. Query: ?since=<id> for events after that id."""
        since = request.args.get("since", type=int)
        with _activity_lock:
            if since is not None:
                events = [e for e in _activity_log if e["id"] > since]
            else:
                events = list(_activity_log)
            latest_id = _activity_id
        return {"events": events, "latest_id": latest_id}

    @app.route("/diff", methods=["GET"])
    def diff():
        """Return file-level diff: SharePoint drive root vs Goodmem space (only_in_sharepoint, only_in_goodmem, in_both)."""
        if not _ensure_space_id():
            return {"error": "Goodmem space not available"}, 503
        diff_result = _compute_file_diff()
        if diff_result is None:
            return {"error": "Diff not available (connector or drive not ready)"}, 503
        # Log a summary so watchers see it
        only_sp = diff_result["only_in_sharepoint"]
        only_gm = diff_result["only_in_goodmem"]
        both = diff_result["in_both"]
        summary = f"Only in SharePoint: {len(only_sp)}; Only in Goodmem: {len(only_gm)}; In both: {len(both)}"
        _log_activity("diff", summary, only_in_sharepoint=[x["name"] for x in only_sp], only_in_goodmem=[x["name"] for x in only_gm], in_both_count=len(both))
        return diff_result

    return app


def main() -> None:
    # Parse optional --env-file before command
    args = sys.argv[1:]
    env_file = None
    if args and args[0] == "--env-file" and len(args) >= 2:
        env_file = args[1]
        args = args[2:]
    cmd = (args[0] if args else "").lower()

    load_env(env_file)
    validate_token_refresh_buffer()
    _validate_subscription_minutes_env()

    # Azure AD (Entra ID) app credentials
    client_id = os.getenv("AZURE_AD_CLIENT_ID")
    tenant_id = os.getenv("AZURE_AD_TENANT_ID")
    client_secret = os.getenv("AZURE_AD_CLIENT_SECRET")
    site_url = os.getenv("SHAREPOINT_SITE_URL")
    if not all([client_id, tenant_id, client_secret, site_url]):
        print(
            "Error: Missing env for Azure AD/SharePoint (AZURE_AD_CLIENT_ID, AZURE_AD_TENANT_ID, AZURE_AD_CLIENT_SECRET, SHAREPOINT_SITE_URL).",
            file=sys.stderr,
        )
        sys.exit(1)

    goodmem_base_url = os.getenv("GOODMEM_BASE_URL")
    goodmem_api_key = os.getenv("GOODMEM_API_KEY")
    if not goodmem_base_url or not goodmem_api_key:
        print("Error: Missing Goodmem env (GOODMEM_BASE_URL, GOODMEM_API_KEY).", file=sys.stderr)
        sys.exit(1)

    notification_url = (os.getenv("GRAPH_NOTIFICATION_URL") or "").strip().rstrip("/")
    client_state = (os.getenv("GRAPH_CLIENT_STATE") or "").strip()
    # Many PaaS (Railway, Render, Heroku) set PORT; fall back to GRAPH_PORT or 5000
    sync_port = int(os.getenv("PORT") or os.getenv("GRAPH_PORT", "5000"))

    if cmd == "create-subscription":
        if not notification_url or not client_state:
            print("Error: GRAPH_NOTIFICATION_URL and GRAPH_CLIENT_STATE are required for create-subscription.", file=sys.stderr)
            sys.exit(1)
        print(f"  Notification URL: {notification_url}")
        connector = SharePointConnector(
            client_id=client_id,
            tenant_id=tenant_id,
            client_secret=client_secret,
            site_url=site_url,
        )
        print("Authenticating with Microsoft Graph...", end=" ", flush=True)
        if not connector.authenticate():
            print("✗ Failed.", file=sys.stderr)
            sys.exit(1)
        print("✓ Success.")
        # Ensure URL has the path Microsoft Graph will POST to (e.g. /sync/webhook)
        if "/sync/webhook" not in notification_url and "/webhook" not in notification_url:
            notification_url = f"{notification_url}/sync/webhook"
        sub = create_subscription(
            connector, notification_url, client_state, expiration_minutes=_get_subscription_minutes()
        )
        if not sub:
            sys.exit(1)
        return

    if cmd == "server":
        if not client_state:
            print("Error: GRAPH_CLIENT_STATE is required for server.", file=sys.stderr)
            sys.exit(1)
        connector = SharePointConnector(
            client_id=client_id,
            tenant_id=tenant_id,
            client_secret=client_secret,
            site_url=site_url,
        )

        def _on_token_refresh(reason: str) -> None:
            start_msg = "OAuth2 authenticating" if reason == "initial" else "OAuth2 re-authenticating"
            _log_activity("oauth2_start", start_msg, reason=reason)
            msg = "OAuth2 initial token obtained" if reason == "initial" else f"OAuth2 re-authenticated ({reason})"
            expires_at = None
            if getattr(connector, "_token_expires_at", None):
                expires_at = connector._token_expires_at.isoformat()
            _log_activity("oauth2_reauth", msg, reason=reason, token_expires_at=expires_at)
            if reason != "initial":
                _sync_queue.put({"value": [], "is_root_sync": False, "force_full_sync": True, "reason": "oauth refresh"})

        connector.on_token_refresh = _on_token_refresh
        print("Authenticating with Microsoft Graph...", end=" ", flush=True)
        if not connector.authenticate():
            print("✗ Failed.", file=sys.stderr)
            sys.exit(1)
        print("✓ Success.")
        drives = connector.get_drives()
        drive_id = drives[0].get("id") if drives else None
        goodmem = GoodmemClient(base_url=goodmem_base_url, api_key=goodmem_api_key)
        # Set globals for webhook handler and renewal thread
        global _connector, _goodmem, _site_url, _drive_id, _client_state, _notification_url
        _connector = connector
        _goodmem = goodmem
        _site_url = site_url
        _drive_id = drive_id
        _client_state = client_state
        _notification_url = (notification_url or "").strip().rstrip("/")
        if _notification_url and "/sync/webhook" not in _notification_url and "/webhook" not in _notification_url:
            _notification_url = f"{_notification_url}/sync/webhook"
        sub_mins = _get_subscription_minutes()
        _log_activity(
            "info",
            f"Graph subscription length: {sub_mins} minutes (set GRAPH_SUBSCRIPTION_MINUTES to test re-subscribe)",
            subscription_minutes=sub_mins,
        )
        app = build_app()
        worker = threading.Thread(target=_worker_loop, daemon=True)
        worker.start()
        renewal = threading.Thread(target=_subscription_renewal_loop, daemon=True)
        renewal.start()
        # Start HTTP server first so Fly/load balancer see the app; run startup sync in background
        def run_startup_sync() -> None:
            print("Running startup full sync (same as sync_once)...", flush=True)
            _run_full_sync(reason="startup")
            _bootstrap_delta_link()
            print("Startup full sync done.", flush=True)
        threading.Thread(target=run_startup_sync, daemon=True).start()
        print(f"Starting webhook server... ✓ Listening on port {sync_port}. Endpoint: /sync/webhook or /")
        app.run(host="0.0.0.0", port=sync_port)
        return

    # Default: print usage
    print("Usage: python listener.py <command>", file=sys.stderr)
    print("  server              Run Microsoft Graph webhook server (must be reachable over HTTPS).", file=sys.stderr)
    print("  create-subscription Create/recreate Graph API subscription for drive changes.", file=sys.stderr)
    print("", file=sys.stderr)
    print("  --env-file PATH   Load this env file. Default: .env", file=sys.stderr)
    print("Env: GRAPH_NOTIFICATION_URL, GRAPH_CLIENT_STATE, GRAPH_PORT (default 5000).", file=sys.stderr)
    print("See permission.md for Azure AD and SharePoint permissions.", file=sys.stderr)
    sys.exit(0)


if __name__ == "__main__":
    main()
