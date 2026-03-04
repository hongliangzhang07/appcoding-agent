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

bundle_name="${APP_NAME}_offline_bundle"
bundle_dir="${OUT_DIR}/${bundle_name}"
bundle_archive="${OUT_DIR}/${bundle_name}.tar.gz"

rm -rf "$bundle_dir" "$bundle_archive"
mkdir -p "${bundle_dir}/install" "${bundle_dir}/packages"

cp "${ROOT_DIR}/install/install.sh" "${bundle_dir}/install/install.sh"
cp "${ROOT_DIR}/install/uninstall.sh" "${bundle_dir}/install/uninstall.sh"
cp "${OUT_DIR}"/${APP_NAME}_*.tar.gz "${bundle_dir}/packages/"
cp "${OUT_DIR}/checksums.txt" "${bundle_dir}/checksums.txt"

cat >"${bundle_dir}/install-offline.sh" <<'EOF'
#!/usr/bin/env bash
set -euo pipefail

base_dir="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
os="$(uname -s | tr '[:upper:]' '[:lower:]')"
arch_raw="$(uname -m)"

case "$arch_raw" in
  x86_64|amd64) arch="amd64" ;;
  arm64|aarch64) arch="arm64" ;;
  *) echo "[offline-install] unsupported arch: $arch_raw" >&2; exit 1 ;;
esac

archive="${base_dir}/packages/appcoding-agent_${os}_${arch}.tar.gz"
if [[ ! -f "$archive" ]]; then
  echo "[offline-install] package not found: $archive" >&2
  exit 1
fi

echo "[offline-install] using local package: $archive"
APP_AGENT_ARCHIVE_FILE="$archive" /bin/bash "${base_dir}/install/install.sh"
EOF
chmod +x "${bundle_dir}/install-offline.sh"

cat >"${bundle_dir}/README.md" <<'EOF'
# appcoding-agent offline bundle

Run:

```bash
bash ./install-offline.sh
```

Manual (if needed):

```bash
APP_AGENT_ARCHIVE_FILE=./packages/appcoding-agent_linux_amd64.tar.gz \
bash ./install/install.sh
```
EOF

tar -C "$bundle_dir" -czf "$bundle_archive" .
if command -v shasum >/dev/null 2>&1; then
  (cd "$OUT_DIR" && shasum -a 256 "$(basename "$bundle_archive")") >> "${OUT_DIR}/checksums.txt"
else
  (cd "$OUT_DIR" && sha256sum "$(basename "$bundle_archive")") >> "${OUT_DIR}/checksums.txt"
fi
rm -rf "$bundle_dir"

echo "[release] done"
ls -lh "$OUT_DIR"
