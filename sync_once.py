#!/usr/bin/env -S uv run --script
# /// script
# requires-python = ">=3.10"
# dependencies = [
#     "requests>=2.31.0",
#     "python-dotenv>=1.0.0",
# ]
# ///
"""
One-time full sync from SharePoint to Goodmem.

Run: python sync_once.py   or   ./sync_once.py (with uv shebang)

Every run syncs all files from SharePoint to Goodmem: ingests new files,
re-ingests updated files (after deleting the old copy), and deletes
memories for files that no longer exist in SharePoint.

List mode (no sync, SharePoint only):
  python sync_once.py list -depth 2 -width 5
  Shows file hierarchy tree in ASCII; -depth limits levels, -width limits siblings per level.

Diff mode (SharePoint vs Goodmem, no sync):
  python sync_once.py diff
  Shows only_in_sharepoint, only_in_goodmem, in_both (by file id).
"""

import argparse
import os
import sys
from urllib.parse import urlparse

import requests
from dotenv import load_dotenv

from goodmem_client import GoodmemClient, uuid_from_file_id
from sharepoint_client import SharePointConnector, validate_token_refresh_buffer


def _format_bytes(n: int) -> str:
    """Format byte count as human-readable (e.g. 1.2 MB)."""
    if n < 0:
        n = 0
    if n < 1024:
        return f"{n} B"
    if n < 1024 * 1024:
        return f"{n / 1024:.1f} KB"
    return f"{n / (1024 * 1024):.1f} MB"


def _progress_bar(current: int, total: int, width: int = 32) -> str:
    """Return a progress bar string; when total is 0, bar stays empty (unknown total)."""
    if total <= 0:
        pct = 0.0
    else:
        pct = min(1.0, current / total)
    filled = int(width * pct)
    bar = "=" * filled + ">" * (1 if filled < width else 0) + " " * (width - filled - 1)
    return f"[{bar}]"


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


def _print_tree(
    nodes: list,
    prefix: str = "",
    depth: int = 0,
    max_depth: int = 2,
    width: int = 5,
) -> None:
    """Print ASCII tree of nodes. Each node has name, type ('folder'|'file'), children."""
    if depth > max_depth:
        return
    total = len(nodes)
    if total > width:
        display_list = nodes[: width - 1] + [
            {"name": f"... ({total - width} more)", "type": "file", "children": []}
        ]
    else:
        display_list = nodes[:width]
    for i, node in enumerate(display_list):
        last_in_show = i == len(display_list) - 1
        name = node.get("name", "?")
        node_type = node.get("type", "file")
        if node_type == "folder":
            name = name + "/"
        branch = "└── " if last_in_show else "├── "
        print(prefix + branch + name)
        children = node.get("children") or []
        if children and depth < max_depth:
            ext = "    " if last_in_show else "│   "
            _print_tree(children, prefix + ext, depth + 1, max_depth, width)


def run_list(depth: int, width: int) -> None:
    """List SharePoint file hierarchy as ASCII tree. No Goodmem, no sync. Expects env already loaded by main()."""
    client_id = os.getenv("AZURE_AD_CLIENT_ID")
    tenant_id = os.getenv("AZURE_AD_TENANT_ID")
    client_secret = os.getenv("AZURE_AD_CLIENT_SECRET")
    site_url = os.getenv("SHAREPOINT_SITE_URL")
    if not all([client_id, tenant_id, client_secret, site_url]):
        print(
            "Error: Missing env vars (AZURE_AD_CLIENT_ID/AZURE_AD_TENANT_ID/AZURE_AD_CLIENT_SECRET and SHAREPOINT_SITE_URL)."
        )
        return
    connector = SharePointConnector(
        client_id=client_id,
        tenant_id=tenant_id,
        client_secret=client_secret,
        site_url=site_url,
    )
    print("Authenticating with Microsoft Graph API...", end=" ", flush=True)
    if not connector.authenticate():
        print("✗ Failed. Exiting.")
        return
    print("✓ Success.")
    print("Connecting to site...", end=" ", flush=True)
    site_id = connector.get_site_id()
    if not site_id:
        print("✗ Failed. Exiting.")
        return
    connector.print_site_info()
    print("✓ Connected.")
    drives = connector.get_drives(site_id)
    if not drives:
        print("No drives found.")
        return
    drive_id = drives[0].get("id")
    drive_name = drives[0].get("name") or "Documents"
    print(f"Listing tree (depth={depth}, width={width})...", end=" ", flush=True)
    tree = connector.get_drive_tree(drive_id=drive_id, folder_id="root", max_depth=depth, site_id=site_id)
    print("✓ Done.")
    print()
    print(drive_name + "/")
    _print_tree(tree, prefix="", depth=1, max_depth=depth, width=width)


