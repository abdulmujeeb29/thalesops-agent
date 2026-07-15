#!/usr/bin/env bash
set -euo pipefail

# -----------------------------------------------------------------------------
# ThalesOps Agent Installer
# Usage:
#   curl -fsSL https://staging-agent.thalesops.com/install.sh | \
#     THALES_TOKEN=tc_agt_xxx \
#     THALES_SERVER_ID=uuid \
#     THALES_BACKEND_URL=https://staging.thalesops.com bash
# -----------------------------------------------------------------------------

AGENT_BASE_URL="https://staging-agent.thalesops.com"
INSTALL_BIN="/usr/local/bin/thalesops-agent"
ENV_DIR="/etc/thalesops"
SERVICE_NAME="thalesops-agent"

# ── Colours ──────────────────────────────────────────────────────────────────
GREEN='\033[0;32m'
RED='\033[0;31m'
YELLOW='\033[1;33m'
NC='\033[0m'

info()    { echo -e "${GREEN}[ThalesOps]${NC} $1"; }
warn()    { echo -e "${YELLOW}[ThalesOps]${NC} $1"; }
error()   { echo -e "${RED}[ThalesOps] ERROR:${NC} $1"; exit 1; }

# ── Validation ────────────────────────────────────────────────────────────────
[ -z "${THALES_TOKEN:-}"     ] && error "THALES_TOKEN is required"
[ -z "${THALES_SERVER_ID:-}" ] && error "THALES_SERVER_ID is required"
[ "$EUID" -ne 0              ] && error "Please run as root (sudo bash or pipe to sudo bash)"

BACKEND_URL="${THALES_BACKEND_URL:-https://thalesops.com}"

info "Installing ThalesOps Agent..."
info "Backend  : $BACKEND_URL"
info "Server ID: $THALES_SERVER_ID"

# ── Detect architecture ───────────────────────────────────────────────────────
ARCH=$(uname -m)
case "$ARCH" in
  x86_64)  BINARY="thalesops-agent-linux-amd64" ;;
  aarch64) BINARY="thalesops-agent-linux-arm64" ;;
  *)        error "Unsupported architecture: $ARCH" ;;
esac

# ── Install deploy prerequisites (Docker + Nixpacks) ──────────────────────────
# These make the server able to actually build & run apps. Installed once here so
# the agent is deploy-ready from the moment it comes online — on any Linux distro.

install_docker() {
  if command -v docker >/dev/null 2>&1; then
    info "Docker already installed ($(docker --version 2>/dev/null))."
    return
  fi
  info "Installing Docker (this can take a minute)..."
  # Docker's official convenience script supports Ubuntu, Debian, CentOS, Rocky, etc.
  if curl -fsSL https://get.docker.com | sh; then
    systemctl enable docker >/dev/null 2>&1 || true
    systemctl start docker  >/dev/null 2>&1 || true
    info "Docker installed."
  else
    warn "Automatic Docker install failed. Install Docker manually, then re-run this script."
  fi
}

install_nixpacks() {
  if command -v nixpacks >/dev/null 2>&1; then
    info "Nixpacks already installed ($(nixpacks --version 2>/dev/null))."
    return
  fi
  info "Installing Nixpacks..."
  if curl -fsSL https://nixpacks.com/install.sh | bash; then
    info "Nixpacks installed."
  else
    warn "Automatic Nixpacks install failed. Install it manually, then re-run this script."
  fi
}

# Decide the proxy backend by respecting whatever already runs on this server.
# Clean server → install Caddy (automatic HTTPS). nginx already here → leave it
# in place and set up certbot so the agent can route through nginx instead.
setup_proxy() {
  local on443=""
  if command -v ss >/dev/null 2>&1; then
    on443=$(ss -ltnp 2>/dev/null | grep -oE 'users:\(\("[a-z]+' | grep -oE '[a-z]+$' | while read -r p; do
      ss -ltnp 2>/dev/null | grep ":443 " | grep -q "\"$p\"" && echo "$p"; done | head -1)
  fi

  if command -v nginx >/dev/null 2>&1 || [ "$on443" = "nginx" ]; then
    info "nginx detected — ThalesOps will route through your existing nginx (not replacing it)."
    if ! command -v certbot >/dev/null 2>&1; then
      info "Installing certbot + nginx plugin for HTTPS..."
      apt-get install -y certbot python3-certbot-nginx >/dev/null 2>&1 \
        || warn "certbot install failed — apps will be served over HTTP until it's installed."
    fi
    mkdir -p /etc/nginx/conf.d
    return
  fi

  install_caddy
}

