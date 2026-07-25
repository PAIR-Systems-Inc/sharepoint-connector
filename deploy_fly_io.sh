#!/usr/bin/env bash
# Deploy to Fly.io: the SharePoint listener, optionally with Goodmem (by mode).
# Default (no mode flag): deploy the listener. --hands-free also provisions Goodmem first.
# Modes: --hands-free, --goodmem-only, --generate-only. The listener Fly config is fly_io.toml (from fly_io.toml.template).
# If the app name is in use, a random suffix is added and creation is retried until no collision.
# Uses a single .env file (create from .env.example). App names = <FLY_CLUSTER>-goodmem, <FLY_CLUSTER>-listener (FLY_CLUSTER required in .env).

set -euo pipefail
SCRIPT_DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
cd "$SCRIPT_DIR"

ENV_FILE=".env"
ENV_FILE_GIVEN=0

# --- Fly region: default sjc. Override with --region or FLY_REGION in .env. ---
FLY_REGION=""

MODE=""
FLY_ORG=""
while [[ $# -gt 0 ]]; do
  case "$1" in
    --env-file)
      ENV_FILE="${2:?Error: --env-file requires a path}"
      ENV_FILE_GIVEN=1
      shift 2
      ;;
    --hands-free)
      MODE="both"
      shift
      ;;
    --goodmem-only)
      MODE="goodmem_only"
      shift
      ;;
    --generate-only)
      MODE="generate_only"
      shift
      ;;
    --org)
      FLY_ORG="${2:?Error: --org requires an organization slug (e.g. pair-systems)}"
      shift 2
      ;;
    --region)
      FLY_REGION="${2:?Error: --region requires a region code (e.g. sjc, lax)}"
      shift 2
      ;;
    -h|--help)
      echo "Usage: $0 [OPTIONS] [--hands-free | --goodmem-only | --generate-only]"
      echo ""
      echo "With no mode flag, deploys the SharePoint listener (the always-on piece)."
      echo ""
      echo "Options:"
      echo "  --env-file F  Use F as env file (default: .env)."
      echo "  --org ORG     Fly.io organization slug (avoids TTY prompt when you have multiple orgs)."
      echo "  --region R    Fly.io region code (default: sjc). e.g. sjc, lax, ewr."
      echo ""
      echo "Modes:"
      echo "  (no flag)       Deploy only the SharePoint listener (uses fly_io.toml + Dockerfile)."
      echo "  --hands-free    Deploy Goodmem first, then the listener."
      echo "  --goodmem-only  Deploy only Goodmem (runs get.goodmem.ai/flyio installer)."
      echo "  --generate-only Generate fly_io.toml from the template (no deploy)."
      echo ""
      echo "Uses .env by default (or --env-file F). Copy from .env.example; set Azure and FLY_CLUSTER (optional FLY_ORG, FLY_REGION)."
      echo "GRAPH_NOTIFICATION_URL and GRAPH_CLIENT_STATE are written/generated for you during deploy."
      echo "App names: <FLY_CLUSTER>-goodmem, <FLY_CLUSTER>-listener (FLY_CLUSTER required in .env)."
      exit 0
      ;;
    *)
      echo "Unknown option: $1. Use -h for help." >&2
      exit 1
      ;;
  esac
done

