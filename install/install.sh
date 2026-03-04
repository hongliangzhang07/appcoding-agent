#!/usr/bin/env bash
set -euo pipefail

APP_NAME="appcoding-agent"
SERVICE_LABEL="com.appcoding.agent"
SERVICE_UNIT="appcoding-agent.service"

REPO="${APP_AGENT_GH_REPO:-hongliangzhang07/appcoding-agent}"
VERSION="${APP_AGENT_VERSION:-latest}"
ARCHIVE_FILE="${APP_AGENT_ARCHIVE_FILE:-}"
ARCHIVE_URL="${APP_AGENT_ARCHIVE_URL:-}"
INSTALL_DIR="${APP_AGENT_INSTALL_DIR:-/usr/local/bin}"
STATE_DIR="${APP_AGENT_STATE_DIR:-$HOME/.appcoding-agent}"
CONFIG_DIR="${APP_AGENT_CONFIG_DIR:-$HOME/.config/appcoding-agent}"

BIN_PATH="${INSTALL_DIR}/${APP_NAME}"
CTL_PATH="${INSTALL_DIR}/${APP_NAME}ctl"
RUNNER_PATH="${STATE_DIR}/run.sh"
ENV_PATH="${CONFIG_DIR}/agent.env"
PLIST_PATH="$HOME/Library/LaunchAgents/${SERVICE_LABEL}.plist"
SYSTEMD_DIR="$HOME/.config/systemd/user"
SYSTEMD_PATH="${SYSTEMD_DIR}/${SERVICE_UNIT}"
CLOUDFLARED_BIN=""

say() { printf '[install] %s\n' "$*"; }
warn() { printf '[install] WARN: %s\n' "$*" >&2; }
die() { printf '[install] ERROR: %s\n' "$*" >&2; exit 1; }

need_cmd() {
  command -v "$1" >/dev/null 2>&1 || die "missing required command: $1"
}

install_exec() {
  local src="$1" dst="$2"
  if mkdir -p "$(dirname "$dst")" 2>/dev/null && install -m 0755 "$src" "$dst" 2>/dev/null; then
    return
  fi
  if command -v sudo >/dev/null 2>&1; then
    sudo mkdir -p "$(dirname "$dst")"
    sudo install -m 0755 "$src" "$dst"
    return
  fi
  die "no permission to write $dst, and sudo is unavailable"
}

