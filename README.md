# SharePoint Connector

The app to sync files on a SharePoint site to a Goodmem memory space. 

Files: 
1. `sharepoint_client.py`: Fetches files (e.g., PDFs) from SharePoint. 
2. `goodmem_client.py`: Client for interacting with the Goodmem API, including creating a memory space, finding a space by name, and ingesting PDFs as memories.
3. `main.py`: The main script to sync files from SharePoint to Goodmem. Everytime you run this script, it will sync all files from SharePoint to Goodmem, ingesting files that are not already in the memory space. 

## Setup

1. Set up SharePoint application
   Follow the [setup instructions](https://docs.airbyte.com/integrations/sources/microsoft-sharepoint#step-1-set-up-sharepoint-application) provided by AirByte.

2. Install dependencies
    ```bash
    uv pip install -r requirements.txt
    ```

3. Configure Credentials

    Copy the `.env.example` file to `.env` and edit it with your credentials.

    **Environment variables used by `main.py`:**
    - **SharePoint:** `SHAREPOINT_CLIENT_ID`, `SHAREPOINT_TENANT_ID`, `SHAREPOINT_CLIENT_SECRET`, `SHAREPOINT_SITE_URL`
    - **Goodmem:** `GOODMEM_BASE_URL` (required), `GOODMEM_API_KEY` (required), `DEFAULT_EMBEDDER_ID` (optional, used when creating a new space; if unset, the first embedder returned by the Goodmem API is used).

## Usage 

```bash
python main.py
```

**Optional — uv shebang:** `main.py` and `sharepoint_client.py` use [PEP-723](https://peps.python.org/pep-0723/) inline script metadata and a `uv` shebang. If you have [uv](https://docs.astral.sh/uv/) installed, you can run them without a pre-created venv; uv will install dependencies on the fly:

```bash
chmod +x main.py
./main.py
```

See [Python and the uv Shebang](https://brojonat.com/posts/uv/) for details.

For Python usage examples (basic usage, listing from a folder or drive), see the docstrings in `sharepoint_client.py`.

For setting up the **Graph sync server** (`graph_listener.py`) — which uses **Microsoft Graph API webhooks** to receive SharePoint change notifications over HTTPS — see **[listern_setup.md](listern_setup.md)**.

## Technical details

### `sharepoint_client.py`

This library should not load credentials from the `.env` file -- except under `__main__` block

Each file returned is like this in JSON:
```json
  {
    "id": "01DSLNGZ2OAHMTF4SKE5BYGBMAYG6X6HMV",
    "name": "claude_usage.pdf",
    "web_url": "https://incorta.sharepoint.com/sites/Pair/Shared%20Documents/claude_usage.pdf",
    "download_url": "...",
    "size": 153749,
    "created_datetime": "2026-01-28T08:27:35Z",
    "modified_datetime": "2026-01-28T08:27:35Z",
    "created_by": "Mohamed Helmy",
    "modified_by": "Mohamed Helmy",
    "mime_type": "application/pdf",
    "file_hash": null
  }
```

The download URL does not need credentials. You can use it to download the file.


### `goodmem_client.py`

This library should not load credentials from the `.env` file -- except under `__main__` block. 

Functionality of this library includes, but is not limited to:
- Creating a new Goodmem space
- Listing all embedders
- **Finding a space by name** — `find_space_by_name(space_name)` returns the space ID if found, or `None` if no space with that name exists. When `main.py` gets `None`, it creates a new space.
- Listing memories in a space (`list_memories` / `list_all_memories`) and deleting a memory by ID (`delete_memory`).
- Ingesting PDFs (and other supported types) as memories

### `main.py`

This script is the main entry point to sync files from SharePoint to Goodmem. It creates a Sharepoint client from `sharepoint_client.py` and a Goodmem client from `goodmem_client.py`, then fetches **all files** from the default SharePoint drive (root folder, recursive), and syncs them to Goodmem according to the rules below. When no files need to be added or deleted, it prints **"Nothing needs to be changed."** 

#### Determining the Goodmem space name

The name of the Goodmem space that stores the files fetched from SharePoint is derived from the SharePoint site URL:

- **Org** = host part before `.sharepoint.com` (e.g. `good` from `good.sharepoint.com`).
- **Site** = the first path segment after `sites/` only. For example, `https://good.sharepoint.com/sites/Mem` or `https://good.sharepoint.com/sites/Mem/Shared%20Documents` both use site name `Mem`; `Shared%20Documents` is not part of the site name.
- **Space name** = `SharePoint_{Org}_{Site}` (e.g. `SharePoint_Good_Mem`). Matching is case-sensitive.
- Trailing slashes on the site URL are stripped before parsing.

If a space with that name already exists, it is used. If not, a new space is created. When creating a new space, the embedder ID is taken from `DEFAULT_EMBEDDER_ID` in `.env` if set; otherwise the first embedder returned by the Goodmem client is used.

#### Metadata for ingested files

Use all fields in the sample file JSON returned by the Sharepoint client to create metadata for the ingested file. 

#### Supported MIME types
Based on the Goodmem source code (`TextContentExtractor`), Goodmem only supports the following MIME types for ingestion:
- All text/* MIME types
- application/pdf
- application/rtf
- application/msword (.doc)
- application/vnd.openxmlformats-officedocument.wordprocessingml.document (.docx)
- Any MIME type containing "+xml" (e.g., application/xhtml+xml)
- Any MIME type containing "json" (e.g., application/json)

If a file is not one of the supported MIME types, skip it.

The mime type of a file can be found in the `mime_type` field of the file JSON returned by the Sharepoint client.

#### File update rules
1. Any file whose `id` is not found in the Goodmem space -- in the metadata -- needs to be ingested into Goodmem.
2. Any file whose `id` is found in the Goodmem space but has a different `modified_datetime` needs to be first deleted (delete the old copy) and then ingested as a new file (of course with the latest metadata) into Goodmem.
3. Any file whose `id` is found in the Goodmem space but no longer exists in SharePoint needs to be deleted from Goodmem.


## Roadmap

Use Sharepoint hook to trigger the sync when a file is created, updated, or deleted, to save the need of manual or periodic sync.