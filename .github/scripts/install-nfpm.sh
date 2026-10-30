#!/usr/bin/env bash
set -euo pipefail
target=${1:?destination directory required}
mkdir -p "$target"
archive="$target/nfpm.tar.gz"
curl --fail --location --retry 3 --connect-timeout 15 --max-time 120 \
    https://github.com/goreleaser/nfpm/releases/download/v2.48.0/nfpm_2.48.0_Linux_x86_64.tar.gz \
    --output "$archive"
printf '%s  %s\n' 6d7f9a28b2f397934403950fcbba83f598b08d02edbe79a0815b3b4897341750 "$archive" | sha256sum --check
tar -xzf "$archive" -C "$target" nfpm
"$target/nfpm" --version
if [ -n "${GITHUB_PATH:-}" ]; then
    printf '%s\n' "$target" >> "$GITHUB_PATH"
fi