ensure_cloudflared() {
  if command -v cloudflared >/dev/null 2>&1; then
    CLOUDFLARED_BIN="$(command -v cloudflared)"
    say "cloudflared found: ${CLOUDFLARED_BIN}"
    return 0
  fi

  if [[ "${APP_AGENT_INSTALL_CLOUDFLARED:-1}" != "1" ]]; then
    warn "skip cloudflared install (APP_AGENT_INSTALL_CLOUDFLARED=0)"
    return 1
  fi

  say "cloudflared not found, trying to install..."
  if [[ "$os" == "darwin" ]]; then
    if command -v brew >/dev/null 2>&1; then
      brew install cloudflared || warn "brew install cloudflared failed"
    else
      warn "homebrew not found, cannot auto-install cloudflared on macOS"
    fi
  elif [[ "$os" == "linux" ]]; then
    if command -v apt-get >/dev/null 2>&1; then
      if command -v sudo >/dev/null 2>&1; then
        sudo apt-get update -y && sudo apt-get install -y cloudflared || warn "apt install cloudflared failed"
      else
        apt-get update -y && apt-get install -y cloudflared || warn "apt install cloudflared failed"
      fi
    elif command -v dnf >/dev/null 2>&1; then
      if command -v sudo >/dev/null 2>&1; then
        sudo dnf install -y cloudflared || warn "dnf install cloudflared failed"
      else
        dnf install -y cloudflared || warn "dnf install cloudflared failed"
      fi
    elif command -v yum >/dev/null 2>&1; then
      if command -v sudo >/dev/null 2>&1; then
        sudo yum install -y cloudflared || warn "yum install cloudflared failed"
      else
        yum install -y cloudflared || warn "yum install cloudflared failed"
      fi
    else
      warn "no supported package manager found for cloudflared auto-install"
    fi
  fi

  if command -v cloudflared >/dev/null 2>&1; then
    CLOUDFLARED_BIN="$(command -v cloudflared)"
    say "cloudflared installed: ${CLOUDFLARED_BIN}"
    return 0
  fi

  local fallback_url=""
  case "${os}:${arch}" in
    linux:amd64) fallback_url="https://github.com/cloudflare/cloudflared/releases/latest/download/cloudflared-linux-amd64" ;;
    linux:arm64) fallback_url="https://github.com/cloudflare/cloudflared/releases/latest/download/cloudflared-linux-arm64" ;;
    darwin:amd64) fallback_url="https://github.com/cloudflare/cloudflared/releases/latest/download/cloudflared-darwin-amd64.tgz" ;;
    darwin:arm64) fallback_url="https://github.com/cloudflare/cloudflared/releases/latest/download/cloudflared-darwin-arm64.tgz" ;;
  esac

  if [[ -n "$fallback_url" ]]; then
    local local_bin_dir="${STATE_DIR}/bin"
    local local_bin="${local_bin_dir}/cloudflared"
    mkdir -p "$local_bin_dir"
    say "trying direct cloudflared binary download..."
    if [[ "$fallback_url" == *.tgz ]]; then
      local cf_tgz="${local_bin_dir}/cloudflared.tgz"
      if curl -fsSL "$fallback_url" -o "$cf_tgz"; then
        tar -xzf "$cf_tgz" -C "$local_bin_dir" >/dev/null 2>&1 || true
        rm -f "$cf_tgz"
      fi
      if [[ -x "${local_bin_dir}/cloudflared" ]]; then
        CLOUDFLARED_BIN="${local_bin_dir}/cloudflared"
      fi
    else
      if curl -fsSL "$fallback_url" -o "$local_bin"; then
        chmod +x "$local_bin" || true
      fi
      if [[ -x "$local_bin" ]]; then
        CLOUDFLARED_BIN="$local_bin"
      fi
    fi
  fi

  if [[ -n "$CLOUDFLARED_BIN" && -x "$CLOUDFLARED_BIN" ]]; then
    say "cloudflared installed (local): ${CLOUDFLARED_BIN}"
    return 0
  fi

  warn "cloudflared is still missing; install manually: https://developers.cloudflare.com/cloudflare-one/connections/connect-networks/downloads/"
  return 1
}

set_env_kv() {
  local key="$1"
  local value="$2"
  local tmp_env="${ENV_PATH}.tmp"
  awk -v k="$key" -v v="$value" '
    BEGIN { done=0 }
    $0 ~ ("^" k "=") { print k "=" v; done=1; next }
    { print }
    END { if (!done) print k "=" v }
  ' "$ENV_PATH" >"$tmp_env"
  mv "$tmp_env" "$ENV_PATH"
}

os="$(uname -s | tr '[:upper:]' '[:lower:]')"
arch_raw="$(uname -m)"
case "$os" in
  darwin|linux) ;;
  *) die "unsupported OS: $os" ;;
esac

case "$arch_raw" in
  x86_64|amd64) arch="amd64" ;;
  arm64|aarch64) arch="arm64" ;;
  *) die "unsupported arch: $arch_raw" ;;
esac

need_cmd curl
need_cmd tar
need_cmd install

asset="${APP_NAME}_${os}_${arch}.tar.gz"
if [[ "$VERSION" == "latest" ]]; then
  base_url="https://github.com/${REPO}/releases/latest/download"
else
  base_url="https://github.com/${REPO}/releases/download/${VERSION}"
fi

tmp_dir="$(mktemp -d)"
trap 'rm -rf "$tmp_dir"' EXIT

if [[ -n "$ARCHIVE_FILE" ]]; then
  [[ -f "$ARCHIVE_FILE" ]] || die "APP_AGENT_ARCHIVE_FILE not found: $ARCHIVE_FILE"
  cp "$ARCHIVE_FILE" "${tmp_dir}/${asset}"
  say "using local archive: $ARCHIVE_FILE"
