#!/usr/bin/env bash
set -euo pipefail

REPO="${APP_AGENT_GH_REPO:-hongliangzhang07/appcoding-agent}"
VERSION="${1:-}"

if [[ -z "$VERSION" ]]; then
  echo "usage: $0 <version-tag>"
  echo "example: $0 v0.1.0"
  exit 1
fi

if ! command -v gh >/dev/null 2>&1; then
  echo "error: gh CLI not found"
  exit 1
fi

ROOT_DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd)"
DIST_DIR="${ROOT_DIR}/dist"

APP_AGENT_VERSION="$VERSION" "${ROOT_DIR}/scripts/release-pack.sh" "$DIST_DIR"

gh release create "$VERSION" \
  "${DIST_DIR}/appcoding-agent_darwin_amd64.tar.gz" \
  "${DIST_DIR}/appcoding-agent_darwin_arm64.tar.gz" \
  "${DIST_DIR}/appcoding-agent_linux_amd64.tar.gz" \
  "${DIST_DIR}/appcoding-agent_linux_arm64.tar.gz" \
  "${DIST_DIR}/checksums.txt" \
  --repo "$REPO" \
  --title "$VERSION" \
  --notes "appCoding agent release $VERSION"

echo "release published: https://github.com/${REPO}/releases/tag/${VERSION}"