def run_diff() -> None:
    """Print file-level diff: SharePoint drive root vs Goodmem space (only_in_sharepoint, only_in_goodmem, in_both). Expects env already loaded by main()."""
    client_id = os.getenv("AZURE_AD_CLIENT_ID")
    tenant_id = os.getenv("AZURE_AD_TENANT_ID")
    client_secret = os.getenv("AZURE_AD_CLIENT_SECRET")
    site_url = os.getenv("SHAREPOINT_SITE_URL")
    if not all([client_id, tenant_id, client_secret, site_url]):
        print(
            "Error: Missing env vars (AZURE_AD_CLIENT_ID/AZURE_AD_TENANT_ID/AZURE_AD_CLIENT_SECRET and SHAREPOINT_SITE_URL)."
        )
        return
    goodmem_base_url = os.getenv("GOODMEM_BASE_URL")
    goodmem_api_key = os.getenv("GOODMEM_API_KEY")
    if not goodmem_base_url or not goodmem_api_key:
        print("Error: Missing Goodmem env vars.")
        return
    connector = SharePointConnector(
        client_id=client_id,
        tenant_id=tenant_id,
        client_secret=client_secret,
        site_url=site_url,
    )
    goodmem = GoodmemClient(base_url=goodmem_base_url, api_key=goodmem_api_key)
    if not connector.authenticate():
        print("Error: Graph auth failed.")
        return
    site_id = connector.get_site_id()
    if not site_id:
        print("Error: Could not resolve site.")
        return
    files = connector.list_files(site_id=site_id)
    # Resolve space same as sync: GOODMEM_SPACE_ID / SPACE_ID / DEFAULT_SPACE_ID, else find by name
    default_space_id = os.getenv("GOODMEM_SPACE_ID") or os.getenv("SPACE_ID") or os.getenv("DEFAULT_SPACE_ID")
    if default_space_id:
        space_id = default_space_id.strip()
    else:
        space_name = _space_name_from_site_url(site_url)
        space_id = goodmem.find_space_by_name(space_name)
        if not space_id:
            print(f"Error: Goodmem space '{space_name}' not found.")
            return
    memories = goodmem.list_all_memories(space_id)
    sharepoint_uuids = {uuid_from_file_id(f["id"]) for f in files}
    goodmem_uuids = {m["memoryId"] for m in memories}
    sp_by_id = {f["id"]: {"id": f["id"], "name": f.get("name") or f["id"], "modified_datetime": f.get("modified_datetime")} for f in files}
    only_sp = [sp_by_id[f["id"]] for f in files if uuid_from_file_id(f["id"]) not in goodmem_uuids]
    only_gm = []
    for mem in memories:
        if mem["memoryId"] not in sharepoint_uuids:
            meta = mem.get("metadata") or {}
            only_gm.append({"id": meta.get("id"), "name": meta.get("name") or meta.get("id"), "modified_datetime": meta.get("modified_datetime")})
    in_both = [sp_by_id[f["id"]] for f in files if uuid_from_file_id(f["id"]) in goodmem_uuids]
    print("File-level diff (SharePoint drive root vs Goodmem space):")
    print(f"  Only in SharePoint: {len(only_sp)}")
    for x in only_sp:
        print(f"    - {x['name']} (id={x['id'][:12]}...)")
    print(f"  Only in Goodmem: {len(only_gm)}")
    for x in only_gm:
        ident = (x.get("id") or "?")[:12] if x.get("id") else "?"
        print(f"    - {x.get('name', '?')} (id={ident}...)")
    print(f"  In both: {len(in_both)}")
    for x in in_both:
        print(f"    - {x['name']} (id={x['id'][:12]}...)")