elif [[ -n "$ARCHIVE_URL" ]]; then
  say "downloading archive from APP_AGENT_ARCHIVE_URL"
  curl -fsSL "$ARCHIVE_URL" -o "${tmp_dir}/${asset}" || die "failed to download archive from APP_AGENT_ARCHIVE_URL"
else
  say "downloading ${asset}"
  curl -fsSL "${base_url}/${asset}" -o "${tmp_dir}/${asset}" || die "failed to download ${asset} from ${base_url}"
  curl -fsSL "${base_url}/checksums.txt" -o "${tmp_dir}/checksums.txt" || true
fi

if [[ -f "${tmp_dir}/checksums.txt" ]]; then
  if command -v shasum >/dev/null 2>&1; then
    (cd "$tmp_dir" && shasum -a 256 -c --ignore-missing checksums.txt) || die "checksum verification failed"
  elif command -v sha256sum >/dev/null 2>&1; then
    (cd "$tmp_dir" && sha256sum -c --ignore-missing checksums.txt) || die "checksum verification failed"
  else
    warn "no checksum utility found (shasum/sha256sum), skip verification"
  fi
fi

tar -xzf "${tmp_dir}/${asset}" -C "$tmp_dir"
binary_src="$(find "$tmp_dir" -type f -name "$APP_NAME" | head -n1 || true)"
[[ -n "$binary_src" ]] || die "binary ${APP_NAME} not found in archive"

install_exec "$binary_src" "$BIN_PATH"
say "installed binary: $BIN_PATH"

mkdir -p "$STATE_DIR" "$CONFIG_DIR"
ensure_cloudflared || true

if [[ ! -f "$ENV_PATH" ]]; then
  cat >"$ENV_PATH" <<'CONF'
AGENT_ADDR=0.0.0.0:8088
AGENT_SKIP_AUTH=0
AGENT_ENFORCE_ACCESS_TOKEN=1
AGENT_ACCESS_TOKEN_TTL_SEC=1800
AGENT_PAIR_TTL_SEC=86400
AGENT_QR_LOG=1
AGENT_STATE_PATH="$HOME/.appcoding-agent/state.json"
AGENT_QR_PATH="$HOME/.appcoding-agent/pairing-qr.png"
AGENT_TUNNEL_AUTOSTART=1
AGENT_TUNNEL_BIN=cloudflared
AGENT_TUNNEL_TARGET_URL=http://127.0.0.1:8088
CONF
  say "created env config: $ENV_PATH"
else
  say "reuse existing env config: $ENV_PATH"
fi

tmp_env="${ENV_PATH}.tmp"
awk '
  BEGIN { done=0 }
  /^AGENT_QR_LOG=/ { print "AGENT_QR_LOG=1"; done=1; next }
  { print }
  END { if (!done) print "AGENT_QR_LOG=1" }
' "$ENV_PATH" >"$tmp_env"
mv "$tmp_env" "$ENV_PATH"
say "set AGENT_QR_LOG=1 in $ENV_PATH"

if [[ -n "$CLOUDFLARED_BIN" && -x "$CLOUDFLARED_BIN" ]]; then
  set_env_kv "AGENT_TUNNEL_BIN" "$CLOUDFLARED_BIN"
  say "set AGENT_TUNNEL_BIN=${CLOUDFLARED_BIN} in $ENV_PATH"
fi

cat >"$RUNNER_PATH" <<RUN
#!/usr/bin/env bash
set -euo pipefail
if [[ -f "$ENV_PATH" ]]; then
  set -a
  source "$ENV_PATH"
  set +a
fi
exec "$BIN_PATH"
RUN
chmod +x "$RUNNER_PATH"

cat >"${tmp_dir}/${APP_NAME}ctl" <<CTL
#!/usr/bin/env bash
set -euo pipefail

