#!/usr/bin/env bash
set -euo pipefail

# Regenerates internal/proto/gen from the vendored protocol buffer definitions in
# proto/. The generated Go code is committed, so this script only needs to run when
# the definitions or the generator versions change.
#
# Pass --update to also re-download the vendored upstream definitions.

project_dir=$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd)

# Keep protoc_gen_go_version in sync with google.golang.org/protobuf in go.mod and
# grpc_version in sync with google.golang.org/grpc.
buf_version=v1.73.0
protoc_gen_go_version=v1.36.12
protoc_gen_go_grpc_version=v1.5.1

# bazelbuild/remote-apis tag v2.12.0.
remote_apis_revision=9e084d0e43e717128ee72b5be584a7ba33e8006b
remote_apis_files=(
  build/bazel/remote/asset/v1/remote_asset.proto
  build/bazel/remote/execution/v2/remote_execution.proto
  build/bazel/semver/semver.proto
)

tool_dir="$project_dir/.tools/bin"
mkdir -p "$tool_dir"

install_tool() {
  local binary=$1 module=$2
  if [[ ! -x "$tool_dir/$binary" ]]; then
    echo "installing $module" >&2
    GOBIN="$tool_dir" go install "$module"
  fi
}

install_tool buf "github.com/bufbuild/buf/cmd/buf@$buf_version"
install_tool protoc-gen-go "google.golang.org/protobuf/cmd/protoc-gen-go@$protoc_gen_go_version"
install_tool protoc-gen-go-grpc "google.golang.org/grpc/cmd/protoc-gen-go-grpc@$protoc_gen_go_grpc_version"

export PATH="$tool_dir:$PATH"

if [[ "${1:-}" == "--update" ]]; then
  for file in "${remote_apis_files[@]}"; do
    mkdir -p "$project_dir/proto/$(dirname "$file")"
    curl -fsSL -o "$project_dir/proto/$file" \
      "https://raw.githubusercontent.com/bazelbuild/remote-apis/$remote_apis_revision/$file"
  done
  (cd "$project_dir/proto" && buf dep update)
fi

cd "$project_dir"
rm -rf internal/proto/gen
buf generate
gofmt -l internal/proto/gen
