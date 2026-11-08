#!/usr/bin/env bash
set -euo pipefail

project_dir=$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd)
go_version=$(tr -d '[:space:]' < "$project_dir/.go-version")
actual_version=$(go version | awk '{print $3}' | sed 's/^go//')
if [[ "$actual_version" != "$go_version" ]]; then
  echo "Go $go_version is required for reproducible release binaries; found $actual_version" >&2
  exit 1
fi

version=${VERSION:-dev}
ldflags="-s -w -buildid= -X main.version=$version"
for program in cache-server cache-optimiser; do
  for target in linux-amd64 linux-arm64 darwin-arm64; do
    if [[ "$program" == cache-optimiser && "$target" == darwin-arm64 ]]; then
      continue
    fi
    output="$project_dir/dist/$program-$target"
    CGO_ENABLED=0 GOOS="${target%-*}" GOARCH="${target#*-}" \
      go build -buildvcs=false -trimpath -ldflags "$ldflags" -o "$output" "./cmd/$program"
    chmod 0755 "$output"
  done
done
(
  cd "$project_dir/dist"
  # Sorted by byte value so the checksum file is reproducible whatever the
  # builder's locale.
  LC_ALL=C shasum -a 256 cache-optimiser-linux-* cache-server-darwin-arm64 cache-server-linux-* > SHA256SUMS
)