SERVICE_LABEL="$SERVICE_LABEL"
SERVICE_UNIT="$SERVICE_UNIT"
ENV_PATH="$ENV_PATH"
STATE_DIR="$STATE_DIR"
PLIST_PATH="$PLIST_PATH"
SYSTEMD_PATH="$SYSTEMD_PATH"

detect_http_addr() {
  local raw="127.0.0.1:8088"
  if [[ -f "\$ENV_PATH" ]]; then
    raw=\$(awk -F= '/^AGENT_ADDR=/{print \$2}' "\$ENV_PATH" | tail -n1 | tr -d '"' | tr -d "'" | xargs || true)
  fi
  if [[ -z "\$raw" ]]; then
    raw="127.0.0.1:8088"
  fi
  if [[ "\$raw" == :* ]]; then
    echo "127.0.0.1\${raw}"
    return
  fi
  if [[ "\$raw" == 0.0.0.0:* ]]; then
    echo "127.0.0.1:\${raw#0.0.0.0:}"
    return
  fi
  echo "\$raw"
}

os=\$(uname -s | tr '[:upper:]' '[:lower:]')
cmd="\${1:-status}"

start_macos() {
  launchctl bootout "gui/\$(id -u)" "\$PLIST_PATH" >/dev/null 2>&1 || true
  launchctl bootstrap "gui/\$(id -u)" "\$PLIST_PATH"
  launchctl enable "gui/\$(id -u)/\${SERVICE_LABEL}" >/dev/null 2>&1 || true
  launchctl kickstart -k "gui/\$(id -u)/\${SERVICE_LABEL}" >/dev/null 2>&1 || true
}

stop_macos() {
  launchctl bootout "gui/\$(id -u)" "\$PLIST_PATH" >/dev/null 2>&1 || true
}

status_macos() {
  launchctl print "gui/\$(id -u)/\${SERVICE_LABEL}" >/dev/null 2>&1 && echo "running" || echo "stopped"
}

start_linux() {
  systemctl --user daemon-reload
  systemctl --user enable --now "\$SERVICE_UNIT"
}

stop_linux() {
  systemctl --user disable --now "\$SERVICE_UNIT" >/dev/null 2>&1 || systemctl --user stop "\$SERVICE_UNIT" >/dev/null 2>&1 || true
}

status_linux() {
  systemctl --user is-active "\$SERVICE_UNIT" >/dev/null 2>&1 && echo "running" || echo "stopped"
}

case "\$cmd" in
  start)
    if [[ "\$os" == "darwin" ]]; then start_macos; else start_linux; fi
    ;;
  stop)
    if [[ "\$os" == "darwin" ]]; then stop_macos; else stop_linux; fi
    ;;
  restart)
    if [[ "\$os" == "darwin" ]]; then stop_macos; start_macos; else stop_linux; start_linux; fi
    ;;
  status)
    if [[ "\$os" == "darwin" ]]; then status_macos; else status_linux; fi
    ;;
  logs)
    tail -n 200 -f "\$STATE_DIR/agent.out.log" "\$STATE_DIR/agent.err.log"
    ;;
  health)
    curl -fsSL "http://\$(detect_http_addr)/health"
    ;;
  pairing)
    curl -fsSL "http://\$(detect_http_addr)/pairing"
    ;;
  tunnel-start)
    curl -fsSL -X POST "http://\$(detect_http_addr)/tunnel/start"
    ;;
  tunnel-stop)
    curl -fsSL -X POST "http://\$(detect_http_addr)/tunnel/stop"
    ;;
  tunnel-status)
    curl -fsSL "http://\$(detect_http_addr)/tunnel/status"
    ;;
  qr-path)
    if [[ -f "\$ENV_PATH" ]]; then
      awk -F= '/^AGENT_QR_PATH=/{print \$2}' "\$ENV_PATH" | tail -n1
    fi
    ;;
  *)
    echo "usage: ${APP_NAME}ctl {start|stop|restart|status|logs|health|pairing|tunnel-start|tunnel-stop|tunnel-status|qr-path}"
    exit 1
    ;;