# Resolve env file path if relative (so paths work from any cwd)
if [[ "$ENV_FILE" != /* ]]; then
  ENV_FILE="$SCRIPT_DIR/$ENV_FILE"
fi
# Create .env from .env.example only when using default .env and it's missing; otherwise require file to exist
if [[ ! -f "$ENV_FILE" ]]; then
  if [[ $ENV_FILE_GIVEN -eq 1 ]]; then
    echo "Error: env file not found: $ENV_FILE" >&2
    exit 1
  fi
  if [[ -f "$SCRIPT_DIR/.env.example" ]]; then
    cp "$SCRIPT_DIR/.env.example" "$ENV_FILE"
    echo "Created $ENV_FILE from .env.example. Fill in Azure credentials and FLY_CLUSTER (and optionally FLY_ORG, FLY_REGION), then run this script again." >&2
    exit 1
  fi
  echo "Error: $ENV_FILE not found. Copy .env.example to .env, set Azure credentials and FLY_CLUSTER, then run this script again." >&2
  exit 1
fi

# App names: <FLY_CLUSTER>-goodmem, <FLY_CLUSTER>-listener. FLY_CLUSTER is required in .env (legacy: APP_CLUSTER_NAME).
FLY_CLUSTER=""
if [[ -f "$ENV_FILE" ]]; then
  FLY_CLUSTER="$(grep -E '^FLY_CLUSTER=' "$ENV_FILE" 2>/dev/null | head -1 | sed -E 's/^FLY_CLUSTER=//; s/[A-Z_][A-Z0-9_]*=.*//; s/^["'\'' ]+//; s/["'\'' ]+$//' | tr -d '\r\n' || true)"
  [[ -z "$FLY_CLUSTER" ]] && FLY_CLUSTER="$(grep -E '^APP_CLUSTER_NAME=' "$ENV_FILE" 2>/dev/null | head -1 | sed -E 's/^APP_CLUSTER_NAME=//; s/[A-Z_][A-Z0-9_]*=.*//; s/^["'\'' ]+//; s/["'\'' ]+$//' | tr -d '\r\n' || true)"
fi
if [[ -z "$FLY_CLUSTER" ]]; then
  echo "Error: FLY_CLUSTER is required in .env (set it in .env.example before copying, or in .env). It controls Fly app names and must be unique to avoid collision. Legacy: APP_CLUSTER_NAME." >&2
  exit 1
fi
GOODMEM_APP_NAME="${FLY_CLUSTER}-goodmem"
LISTENER_APP_NAME="${FLY_CLUSTER}-listener"

# Use FLY_ORG and FLY_REGION from env file if not set by --org / --region (one var per line; value only)
if [[ -f "$ENV_FILE" ]]; then
  [[ -z "$FLY_ORG" ]] && FLY_ORG="$(grep -E '^FLY_ORG=' "$ENV_FILE" 2>/dev/null | head -1 | sed -E 's/^FLY_ORG=//; s/[A-Z_][A-Z0-9_]*=.*//; s/^["'\'' ]+//; s/["'\'' ]+$//' | tr -d '\r\n')" || :
  [[ -z "$FLY_REGION" ]] && FLY_REGION="$(grep -E '^FLY_REGION=' "$ENV_FILE" 2>/dev/null | head -1 | sed -E 's/^FLY_REGION=//; s/[A-Z_][A-Z0-9_]*=.*//; s/^["'\'' ]+//; s/["'\'' ]+$//' | tr -d '\r\n')" || :
fi
[[ -z "$FLY_REGION" ]] && FLY_REGION="sjc"

# Default: no mode flag deploys the listener (the always-on piece).
if [[ -z "$MODE" ]]; then
  MODE="listener_only"
fi

# Generate fly_io.toml from fly_io.toml.template (app name and region from script/env).
generate_fly_configs() {
  local template_dir="$SCRIPT_DIR"
  local t="fly_io.toml.template"
  local out="fly_io.toml"
  if [[ ! -f "$template_dir/$t" ]]; then
    echo "Error: template $t not found." >&2
    exit 1
  fi
  sed "s|{{APP_NAME}}|$LISTENER_APP_NAME|g; s|{{PRIMARY_REGION}}|$FLY_REGION|g" "$template_dir/$t" > "$SCRIPT_DIR/$out"
  echo "Generated $out (app=$LISTENER_APP_NAME, region=$FLY_REGION)"
}

