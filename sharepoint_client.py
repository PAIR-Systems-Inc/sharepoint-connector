#!/usr/bin/env -S uv run --script
# /// script
# requires-python = ">=3.10"
# dependencies = [
#     "requests>=2.31.0",
#     "python-dotenv>=1.0.0",
# ]
# ///
"""
SharePoint Connector using Microsoft Graph API
Connects to SharePoint and lists files from a specified site.

Basic Usage
----------
::

    from sharepoint_client import SharePointConnector
    import os
    from dotenv import load_dotenv

    load_dotenv()

    connector = SharePointConnector(
        client_id=os.getenv("SHAREPOINT_CLIENT_ID"),
        tenant_id=os.getenv("SHAREPOINT_TENANT_ID"),
        client_secret=os.getenv("SHAREPOINT_CLIENT_SECRET"),
        site_url=os.getenv("SHAREPOINT_SITE_URL")
    )

    connector.authenticate()
    files = connector.list_files()
    connector.print_files(files)

List Files from Specific Folder
-------------------------------
::

    files = connector.list_files(folder_path="Documents/Reports")

List Files from Specific Drive
------------------------------
::

    drives = connector.get_drives()
    files = connector.list_files(drive_id=drives[0]["id"])
"""

import os
import requests
import json
from typing import List, Dict, Optional
from datetime import datetime
from dotenv import load_dotenv

# Load environment variables from .env file
load_dotenv()