esac
CTL
install_exec "${tmp_dir}/${APP_NAME}ctl" "$CTL_PATH"
say "installed control command: $CTL_PATH"

if [[ "$os" == "darwin" ]]; then
  mkdir -p "$HOME/Library/LaunchAgents"
  cat >"$PLIST_PATH" <<PLIST
<?xml version="1.0" encoding="UTF-8"?>
<!DOCTYPE plist PUBLIC "-//Apple//DTD PLIST 1.0//EN" "http://www.apple.com/DTDs/PropertyList-1.0.dtd">
<plist version="1.0">
<dict>
  <key>Label</key>
  <string>${SERVICE_LABEL}</string>
  <key>ProgramArguments</key>
  <array>
    <string>/bin/bash</string>
    <string>-lc</string>
    <string>${RUNNER_PATH}</string>
  </array>
  <key>RunAtLoad</key>
  <true/>
  <key>KeepAlive</key>
  <true/>
  <key>StandardOutPath</key>
  <string>${STATE_DIR}/agent.out.log</string>
  <key>StandardErrorPath</key>
  <string>${STATE_DIR}/agent.err.log</string>
</dict>
</plist>
PLIST
  "$CTL_PATH" restart || warn "failed to auto-start launchd service, run: ${APP_NAME}ctl start"
elif [[ "$os" == "linux" ]]; then
  mkdir -p "$SYSTEMD_DIR"
  cat >"$SYSTEMD_PATH" <<UNIT
[Unit]
Description=appCoding desktop agent
After=network-online.target

[Service]
Type=simple
ExecStart=${RUNNER_PATH}
Restart=always
RestartSec=2
StandardOutput=append:${STATE_DIR}/agent.out.log
StandardError=append:${STATE_DIR}/agent.err.log

[Install]
WantedBy=default.target
UNIT
  if command -v systemctl >/dev/null 2>&1; then
    "$CTL_PATH" restart || warn "failed to auto-start systemd user service, run: ${APP_NAME}ctl start"
  else
    warn "systemctl not found, start manually: ${RUNNER_PATH}"
  fi
fi

if [[ -n "$CLOUDFLARED_BIN" && -x "$CLOUDFLARED_BIN" ]] || command -v cloudflared >/dev/null 2>&1; then
  "$CTL_PATH" tunnel-start >/dev/null 2>&1 || warn "failed to auto-start tunnel"
  sleep 1
else
  warn "cloudflared is not installed, tunnel-start will fail until installed"
fi

say "安装完成"
echo
echo "请执行以下命令："
echo "  启动服务：${APP_NAME}ctl start"
echo "  查看状态：${APP_NAME}ctl status"
echo "  生成二维码（配对信息）：${APP_NAME}ctl pairing"
echo "  查看日志：${APP_NAME}ctl logs"
echo "  查看隧道状态：${APP_NAME}ctl tunnel-status"

echo
echo "安装后自动检查："
"$CTL_PATH" status || true

http_addr="127.0.0.1:8088"
if [[ -f "$ENV_PATH" ]]; then
  raw_addr="$(awk -F= '/^AGENT_ADDR=/{print $2}' "$ENV_PATH" | tail -n1 | tr -d '"' | tr -d "'" | xargs || true)"
  if [[ -n "$raw_addr" ]]; then
    if [[ "$raw_addr" == :* ]]; then
      http_addr="127.0.0.1${raw_addr}"
    elif [[ "$raw_addr" == 0.0.0.0:* ]]; then
      http_addr="127.0.0.1:${raw_addr#0.0.0.0:}"
    else
      http_addr="$raw_addr"
    fi
  fi
fi

ready=0
for ((i=0; i<20; i++)); do
  if curl -fsSL "http://${http_addr}/health" >/dev/null 2>&1; then
    ready=1
    break
  fi
  sleep 1
done

if [[ "$ready" == "1" ]]; then
  "$CTL_PATH" tunnel-status || true
  "$CTL_PATH" pairing || true
else
  warn "服务尚未就绪: http://${http_addr}"
  warn "可执行: ${APP_NAME}ctl logs"
fi