def main() -> None:
    parser = argparse.ArgumentParser(
        prog="sync_once.py",
        description="One-time full sync from SharePoint to Goodmem. Use 'list' to show file hierarchy only; 'diff' to compare SharePoint vs Goodmem (no sync).",
        epilog="Examples:\n  python sync_once.py                 Run full sync\n  python sync_once.py list -depth 2 -width 5   List file tree\n  python sync_once.py diff   SharePoint vs Goodmem diff",
        formatter_class=argparse.RawDescriptionHelpFormatter,
    )
    parser.add_argument(
        "command",
        nargs="?",
        default="sync",
        help="'sync' (default), 'list', or 'diff'",
    )
    parser.add_argument(
        "-depth",
        type=int,
        default=2,
        metavar="N",
        help="(list) max tree depth (default: 2)",
    )
    parser.add_argument(
        "-width",
        type=int,
        default=5,
        metavar="N",
        help="(list) max siblings per level (default: 5)",
    )
    parser.add_argument(
        "--env-file",
        metavar="PATH",
        default=None,
        help="Load this env file. Default: .env if present, else .env.example.",
    )
    args = parser.parse_args()

    # Resolve env file: --env-file if given; else .env if exists, else .env.example
    env_file = args.env_file
    if env_file is None:
        env_file = ".env" if os.path.isfile(".env") else ".env.example"
    if not os.path.isfile(env_file):
        print(f"Error: Env file not found: {env_file}", file=sys.stderr)
        sys.exit(1)
    load_dotenv(env_file, override=True)
    validate_token_refresh_buffer()

    if args.command and args.command.lower() == "list":
        run_list(depth=args.depth, width=args.width)
        return
    if args.command and args.command.lower() == "diff":
        run_diff()
        return

    # Required vars for sync (SharePoint + Goodmem)
    client_id = os.getenv("AZURE_AD_CLIENT_ID")
    tenant_id = os.getenv("AZURE_AD_TENANT_ID")
    client_secret = os.getenv("AZURE_AD_CLIENT_SECRET")
    site_url = os.getenv("SHAREPOINT_SITE_URL")
    goodmem_base_url = os.getenv("GOODMEM_BASE_URL")
    goodmem_api_key = os.getenv("GOODMEM_API_KEY")
    missing = []
    if not client_id:
        missing.append("AZURE_AD_CLIENT_ID")
    if not tenant_id:
        missing.append("AZURE_AD_TENANT_ID")
    if not client_secret:
        missing.append("AZURE_AD_CLIENT_SECRET")
    if not site_url:
        missing.append("SHAREPOINT_SITE_URL")
    if not goodmem_base_url:
        missing.append("GOODMEM_BASE_URL")
    if not goodmem_api_key:
        missing.append("GOODMEM_API_KEY")
    if missing:
        print(f"Error: Missing in {env_file}: {', '.join(missing)}", file=sys.stderr)
        print("Fill these in (see .env.example). Use --env-file to load a different file.", file=sys.stderr)
        sys.exit(1)

    connector = SharePointConnector(
        client_id=client_id,
        tenant_id=tenant_id,
        client_secret=client_secret,
        site_url=site_url,
    )
    goodmem = GoodmemClient(base_url=goodmem_base_url, api_key=goodmem_api_key)

    print("Authenticating with Microsoft Graph API...", end=" ", flush=True)
    if not connector.authenticate():
        print("✗ Failed. Exiting.")
        return
    print("✓ Success.")

    print("Connecting to site...", end=" ", flush=True)
    site_id = connector.get_site_id()
    if not site_id:
        print("✗ Failed. Exiting.")
        return
    connector.print_site_info()
    print("✓ Connected.")

    print("Fetching files from SharePoint...", end=" ", flush=True)
    files = connector.list_files(site_id=site_id)
    if not files:
        print("✗ No files found.")
        return
    print(f"✓ Found {len(files)} file(s).")

    default_space_id = os.getenv("GOODMEM_SPACE_ID") or os.getenv("SPACE_ID") or os.getenv("DEFAULT_SPACE_ID")
    if default_space_id:
        space_id = default_space_id.strip()
        print(f"Goodmem: Using space from GOODMEM_SPACE_ID: {space_id}")
    else:
        space_name = _space_name_from_site_url(site_url)
        print(f"Goodmem: Looking up space '{space_name}'...", end=" ", flush=True)
        space_id = goodmem.find_space_by_name(space_name)
        if space_id is None:
            print("Does not exist. Need to create one.")
            embedder_id = os.getenv("GOODMEM_EMBEDDER_ID") or os.getenv("EMBEDDER_ID") or os.getenv("DEFAULT_EMBEDDER_ID")
            embedder_name: str | None = None
            if not embedder_id:
                print("Goodmem: No embedder specified. Listing embedders...", end=" ", flush=True)
                embedders = goodmem.list_embedders()
                if not embedders:
                    print("Error: No embedders found and GOODMEM_EMBEDDER_ID not set.")
                    return
                first = embedders[0]
                embedder_id = first.get("embedderId")
                embedder_name = first.get("name") or first.get("embedderName")
                if not embedder_id:
                    print("Error: First embedder has no embedderId.")
                    return
                count = len(embedders)
                name_part = f' "{embedder_name}"' if embedder_name else ""
                print(f"Found {count}. Using embedder{name_part} <{embedder_id}>.")
            else:
                print(f"Goodmem: Using embedder from GOODMEM_EMBEDDER_ID: <{embedder_id}>.")
            name_part = f' "{embedder_name}"' if embedder_name else ""
            print(f"Goodmem: Creating space '{space_name}' with embedder{name_part} <{embedder_id}>...", end=" ", flush=True)
            created = goodmem.create_space(space_name=space_name, embedder_id=embedder_id)
            space_id = created.get("spaceId")
            if not space_id:
                print("Error: create_space did not return spaceId.")
                return
            print(f"Created. spaceId={space_id}")
        else:
            print("Found. Using existing space.")

    print("Goodmem: Listing memories...", end=" ", flush=True)
    memories = goodmem.list_all_memories(space_id)
    print(f"Found {len(memories)}.")

    # UUID set operations (same logic as listener)
    sp_by_id = {f["id"]: f for f in files}
    sharepoint_uuids = {uuid_from_file_id(fid) for fid in sp_by_id}
    goodmem_uuids = {m["memoryId"] for m in memories}
    uuid_to_file_id = {uuid_from_file_id(fid): fid for fid in sp_by_id}

    to_add_uuids = sharepoint_uuids - goodmem_uuids
    to_delete_uuids = goodmem_uuids - sharepoint_uuids
    both_uuids = sharepoint_uuids & goodmem_uuids

    # Batch-get both_uuids to decide which need update (Goodmem modified_datetime older than SharePoint)
    to_update_uuids: set[str] = set()
    if both_uuids:
        both_meta = goodmem.batch_get_memories(list(both_uuids))
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
                print(f"Goodmem: Unexpected — Goodmem modified_datetime newer than SharePoint for file_id={fid}", file=sys.stderr)

    to_add_file_infos = [
        sp_by_id[uuid_to_file_id[u]]
        for u in to_add_uuids
        if u in uuid_to_file_id and _is_mime_type_supported(sp_by_id[uuid_to_file_id[u]].get("mime_type"))
    ]
    to_update_file_infos = [
        sp_by_id[uuid_to_file_id[u]]
        for u in to_update_uuids
        if u in uuid_to_file_id and _is_mime_type_supported(sp_by_id[uuid_to_file_id[u]].get("mime_type"))
    ]

    # Delete memories no longer in SharePoint (by UUID)
    if to_delete_uuids:
        delete_meta = goodmem.batch_get_memories(list(to_delete_uuids))
        for u in to_delete_uuids:
            meta = (delete_meta.get(u) or {}).get("metadata") or {}
            name = meta.get("name") or meta.get("id") or u
            print(f"Goodmem: Deleting memory for removed file: {name}")
            goodmem.delete_memory(u)

    # Add: ingest with memoryId=uuid_from_file_id(file_id)
    # Update: delete then ingest with same memoryId
    files_to_ingest = to_add_file_infos + to_update_file_infos
    total_bytes_to_ingest = sum(f.get("size") or 0 for f in files_to_ingest)
    num_to_ingest = len(files_to_ingest)
    changed = bool(to_delete_uuids or files_to_ingest)

    if num_to_ingest:
        print(f"Goodmem: Ingesting {num_to_ingest} file(s) ({_format_bytes(total_bytes_to_ingest)} total)...")
    bytes_ingested = 0
    for file_info in files_to_ingest:
        file_id = file_info.get("id")
        memory_uuid = uuid_from_file_id(file_id)
        is_update = memory_uuid in to_update_uuids
        if is_update:
            goodmem.delete_memory(memory_uuid)

        download_url = file_info.get("download_url")
        if not download_url:
            print(f"  Skipping (no download_url): {file_info.get('name')}")
            continue

        try:
            content_bytes = _download_file(download_url)
        except Exception as e:
            print(f"  Failed to download {file_info.get('name')}: {e}")
            continue

        mime_type = file_info.get("mime_type") or "application/octet-stream"
        metadata = {k: v for k, v in file_info.items() if v is not None}

        try:
            goodmem.insert_memory_binary(
                space_id=space_id,
                content_bytes=content_bytes,
                content_type=mime_type,
                metadata=metadata,
                memory_id=memory_uuid,
                filename=file_info.get("name") or "upload",
            )
        except Exception as e:
            print(f"  Failed to ingest {file_info.get('name')}: {e}")
            continue

        bytes_ingested += len(content_bytes)
        bar = _progress_bar(bytes_ingested, total_bytes_to_ingest)
        total_str = _format_bytes(total_bytes_to_ingest) if total_bytes_to_ingest else "?"
        sys.stdout.write(f"\r  {bar} {_format_bytes(bytes_ingested)} / {total_str}    ")
        sys.stdout.flush()

    if num_to_ingest:
        sys.stdout.write("\n")
        sys.stdout.flush()

    if changed:
        print("Sync complete.")
    else:
        print("Nothing needs to be changed.")


if __name__ == "__main__":
    main()
