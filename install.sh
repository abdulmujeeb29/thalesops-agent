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

install_docker
install_nixpacks

# ── Download binary ───────────────────────────────────────────────────────────
info "Downloading agent binary ($ARCH)..."
curl -fsSL "$AGENT_BASE_URL/releases/$BINARY" -o "$INSTALL_BIN"
chmod +x "$INSTALL_BIN"
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
