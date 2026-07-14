# Multica CLI (fork build with Cline)

Prebuilt binaries for this fork are published as **GitHub Release** assets
(not committed into git — each archive is ~5–6 MB compressed / ~15 MB binary).

## Version

See the latest `v*-cline*` release on the fork:

https://github.com/bamecho/multica/releases

Build metadata for a local `./scripts/package-cli.sh` run is written to
`BUILD_INFO.txt` next to the archives (gitignored with the archives).

## Install (Linux/macOS)

```bash
VERSION=0.9.0-cline.1
OS=$(uname -s | tr '[:upper:]' '[:lower:]')   # linux | darwin
ARCH=$(uname -m)
[[ "$ARCH" == "x86_64" ]] && ARCH=amd64
[[ "$ARCH" == "aarch64" ]] && ARCH=arm64

BASE="https://github.com/bamecho/multica/releases/download/v${VERSION}"
curl -fsSL "${BASE}/multica-cli-${VERSION}-${OS}-${ARCH}.tar.gz" -o /tmp/multica.tar.gz
tar -xzf /tmp/multica.tar.gz -C /tmp multica
sudo mv /tmp/multica /usr/local/bin/multica
multica version
```

Then point at your self-hosted server and start the daemon:

```bash
multica setup self-host   # or: multica login with MULTICA_SERVER_URL
multica daemon start
```

## Rebuild locally

```bash
./scripts/package-cli.sh 0.9.0-cline.1
# optional: publish to a fork release
gh release create "v0.9.0-cline.1" --repo bamecho/multica \
  --title "CLI v0.9.0-cline.1 (Cline)" \
  --notes "Fork CLI with Cline NDJSON adapter" \
  --target feat/cline-ndjson-adapter \
  releases/cli/multica-cli-* releases/cli/checksums.txt releases/cli/BUILD_INFO.txt
```

Do **not** install `multica-ai/tap/multica` for Cline — that is upstream without this adapter.