# Prefer flyctl; fall back to fly
FLY_CMD=""
for cmd in flyctl fly; do
  if command -v "$cmd" &>/dev/null; then
    FLY_CMD="$cmd"
    break
  fi
done
if [[ -z "$FLY_CMD" ]]; then
  echo "Error: Fly CLI not found. Install it: https://fly.io/docs/hands-on/install-flyctl/" >&2
  exit 1
fi

# --- Goodmem: run official installer (creates Goodmem + Postgres apps), then write GOODMEM_* to ENV_FILE ---
do_goodmem() {
  echo "=== Deploying Goodmem (get.goodmem.ai/flyio) ==="
  echo "Goodmem app name: $GOODMEM_APP_NAME"
  echo "Env file: $ENV_FILE"
  echo "Region: $FLY_REGION"
  [[ -n "$FLY_ORG" ]] && echo "Fly org: $FLY_ORG"
  echo ""
  goodmem_args=(--app-name "$GOODMEM_APP_NAME" --tier small --region "$FLY_REGION")
  [[ -n "$FLY_ORG" ]] && goodmem_args+=(--org "$FLY_ORG")
  goodmem_out="$(mktemp)"
  trap "rm -f '$goodmem_out'" RETURN
  curl -s https://get.goodmem.ai/flyio | bash -s -- "${goodmem_args[@]}" 2>&1 | tee "$goodmem_out"
  # goodmem_out now has installer stdout+stderr (including "Root API key: gm_...")
  echo ""

  goodmem_url="https://${GOODMEM_APP_NAME}.fly.dev"
  if [[ -f "$ENV_FILE" ]] && grep -q '^GOODMEM_BASE_URL=' "$ENV_FILE" 2>/dev/null; then
    sed -i.bak "s|^GOODMEM_BASE_URL=.*|GOODMEM_BASE_URL=$goodmem_url|" "$ENV_FILE" 2>/dev/null || true
  else
    echo "GOODMEM_BASE_URL=$goodmem_url" >> "$ENV_FILE"
  fi
  echo "Set GOODMEM_BASE_URL=$goodmem_url in $ENV_FILE"

  api_key="$(sed -n 's/.*Root API key: \(gm_[a-zA-Z0-9]*\).*/\1/p' "$goodmem_out" | head -1)"
  if [[ -n "$api_key" ]]; then
    if [[ -f "$ENV_FILE" ]] && grep -q '^GOODMEM_API_KEY=' "$ENV_FILE" 2>/dev/null; then
      sed -i.bak "s|^GOODMEM_API_KEY=.*|GOODMEM_API_KEY=$api_key|" "$ENV_FILE" 2>/dev/null || true
    else
      echo "GOODMEM_API_KEY=$api_key" >> "$ENV_FILE"
    fi
    echo "Set GOODMEM_API_KEY in $ENV_FILE (from installer output)"
  else
    echo "Could not parse Root API key from installer output; set GOODMEM_API_KEY in $ENV_FILE manually."
  fi
  echo "Scaling Goodmem to one machine..."
  $FLY_CMD scale count 1 -a "$GOODMEM_APP_NAME" --yes 2>/dev/null || true
  echo ""

  # Optional: create OpenAI embedder if OPENAI_API_KEY is set in ENV_FILE
  goodmem_base="$(grep -E '^GOODMEM_BASE_URL=' "$ENV_FILE" 2>/dev/null | cut -d= -f2- | sed "s/^['\"]//;s/['\"]$//" | tr -d '\r' || true)"
  goodmem_key="$(grep -E '^GOODMEM_API_KEY=' "$ENV_FILE" 2>/dev/null | cut -d= -f2- | sed "s/^['\"]//;s/['\"]$//" | tr -d '\r' || true)"
  openai_key="$(grep -E '^OPENAI_API_KEY=' "$ENV_FILE" 2>/dev/null | cut -d= -f2- | sed "s/^['\"]//;s/['\"]$//" | tr -d '\r' || true)"
  if [[ -n "$openai_key" ]] && [[ -n "$goodmem_base" ]] && [[ -n "$goodmem_key" ]]; then
    echo "Creating OpenAI embedder (text-embedding-3-small)..."
    embedder_url="${goodmem_base%/}/v1/embedders"
    embedder_json="$(OPENAI_API_KEY="$openai_key" python3 -c '
import json, os
key = os.environ.get("OPENAI_API_KEY", "")
print(json.dumps({
    "displayName": "text-embedding-3-small",
    "providerType": "OPENAI",
    "endpointUrl": "https://api.openai.com/v1/",
    "apiPath": "/embeddings",
    "modelIdentifier": "text-embedding-3-small",
    "dimensionality": 1536,
    "distributionType": "DENSE",
    "maxSequenceLength": 8192,
    "supportedModalities": ["TEXT"],
    "credentials": {
        "kind": "CREDENTIAL_KIND_API_KEY",
        "apiKey": {"inlineSecret": key}
    }
}))')"
    http_code="$(curl -s -o /tmp/embedder_resp.json -w '%{http_code}' -X POST "$embedder_url" \
      -H "Content-Type: application/json" \
      -H "x-api-key: $goodmem_key" \
      -d "$embedder_json" 2>/dev/null)"
    if [[ "$http_code" == "201" ]]; then
      echo "Created embedder text-embedding-3-small."
    elif [[ "$http_code" == "409" ]]; then
      echo "Embedder text-embedding-3-small already exists."
    else
      echo "Embedder creation returned HTTP $http_code (see /tmp/embedder_resp.json). Continuing."
    fi
    rm -f /tmp/embedder_resp.json 2>/dev/null || true
  else
    [[ -z "$openai_key" ]] && echo "Skipping embedder creation (OPENAI_API_KEY not set in $ENV_FILE)."
  fi
  echo ""
  echo "Goodmem deploy finished. Listener will use $ENV_FILE when you run --hands-free or deploy the listener next."
}

