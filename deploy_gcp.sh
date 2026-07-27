#!/usr/bin/env bash
#
# Deploy the Goodmem connector listener to a Google Compute Engine VM.
#
# Why GCP: it is the only KEYLESS Google Drive path. The VM runs as an attached
# service account, so no service-account key is ever downloaded or stored. The
# one sharp edge is the access scope — a VM must be created with
# drive.readonly, or the metadata token won't cover the Drive API.
#
# Works for either source (SOURCE=sharepoint or google-drive). Optionally
# installs a Goodmem server on the same VM (--with-goodmem), in which case the
# listener talks to it over localhost and nothing needs a public address.
#
# Usage:
#   ./deploy_gcp.sh --project P --service-account SA_EMAIL [options]
#
# Options:
#   --project ID           GCP project (required)
#   --service-account SA   Service account to attach (required; must be a Viewer
#                          on the Shared Drive for the google-drive source)
#   --zone Z               Default: us-central1-a
#   --machine-type T       Default: e2-standard-2 (e2-medium is enough without --with-goodmem)
#   --vm NAME              Default: goodmem-connector
#   --with-goodmem         Also install a Goodmem server + pgvector on the VM
#   --env-file PATH        Env file to ship to the VM (default: .env)
#   --no-create            Skip VM creation; just (re)deploy onto an existing VM
#   --delete               Delete the VM and exit
#
set -euo pipefail

PROJECT=""; SA=""; ZONE="us-central1-a"; MACHINE="e2-standard-2"
VM="goodmem-connector"; WITH_GOODMEM=false; ENV_FILE=".env"
NO_CREATE=false; DO_DELETE=false; GCLOUD_CONFIG=""

while [ $# -gt 0 ]; do
  case "$1" in
    --project) PROJECT="$2"; shift 2;;
    --service-account) SA="$2"; shift 2;;
    --zone) ZONE="$2"; shift 2;;
    --machine-type) MACHINE="$2"; shift 2;;
    --vm) VM="$2"; shift 2;;
    --with-goodmem) WITH_GOODMEM=true; shift;;
    --env-file) ENV_FILE="$2"; shift 2;;
    --no-create) NO_CREATE=true; shift;;
    --delete) DO_DELETE=true; shift;;
    --configuration) GCLOUD_CONFIG="$2"; shift 2;;
    -h|--help) sed -n '2,34p' "$0"; exit 0;;
    *) echo "Unknown option: $1" >&2; exit 2;;
  esac
done

[ -n "$PROJECT" ] || { echo "Error: --project is required" >&2; exit 2; }
G=(gcloud --project="$PROJECT")
[ -n "$GCLOUD_CONFIG" ] && G+=(--configuration="$GCLOUD_CONFIG")

if $DO_DELETE; then
  echo "Deleting VM $VM in $ZONE …"
  "${G[@]}" compute instances delete "$VM" --zone="$ZONE" --quiet
  echo "Deleted. (Cloud NAT / firewall rules, if this script created them, are left in place.)"
  exit 0
fi

[ -n "$SA" ] || { echo "Error: --service-account is required" >&2; exit 2; }
[ -f "$ENV_FILE" ] || { echo "Error: env file $ENV_FILE not found" >&2; exit 2; }

SSH=("${G[@]}" compute ssh "$VM" --zone="$ZONE" --tunnel-through-iap --quiet)
SCP=("${G[@]}" compute scp --zone="$ZONE" --tunnel-through-iap --quiet)

# --- 1. Create the VM (attached SA + the Drive access scope) ------------------
if ! $NO_CREATE; then
  if "${G[@]}" compute instances describe "$VM" --zone="$ZONE" >/dev/null 2>&1; then
    echo "VM $VM already exists — skipping creation."
  else
    echo "=== Creating VM $VM ($MACHINE, $ZONE) ==="
    # --scopes=drive.readonly is the critical bit: the metadata token is minted
    # with exactly these scopes, and a cloud-platform token does NOT cover Drive.
    # --no-address keeps the VM private; many orgs block external IPs outright
    # (constraints/compute.vmExternalIpAccess). Egress then needs Cloud NAT and
    # SSH goes through IAP — see the notes printed at the end.
    if ! "${G[@]}" compute instances create "$VM" \
        --zone="$ZONE" --machine-type="$MACHINE" \
        --image-family=debian-12 --image-project=debian-cloud \
        --boot-disk-size=30GB --boot-disk-type=pd-balanced \
        --service-account="$SA" \
        --scopes=https://www.googleapis.com/auth/drive.readonly \
        --no-address 2>&1 | tail -3; then
      echo "VM creation failed." >&2; exit 1
    fi
    echo "Waiting for SSH …"
    for _ in $(seq 1 30); do "${SSH[@]}" --command=true >/dev/null 2>&1 && break; sleep 10; done
  fi
fi