class SharePointConnector:
    """Connector for accessing SharePoint files via Microsoft Graph API."""
    
    def __init__(self, client_id: str, tenant_id: str, client_secret: str, site_url: str):
        """
        Initialize the SharePoint connector.
        
        Args:
            client_id: Azure AD Application (client) ID
            tenant_id: Azure AD Directory (tenant) ID
            client_secret: Client secret value
            site_url: SharePoint site URL (e.g., https://tenant.sharepoint.com/sites/SiteName)
        """
        self.client_id = client_id
        self.tenant_id = tenant_id
        self.client_secret = client_secret
        self.site_url = site_url
        self.access_token: Optional[str] = None
        self.base_url = "https://graph.microsoft.com/v1.0"
        self._last_site_data: Optional[Dict] = None
        
    def _log_request_exception(self, context: str, e: requests.exceptions.RequestException):
        """Log raw HTTP request and response details for debugging."""
        print(f"\n--- {context}: raw HTTP details ---")
        # Request details
        req = getattr(e, "request", None)
        if req is not None:
            print("Request:")
            print(f"  Method: {req.method}")
            print(f"  URL:    {req.url}")
            if req.headers:
                print("  Headers:")
                for k, v in req.headers.items():
                    print(f"    {k}: {v}")
            body = req.body
            if body:
                # Avoid dumping extremely large or binary bodies
                body_str = body if isinstance(body, str) else body.decode("utf-8", errors="replace")
                print("  Body:")
                print(f"    {body_str[:2000]}")
        else:
            print("No request information available on exception.")

        # Response details
        resp = getattr(e, "response", None)
        if resp is not None:
            print("\nResponse:")
            print(f"  Status: {resp.status_code}")
            if resp.headers:
                print("  Headers:")
                for k, v in resp.headers.items():
                    print(f"    {k}: {v}")
            try:
                text = resp.text
            except Exception:
                text = "<unable to read response text>"
            if text:
                print("  Body:")
                print(f"    {text[:4000]}")
        else:
            print("No response information available on exception.")
        print("--- End raw HTTP details ---\n")

    def authenticate(self) -> bool:
        """
        Authenticate with Microsoft Graph API using OAuth2 client credentials flow.
        
        Returns:
            True if authentication successful, False otherwise
        """
        token_url = f"https://login.microsoftonline.com/{self.tenant_id}/oauth2/v2.0/token"
        
        token_data = {
            "client_id": self.client_id,
            "client_secret": self.client_secret,
            "scope": "https://graph.microsoft.com/.default",
            "grant_type": "client_credentials"
        }
        
        try:
            response = requests.post(token_url, data=token_data)
            response.raise_for_status()
            
            token_response = response.json()
            self.access_token = token_response.get("access_token")
            
            if not self.access_token:
                print("Error: No access token received")
                return False
            return True

        except requests.exceptions.RequestException as e:
            print(f"✗ Authentication failed: {e}")
            if hasattr(e, "response") and e.response is not None:
                try:
                    error_detail = e.response.json()
                    print(f"Error details: {json.dumps(error_detail, indent=2)}")
                except Exception:
                    print(f"Response: {e.response.text}")
            self._log_request_exception("Authentication failure", e)
            return False
    
    def _get_headers(self) -> Dict[str, str]:
        """Get HTTP headers with authentication token."""
        if not self.access_token:
            raise ValueError("Not authenticated. Call authenticate() first.")
        return {
            "Authorization": f"Bearer {self.access_token}",
            "Content-Type": "application/json"
        }
    
    def get_site_id(self) -> Optional[str]:
        """
        Get the site ID from the SharePoint site URL.
        
        Returns:
            Site ID if found, None otherwise
        """
        # Extract hostname and path from URL
        # Format: https://tenant.sharepoint.com/sites/SiteName
        url_parts = self.site_url.replace("https://", "").replace("http://", "").split("/")
        hostname = url_parts[0]
        site_path = "/" + "/".join(url_parts[1:]) if len(url_parts) > 1 else "/"
        
        # API endpoint to get site by hostname and path
        # Format: /sites/{hostname}:/{server-relative-path}
        endpoint = f"{self.base_url}/sites/{hostname}:{site_path}"
        
        try:
            response = requests.get(endpoint, headers=self._get_headers())
            response.raise_for_status()
            
            site_data = response.json()
            self._last_site_data = site_data
            return site_data.get("id")

        except requests.exceptions.RequestException as e:
            print(f"✗ Failed to get site ID: {e}")
            if hasattr(e, "response") and e.response is not None:
                try:
                    error_detail = e.response.json()
                    print(f"Error details: {json.dumps(error_detail, indent=2)}")
                except Exception:
                    print(f"Response: {e.response.text}")
            self._log_request_exception("Get site ID failure", e)
            return None

    def print_site_info(self) -> None:
        """
        Print basic site info from the last get_site_id() call (webUrl and ID breakdown).
        The Graph site ID is: hostname, siteCollectionId, siteId (comma-separated).
        """
        data = self._last_site_data
        if not data:
            return
        web_url = data.get("webUrl") or self.site_url
        site_id_raw = data.get("id") or ""
        parts = [p.strip() for p in site_id_raw.split(",")] if site_id_raw else []
        hostname = parts[0] if len(parts) > 0 else "(unknown)"
        site_collection_id = parts[1] if len(parts) > 1 else "(unknown)"
        site_id = parts[2] if len(parts) > 2 else "(unknown)"
        print(f"  Site URL: {web_url}")
        print(f"  Site ID: hostname={hostname}, siteCollectionId={site_collection_id}, siteId={site_id}")

    def get_drives(self, site_id: Optional[str] = None) -> List[Dict]:
        """
        Get all drives (document libraries) for the site.
        
        Args:
            site_id: Site ID. If None, will be fetched automatically.
            
        Returns:
            List of drive objects
        """
        if site_id is None:
            site_id = self.get_site_id()
            if not site_id:
                return []
        
        endpoint = f"{self.base_url}/sites/{site_id}/drives"
        
        try:
            response = requests.get(endpoint, headers=self._get_headers())
            response.raise_for_status()
            
            drives_data = response.json()
            drives = drives_data.get("value", [])
            return drives

        except requests.exceptions.RequestException as e:
            print(f"✗ Failed to get drives: {e}")
            if hasattr(e, "response") and e.response is not None:
                try:
                    error_detail = e.response.json()
                    print(f"Error details: {json.dumps(error_detail, indent=2)}")
                except Exception:
                    print(f"Response: {e.response.text}")
            self._log_request_exception("Get drives failure", e)
            return []
    
    def list_files(self,
                   drive_id: Optional[str] = None,
                   folder_path: str = "",
                   recursive: bool = True,
                   site_id: Optional[str] = None) -> List[Dict]:
        """
        List files from a SharePoint drive.

        Args:
            drive_id: Drive ID. If None, uses the first available drive.
            folder_path: Path to folder within the drive (e.g., "Documents/Subfolder")
            recursive: Whether to recursively list files in subfolders
            site_id: Site ID. If None, fetches via get_site_id().

        Returns:
            List of file objects with metadata

        Examples:
            List from a specific folder::

                files = connector.list_files(folder_path="Documents/Reports")

            List from a specific drive::

                drives = connector.get_drives()
                files = connector.list_files(drive_id=drives[0]["id"])
        """
        if site_id is None:
            site_id = self.get_site_id()
        if not site_id:
            return []
        
        # Get drives if drive_id not provided
        if drive_id is None:
            drives = self.get_drives(site_id)
            if not drives:
                print("✗ No drives found")
                return []
            drive_id = drives[0].get("id")
        
        # Build endpoint
        if folder_path:
            # URL encode the folder path
            folder_path_encoded = "/".join(requests.utils.quote(part) for part in folder_path.split("/"))
            endpoint = f"{self.base_url}/drives/{drive_id}/root:/{folder_path_encoded}:/children"
        else:
            endpoint = f"{self.base_url}/drives/{drive_id}/root/children"
        
        all_files = []
        
        try:
            # Get initial page
            response = requests.get(endpoint, headers=self._get_headers())
            response.raise_for_status()
            
            data = response.json()
            items = data.get("value", [])
            
            # Process items
            for item in items:
                if "file" in item:
                    # It's a file
                    all_files.append(self._format_file_info(item))
                elif "folder" in item and recursive:
                    # It's a folder, recursively get files
                    folder_id = item.get("id")
                    folder_files = self._get_files_from_folder(drive_id, folder_id)
                    all_files.extend(folder_files)
            
            # Handle pagination
            while "@odata.nextLink" in data:
                next_url = data["@odata.nextLink"]
                response = requests.get(next_url, headers=self._get_headers())
                response.raise_for_status()
                data = response.json()
                items = data.get("value", [])
                
                for item in items:
                    if "file" in item:
                        all_files.append(self._format_file_info(item))
                    elif "folder" in item and recursive:
                        folder_id = item.get("id")
                        folder_files = self._get_files_from_folder(drive_id, folder_id)
                        all_files.extend(folder_files)
            
            return all_files

        except requests.exceptions.RequestException as e:
            print(f"✗ Failed to list files: {e}")
            if hasattr(e, "response") and e.response is not None:
                try:
                    error_detail = e.response.json()
                    print(f"Error details: {json.dumps(error_detail, indent=2)}")
                except Exception:
                    print(f"Response: {e.response.text}")
            self._log_request_exception("List files failure", e)
            return []
    
    def _get_files_from_folder(self, drive_id: str, folder_id: str) -> List[Dict]:
        """Recursively get files from a folder."""
        endpoint = f"{self.base_url}/drives/{drive_id}/items/{folder_id}/children"
        files = []
        
        try:
            response = requests.get(endpoint, headers=self._get_headers())
            response.raise_for_status()
            
            data = response.json()
            items = data.get("value", [])
            
            for item in items:
                if "file" in item:
                    files.append(self._format_file_info(item))
                elif "folder" in item:
                    # Recursively get files from subfolder
                    subfolder_id = item.get("id")
                    subfolder_files = self._get_files_from_folder(drive_id, subfolder_id)
                    files.extend(subfolder_files)
            
            # Handle pagination
            while "@odata.nextLink" in data:
                next_url = data["@odata.nextLink"]
                response = requests.get(next_url, headers=self._get_headers())
                response.raise_for_status()
                data = response.json()
                items = data.get("value", [])
                
                for item in items:
                    if "file" in item:
                        files.append(self._format_file_info(item))
                    elif "folder" in item:
                        subfolder_id = item.get("id")
                        subfolder_files = self._get_files_from_folder(drive_id, subfolder_id)
                        files.extend(subfolder_files)
            
        except requests.exceptions.RequestException as e:
            print(f"Warning: Failed to get files from folder {folder_id}: {e}")
        
        return files

    def _get_item_children(self, drive_id: str, item_id: str) -> List[Dict]:
        """Get direct children (files and folders) of a drive item. item_id 'root' = drive root."""
        if item_id == "root":
            endpoint = f"{self.base_url}/drives/{drive_id}/root/children"
        else:
            endpoint = f"{self.base_url}/drives/{drive_id}/items/{item_id}/children"
        try:
            response = requests.get(endpoint, headers=self._get_headers(), timeout=30)
            response.raise_for_status()
            data = response.json()
            items = data.get("value", [])
            # Handle pagination: collect all pages
            while "@odata.nextLink" in data:
                next_url = data["@odata.nextLink"]
                response = requests.get(next_url, headers=self._get_headers(), timeout=30)
                response.raise_for_status()
                data = response.json()
                items.extend(data.get("value", []))
            return items
        except requests.exceptions.RequestException as e:
            print(f"Warning: Failed to get children for {item_id}: {e}")
            return []

    def get_drive_tree(
        self,
        drive_id: Optional[str] = None,
        folder_id: str = "root",
        max_depth: int = 2,
        site_id: Optional[str] = None,
    ) -> List[Dict]:
        """
        Build a tree of drive items (folders and files) up to max_depth.
        Returns list of nodes; each node is {"name": str, "type": "folder"|"file", "children": [...]}.
        """
        if site_id is None:
            site_id = self.get_site_id()
        if not site_id:
            return []
        if drive_id is None:
            drives = self.get_drives(site_id)
            if not drives:
                return []
            drive_id = drives[0].get("id")
        return self._build_tree_level(drive_id, folder_id, current_depth=0, max_depth=max_depth)

    def _build_tree_level(
        self, drive_id: str, folder_id: str, current_depth: int, max_depth: int
    ) -> List[Dict]:
        if current_depth >= max_depth:
            return []
        items = self._get_item_children(drive_id, folder_id)
        nodes = []
        for item in items:
            name = item.get("name") or "(unnamed)"
            if "folder" in item:
                child_id = item.get("id")
                children = (
                    self._build_tree_level(drive_id, child_id, current_depth + 1, max_depth)
                    if current_depth + 1 < max_depth
                    else []
                )
                nodes.append({"name": name, "type": "folder", "id": child_id, "children": children})
            else:
                nodes.append({"name": name, "type": "file", "id": item.get("id"), "children": []})
        return nodes

    def get_file_by_id(self, drive_id: str, item_id: str) -> Optional[Dict]:
        """
        Get a single drive item (file or folder) by drive ID and item ID.
        Returns formatted file info if the item is a file; None if folder or not found.
        """
        endpoint = f"{self.base_url}/drives/{drive_id}/items/{item_id}"
        try:
            response = requests.get(endpoint, headers=self._get_headers())
            response.raise_for_status()
            item = response.json()
            if "file" not in item:
                return None  # folder or other non-file
            return self._format_file_info(item)
        except requests.exceptions.RequestException as e:
            print(f"Warning: Failed to get item {item_id}: {e}")
            return None

    def _format_file_info(self, item: Dict) -> Dict:
        """Format file information into a clean dictionary."""
        file_info = item.get("file", {})
        return {
            "id": item.get("id"),
            "name": item.get("name"),
            "web_url": item.get("webUrl"),
            "download_url": item.get("@microsoft.graph.downloadUrl"),
            "size": item.get("size"),
            "created_datetime": item.get("createdDateTime"),
            "modified_datetime": item.get("lastModifiedDateTime"),
            "created_by": item.get("createdBy", {}).get("user", {}).get("displayName"),
            "modified_by": item.get("lastModifiedBy", {}).get("user", {}).get("displayName"),
            "mime_type": file_info.get("mimeType"),
            "file_hash": file_info.get("hashes", {}).get("sha1Hash"),
        }
    
    def print_files(self, files: List[Dict]):
        """Print files in a formatted way."""
        if not files:
            print("\nNo files found.")
            return
        
        print(f"\n{'='*80}")
        print(f"Found {len(files)} file(s):")
        print(f"{'='*80}\n")
        
        for i, file in enumerate(files, 1):
            print(f"{i}. {file['name']}")
            print(f"   ID: {file['id']}")
            print(f"   Size: {self._format_size(file['size'])}")
            print(f"   Type: {file['mime_type'] or 'Unknown'}")
            print(f"   Created: {file['created_datetime']}")
            print(f"   Modified: {file['modified_datetime']}")
            if file['web_url']:
                print(f"   URL: {file['web_url']}")
            print()
    
    @staticmethod
    def _format_size(size_bytes: Optional[int]) -> str:
        """Format file size in human-readable format."""
        if size_bytes is None:
            return "Unknown"
        
        for unit in ['B', 'KB', 'MB', 'GB', 'TB']:
            if size_bytes < 1024.0:
                return f"{size_bytes:.2f} {unit}"
            size_bytes /= 1024.0
        return f"{size_bytes:.2f} PB"