# --- Listener (or both): create app (with retry on name collision), secrets, deploy ---
# $1 = app name
do_listener() {
  local name="$1"
  local config="fly_io.toml"
  generate_fly_configs
  echo ""
  echo "=== Deploying SharePoint listener ==="
  echo "App name: $name"
  echo "Env file: $ENV_FILE"
  echo "Config: $config"
  echo "Region: $FLY_REGION"
  [[ -n "$FLY_ORG" ]] && echo "Fly org: $FLY_ORG"
  echo ""

  fly_launch_extra=(-r "$FLY_REGION")
  [[ -n "$FLY_ORG" ]] && fly_launch_extra+=(-o "$FLY_ORG")

  while true; do
    if $FLY_CMD status -a "$name" &>/dev/null; then
      echo "Using existing app: $name"
      break
    fi
    echo "Creating Fly app: $name"
    launch_out="$($FLY_CMD launch --no-deploy --name "$name" --config "$config" --yes "${fly_launch_extra[@]}" 2>&1)" && break
    if echo "$launch_out" | grep -qiE "already exists|already in use|name.*taken|is not available|has been taken"; then
      name="${name}-${RANDOM}"
      echo "App name in use, trying: $name"
    else
      echo "$launch_out" >&2
      exit 1
    fi
  done

  echo "Using Fly app name: $name"
  echo ""

  # Remove any existing notification URL line so no stale value wins (new and legacy names)
  sed -i.bak '/^[[:space:]]*GRAPH_NOTIFICATION_URL[[:space:]]*=/d' "$ENV_FILE" 2>/dev/null || true
  sed -i.bak '/^[[:space:]]*SYNC_NOTIFICATION_URL[[:space:]]*=/d' "$ENV_FILE" 2>/dev/null || true
  echo "GRAPH_NOTIFICATION_URL=https://$name.fly.dev/sync/webhook" >> "$ENV_FILE"
  echo "Set GRAPH_NOTIFICATION_URL=https://$name.fly.dev/sync/webhook in $ENV_FILE"
  echo ""

  # Ensure a Graph clientState secret exists; generate a random one only if unset or still the template placeholder.
  # (Reused on re-deploy so it stays stable — changing it would break validation of an existing subscription.)
  client_state="$(grep -E '^[[:space:]]*GRAPH_CLIENT_STATE[[:space:]]*=' "$ENV_FILE" 2>/dev/null | head -1 | sed -E 's/^[^=]*=//; s/[[:space:]]*#.*$//; s/^["'\'' ]+//; s/["'\'' ]+$//' | tr -d '\r' || true)"
  if [[ -z "$client_state" || "$client_state" == your-secret* ]]; then
    client_state="$(openssl rand -hex 24 2>/dev/null || (head -c 32 /dev/urandom | base64 | tr -dc 'a-zA-Z0-9' | head -c 40))"
    sed -i.bak '/^[[:space:]]*GRAPH_CLIENT_STATE[[:space:]]*=/d' "$ENV_FILE" 2>/dev/null || true
    echo "GRAPH_CLIENT_STATE=$client_state" >> "$ENV_FILE"
    echo "Generated a random GRAPH_CLIENT_STATE in $ENV_FILE"
    echo ""
  fi

  echo "Importing secrets from $ENV_FILE..."
  $FLY_CMD secrets import -a "$name" < "$ENV_FILE"
  echo ""

  # Ensure a persistent volume for durable state (delta cursor + pending-retry
  # sets) exists in the primary region before deploying (the [mounts] in the
  # config requires it). Single machine → one volume.
  if ! $FLY_CMD volumes list -a "$name" 2>/dev/null | grep -q 'sharepoint_state'; then
    echo "Creating persistent volume 'sharepoint_state' (1GB, $FLY_REGION) for durable state..."
    $FLY_CMD volumes create sharepoint_state -a "$name" -r "$FLY_REGION" -n 1 -s 1 --yes
    echo ""
  fi

  echo "Deploying (single machine: --ha=false, region: $FLY_REGION)..."
  $FLY_CMD deploy -a "$name" --config "$config" --ha=false --primary-region "$FLY_REGION"
  echo ""

  # Listener must stay on 24/7 to receive Graph webhooks (no auto sleep/stop)
  echo "Ensuring listener stays running (no auto stop)..."
  $FLY_CMD scale count 1 -a "$name" --yes 2>/dev/null || true
  echo ""

  echo "App is at: https://$name.fly.dev"
  echo "Listener creates the Graph subscription on startup if none exists. Watch: connector watch https://$name.fly.dev"
}

# --- Run by mode ---
case "$MODE" in
  both)
    # OPENAI_API_KEY required for hands-free: script creates text-embedding-3-small embedder in Goodmem
    openai_key="$(grep -E '^OPENAI_API_KEY=' "$ENV_FILE" 2>/dev/null | cut -d= -f2- | sed "s/^['\"]//;s/['\"]$//" | tr -d '\r' | head -1 || true)"
    if [[ -z "${openai_key:-}" ]]; then
      echo "Error: OPENAI_API_KEY is required in $ENV_FILE for --hands-free. Set it so the script can create a text-embedding-3-small embedder in Goodmem." >&2
      exit 1
    fi
    do_goodmem
    echo ""
    do_listener "$LISTENER_APP_NAME"
    ;;
  listener_only)
    do_listener "$LISTENER_APP_NAME"
    ;;
  goodmem_only)
    do_goodmem
    ;;
  generate_only)
    generate_fly_configs
    echo ""
    echo "Done. Deploy with: fly deploy -a $LISTENER_APP_NAME --config fly_io.toml."
    exit 0
    ;;
  *)
    echo "Error: invalid mode $MODE" >&2
    exit 1
    ;;
esac

echo ""
echo "Done."
