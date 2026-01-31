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

Requires: SYNC_NOTIFICATION_URL, SYNC_CLIENT_STATE, and same SharePoint/Goodmem env as sync_once.py.
See permission.md for Azure AD / SharePoint permissions.
"""

import os
import re
import sys
import threading
from datetime import datetime, timezone, timedelta
from typing import Optional
from urllib.parse import unquote

import requests
from dotenv import load_dotenv
from flask import Flask, request

from goodmem_client import GoodmemClient
from sharepoint_client import SharePointConnector

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

# driveItem max expiration per Microsoft docs (under 30 days)
DRIVE_ITEM_SUBSCRIPTION_MAX_MINUTES = 42_300
# Default: 3 days
DEFAULT_SUBSCRIPTION_MINUTES = 3 * 24 * 60

# App-global state for webhook handler (set by server entrypoint)
_connector: Optional[SharePointConnector] = None
_goodmem: Optional[GoodmemClient] = None
_site_url: Optional[str] = None
_drive_id: Optional[str] = None
_space_id: Optional[str] = None
_client_state: Optional[str] = None

# Activity log for watchers (last N events)
_activity_log: list[dict] = []
_activity_lock = threading.Lock()
_activity_max = 200
_activity_id = 0


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


def _ensure_space_id() -> Optional[str]:
    """Resolve Goodmem space ID from site URL; create space if needed."""
    global _space_id, _goodmem, _site_url
    if _space_id:
        return _space_id
    if not _goodmem or not _site_url:
        return None
    space_name = _space_name_from_site_url(_site_url)
    _space_id = _goodmem.find_space_by_name(space_name)
    if _space_id is None:
        embedders = _goodmem.list_embedders()
        embedder_id = (os.getenv("DEFAULT_EMBEDDER_ID") or
                      (embedders[0].get("embedderId") if embedders else None))
        if not embedder_id:
            print("[listener] No embedder available; cannot create space.", file=sys.stderr)
            return None
        created = _goodmem.create_space(space_name=space_name, embedder_id=embedder_id)
        _space_id = created.get("spaceId")
    return _space_id


def _sync_one_file_to_goodmem(file_info: dict) -> Optional[tuple[str, str]]:
    """Download file and insert (or replace) in Goodmem. Returns ('synced', name), ('skipped', reason), ('error', msg), or None."""
    global _goodmem, _drive_id, _space_id
    if not _goodmem or not _space_id:
        return None
    file_id = file_info.get("id")
    name = file_info.get("name") or "(unknown)"
    mime_type = file_info.get("mime_type")
    if not _is_mime_type_supported(mime_type):
        _log_activity("skipped", f"Unsupported MIME: {name}", file_name=name)
        return ("skipped", f"Unsupported MIME: {name}")
    download_url = file_info.get("download_url")
    if not download_url:
        _log_activity("skipped", f"No download URL: {name}", file_name=name)
        return ("skipped", f"No download URL: {name}")
    # For updates: remove existing memory keyed by SharePoint file id
    memories = _goodmem.list_all_memories(_space_id)
    for mem in memories:
        meta = mem.get("metadata") or {}
        if meta.get("id") == file_id:
            _goodmem.delete_memory(mem.get("memoryId"))
            break
    try:
        content_bytes = _download_file(download_url)
    except Exception as e:
        print(f"[listener] Download failed for {file_info.get('name')}: {e}", file=sys.stderr)
        _log_activity("error", f"Download failed: {name}", error=str(e))
        return ("error", f"Download failed: {name}")
    metadata = {k: v for k, v in file_info.items() if v is not None}
    try:
        _goodmem.insert_memory_binary(
            space_id=_space_id,
            content_bytes=content_bytes,
            content_type=mime_type or "application/octet-stream",
            metadata=metadata,
        )
        _log_activity("synced", f"Synced to Goodmem: {name}", file_name=name, file_id=file_id)
        return ("synced", name)
    except Exception as e:
        print(f"[listener] Ingest failed for {file_info.get('name')}: {e}", file=sys.stderr)
        _log_activity("error", f"Ingest failed: {name}", error=str(e), file_name=name)
        return ("error", f"Ingest failed: {name}")


def _remove_memory_for_file_id(file_id: str) -> Optional[str]:
    """Delete Goodmem memory whose metadata.id equals file_id. Returns deleted file name or None."""
    global _goodmem, _space_id
    if not _goodmem or not _space_id:
        return None
    memories = _goodmem.list_all_memories(_space_id)
    for mem in memories:
        meta = mem.get("metadata") or {}
        if meta.get("id") == file_id:
            name = meta.get("name") or file_id
            _goodmem.delete_memory(mem.get("memoryId"))
            _log_activity("deleted", f"Removed from Goodmem: {name}", file_name=name, file_id=file_id)
            return name
    return None


def _list_subscriptions(connector: SharePointConnector) -> list[dict]:
    """Return list of change-notification subscriptions for the app."""
    url = f"{connector.base_url}/subscriptions"
    try:
        resp = requests.get(url, headers=connector._get_headers(), timeout=30)
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
        resp = requests.patch(
            url,
            json={"expirationDateTime": expiration_str},
            headers=connector._get_headers(),
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
    expiration_minutes: int = DEFAULT_SUBSCRIPTION_MINUTES,
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
    expiration_minutes = min(expiration_minutes, DRIVE_ITEM_SUBSCRIPTION_MAX_MINUTES)
    expiration = datetime.now(timezone.utc) + timedelta(minutes=expiration_minutes)
    expiration_str = expiration.strftime("%Y-%m-%dT%H:%M:%S.000Z")
    resource = f"sites/{site_id}/drives/{drive_id}/root"

    print("Creating or renewing Graph subscription...", end=" ", flush=True)
    # If an existing subscription exists for this resource and clientState, renew it
    for sub in _list_subscriptions(connector):
        if sub.get("resource") == resource and sub.get("clientState") == client_state:
            renewed = _renew_subscription(connector, sub["id"], expiration_str)
            if renewed:
                print(f"✓ Renewed. id={renewed.get('id')}, expires={expiration_str}")
            return renewed  # return renewed sub or None on failure

    # No matching subscription: create new one. Drive root supports only "updated".
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
        resp = requests.post(url, json=body, headers=connector._get_headers(), timeout=30)
        resp.raise_for_status()
        sub = resp.json()
        print(f"✓ Created. id={sub.get('id')}, expires={expiration_str}")
        return sub
    except requests.RequestException as e:
        print(f"✗ Failed. {e}", file=sys.stderr)
        if hasattr(e, "response") and e.response is not None:
            try:
                print(e.response.json(), file=sys.stderr)
            except Exception:
                print(e.response.text, file=sys.stderr)
        return None


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

        # Change notification from Microsoft Graph
        try:
            data = request.get_json(force=True, silent=True)
        except Exception:
            return "Bad request", 400
        if not data or "value" not in data:
            return "OK", 200

        value = data.get("value", [])
        if not isinstance(value, list):
            return "OK", 200

        _log_activity("notification_received", f"Received {len(value)} change(s) from Graph", count=len(value))

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
                # Drive root subscription only sends "updated"; new files appear as root "updated".
                # When resource is the root or we have no item id, sync all files under root.
                if change_type == "updated" and drive_id and (not item_id or _is_root_resource(resource)):
                    if not _ensure_space_id():
                        _log_activity("skipped", "No Goodmem space (root updated)", item_id=None)
                        skipped_reasons.append("no Goodmem space")
                        continue
                    root_files = _connector.list_files(drive_id=drive_id, folder_path="", recursive=True)
                    for file_info in root_files:
                        result = _sync_one_file_to_goodmem(file_info)
                        if result:
                            outcome, detail = result
                            if outcome == "synced":
                                synced_names.append(detail)
                            elif outcome == "skipped":
                                skipped_reasons.append(detail)
                            elif outcome == "error":
                                error_msgs.append(detail)
                    continue
                if not drive_id or not item_id:
                    continue
                if change_type == "deleted":
                    name = _remove_memory_for_file_id(item_id)
                    if name:
                        deleted_names.append(name)
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
                    result = _sync_one_file_to_goodmem(file_info)
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
            parts = []
            if synced_names:
                parts.append(f"{len(synced_names)} synced ({', '.join(synced_names)})")
            if deleted_names:
                parts.append(f"{len(deleted_names)} deleted ({', '.join(deleted_names)})")
            if skipped_reasons:
                parts.append(f"{len(skipped_reasons)} skipped ({'; '.join(skipped_reasons[:5])}{'...' if len(skipped_reasons) > 5 else ''})")
            if error_msgs:
                parts.append(f"{len(error_msgs)} errors ({'; '.join(error_msgs[:5])}{'...' if len(error_msgs) > 5 else ''})")
            if processing_error:
                parts.append(f"processing error: {processing_error}")
            summary = "; ".join(parts) if parts else "no actions (filtered or ignored)"
            _log_activity("what_happened", f"Processed {len(value)} change(s): {summary}", synced=len(synced_names), deleted=len(deleted_names), skipped=len(skipped_reasons), errors=len(error_msgs))

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

    return app


def main() -> None:
    load_dotenv()
    cmd = (sys.argv[1] if len(sys.argv) > 1 else "").lower()

    client_id = os.getenv("SHAREPOINT_CLIENT_ID")
    tenant_id = os.getenv("SHAREPOINT_TENANT_ID")
    client_secret = os.getenv("SHAREPOINT_CLIENT_SECRET")
    site_url = os.getenv("SHAREPOINT_SITE_URL")
    if not all([client_id, tenant_id, client_secret, site_url]):
        print("Error: Missing SharePoint env (SHAREPOINT_CLIENT_ID, TENANT_ID, CLIENT_SECRET, SITE_URL).", file=sys.stderr)
        sys.exit(1)

    goodmem_base_url = os.getenv("GOODMEM_BASE_URL")
    goodmem_api_key = os.getenv("GOODMEM_API_KEY")
    if not goodmem_base_url or not goodmem_api_key:
        print("Error: Missing Goodmem env (GOODMEM_BASE_URL, GOODMEM_API_KEY).", file=sys.stderr)
        sys.exit(1)

    notification_url = (os.getenv("SYNC_NOTIFICATION_URL") or "").strip().rstrip("/")
    client_state = (os.getenv("SYNC_CLIENT_STATE") or "").strip()
    # Many PaaS (Railway, Render, Heroku) set PORT; fall back to SYNC_PORT or 5000
    sync_port = int(os.getenv("PORT") or os.getenv("SYNC_PORT", "5000"))

    if cmd == "create-subscription":
        if not notification_url or not client_state:
            print("Error: SYNC_NOTIFICATION_URL and SYNC_CLIENT_STATE are required for create-subscription.", file=sys.stderr)
            sys.exit(1)
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
        sub = create_subscription(connector, notification_url, client_state)
        if not sub:
            sys.exit(1)
        return

    if cmd == "server":
        if not client_state:
            print("Error: SYNC_CLIENT_STATE is required for server.", file=sys.stderr)
            sys.exit(1)
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
        drives = connector.get_drives()
        drive_id = drives[0].get("id") if drives else None
        goodmem = GoodmemClient(base_url=goodmem_base_url, api_key=goodmem_api_key)
        # Set globals for webhook handler
        global _connector, _goodmem, _site_url, _drive_id, _client_state
        _connector = connector
        _goodmem = goodmem
        _site_url = site_url
        _drive_id = drive_id
        _client_state = client_state
        app = build_app()
        print(f"Starting webhook server... ✓ Listening on port {sync_port}. Endpoint: /sync/webhook or /")
        app.run(host="0.0.0.0", port=sync_port)
        return

    # Default: print usage
    print("Usage: python listener.py <command>", file=sys.stderr)
    print("  server              Run Microsoft Graph webhook server (must be reachable over HTTPS).", file=sys.stderr)
    print("  create-subscription Create/recreate Graph API subscription for drive changes.", file=sys.stderr)
    print("", file=sys.stderr)
    print("Env: SYNC_NOTIFICATION_URL, SYNC_CLIENT_STATE, SYNC_PORT (default 5000).", file=sys.stderr)
    print("See permission.md for Azure AD and SharePoint permissions.", file=sys.stderr)
    sys.exit(0)


if __name__ == "__main__":
    main()
