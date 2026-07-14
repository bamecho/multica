#!/usr/bin/env bash
# Build multica CLI archives for fork distribution (Cline-enabled branch).
# Usage: ./scripts/package-cli.sh [version]
# Example: ./scripts/package-cli.sh 0.9.0-cline.1
set -euo pipefail

ROOT="$(cd "$(dirname "$0")/.." && pwd)"
VERSION="${1:-0.9.0-cline.1}"
COMMIT="$(git -C "$ROOT" rev-parse --short HEAD 2>/dev/null || echo unknown)"
DATE="$(date -u '+%Y-%m-%dT%H:%M:%SZ')"
OUT="${ROOT}/releases/cli"

mkdir -p "$OUT"
rm -f "$OUT"/multica-cli-* "$OUT"/checksums.txt "$OUT"/BUILD_INFO.txt

build_one() {
  local goos=$1 goarch=$2
  local name="multica-cli-${VERSION}-${goos}-${goarch}"
  local ext=""
  [[ "$goos" == "windows" ]] && ext=".exe"
  echo "Building $name ..."
  (
    cd "$ROOT/server"
    CGO_ENABLED=0 GOOS="$goos" GOARCH="$goarch" go build \
      -ldflags "-s -w -X main.version=${VERSION} -X main.commit=${COMMIT} -X main.date=${DATE}" \
      -o "${OUT}/multica${ext}" \
      ./cmd/multica
  )
  if [[ "$goos" == "windows" ]]; then
    (cd "$OUT" && zip -q "${name}.zip" "multica.exe" && rm -f multica.exe)
  else
    (cd "$OUT" && tar -czf "${name}.tar.gz" multica && rm -f multica)
  fi
}

build_one linux amd64
build_one linux arm64
build_one darwin amd64
build_one darwin arm64
build_one windows amd64
build_one windows arm64

if command -v sha256sum >/dev/null 2>&1; then
  (cd "$OUT" && sha256sum multica-cli-* > checksums.txt)
else
  (cd "$OUT" && shasum -a 256 multica-cli-* > checksums.txt)
fi

{
  echo "VERSION=${VERSION}"
  echo "COMMIT=${COMMIT}"
  echo "DATE=${DATE}"
  echo "BRANCH=$(git -C "$ROOT" rev-parse --abbrev-ref HEAD 2>/dev/null || echo unknown)"
} > "$OUT/BUILD_INFO.txt"

echo
echo "Artifacts in $OUT:"
ls -lh "$OUT"
echo
du -sh "$OUT"
