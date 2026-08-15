#!/usr/bin/env bash
#
# Writes /etc/systemd/system/whatsapp.service if it does not already exist.
#
# An existing unit is never modified: once the file is on disk it is treated as
# the operator's, since it may carry hand-tuned limits, a different user or
# extra hardening. Use --force only when you intend to discard those edits; the
# current file is backed up beside it first.
#
# Usage:
#   sudo ./deploy/install-service.sh [--force]
#
# Environment overrides:
#   SERVICE_NAME  unit name without .service   (default: whatsapp)
#   APP_DIR       working directory            (default: this repo)
#   RUN_USER      user to run as               (default: current owner of APP_DIR)
#   GO_BIN        go binary to run with        (default: `command -v go`)

set -euo pipefail

SERVICE_NAME="${SERVICE_NAME:-whatsapp}"
UNIT_PATH="/etc/systemd/system/${SERVICE_NAME}.service"
APP_DIR="${APP_DIR:-$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd)}"

FORCE=0
[[ "${1:-}" == "--force" ]] && FORCE=1

# --- the guard the whole script exists for ----------------------------------
if [[ -e "$UNIT_PATH" && $FORCE -eq 0 ]]; then
    echo "✓ $UNIT_PATH already exists — leaving it untouched."
    echo "  Review it with:  systemctl cat ${SERVICE_NAME}.service"
    echo "  Replace it with: sudo $0 --force"
    exit 0
fi

if [[ $EUID -ne 0 ]]; then
    echo "This writes to /etc/systemd/system; re-run with sudo." >&2
    exit 1
fi

RUN_USER="${RUN_USER:-$(stat -c '%U' "$APP_DIR" 2>/dev/null || echo root)}"
GO_BIN="${GO_BIN:-$(command -v go || echo /usr/local/go/bin/go)}"

if [[ ! -x "$GO_BIN" ]]; then
    echo "go binary not found at '$GO_BIN'; set GO_BIN=/path/to/go" >&2
    exit 1
fi

if [[ ! -f "$APP_DIR/.env" ]]; then
    echo "warning: $APP_DIR/.env does not exist yet — the service will fail to" >&2
    echo "         start until you create it (cp .env.example .env)." >&2
fi

if [[ -e "$UNIT_PATH" ]]; then
    BACKUP="${UNIT_PATH}.bak.$(date +%Y%m%d%H%M%S)"
    cp -p "$UNIT_PATH" "$BACKUP"
    echo "! replacing existing unit; previous version saved to $BACKUP"
fi

cat > "$UNIT_PATH" <<UNIT
[Unit]
Description=WhatsApp campaign automation platform
Documentation=file://${APP_DIR}/README.md
After=network-online.target
Wants=network-online.target

# A crash loop should back off rather than hammer the Green API on every
# retry. These are [Unit] options in systemd 229 and later.
StartLimitIntervalSec=300
StartLimitBurst=10

[Service]
Type=simple
User=${RUN_USER}
WorkingDirectory=${APP_DIR}

# Configuration lives in .env, which is not in version control.
EnvironmentFile=${APP_DIR}/.env
Environment=HOME=${APP_DIR}

ExecStart=${GO_BIN} run ./cmd/server

Restart=always
RestartSec=5s

# SQLite writes need the database file to survive a restart, so give the
# service time to finish an in-flight commit before it is killed.
TimeoutStopSec=30
KillSignal=SIGINT

StandardOutput=journal
StandardError=journal
SyslogIdentifier=${SERVICE_NAME}

# --- hardening --------------------------------------------------------------
# The service only needs to write its own directory: the SQLite file, the
# uploaded media and the Go build cache.
#
# ProtectHome is deliberately not set: deployments commonly live under /home,
# and locking that down would block the database writes this list allows.
NoNewPrivileges=true
PrivateTmp=true
ProtectSystem=full
ReadWritePaths=${APP_DIR}

[Install]
WantedBy=multi-user.target
UNIT

chmod 0644 "$UNIT_PATH"
echo "✓ wrote $UNIT_PATH"

systemctl daemon-reload
systemctl enable "${SERVICE_NAME}.service" >/dev/null
echo "✓ enabled ${SERVICE_NAME}.service (starts on boot)"
echo
echo "Start it with:   sudo systemctl start ${SERVICE_NAME}.service"
echo "Check it with:   systemctl status ${SERVICE_NAME}.service"
echo "Follow logs:     journalctl -u ${SERVICE_NAME}.service -f"
