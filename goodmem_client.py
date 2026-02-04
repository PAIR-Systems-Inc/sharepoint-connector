# Copyright 2026 pairsys.ai (DBA Goodmem.ai)
#
# Licensed under the Apache License, Version 2.0 (the "License");
# you may not use this file except in compliance with the License.
# You may obtain a copy of the License at
#
#     http://www.apache.org/licenses/LICENSE-2.0
#
# Unless required by applicable law or agreed to in writing, software
# distributed under the License is distributed on an "AS IS" BASIS,
# WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
# See the License for the specific language governing permissions and
# limitations under the License.

"""Goodmem API client for interacting with Goodmem.ai."""

# Max memory IDs per batch-get request. Backend has no limit; smaller batches
# reduce risk of OOM or transmission issues.
BATCH_GET_MEMORIES_SIZE = 20

import base64
import json
import uuid
from typing import Any
from typing import Dict
from typing import List
from typing import Optional
from urllib.parse import quote

import requests


def uuid_from_file_id(file_id: str) -> str:
    """Deterministic UUID v5 for a SharePoint file id. Used as Goodmem memoryId."""
    namespace = uuid.uuid5(uuid.NAMESPACE_DNS, "sharepoint.file.id")
    return str(uuid.uuid5(namespace, file_id))


# ---------------------------------------------------------------------------
# Commented-out: custom multipart boundary so boundary does NOT appear in file.
# Some servers split the multipart body on the boundary string. If the file
# content itself contains a substring that equals the boundary (e.g. random
# hex in a PDF), the server can mis-parse and return 400 Bad Request or
# "Invalid JSON". The code below picks a boundary with _choose_multipart_boundary
# (random hex that is verified not to appear in content_bytes) and builds the
# body manually with _build_multipart_body. Uncomment and use it if you see
# 400s on certain files (and add: import secrets at top); otherwise the
# standard requests data+files approach (used in insert_memory_binary below)
# is simpler.
# ---------------------------------------------------------------------------
# def _choose_multipart_boundary(content_bytes: bytes, length: int = 64) -> str:
#   """Return a boundary string that does not appear in content_bytes."""
#   while True:
#     boundary = secrets.token_hex(length // 2)  # length hex chars
#     if boundary.encode("ascii") not in content_bytes:
#       return boundary
#     length += 16
#
# def _build_multipart_body(
#     boundary: str,
#     request_json: bytes,
#     file_bytes: bytes,
#     file_content_type: str,
# ) -> bytes:
#   """Build multipart/form-data body with part 'request' (JSON) then 'file' (binary)."""
#   b = boundary.encode("ascii")
#   crlf = b"\r\n"
#   part1 = (
#       b"--" + b + crlf
#       + b'Content-Disposition: form-data; name="request"' + crlf
#       + b"Content-Type: application/json" + crlf
#       + crlf
#       + request_json + crlf
#   )
#   part2 = (
#       b"--" + b + crlf
#       + b'Content-Disposition: form-data; name="file"; filename="upload"' + crlf
#       + b"Content-Type: " + file_content_type.encode("ascii") + crlf
#       + crlf
#       + file_bytes + crlf
#   )
#   end = b"--" + b + b"--" + crlf
#   return part1 + part2 + end


