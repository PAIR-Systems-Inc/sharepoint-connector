# SharePoint Connector

Python connector for accessing SharePoint files via Microsoft Graph API.

## Prerequisites

- Azure AD App Registration with `Files.Read.All` application permission (admin consent required). If you need to upload files, you will also need to request `Files.ReadWrite.All` application permission.
- Python 3.7+

## Setup

### 1. Install Dependencies

```bash
uv pip install -r requirements.txt
```

### 2. Configure Credentials

```bash
cp .env.example .env
# Edit .env with your credentials
```

Required variables in `.env`:
- `SHAREPOINT_CLIENT_ID` - Azure AD Application (client) ID
- `SHAREPOINT_TENANT_ID` - Azure AD Directory (tenant) ID
- `SHAREPOINT_CLIENT_SECRET` - Client secret
- `SHAREPOINT_SITE_URL` - SharePoint site URL

### 3. Run the Connector

```bash
python sharepoint_connector.py
```

## Usage Examples

### Basic Usage

```python
from sharepoint_connector import SharePointConnector
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
```

### List Files from Specific Folder

```python
files = connector.list_files(folder_path="Documents/Reports")
```

### List Files from Specific Drive

```python
drives = connector.get_drives()
files = connector.list_files(drive_id=drives[0]["id"])
```


## File Object Structure

Each file returned contains:
- `id`, `name`, `web_url`, `download_url`
- `size`, `created_datetime`, `modified_datetime`
- `created_by`, `modified_by`, `mime_type`, `file_hash`
