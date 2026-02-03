#!/usr/bin/env -S uv run --script
# /// script
# requires-python = ">=3.10"
# dependencies = [
#     "requests>=2.31.0",
#     "python-dotenv>=1.0.0",
# ]
# ///
"""
Test that Azure AD app has the Graph permissions needed for sync_once.py and listener.py.

Run from your laptop (no webhook needed). Uses only SharePoint/.env credentials.
Exits 0 if all checks pass, 1 otherwise.

  python test_graph_permissions.py
"""

import os
import sys

import requests
from dotenv import load_dotenv

from sharepoint_client import SharePointConnector


def main() -> int:
    load_dotenv()

    client_id = os.getenv("AZURE_AD_CLIENT_ID")
    tenant_id = os.getenv("AZURE_AD_TENANT_ID")
    client_secret = os.getenv("AZURE_AD_CLIENT_SECRET")
    site_url = os.getenv("SHAREPOINT_SITE_URL")

    if not all([client_id, tenant_id, client_secret, site_url]):
        print("Missing .env: need AZURE_AD_CLIENT_ID, AZURE_AD_TENANT_ID, AZURE_AD_CLIENT_SECRET, SHAREPOINT_SITE_URL", file=sys.stderr)
        return 1

    connector = SharePointConnector(
        client_id=client_id,
        tenant_id=tenant_id,
        client_secret=client_secret,
        site_url=site_url,
    )

    # 1. Authenticate (needs app registration and client secret)
    print("1. Authenticating with Microsoft Graph...", end=" ")
    if not connector.authenticate():
        print("✗ FAIL. Check client ID, tenant ID, and client secret.", file=sys.stderr)
        return 1
    print("✓ Success.")

    # 2. Get site ID (needs Sites.Read.All)
    print("2. Getting site ID (needs Sites.Read.All)...", end=" ", flush=True)
    site_id = connector.get_site_id()
    if not site_id:
        print("✗ FAIL. Grant Sites.Read.All (application) and admin consent.", file=sys.stderr)
        return 1
    print("✓ Success.")
    connector.print_site_info()

    # 3. Get drives (needs Sites.Read.All)
    print("3. Getting drives (needs Sites.Read.All)...", end=" ")
    drives = connector.get_drives(site_id)
    if not drives:
        print("✗ FAIL. Check Sites.Read.All and that the site has a document library.", file=sys.stderr)
        return 1
    print("✓ Success.")

    # 4. List files from root (needs Files.Read.All)
    print("4. Listing files from drive root (needs Files.Read.All)...", end=" ", flush=True)
    drive_id = drives[0].get("id")
    try:
        endpoint = f"{connector.base_url}/drives/{drive_id}/root/children"
        resp = connector._request("GET", endpoint, timeout=30)
        resp.raise_for_status()
    except Exception as e:
        print(f"✗ FAIL. {e} Grant Files.Read.All (application) and admin consent.", file=sys.stderr)
        return 1
    print("✓ Success.")

    print("\nAll required Azure AD / Graph permissions are set (Sites.Read.All, Files.Read.All).")
    return 0


if __name__ == "__main__":
    sys.exit(main())
