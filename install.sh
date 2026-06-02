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
