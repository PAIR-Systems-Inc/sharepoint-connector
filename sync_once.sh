#!/usr/bin/env bash
# Run manual full sync (SharePoint → Goodmem), same as Setup steps 1–3.
# Prerequisite: a correct .env (or use --env-file). This script checks required vars and runs sync_once.py.

set -euo pipefail
SCRIPT_DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
cd "$SCRIPT_DIR"

ENV_FILE=".env"

while [[ $# -gt 0 ]]; do
  case "$1" in
    --env-file)
      ENV_FILE="${2:?Error: --env-file requires a path (e.g. .env.mycluster)}"
      shift 2
      ;;
    -h|--help)
      echo "Usage: $0 [OPTIONS]"
      echo ""
      echo "Runs a one-time full sync from SharePoint to Goodmem."
      echo "Prerequisite: .env (or --env-file) with at least SharePoint vars. If GOODMEM_BASE_URL/GOODMEM_API_KEY"
      echo "are missing and Fly CLI is installed, Goodmem is installed automatically (get.goodmem.ai/flyio)."
      echo ""
      echo "Options:"
      echo "  --env-file PATH  Use this env file (default: .env)."
      echo "  -h, --help      Show this help."
      exit 0
      ;;
    *)
      echo "Unknown option: $1. Use -h for help." >&2
      exit 1
      ;;
  esac
done

# Ensure env file exists
if [[ ! -f "$ENV_FILE" ]]; then
  if [[ "$ENV_FILE" == ".env" ]]; then
    if [[ -f .env.example ]]; then
      cp .env.example .env
      echo "Created .env from .env.example. Fill in at least SharePoint vars (and Goodmem vars if you have them); run again to sync or to install Goodmem automatically." >&2
    else
      echo "Error: .env not found and .env.example not found. Create .env with SharePoint credentials." >&2
    fi
  else
    echo "Error: Env file not found: $ENV_FILE" >&2
  fi
  exit 1
fi

# Helper: read a var from ENV_FILE (non-empty value)
_env_var() {
  local var="$1"
  grep -E "^${var}=" "$ENV_FILE" 2>/dev/null | cut -d= -f2- | sed "s/^['\"]//;s/['\"]$//" | tr -d '\r' || true
}

# If Goodmem vars missing, run Goodmem installer (Fly.io) and write GOODMEM_* to ENV_FILE
goodmem_url="$(_env_var GOODMEM_BASE_URL)"
goodmem_key="$(_env_var GOODMEM_API_KEY)"
if [[ -z "${goodmem_url:-}" ]] || [[ -z "${goodmem_key:-}" ]]; then
  FLY_CMD=""
  for cmd in flyctl fly; do
    if command -v "$cmd" &>/dev/null; then
      FLY_CMD="$cmd"
      break
    fi
  done
  if [[ -z "$FLY_CMD" ]]; then
    echo "Error: GOODMEM_BASE_URL and/or GOODMEM_API_KEY are missing in $ENV_FILE, and Fly CLI not found." >&2
    echo "Either: (1) Install Goodmem (see goodmem.ai/quick-start), set GOODMEM_BASE_URL and GOODMEM_API_KEY in $ENV_FILE, then run again." >&2
    echo "Or: (2) Install Fly CLI (https://fly.io/docs/hands-on/install-flyctl/) and run this script again to install Goodmem automatically." >&2
    exit 1
  fi
  GOODMEM_APP_NAME="${GOODMEM_APP_NAME:-sharepoint-sync-goodmem}"
  FLY_REGION="$(_env_var FLY_REGION)"
  [[ -z "$FLY_REGION" ]] && FLY_REGION="sjc"
  FLY_ORG="$(_env_var FLY_ORG)"
  echo "=== Goodmem not configured; installing via get.goodmem.ai/flyio ==="
  goodmem_out="$(mktemp)"
  trap "rm -f '$goodmem_out'" RETURN
  goodmem_args=(--app-name "$GOODMEM_APP_NAME" --tier small --region "$FLY_REGION")
  [[ -n "$FLY_ORG" ]] && goodmem_args+=(--org "$FLY_ORG")
  if ! curl -s https://get.goodmem.ai/flyio | bash -s -- "${goodmem_args[@]}" 2>&1 | tee "$goodmem_out"; then
    echo "Goodmem installer failed. Set GOODMEM_BASE_URL and GOODMEM_API_KEY in $ENV_FILE manually and run again." >&2
    exit 1
  fi
  goodmem_url="https://${GOODMEM_APP_NAME}.fly.dev"
  if grep -q '^GOODMEM_BASE_URL=' "$ENV_FILE" 2>/dev/null; then
    sed -i.bak "s|^GOODMEM_BASE_URL=.*|GOODMEM_BASE_URL=$goodmem_url|" "$ENV_FILE" 2>/dev/null || true
  else
    echo "GOODMEM_BASE_URL=$goodmem_url" >> "$ENV_FILE"
  fi
  api_key="$(sed -n 's/.*Root API key: \(gm_[a-zA-Z0-9]*\).*/\1/p' "$goodmem_out" | head -1)"
  if [[ -n "$api_key" ]]; then
    if grep -q '^GOODMEM_API_KEY=' "$ENV_FILE" 2>/dev/null; then
      sed -i.bak "s|^GOODMEM_API_KEY=.*|GOODMEM_API_KEY=$api_key|" "$ENV_FILE" 2>/dev/null || true
    else
      echo "GOODMEM_API_KEY=$api_key" >> "$ENV_FILE"
    fi
  fi
  $FLY_CMD scale count 1 -a "$GOODMEM_APP_NAME" --yes 2>/dev/null || true
  echo "Goodmem installed. Continuing with sync."
fi

# Check required vars (non-empty)
REQUIRED_VARS=(
  SHAREPOINT_CLIENT_ID
  SHAREPOINT_TENANT_ID
  SHAREPOINT_CLIENT_SECRET
  SHAREPOINT_SITE_URL
  GOODMEM_BASE_URL
  GOODMEM_API_KEY
)
missing=()
for var in "${REQUIRED_VARS[@]}"; do
  val="$(_env_var "$var")"
  if [[ -z "${val:-}" ]]; then
    missing+=("$var")
  fi
done
if [[ ${#missing[@]} -gt 0 ]]; then
  echo "Error: Missing or empty in $ENV_FILE: ${missing[*]}" >&2
  echo "Fill in these variables and run again. See .env.example for reference." >&2
  exit 1
fi

# Run sync: prefer uv (installs deps on the fly), else python (install deps if needed)
run_sync() {
  if command -v uv &>/dev/null; then
    uv run sync_once.py --env-file "$ENV_FILE"
  else
    python sync_once.py --env-file "$ENV_FILE"
  fi
}
if run_sync; then
  exit 0
fi
if command -v uv &>/dev/null; then
  exit 1
fi
echo "Installing Python dependencies (pip install -r requirements.txt) and retrying..." >&2
pip install -r requirements.txt
run_sync
