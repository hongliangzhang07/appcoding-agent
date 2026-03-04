#!/usr/bin/env bash
set -euo pipefail

APP_NAME="appcoding-agent"
SERVICE_LABEL="com.appcoding.agent"
SERVICE_UNIT="appcoding-agent.service"

INSTALL_DIR="${APP_AGENT_INSTALL_DIR:-/usr/local/bin}"
STATE_DIR="${APP_AGENT_STATE_DIR:-$HOME/.appcoding-agent}"
CONFIG_DIR="${APP_AGENT_CONFIG_DIR:-$HOME/.config/appcoding-agent}"

BIN_PATH="${INSTALL_DIR}/${APP_NAME}"
CTL_PATH="${INSTALL_DIR}/${APP_NAME}ctl"
PLIST_PATH="$HOME/Library/LaunchAgents/${SERVICE_LABEL}.plist"
SYSTEMD_PATH="$HOME/.config/systemd/user/${SERVICE_UNIT}"

say() { printf '[uninstall] %s\n' "$*"; }
warn() { printf '[uninstall] WARN: %s\n' "$*" >&2; }

remove_path() {
  local target="$1"
  [[ -e "$target" ]] || return 0
  if rm -f "$target" 2>/dev/null; then
    return 0
  fi
  if command -v sudo >/dev/null 2>&1; then
    sudo rm -f "$target"
    return 0
  fi
  warn "cannot remove $target"
}

os="$(uname -s | tr '[:upper:]' '[:lower:]')"

if [[ "$os" == "darwin" ]]; then
  launchctl bootout "gui/$(id -u)" "$PLIST_PATH" >/dev/null 2>&1 || true
  remove_path "$PLIST_PATH"
elif [[ "$os" == "linux" ]]; then
  if command -v systemctl >/dev/null 2>&1; then
    systemctl --user disable --now "$SERVICE_UNIT" >/dev/null 2>&1 || true
    systemctl --user daemon-reload >/dev/null 2>&1 || true
  fi
  remove_path "$SYSTEMD_PATH"
fi

remove_path "$CTL_PATH"
remove_path "$BIN_PATH"
remove_path "${STATE_DIR}/run.sh"

if [[ "${APP_AGENT_REMOVE_DATA:-0}" == "1" ]]; then
  rm -rf "$STATE_DIR" "$CONFIG_DIR"
  say "removed data dirs: $STATE_DIR $CONFIG_DIR"
else
  say "kept data dirs (set APP_AGENT_REMOVE_DATA=1 to remove):"
  say "  $STATE_DIR"
  say "  $CONFIG_DIR"
fi

say "done"
