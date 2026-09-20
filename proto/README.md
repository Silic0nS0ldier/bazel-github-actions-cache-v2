# Vendored protocol buffer definitions

The files under `build/` are copied verbatim from
[bazelbuild/remote-apis](https://github.com/bazelbuild/remote-apis) at revision
`9e084d0e43e717128ee72b5be584a7ba33e8006b` (tag `v2.12.0`). Do not edit them by
hand; re-run `scripts/generate-proto.sh --update` to refresh both the vendored
sources and the generated Go code.

Generated Go code lives in `internal/proto/gen` and is committed so that the
normal `go build` / `go test` flow needs neither `buf` nor `protoc`.
