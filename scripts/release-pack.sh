#!/usr/bin/env bash
set -euo pipefail

APP_NAME="appcoding-agent"
ROOT_DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd)"
OUT_DIR="${1:-${ROOT_DIR}/dist}"
VERSION="${APP_AGENT_VERSION:-dev}"

mkdir -p "$OUT_DIR"
rm -f "$OUT_DIR"/checksums.txt

platforms=(
  "darwin amd64"
  "darwin arm64"
  "linux amd64"
  "linux arm64"
)

echo "[release] version=${VERSION}"
echo "[release] out=${OUT_DIR}"

for item in "${platforms[@]}"; do
  os="${item%% *}"
  arch="${item##* }"
  name="${APP_NAME}_${os}_${arch}"
  work="${OUT_DIR}/${name}"
  archive="${OUT_DIR}/${name}.tar.gz"

  rm -rf "$work" "$archive"
  mkdir -p "$work"

  echo "[release] build ${os}/${arch}"
  (cd "$ROOT_DIR" && CGO_ENABLED=0 GOOS="$os" GOARCH="$arch" go build -ldflags "-s -w" -o "${work}/${APP_NAME}" .)
  cp "${ROOT_DIR}/README.md" "$work/README.md"

  tar -C "$work" -czf "$archive" .

  if command -v shasum >/dev/null 2>&1; then
    (cd "$OUT_DIR" && shasum -a 256 "$(basename "$archive")") >> "${OUT_DIR}/checksums.txt"
  else
    (cd "$OUT_DIR" && sha256sum "$(basename "$archive")") >> "${OUT_DIR}/checksums.txt"
  fi

  rm -rf "$work"
done

echo "[release] done"
ls -lh "$OUT_DIR"