install_caddy() {
  if command -v caddy >/dev/null 2>&1; then
    info "Caddy already installed ($(caddy version 2>/dev/null | head -1))."
  else
    info "No reverse proxy found — installing Caddy (automatic HTTPS)..."
    if apt-get install -y debian-keyring debian-archive-keyring apt-transport-https curl >/dev/null 2>&1 \
       && curl -1sLf 'https://dl.cloudsmith.io/public/caddy/stable/gpg.key' | gpg --dearmor -o /usr/share/keyrings/caddy-stable-archive-keyring.gpg \
       && curl -1sLf 'https://dl.cloudsmith.io/public/caddy/stable/debian.deb.txt' > /etc/apt/sources.list.d/caddy-stable.list \
       && apt-get update >/dev/null 2>&1 && apt-get install -y caddy >/dev/null 2>&1; then
      info "Caddy installed."
    else
      warn "Automatic Caddy install failed. Apps will be reachable on their host port until a proxy is set up."
      return
    fi
  fi

  # Base Caddyfile that imports one snippet per app (the agent writes those).
  mkdir -p /etc/thalesops/caddy
  if ! grep -q "/etc/thalesops/caddy" /etc/caddy/Caddyfile 2>/dev/null; then
    cat > /etc/caddy/Caddyfile <<'CADDY'
# Managed by ThalesOps — per-app routes live in /etc/thalesops/caddy/*.caddy
import /etc/thalesops/caddy/*.caddy
CADDY
  fi
  systemctl enable caddy >/dev/null 2>&1 || true
  systemctl reload caddy >/dev/null 2>&1 || systemctl restart caddy >/dev/null 2>&1 || true
}

open_firewall() {
  # Apps bind to localhost; only the proxy's web ports need to be public.
  if command -v ufw >/dev/null 2>&1; then
    info "Allowing web traffic (80/443) through the OS firewall (ufw)..."
    ufw allow 22/tcp  >/dev/null 2>&1 || true   # never lock out SSH
    ufw allow 80/tcp  >/dev/null 2>&1 || true
    ufw allow 443/tcp >/dev/null 2>&1 || true
  fi
  warn "If your cloud provider has its own firewall / security group, open ports 80 and 443 there too — that's the only place ThalesOps can't reach."
}

# These are optional prerequisites — a failure here (e.g. certbot conflict on an
# existing nginx server) must NOT abort the script before the agent is installed.
set +e
install_docker
install_nixpacks
setup_proxy
open_firewall
set -e

# ── Download binary ───────────────────────────────────────────────────────────
# Download to a sibling temp file, then rename over the target. Writing directly
# onto a RUNNING binary fails with ETXTBSY (curl error 23) — which is exactly
# what happens on a self-update. A rename in the same directory is atomic and
# allowed; the running agent keeps its old inode until systemd restarts it.
info "Downloading agent binary ($ARCH)..."
TMP_BIN="${INSTALL_BIN}.new"
curl -fsSL "$AGENT_BASE_URL/releases/$BINARY" -o "$TMP_BIN"
chmod +x "$TMP_BIN"
mv -f "$TMP_BIN" "$INSTALL_BIN"
info "Binary installed at $INSTALL_BIN"

# ── Write env file ────────────────────────────────────────────────────────────
mkdir -p "$ENV_DIR"
cat > "$ENV_DIR/.env" <<EOF
BACKEND_URL=$BACKEND_URL
SERVER_ID=$THALES_SERVER_ID
AGENT_TOKEN=$THALES_TOKEN
HEARTBEAT_INTERVAL=60
COMMAND_TIMEOUT=300
DEPLOY_TIMEOUT=1200
EOF
chmod 600 "$ENV_DIR/.env"
info "Config written to $ENV_DIR/.env"

# ── Create systemd service ────────────────────────────────────────────────────
cat > "/etc/systemd/system/$SERVICE_NAME.service" <<EOF
[Unit]
Description=ThalesOps Agent
After=network-online.target
Wants=network-online.target

[Service]
Type=simple
User=root
WorkingDirectory=$ENV_DIR
EnvironmentFile=$ENV_DIR/.env
ExecStart=$INSTALL_BIN
Restart=always
RestartSec=10
StandardOutput=journal
StandardError=journal

[Install]
WantedBy=multi-user.target
EOF

# ── Enable and start ──────────────────────────────────────────────────────────
systemctl daemon-reload
systemctl enable "$SERVICE_NAME" --quiet
systemctl restart "$SERVICE_NAME"

# ── Done ──────────────────────────────────────────────────────────────────────
echo ""
info "Agent installed and started successfully!"
echo ""
echo "  Status : systemctl status $SERVICE_NAME"
echo "  Logs   : journalctl -u $SERVICE_NAME -f"
echo "  Stop   : systemctl stop $SERVICE_NAME"
echo ""
info "Your server should appear ONLINE in the ThalesOps dashboard within 60 seconds."