def main():
    """Main function to demonstrate the connector."""
    # Load credentials from environment variables
    CLIENT_ID = os.getenv("SHAREPOINT_CLIENT_ID")
    TENANT_ID = os.getenv("SHAREPOINT_TENANT_ID")
    CLIENT_SECRET = os.getenv("SHAREPOINT_CLIENT_SECRET")
    SITE_URL = os.getenv("SHAREPOINT_SITE_URL")
    
    # Validate that all required credentials are present
    if not all([CLIENT_ID, TENANT_ID, CLIENT_SECRET, SITE_URL]):
        print("Error: Missing required environment variables.")
        print("Please ensure .env file exists with the following variables:")
        print("  - SHAREPOINT_CLIENT_ID")
        print("  - SHAREPOINT_TENANT_ID")
        print("  - SHAREPOINT_CLIENT_SECRET")
        print("  - SHAREPOINT_SITE_URL")
        print("\nYou can copy .env.example to .env and fill in your credentials.")
        return
    
    # Create connector
    connector = SharePointConnector(
        client_id=CLIENT_ID,
        tenant_id=TENANT_ID,
        client_secret=CLIENT_SECRET,
        site_url=SITE_URL
    )
    
    # Authenticate
    print("Authenticating with Microsoft Graph API...", end=" ", flush=True)
    if not connector.authenticate():
        print("✗ Failed. Exiting.")
        return
    print("✓ Success.")

    # List files
    print("Fetching files from SharePoint...", end=" ", flush=True)
    files = connector.list_files()
    print(f"✓ Found {len(files)} file(s)." if files else "✗ No files found.")
    
    # Print results
    connector.print_files(files)
    
    # Optionally return files as JSON
    if files:
        print("\n" + "="*80)
        print("Files as JSON:")
        print("="*80)
        print(json.dumps(files, indent=2))


if __name__ == "__main__":
    main()
