#!/usr/bin/env -S uv run --script
# /// script
# requires-python = ">=3.10"
# dependencies = [
#     "requests>=2.31.0",
#     "python-dotenv>=1.0.0",
# ]
# ///
"""
Main script to sync files from SharePoint to Goodmem.

Run: python main.py   or   ./main.py (with uv shebang)

Every run syncs all files from SharePoint to Goodmem: ingests new files,
re-ingests updated files (after deleting the old copy), and deletes
memories for files that no longer exist in SharePoint.
"""

import os
from urllib.parse import urlparse

import requests
from dotenv import load_dotenv

from goodmem_client import GoodmemClient
from sharepoint_client import SharePointConnector


def _is_mime_type_supported(mime_type: str) -> bool:
    """Checks if a MIME type is supported by Goodmem's TextContentExtractor.

    Based on the Goodmem source code, TextContentExtractor supports:
    - All text/* MIME types
    - application/pdf
    - application/rtf
    - application/msword (.doc)
    - application/vnd.openxmlformats-officedocument.wordprocessingml.document (.docx)
    - Any MIME type containing "+xml" (e.g., application/xhtml+xml, application/epub+zip)
    - Any MIME type containing "json" (e.g., application/json)

    Args:
      mime_type: The MIME type to check (e.g., "image/png", "application/pdf").

    Returns:
      True if the MIME type is supported by Goodmem, False otherwise.
    """
    if not mime_type:
        return False

    mime_type_lower = mime_type.lower()

    # All text/* types are supported
    if mime_type_lower.startswith("text/"):
        return True

    # Specific application types
    if mime_type_lower in (
        "application/pdf",
        "application/rtf",
        "application/msword",
        "application/vnd.openxmlformats-officedocument.wordprocessingml.document",
    ):
        return True

    # XML-based formats (contains "+xml")
    if "+xml" in mime_type_lower:
        return True

    # JSON formats (contains "json")
    if "json" in mime_type_lower:
        return True

    return False


def _space_name_from_site_url(site_url: str) -> str:
    """Derives the Goodmem space name from the SharePoint site URL.

    Example: https://good.sharepoint.com/sites/Mem -> SharePoint_Good_Mem
    Org = host part before .sharepoint.com; site = first path segment after /sites/.

    Args:
      site_url: SharePoint site URL (e.g. https://tenant.sharepoint.com/sites/SiteName).

    Returns:
      Space name like SharePoint_Org_Site (case-sensitive).
    """
    url = site_url.rstrip("/")
    parsed = urlparse(url)
    host = parsed.netloc or ""
    path = (parsed.path or "").strip("/")

    # Org: part before .sharepoint.com
    if ".sharepoint.com" in host:
        org = host.split(".sharepoint.com")[0]
    else:
        org = host

    # Site: first segment after "sites" in path (e.g. sites/Mem -> Mem)
    site = ""
    if path.lower().startswith("sites/"):
        rest = path[6:]  # after "sites/"
        site = rest.split("/")[0] if rest else ""

    return f"SharePoint_{org}_{site}"


def _download_file(download_url: str) -> bytes:
    """Downloads file content from a URL (no auth required for SharePoint download URL)."""
    resp = requests.get(download_url, timeout=60)
    resp.raise_for_status()
    return resp.content