# --- 2. Docker (needed for Goodmem; skipped otherwise) ------------------------
if $WITH_GOODMEM; then
  echo "=== Installing Docker (with the compose plugin) ==="
  # Debian's docker.io package has no `docker compose` v2 plugin, which the
  # Goodmem installer requires — so install Docker CE from get.docker.com.
  "${SSH[@]}" --command='command -v docker >/dev/null && docker compose version >/dev/null 2>&1 || { curl -fsSL https://get.docker.com | sudo sh >/dev/null 2>&1; }; docker --version; docker compose version | head -1'

  echo "=== Installing Goodmem (server + pgvector) ==="
  DBPW="gm-$(openssl rand -hex 12)"
  "${SSH[@]}" --command="test -f ~/.goodmem/config.toml || curl -s https://get.goodmem.ai | bash -s -- --handsfree --db-password '$DBPW' >/tmp/goodmem-install.log 2>&1; grep -oE 'gm_[a-z0-9]+' ~/.goodmem/config.toml | head -1 >/tmp/gmkey"
  # The server presents a self-signed cert for localhost; trust it so the
  # connector (which uses the system cert pool) can reach it over HTTPS.
  "${SSH[@]}" --command='openssl s_client -connect localhost:8080 -showcerts </dev/null 2>/dev/null | openssl x509 -outform PEM | sudo tee /usr/local/share/ca-certificates/goodmem-local.crt >/dev/null && sudo update-ca-certificates >/dev/null 2>&1; echo "Goodmem REST: https://localhost:8080"'
fi

# --- 3. Build and ship the connector -----------------------------------------
echo "=== Building the connector (static linux/amd64) ==="
TMPBIN="$(mktemp -d)/connector"
CGO_ENABLED=0 GOOS=linux GOARCH=amd64 go build -ldflags "-s -w" -o "$TMPBIN" ./cmd/connector
"${SCP[@]}" "$TMPBIN" "$VM:~/connector"
"${SCP[@]}" "$ENV_FILE" "$VM:~/connector.env"

# --- 4. Install as a systemd service -----------------------------------------
echo "=== Installing the systemd service ==="
GOODMEM_LOCAL=$($WITH_GOODMEM && echo yes || echo no)
"${SSH[@]}" --command="
set -e
sudo install -m 0755 ~/connector /usr/local/bin/connector
sudo mkdir -p /var/lib/goodmem-connector
sudo install -m 0600 ~/connector.env /etc/goodmem-connector.env && rm -f ~/connector.env ~/connector
# State lives on the (persistent) boot disk so it survives restarts.
grep -q '^GRAPH_DELTA_TOKEN_FILE=' /etc/goodmem-connector.env || \
  echo 'GRAPH_DELTA_TOKEN_FILE=/var/lib/goodmem-connector/.delta' | sudo tee -a /etc/goodmem-connector.env >/dev/null
if [ '$GOODMEM_LOCAL' = yes ]; then
  sudo sed -i '/^GOODMEM_BASE_URL=/d;/^GOODMEM_API_KEY=/d' /etc/goodmem-connector.env
  { echo 'GOODMEM_BASE_URL=https://localhost:8080'; echo \"GOODMEM_API_KEY=\$(cat /tmp/gmkey)\"; } | sudo tee -a /etc/goodmem-connector.env >/dev/null
fi
sudo tee /etc/systemd/system/goodmem-connector.service >/dev/null <<'UNIT'
[Unit]
Description=Goodmem connector listener
After=network-online.target docker.service
Wants=network-online.target

[Service]
EnvironmentFile=/etc/goodmem-connector.env
ExecStart=/usr/local/bin/connector serve --env-file /dev/null
Restart=always
RestartSec=5

[Install]
WantedBy=multi-user.target
UNIT
sudo systemctl daemon-reload
sudo systemctl enable --now goodmem-connector >/dev/null 2>&1
sudo systemctl restart goodmem-connector
sleep 15
systemctl is-active goodmem-connector
"

# --- 5. Verify ----------------------------------------------------------------
echo "=== Verifying ==="
"${SSH[@]}" --command='
echo "  /healthz -> $(curl -s -o /dev/null -w "%{http_code}" --max-time 10 http://localhost:${PORT:-5000}/healthz)"
echo "  /readyz  -> $(curl -s -o /dev/null -w "%{http_code}" --max-time 10 http://localhost:${PORT:-5000}/readyz)"
curl -s --max-time 10 http://localhost:${PORT:-5000}/metrics | grep -E "^connector_(up|full_syncs_total)" | sed "s/^/  /"
echo "  --- recent log ---"
sudo journalctl -u goodmem-connector --no-pager -n 6 | sed "s/gm_[a-z0-9]*/gm_***/g" | sed "s/^/  /"'

cat <<EOF

Done. The listener runs as a systemd service on $VM.

  logs:     gcloud compute ssh $VM --zone=$ZONE --tunnel-through-iap --command='sudo journalctl -u goodmem-connector -f'
  restart:  … --command='sudo systemctl restart goodmem-connector'
  delete:   $0 --project $PROJECT --zone $ZONE --vm $VM --delete

Notes:
  • The VM has NO external IP. That needs Cloud NAT in this region for egress
    and an IAP firewall rule for SSH:
      gcloud compute routers create <r> --network=default --region=<region>
      gcloud compute routers nats create <n> --router=<r> --region=<region> \\
        --auto-allocate-nat-external-ips --nat-all-subnet-ip-ranges
      gcloud compute firewall-rules create allow-iap-ssh --network=default \\
        --allow=tcp:22 --source-ranges=35.235.240.0/20
  • For google-drive, the attached service account must also be a Viewer on the
    Shared Drive — GCP IAM alone grants no Drive access.
EOF
