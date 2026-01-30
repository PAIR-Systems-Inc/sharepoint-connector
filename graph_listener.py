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
  python graph_listener.py server

Create or renew the Graph subscription (run once, or before expiration):
  python graph_listener.py create-subscription

Requires: SYNC_NOTIFICATION_URL, SYNC_CLIENT_STATE, and same SharePoint/Goodmem env as main.py.
See permission.md for Azure AD / SharePoint permissions.
"""

import os
import re
import sys
from datetime import datetime, timezone, timedelta
from typing import Optional
from urllib.parse import unquote

import requests
from dotenv import load_dotenv
from flask import Flask, request

from goodmem_client import GoodmemClient
from sharepoint_client import SharePointConnector

# --- Reused logic from main.py (not importing from main to avoid touching it) ---

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
            print("[graph_listener] No embedder available; cannot create space.", file=sys.stderr)
            return None
        created = _goodmem.create_space(space_name=space_name, embedder_id=embedder_id)
        _space_id = created.get("spaceId")
    return _space_id


def _sync_one_file_to_goodmem(file_info: dict) -> bool:
    """Download file and insert (or replace) in Goodmem. Returns True if successful."""
    global _goodmem, _drive_id, _space_id
    if not _goodmem or not _space_id:
        return False
    file_id = file_info.get("id")
    mime_type = file_info.get("mime_type")
    if not _is_mime_type_supported(mime_type):
        return False
    download_url = file_info.get("download_url")
    if not download_url:
        return False
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
        print(f"[graph_listener] Download failed for {file_info.get('name')}: {e}", file=sys.stderr)
        return False
    metadata = {k: v for k, v in file_info.items() if v is not None}
    try:
        _goodmem.insert_memory_binary(
            space_id=_space_id,
            content_bytes=content_bytes,
            content_type=mime_type or "application/octet-stream",
            metadata=metadata,
        )
        return True
    except Exception as e:
        print(f"[graph_listener] Ingest failed for {file_info.get('name')}: {e}", file=sys.stderr)
        return False


def _remove_memory_for_file_id(file_id: str) -> bool:
    """Delete Goodmem memory whose metadata.id equals file_id."""
    global _goodmem, _space_id
    if not _goodmem or not _space_id:
        return False
    memories = _goodmem.list_all_memories(_space_id)
    for mem in memories:
        meta = mem.get("metadata") or {}
        if meta.get("id") == file_id:
            _goodmem.delete_memory(mem.get("memoryId"))
            return True
    return False


def create_subscription(
    connector: SharePointConnector,
    notification_url: str,
    client_state: str,
    drive_id: Optional[str] = None,
    expiration_minutes: int = DEFAULT_SUBSCRIPTION_MINUTES,
) -> Optional[dict]:
    """
    Create a Microsoft Graph subscription for driveItem changes on the given drive.
    Returns the subscription JSON on success, None on failure.
    """
    site_id = connector.get_site_id()
    if not site_id:
        print("[graph_listener] Could not resolve site ID.", file=sys.stderr)
        return None
    if not drive_id:
        drives = connector.get_drives(site_id)
        if not drives:
            print("[graph_listener] No drives found for site.", file=sys.stderr)
            return None
        drive_id = drives[0].get("id")
    expiration_minutes = min(expiration_minutes, DRIVE_ITEM_SUBSCRIPTION_MAX_MINUTES)
    expiration = datetime.now(timezone.utc) + timedelta(minutes=expiration_minutes)
    expiration_str = expiration.strftime("%Y-%m-%dT%H:%M:%S.000Z")
    resource = f"sites/{site_id}/drives/{drive_id}/root"
    body = {
        "changeType": "created,updated,deleted",
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
        print(f"[graph_listener] Subscription created: id={sub.get('id')}, expires={expiration_str}")
        return sub
    except requests.RequestException as e:
        print(f"[graph_listener] Create subscription failed: {e}", file=sys.stderr)
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

        for entry in value:
            if not isinstance(entry, dict):
                continue
            client_state = entry.get("clientState")
            if _client_state and client_state != _client_state:
                continue
            change_type = entry.get("changeType", "")
            resource = entry.get("resource", "")
            resource_data = entry.get("resourceData") or {}
            item_id = resource_data.get("id")
            parsed = _parse_drive_and_item_from_resource(resource)
            if parsed:
                drive_id, item_id = parsed
            else:
                drive_id = _drive_id
                if not item_id and resource and "/" not in resource:
                    item_id = resource
            if not drive_id or not item_id:
                continue
            if not _connector:
                continue
            if change_type == "deleted":
                _remove_memory_for_file_id(item_id)
                continue
            if change_type in ("created", "updated"):
                file_info = _connector.get_file_by_id(drive_id, item_id)
                if file_info and _ensure_space_id():
                    _sync_one_file_to_goodmem(file_info)

        return "", 200

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
    goodmem_api_key = os.getenv("GOODMEM_API_KEY") or os.getenv("GOOMEM_API_KEY")
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
        if not connector.authenticate():
            print("Failed to authenticate with Microsoft Graph.", file=sys.stderr)
            sys.exit(1)
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
        if not connector.authenticate():
            print("Failed to authenticate with Microsoft Graph.", file=sys.stderr)
            sys.exit(1)
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
        print(f"[graph_listener] Microsoft Graph webhook server listening on port {sync_port}. Endpoint: /sync/webhook or /")
        app.run(host="0.0.0.0", port=sync_port)
        return

    # Default: print usage
    print("Usage: python graph_listener.py <command>", file=sys.stderr)
    print("  server              Run Microsoft Graph webhook server (must be reachable over HTTPS).", file=sys.stderr)
    print("  create-subscription Create/recreate Graph API subscription for drive changes.", file=sys.stderr)
    print("", file=sys.stderr)
    print("Env: SYNC_NOTIFICATION_URL, SYNC_CLIENT_STATE, SYNC_PORT (default 5000).", file=sys.stderr)
    print("See permission.md for Azure AD and SharePoint permissions.", file=sys.stderr)
    sys.exit(0)


if __name__ == "__main__":
    main()