class GoodmemClient:
  """Client for interacting with the Goodmem API.

  Attributes:
    _base_url: The base URL for the Goodmem API.
    _api_key: The API key for authentication.
    _headers: HTTP headers for API requests.
  """

  def __init__(self, base_url: str, api_key: str, debug: bool = False) -> None:
    """Initializes the Goodmem client.

    Args:
      base_url: The base URL for the Goodmem API, without the /v1 suffix
        (e.g., "https://api.goodmem.ai").
      api_key: The Goodmem API key for authentication.
    """
    # Remove trailing slash if present to avoid double slashes in URLs
    self._base_url = base_url.rstrip("/")
    self._api_key = api_key
    self._headers = {
        "x-api-key": self._api_key,
        "Content-Type": "application/json",
    }
    self._debug = debug

  def _safe_json_dumps(self, value: Any) -> str:
    try:
      return json.dumps(value, indent=2)
    except (TypeError, ValueError):
      return f"<non-serializable: {type(value).__name__}>"

  def create_space(self, space_name: str, embedder_id: str) -> Dict[str, Any]:
    """Creates a new Goodmem space.

    Args:
      space_name: The name of the space to create.
      embedder_id: The embedder ID to use for the space.

    Returns:
      The response JSON containing spaceId.

    Raises:
      requests.exceptions.RequestException: If the API request fails.
    """
    url = f"{self._base_url}/v1/spaces"
    payload = {
        "name": space_name,
        "spaceEmbedders": [
            {"embedderId": embedder_id, "defaultRetrievalWeight": 1.0}
        ],
        "defaultChunkingConfig": {
            "recursive": {
                "chunkSize": 512,
                "chunkOverlap": 64,
                "keepStrategy": "KEEP_END",
                "lengthMeasurement": "CHARACTER_COUNT",
            }
        },
    }
    response = requests.post(
        url, json=payload, headers=self._headers, timeout=30
    )
    response.raise_for_status()
    return response.json()

  def insert_memory(
      self,
      space_id: str,
      content: str,
      content_type: str = "text/plain",
      metadata: Optional[Dict[str, Any]] = None,
  ) -> Dict[str, Any]:
    """Inserts a text memory into a Goodmem space.

    Args:
      space_id: The ID of the space to insert into.
      content: The content of the memory.
      content_type: The content type (default: text/plain).
      metadata: Optional metadata dict (e.g., session_id, user_id).

    Returns:
      The response JSON containing memoryId and processingStatus.

    Raises:
      requests.exceptions.RequestException: If the API request fails.
    """
    url = f"{self._base_url}/v1/memories"
    payload: Dict[str, Any] = {
        "spaceId": space_id,
        "originalContent": content,
        "contentType": content_type,
    }
    if metadata:
      payload["metadata"] = metadata
    response = requests.post(
        url, json=payload, headers=self._headers, timeout=30
    )
    response.raise_for_status()
    return response.json()

  def insert_memory_binary(
      self,
      space_id: str,
      content_bytes: bytes,
      content_type: str,
      metadata: Optional[Dict[str, Any]] = None,
      *,
      memory_id: Optional[str] = None,
      use_base64_fallback: bool = True,
      filename: str = "upload",
  ) -> Dict[str, Any]:
    """Inserts a binary memory into a Goodmem space using multipart upload.

    Args:
      space_id: The ID of the space to insert into.
      content_bytes: The raw binary content as bytes.
      content_type: The MIME type (e.g., application/pdf, image/png).
      metadata: Optional metadata dict (e.g., session_id, user_id, filename).
      memory_id: Optional deterministic memory ID (e.g. uuid_from_file_id(file_id)).
      filename: Filename for the multipart file part (default "upload"). Stored as metadata.filename by the backend.

    Returns:
      The response JSON containing memoryId and processingStatus.

    Raises:
      requests.exceptions.RequestException: If the API request fails.
    """
    url = f"{self._base_url}/v1/memories"

    if self._debug:
      print("[DEBUG] insert_memory_binary called:")
      print(f"  - space_id: {space_id}")
      print(f"  - content_type: {content_type}")
      print(f"  - content_bytes length: {len(content_bytes)} bytes")
      print(f"  - filename: {filename}")
      if metadata:
        print(f"  - metadata:\n{self._safe_json_dumps(metadata)}")

    # Build the JSON request metadata
    request_data: Dict[str, Any] = {
        "spaceId": space_id,
        "contentType": content_type,
    }
    if memory_id:
      request_data["memoryId"] = memory_id
    if metadata:
      request_data["metadata"] = metadata

    if self._debug:
      print(f"[DEBUG] request_data:\n{self._safe_json_dumps(request_data)}")

    # Standard multipart: requests chooses the boundary. If you see 400 on some
    # files (e.g. "Invalid JSON") and the server splits on boundary, consider
    # using the commented-out custom boundary logic above (_choose_multipart_boundary
    # + _build_multipart_body) so the boundary is guaranteed not to appear in the file.
    data = {"request": json.dumps(request_data)}
    files = {"file": (filename, content_bytes, content_type)}
    headers = {"x-api-key": self._api_key}

    if self._debug:
      print(f"[DEBUG] Making POST request to {url}")
    response = requests.post(
        url, data=data, files=files, headers=headers, timeout=120
    )
    if self._debug:
      print(f"[DEBUG] Response status: {response.status_code}")

    if not response.ok:
      try:
        err_body = response.json()
        err_msg = self._safe_json_dumps(err_body)
        err_str = err_body.get("error", "") if isinstance(err_body, dict) else str(err_body)
      except Exception:
        err_msg = response.text or response.reason
        err_str = response.text or ""
      # Fallback: if server returned 400 and tried to parse non-JSON as JSON
      # (e.g. multipart boundary or file bytes), retry with JSON + base64.
      if (
          use_base64_fallback
          and response.status_code == 400
          and "Invalid JSON" in err_str
          and len(content_bytes) < 20 * 1024 * 1024
      ):  # Skip fallback for very large files (>20MB)
        if self._debug:
          print("[DEBUG] Retrying with application/json + originalContentB64")
        json_payload: Dict[str, Any] = {
            "spaceId": space_id,
            "contentType": content_type,
            "originalContentB64": base64.standard_b64encode(content_bytes).decode("ascii"),
        }
        if memory_id:
          json_payload["memoryId"] = memory_id
        if metadata:
          json_payload["metadata"] = metadata
        response = requests.post(
            url,
            json=json_payload,
            headers={**self._headers, "x-api-key": self._api_key},
            timeout=120,
        )
        if response.ok:
          result = response.json()
          if self._debug:
            print(f"[DEBUG] Response:\n{self._safe_json_dumps(result)}")
          return result
        try:
          err_msg = self._safe_json_dumps(response.json())
        except Exception:
          err_msg = response.text or response.reason
      raise requests.exceptions.HTTPError(
          f"HTTP {response.status_code}: {response.reason}. Response: {err_msg}",
          response=response,
      )
    result = response.json()
    if self._debug:
      print(f"[DEBUG] Response:\n{self._safe_json_dumps(result)}")
    return result

  def retrieve_memories(
      self,
      query: str,
      space_ids: List[str],
      request_size: int = 5,
  ) -> List[Dict[str, Any]]:
    """Searches for chunks matching a query in given spaces.

    Args:
      query: The search query message.
      space_ids: List of space IDs to search in.
      request_size: The number of chunks to retrieve.

    Returns:
      List of matching chunks (parsed from NDJSON response).

    Raises:
      requests.exceptions.RequestException: If the API request fails.
    """
    url = f"{self._base_url}/v1/memories:retrieve"
    headers = self._headers.copy()
    headers["Accept"] = "application/x-ndjson"

    payload = {
        "message": query,
        "spaceKeys": [{"spaceId": space_id} for space_id in space_ids],
        "requestedSize": request_size,
    }

    response = requests.post(url, json=payload, headers=headers, timeout=30)
    response.raise_for_status()

    chunks = []
    for line in response.text.strip().split("\n"):
      if line.strip():  # Skip blank/empty lines
        try:
          tmp_dict = json.loads(line)
          if "retrievedItem" in tmp_dict:
            chunks.append(tmp_dict)
        except json.JSONDecodeError:
          # Skip malformed lines (e.g., transmission errors)
          continue
    return chunks

  def list_spaces(self, name: Optional[str] = None) -> List[Dict[str, Any]]:
    """Lists spaces, optionally filtering by name.

    Returns:
      List of spaces (optionally filtered by name).

    Raises:
      requests.exceptions.RequestException: If the API request fails.
    """
    url = f"{self._base_url}/v1/spaces"

    all_spaces = []
    next_token = None
    max_results = 1000

    while True:
      # Build query parameters
      params = {"maxResults": max_results}
      if next_token:
        params["nextToken"] = next_token
      if name:
        params["nameFilter"] = name

      response = requests.get(
          url, headers=self._headers, params=params, timeout=30
      )
      response.raise_for_status()

      data = response.json()
      spaces = data.get("spaces", [])
      all_spaces.extend(spaces)

      # Check for next page
      next_token = data.get("nextToken")
      if not next_token:
        break

    return all_spaces

  def list_embedders(self) -> List[Dict[str, Any]]:
    """Lists all embedders.

    Returns:
      List of embedders.

    Raises:
      requests.exceptions.RequestException: If the API request fails.
    """
    url = f"{self._base_url}/v1/embedders"
    response = requests.get(url, headers=self._headers, timeout=30)
    response.raise_for_status()
    return response.json().get("embedders", [])

  def get_memory_by_id(self, memory_id: str) -> Dict[str, Any]:
    """Gets a memory by its ID.

    Args:
      memory_id: The ID of the memory to retrieve.

    Returns:
      The memory object including metadata, contentType, etc.

    Raises:
      requests.exceptions.RequestException: If the API request fails.
    """
    # URL-encode the memory_id to handle special characters
    encoded_memory_id = quote(memory_id, safe="")
    url = f"{self._base_url}/v1/memories/{encoded_memory_id}"
    response = requests.get(url, headers=self._headers, timeout=30)
    response.raise_for_status()
    return response.json()

  def batch_get_memories(
      self, memory_ids: List[str]
  ) -> Dict[str, Dict[str, Any]]:
    """Fetches multiple memories by ID in batches of BATCH_GET_MEMORIES_SIZE.

    Args:
      memory_ids: List of memory IDs to fetch.

    Returns:
      Dict mapping memory_id -> memory dict for each successfully retrieved
      memory. IDs that are not found (404) are omitted.
    """
    result: Dict[str, Dict[str, Any]] = {}
    for i in range(0, len(memory_ids), BATCH_GET_MEMORIES_SIZE):
      chunk = memory_ids[i : i + BATCH_GET_MEMORIES_SIZE]
      for mid in chunk:
        try:
          result[mid] = self.get_memory_by_id(mid)
        except requests.exceptions.HTTPError as e:
          if e.response is not None and e.response.status_code == 404:
            continue
          raise
    return result

  def list_memories(
      self,
      space_id: str,
      *,
      include_content: bool = False,
      max_results: int = 500,
      next_token: Optional[str] = None,
      status_filter: Optional[str] = None,
  ) -> Dict[str, Any]:
    """Lists memories in a space with pagination.

    Args:
      space_id: The ID of the space to list memories from.
      include_content: Whether to include original content in the response.
      max_results: Maximum number of results per page (1–500).
      next_token: Opaque pagination token for the next page.
      status_filter: Filter by processing status (PENDING, PROCESSING, COMPLETED, FAILED).

    Returns:
      Response dict with "memories" (list) and optional "nextToken".

    Raises:
      requests.exceptions.RequestException: If the API request fails.
    """
    encoded_space_id = quote(space_id, safe="")
    url = f"{self._base_url}/v1/spaces/{encoded_space_id}/memories"
    params: Dict[str, Any] = {"maxResults": max_results}
    if include_content:
      params["includeContent"] = "true"
    if next_token:
      params["nextToken"] = next_token
    if status_filter:
      params["statusFilter"] = status_filter
    response = requests.get(
        url, headers=self._headers, params=params, timeout=30
    )
    response.raise_for_status()
    return response.json()

  def list_all_memories(self, space_id: str) -> List[Dict[str, Any]]:
    """Lists all memories in a space, following pagination.

    Args:
      space_id: The ID of the space to list memories from.

    Returns:
      List of memory dicts (each includes memoryId, metadata, etc.).

    Raises:
      requests.exceptions.RequestException: If the API request fails.
    """
    all_memories: List[Dict[str, Any]] = []
    next_token: Optional[str] = None
    while True:
      resp = self.list_memories(
          space_id, max_results=500, next_token=next_token
      )
      memories = resp.get("memories", [])
      all_memories.extend(memories)
      next_token = resp.get("nextToken")
      if not next_token:
        break
    return all_memories

  def delete_memory(self, memory_id: str) -> None:
    """Deletes a memory by its ID.

    Args:
      memory_id: The ID of the memory to delete.

    Raises:
      requests.exceptions.RequestException: If the API request fails.
    """
    encoded_memory_id = quote(memory_id, safe="")
    url = f"{self._base_url}/v1/memories/{encoded_memory_id}"
    response = requests.delete(url, headers=self._headers, timeout=30)
    response.raise_for_status()

  def find_space_by_name(self, space_name: str) -> Optional[str]:
    """Finds a space ID by its name.

    Args:
      space_name: The name of the space to find.

    Returns:
      The space ID if found, None otherwise.
    """
    spaces = self.list_spaces(name=space_name)
    for space in spaces:
      if space.get("name") == space_name:
        space_id = space.get("spaceId")
        if space_id:
          return space_id
    return None