def main() -> None:
    load_dotenv()

    # SharePoint
    client_id = os.getenv("SHAREPOINT_CLIENT_ID")
    tenant_id = os.getenv("SHAREPOINT_TENANT_ID")
    client_secret = os.getenv("SHAREPOINT_CLIENT_SECRET")
    site_url = os.getenv("SHAREPOINT_SITE_URL")
    if not all([client_id, tenant_id, client_secret, site_url]):
        print("Error: Missing SharePoint env vars (SHAREPOINT_CLIENT_ID, TENANT_ID, CLIENT_SECRET, SITE_URL).")
        return

    # Goodmem
    goodmem_base_url = os.getenv("GOODMEM_BASE_URL")
    goodmem_api_key = os.getenv("GOODMEM_API_KEY") or os.getenv("GOOMEM_API_KEY")  # typo in .env.example
    if not goodmem_base_url or not goodmem_api_key:
        print("Error: Missing Goodmem env vars (GOODMEM_BASE_URL, GOODMEM_API_KEY).")
        return

    connector = SharePointConnector(
        client_id=client_id,
        tenant_id=tenant_id,
        client_secret=client_secret,
        site_url=site_url,
    )
    goodmem = GoodmemClient(base_url=goodmem_base_url, api_key=goodmem_api_key)

    print("Authenticating with Microsoft Graph API...")
    if not connector.authenticate():
        print("Failed to authenticate. Exiting.")
        return

    print("Fetching files from SharePoint...")
    files = connector.list_files()
    if not files:
        print("No files found in SharePoint.")
        return

    space_name = _space_name_from_site_url(site_url)
    space_id = goodmem.find_space_by_name(space_name)
    if space_id is None:
        embedder_id = os.getenv("DEFAULT_EMBEDDER_ID")
        if not embedder_id:
            embedders = goodmem.list_embedders()
            if not embedders:
                print("Error: No embedders found and DEFAULT_EMBEDDER_ID not set.")
                return
            embedder_id = embedders[0].get("embedderId")
            if not embedder_id:
                print("Error: First embedder has no embedderId.")
                return
        print(f"Creating Goodmem space: {space_name}")
        created = goodmem.create_space(space_name=space_name, embedder_id=embedder_id)
        space_id = created.get("spaceId")
        if not space_id:
            print("Error: create_space did not return spaceId.")
            return
    else:
        print(f"Using existing Goodmem space: {space_name}")

    print("Listing memories in Goodmem space...")
    memories = goodmem.list_all_memories(space_id)
    # Map SharePoint file id -> { memory_id, modified_datetime } from metadata
    goodmem_by_file_id: dict = {}
    for mem in memories:
        meta = mem.get("metadata") or {}
        file_id = meta.get("id")
        if file_id is not None:
            goodmem_by_file_id[file_id] = {
                "memory_id": mem.get("memoryId"),
                "modified_datetime": meta.get("modified_datetime"),
                "name": meta.get("name"),
            }

    current_sharepoint_ids = {f["id"] for f in files}
    changed = False

    # Rule 3: Delete memories for files no longer in SharePoint
    for file_id, info in list(goodmem_by_file_id.items()):
        if file_id not in current_sharepoint_ids:
            mid = info.get("memory_id")
            if mid:
                file_name = info.get("name") or "(unknown name)"
                print(f"Deleting memory for removed file: {file_id} ({file_name})")
                goodmem.delete_memory(mid)
                del goodmem_by_file_id[file_id]
                changed = True

    # Rules 1 & 2: Ingest new files; for updated files, delete then ingest
    for file_info in files:
        file_id = file_info.get("id")
        mime_type = file_info.get("mime_type")
        if not _is_mime_type_supported(mime_type):
            print(f"Skipping unsupported MIME type: {file_info.get('name')} ({mime_type})")
            continue

        existing = goodmem_by_file_id.get(file_id)
        if existing:
            if existing.get("modified_datetime") == file_info.get("modified_datetime"):
                continue  # Already up to date
            # Rule 2: different modified_datetime -> delete then ingest
            mid = existing.get("memory_id")
            if mid:
                print(f"Re-ingesting updated file: {file_info.get('name')}")
                goodmem.delete_memory(mid)
                changed = True
        else:
            print(f"Ingesting new file: {file_info.get('name')}")
            changed = True

        download_url = file_info.get("download_url")
        if not download_url:
            print(f"  Skipping (no download_url): {file_info.get('name')}")
            continue

        try:
            content_bytes = _download_file(download_url)
        except Exception as e:
            print(f"  Failed to download: {e}")
            continue

        # Metadata: all fields from the file JSON (README)
        metadata = {k: v for k, v in file_info.items() if v is not None}

        try:
            goodmem.insert_memory_binary(
                space_id=space_id,
                content_bytes=content_bytes,
                content_type=mime_type or "application/octet-stream",
                metadata=metadata,
            )
        except Exception as e:
            print(f"  Failed to ingest: {e}")

    if changed:
        print("Sync complete.")
    else:
        print("Nothing needs to be changed.")


if __name__ == "__main__":
    main()
