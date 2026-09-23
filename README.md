# Bazel GitHub Actions Cache v2

An experimental GitHub Action that starts a loopback-only Bazel HTTP remote
cache for the lifetime of a job and persists its objects in GitHub Actions
cache v2. It needs no external cache server, cloud account, or secret.

```text
Bazel ── HTTP ──> 127.0.0.1:<dynamic port> ── cache v2 ──> GitHub
```

The Go server runs as a detached process after the action's main step. The
action's post step shuts it down, reports statistics in the job summary, and
removes its private runner temporary directory.

> [!IMPORTANT]
> This project is experimental. It uses the cache-v2 runner service exposed to
> GitHub Actions, through `github.com/tonistiigi/go-actions-cache`. GitHub does
> not document that runner upload/download protocol as a stable public API.
> Publishing the usage record likewise speaks the runner's artifact service
> directly, the same undocumented API behind `actions/upload-artifact`.
> Pin this action to a full commit SHA and evaluate the limits below before
> making it a required CI dependency.

## Usage

```yaml
permissions:
  contents: read

steps:
  - uses: actions/checkout@<FULL_COMMIT_SHA>

  - id: bazel-cache
    uses: cre4ture/bazel-github-actions-cache-v2@<FULL_COMMIT_SHA>
    with:
      write: auto

  - name: Test
    env:
      CACHE_URL: ${{ steps.bazel-cache.outputs.grpc-url }}
      CACHE_WRITABLE: ${{ steps.bazel-cache.outputs.writable }}
    run: >
      bazel test //...
      --remote_cache="$CACHE_URL"
      --remote_upload_local_results="$CACHE_WRITABLE"
```

Swap `grpc-url` for `url` to use the HTTP endpoint instead; both serve the same
cache and either can be used without changing anything else.

`write: auto` is deliberately conservative: only a `push` to the repository's
default branch publishes entries. Pull requests are read-only by default, and
fork pull requests remain read-only even if `write: true` is requested. The
server validates but discards accidental PUTs in read-only mode so an optional
cache cannot fail the build.

For a trusted release or scheduled workflow, set `write: true`. For all
untrusted code, keep `write: false` and pass the emitted `writable` value to
Bazel.

### CARv2 packs and manifest DAGs

`storage-mode: packs` is the v0.3 storage format. Instead of creating one
GitHub cache entry for every CAS or Action Cache object, it writes many values
to a modest CARv2 archive (8 MiB by default) and publishes one immutable
DAG-CBOR manifest after that archive is available. A large output is placed in
its own archive. The result is normally two Actions-cache creations per pack
(one pack and one manifest), rather than one per Bazel object.

```yaml
permissions:
  contents: read
  actions: read # manifest discovery for storage-mode: packs

steps:
  - id: bazel-cache
    uses: cre4ture/bazel-github-actions-cache-v2@<FULL_COMMIT_SHA>
    with:
      write: auto
      key-prefix: ironmesh-bazel-car-v1
      storage-mode: packs
      pack-size-mb: "8"
```

The `github-token` input defaults to `${{ github.token }}` and is used only to
list immutable manifest keys through GitHub's documented Actions-cache REST
endpoint. It must have `actions: read`; no external service, secret, or write
token is needed. Keep the packed mode in a new `key-prefix`: it deliberately
does not reinterpret or mix v0.2 object keys.

Writers discover all current manifest heads at startup and make each new
manifest a child of every head they observed. Parallel writers can therefore
publish siblings without overwriting each other. Readers discover and merge all
heads. If two manifests contain different Action Results for the same action
digest, that digest is treated as a cache miss rather than picking one result.

Each manifest key names the pack it commits:

```text
<key-prefix>-car-pack-v1-<pack-sha256>
<key-prefix>-car-manifest-v2-<pack-sha256>-<manifest-cid>
```

Discovery lists both namespaces in full, newest first. Listing reads metadata
only, so it does not renew any entry's retention, and it is the download that is
worth limiting. Every mapping a manifest introduces lives in the pack its key
names, so a manifest missing from the pack listing is never downloaded. That
matters because reading a cache entry renews its retention: a small manifest
that is read on every run would otherwise outlive the large pack it describes
indefinitely, and GitHub would keep evicting packs instead. Unread manifests
simply expire.

`max-manifests` is applied after that filter, so the budget is spent on
manifests that can still produce a cache hit rather than on orphans.
`manifests_orphaned`, `manifests_skipped`, and `pack_loads_skipped` report this
in the final statistics.

Manifest discovery is eventually consistent by design: an unseen manifest is
only a temporary miss.

A `HEAD /cas/<digest>` is answered from the manifest view without restoring the
pack, so `--remote_download_minimal` builds do not pull archives they never
read. Because reading an entry is what resets its retention, a pack that only
ever answers presence checks would then be evicted while Bazel still depends on
it. The server therefore renews it explicitly: once per pack per job, batched
for `pack-renew-seconds` so a pack that gets downloaded in the meantime costs no
extra API call, and flushed at shutdown. A renewal resolves cache metadata
without transferring the pack, and is reported as `pack_renewals`.

Action results use the same presence check. `GET /ac/<digest>` restores only the
pack holding the result itself, then confirms every referenced blob is mapped to
a pack that still exists and that its recorded size matches the action result.
An action result is a cache miss whenever any output is unmapped, or lives in a
pack that eviction has removed. Serving it renews the retention of every pack in
its closure, so a result that Bazel keeps hitting keeps its outputs alive even
when they are never downloaded.

The tradeoff is deliberate: availability is judged from the manifest view and
the pack listing rather than by restoring every output. A pack that disappears
between discovery and a later fetch yields a miss on that fetch instead of
suppressing the action result up front.

The server flushes a pending pack when it reaches `pack-size-mb`, at least once
per `pack-flush-seconds`, and unconditionally during the action post step. It
uploads the CARv2 file first and the manifest second; the manifest is the
commit point. Thus cancellation, eviction, a corrupt pack, or a missing output
closure can only lose a cache hit and cannot supply incomplete build output.

### Pack compression

Nothing between this server and GitHub compresses anything: the cache-v2 client
uploads bytes verbatim and registers the raw length, which is what the 10 GB
repository quota is charged. Pack blocks are therefore compressed here, with
zstd, before upload.

Compression is per block rather than per pack, so the CARv2 index stays useful:
restoring a pack and serving one object decompresses that object alone instead
of expanding the whole archive. It also lets one pack mix compressed and
verbatim blocks, which matters when a build produces both text and
already-compressed artifacts such as OCI layers.

A block is stored verbatim when compression cannot pay for itself: below 256
bytes, or when the result is not at least 5% smaller. Objects larger than 1 MiB
are sampled first, so an incompressible one is rejected after a few hundred KB
rather than after compressing the whole thing. `compressed_blocks` and
`compression_saved_bytes` report the outcome.

A compressed block is addressed in the pack by the digest of its compressed
bytes, and the manifest records that block CID alongside the object's own CID.
A reader verifies the stored block against the block CID, decompresses it under
a size bound taken from the manifest, and then verifies the result against the
object CID, so a corrupt or truncated block is a cache miss rather than bad
build input. Disabling `pack-compression` stops writing compressed blocks but
never stops reading them.

### IronMesh example

The action can replace the external-cache URL and token in the Bazel job:

```yaml
- id: bazel-cache
  uses: cre4ture/bazel-github-actions-cache-v2@<FULL_COMMIT_SHA>
  with:
    write: auto
    key-prefix: ironmesh-bazel-v1
    fail-on-cache-error: "false"

- name: Bazel unit tests
  env:
    CACHE_URL: ${{ steps.bazel-cache.outputs.url }}
    CACHE_WRITABLE: ${{ steps.bazel-cache.outputs.writable }}
  run: |
    bazel test //:unit \
      --remote_cache="$CACHE_URL" \
      --remote_upload_local_results="$CACHE_WRITABLE"
```

Do not enable Bazel remote-cache compression with this release.

## Inputs and outputs

| Input | Default | Meaning |
|---|---:|---|
| `write` | `auto` | `auto`, `true`, or `false`; forks are always read-only |
| `fail-on-cache-error` | `false` | Strict mode; by default backend failures degrade to misses/soft upload success |
| `key-prefix` | `bazel-http-v1` | Immutable namespace; change to invalidate entries |
| `storage-mode` | `objects` | `objects` (v0.2) or `packs` (CARv2 and manifest DAG) |
| `pack-size-mb` | `8` | Target size for a CARv2 archive; only for `packs`, 1–32 MiB |
| `pack-flush-seconds` | `30` | Maximum local staging interval; only for `packs` |
| `pack-renew-seconds` | `15` | Batching delay for pack retention renewals; only for `packs` |
| `pack-compression` | `true` | Compress pack blocks with zstd; only for `packs` |
| `pack-compression-level` | `3` | zstd level for pack blocks, 1–19; only for `packs` |
| `max-manifests` | `2048` | Maximum manifests downloaded during discovery; only for `packs` |
| `github-token` | `${{ github.token }}` | `actions: read` token for packed-manifest discovery |
| `max-blob-size-mb` | `512` | Maximum spooled upload/download size |
| `max-concurrent-operations` | `4` | Backend-operation backpressure |
| `max-uploads-per-minute` | `180` | Evenly spaced uploads; must be below 200 |
| `backend-timeout-seconds` | `300` | Timeout for one GitHub cache operation and initial packed-manifest discovery |
| `port` | `0` | Loopback port; zero chooses a free dynamic port |
| `grpc-port` | `0` | Loopback port for the gRPC API; zero chooses a free dynamic port |
| `usage-artifact` | `bazel-cache-usage` | Job artifact to publish the usage record to; empty disables it |

The main step outputs `url`, `grpc-url`, `stats-url`, `writable`,
`bazel-args`, `grpc-bazel-args`, and `initial-stats`. The post step emits
`final-stats` and always writes the final counts to the job summary. Because
post steps run after normal job steps, consume `stats-url` during the job if a
later step must read it.

### Which entries a job used

The post step publishes a job artifact named by `usage-artifact`, holding a
single `cache-usage.json`. It reports every cache entry the job touched, and
whether it was downloaded or only presence-checked:

```json
{
  "entries": [
    {"kind": "cas", "digest": "…", "pack": "…",
     "presence_checks": 1, "downloads": 0},
    {"kind": "cas", "digest": "…", "pack": "…", "size": 4096,
     "presence_checks": 1, "downloads": 1}
  ],
  "packs": [{"id": "…", "size": 8388608, "declared_bytes": 9437184,
             "bytes_used": 4096, "restored": true}],
  "pack_bytes_restored": 8388608,
  "pack_bytes_declared": 9437184,
  "pack_bytes_used": 4096
}
```

`size` is absent when nothing reported one, which objects mode never does for a
bare presence check. It is never defaulted to zero, because zero is a real size
belonging to a blob with a well-known digest.

The distinction matters. Under `--remote_download_minimal` most outputs are only
ever presence-checked: they still have to exist, or the action result referencing
them becomes a miss, but nothing needs their bytes. An entry with no presence
checks and no downloads is the only kind nothing depends on.

`pack_bytes_used` against `pack_bytes_declared` is the headline number for
`packs` mode. It is the share of a restored pack's content a job turned out to
want, so a low ratio means packs are placing frequently and rarely fetched
content together. `pack_bytes_restored` is what the transfers actually cost;
because blocks are compressed it is not comparable with either of the other two,
and a ratio built on it can exceed 1. All three appear in the final statistics.

To keep the record, nothing is required. Set `usage-artifact` to an empty string
to opt out, or to a distinct name per instance if one job runs this action more
than once, since artifact names must be unique within a job. A failed upload
warns and never fails the job.

### Rebuilding the layout around how it is read

Packs group whatever a job happened to produce together. Over time that stops
matching how they are read, and restoring one pack to get one blob pays for the
rest. A second action regroups pack contents around the usage records and
deletes what it replaces.

It only applies to `storage-mode: packs`. Run it on a schedule, in its own
workflow:

```yaml
name: Optimise Bazel cache

on:
  schedule:
    - cron: "17 4 * * 0"
  workflow_dispatch:

permissions:
  contents: read

# A second pass would plan against a view the first is already changing.
concurrency:
  group: optimise-bazel-cache
  cancel-in-progress: false

jobs:
  optimise:
    runs-on: ubuntu-24.04
    permissions:
      contents: read
      # Deleting the cache entries this pass replaces is the only thing that
      # needs write.
      actions: write
    steps:
      - uses: cre4ture/bazel-github-actions-cache-v2/optimise@<FULL_COMMIT_SHA>
        with:
          github-token: ${{ github.token }}
          key-prefix: bazel-http-v1
```

`key-prefix` must match the one the cache action uses, and `usage-artifact` must
match where it publishes its records. The action writes a job summary and sets
`summary`, `rebuilt`, `deleted` and `reaped` outputs.

Start with `dry-run: true`. It reports the same numbers without publishing or
deleting anything, and never even constructs a client that can delete:

```yaml
      - uses: cre4ture/bazel-github-actions-cache-v2/optimise@<FULL_COMMIT_SHA>
        with:
          github-token: ${{ github.token }}
          dry-run: true
```

Entries are grouped by the set of runs that downloaded them, so entries wanted
by the same jobs share a pack and entries nothing downloads are moved out of the
way. A pack is only rebuilt when `min-waste-fraction` of it is bytes an average
restore pays for and does not want.

| Input | Default | Purpose |
| --- | --- | --- |
| `github-token` | required | Needs `actions: write` |
| `key-prefix` | `bazel-http-v1` | Must match the cache action |
| `usage-artifact` | `bazel-cache-usage` | Must match the cache action |
| `max-records` | `25` | How many recent runs to plan from |
| `min-runs` | `3` | Refuses to plan from a smaller window |
| `pack-size-mb` | `8` | Target size of a rebuilt pack |
| `min-waste-fraction` | `0.25` | How wasteful a pack must be to be rebuilt |
| `max-new-mb` | `2048` | Stop once this much has been published |
| `uploads-per-minute` | `180` | Evenly spaced uploads; must be below 200 |
| `reap-unmanifested-packs` | `false` | Delete packs no manifest names |
| `reap-older-than-hours` | `24` | Age below which a pack is never reaped |
| `dry-run` | `false` | Plan and report only |

A rebuilt pack is published before the one it replaces is deleted, so a cache
near its quota would briefly have to hold both layouts. `max-new-mb` bounds
that: a pass stops once it has published that much, and each source pack is
deleted as soon as every entry it served has been republished. Stopping early is
safe, because the packs not yet rebuilt were never touched. The next pass
re-plans from where this one left off.

Three properties are worth knowing before enabling it:

- **Nothing is deleted until its replacement is published.** An interrupted pass
  leaves duplicated storage, never a hole.
- **No CAS entry is ever dropped.** A manifest records an action's closure as
  pack IDs rather than digests, so the optimiser cannot prove a blob is
  unreferenced. It reclaims only packs whose every copy already lost the merge.
- **A job running during the pass may lose hits, not data.** It resolved the old
  layout and will find packs gone. Scheduling the pass when the repository is
  quiet narrows that window; nothing can close it.

It only applies from the default branch. A pass reads, rebuilds and deletes
entirely within the Git reference it runs on, so running it from a branch would
rebuild that branch's own caches rather than the ones jobs read. `dry-run` works
from any ref.

Everything a pass touches belongs to one reference. The listing is narrowed to
it, so entries another branch owns never appear, and deletion names the same
reference, so a copy this pass never listed is never removed. That also means
the "not restorable here" count reflects genuine eviction rather than entries
that were only ever visible to another branch.

A pack is never a deletion candidate unless the manifest describing it was read
successfully.

### Reaping unreachable packs

A reader only ever finds a pack through a manifest naming it, so a pack no
manifest names is unreachable and is pure storage cost. `reap-unmanifested-packs`
deletes those. It is off by default and is the one thing here that removes data
without publishing a replacement.

Two things keep it from taking anything live. A pack whose manifest exists but
this job could not read is *not* unmanifested and is never a candidate, because
another ref may still be reading it. And `reap-older-than-hours` keeps a pack a
running job has published but not yet committed a manifest for out of reach: the
two are indistinguishable except by age. A pack whose creation time the listing
does not report is left alone.

Fractions of an hour are accepted so that a test can reap something it has just
created. Anything under an hour warns, because it is short enough to take a pack
a running job is still publishing.

A dry run applies the age filter and reports how many packs it would take, so
"packs old enough to reap" is the number to look at before enabling it.

This is worth enabling when the job summary shows a large "packs no manifest
names" count, which usually means manifests were evicted while their packs
survived.

`min-runs` refuses to plan from too small a window, since a handful of records
describes those particular jobs rather than the repository. A pass with too few
records, or with nothing worth rebuilding, reports that in the job summary and
succeeds; a quiet week does not turn a scheduled workflow red.

## Protocol support

The adapter serves two endpoints from one process against one cache: HTTP on
`url` and gRPC on `grpc-url`. Point Bazel at either with `--remote_cache`. The
gRPC endpoint is the faster of the two because `FindMissingBlobs` settles a
whole input closure in one call, where HTTP needs a `HEAD` per blob.

Supported over HTTP:

- `GET`, `HEAD`, and `PUT` on exactly `/cas/<lowercase-sha256>`
- `GET`, `HEAD`, and `PUT` on exactly `/ac/<lowercase-sha256>`
- mandatory `Content-Length` and identity encoding

Supported over gRPC:

- `ContentAddressableStorage.FindMissingBlobs`, `BatchUpdateBlobs`,
  `BatchReadBlobs`, and `GetTree`
- `ActionCache.GetActionResult` and `UpdateActionResult`
- `Capabilities.GetCapabilities`
- `ByteStream.Read` and `Write` for blobs above the 4 MiB batch limit
- `asset.v1.Fetch.FetchBlob`, backing Bazel's remote downloader

## Caching repository downloads

Action and CAS caching never covers the archives that repository rules fetch,
because those are extracted outside the action graph. Bazel's remote downloader
closes that gap:

```bash
bazel test //... \
  --remote_cache="$CACHE_URL" \
  --experimental_remote_downloader="$CACHE_URL" \
  --experimental_remote_downloader_local_fallback
```

The fallback flag matters: it defaults to `false`, and Bazel treats any non-OK
fetch as an error rather than a miss, so without it the first build that cannot
be served fails outright.

A repository rule must declare a checksum for its download to be cached. Bazel
sends it as a `checksum.sri` qualifier, in any of the algorithms it supports:
`sha1`, `sha256`, `sha384`, `sha512`, and `blake3`. That matters in practice
because checksums arrive from lockfiles rather than by hand, and npm integrity
is `sha512`. When a rule declares no checksum Bazel explicitly forbids cached
content, and this server answers `NOT_FOUND`.

Content is always stored under its sha256, so a `sha256` checksum addresses the
cache directly. Any other algorithm resolves through an alias recorded the first
time the asset was fetched. An alias is an action-cache record pointing at the
blob, which means the existing closure validation applies to it: an alias whose
blob has been evicted reads as a miss, and in packed mode it travels inside a
CARv2 pack and renews that pack's retention, rather than costing one
Actions-cache creation each.

Bazel's downloader never pushes a fetched archive back, so a cache-only
implementation could never be populated. On a miss this server fetches the asset
itself, over `https` only, verifies it against the declared checksum before
storing anything, and publishes it so that later jobs get a hit. Credentials are
never forwarded: `http_header` qualifiers are ignored, so private origins are not
supported. `asset_downloads` and `asset_fetch_errors` report that activity.

Every failure names the resource it refers to, because Bazel reports the message
on its own:

```text
WARNING: Remote Cache: NOT_FOUND: https://registry.npmjs.org/pkg/-/pkg-1.0.0.tgz: origin returned HTTP 401
```

Supported by both:

- CAS SHA-256 verification before publication and after download
- structural validation of REAPI `ActionResult`, `Tree`, and `Directory`
  messages
- AC hits only when every referenced CAS object still exists
- AC publication only after every referenced CAS object is persistent
- implicit handling of the standard SHA-256 zero-byte CAS digest
- immutable cache keys
- per-job coalescing of duplicate immutable uploads before rate limiting and backend publication
- opt-in CARv2 archives with footer indexes, DAG-CBOR manifests, concurrent
  writer head merging, and action-result conflict detection

Not currently supported:

- Bazel `instance_name` prefixes; a non-empty one is rejected rather than ignored
- HTTP or zstd remote-cache compression, including `compressed-blobs` resources
- resumable `ByteStream` uploads; `QueryWriteStatus` always reports no progress
- `asset.v1.Push`, `FetchDirectory`, and authenticated asset origins
- remote execution
- range requests
- Windows or macOS runners

## Limits and operational model

The `objects` format maps every Bazel AC or CAS object to one GitHub Actions
cache entry. It remains the default rollback path. GitHub documents a limit of
200 cache creations per minute; the action defaults to 180 evenly spaced
uploads. In `packs` mode a successful flush consumes one creation for the CARv2
pack and one for its manifest, so a large build graph is governed by pack count
instead of object count. `pack_uploads`, `manifest_uploads`, and
`pack_downloads` make that distinction explicit in the final statistics.

Three counters describe load, and they deliberately do not agree:

| Counter | Counts | Reading it |
| --- | --- | --- |
| `requests` | One per client call: an HTTP request, or a gRPC RPC | Over gRPC this should be far below `operations`; if it is not, the client is not batching |
| `operations` | One per cache object the client asked about | Comparable between HTTP and gRPC, so it measures the build rather than the protocol |
| `backend_requests` | One per call into the GitHub Actions cache | Tracks API pressure and quota risk; closure validation and packed reads raise it without any matching client call |

Manifest discovery talks to the GitHub REST API instead, under a separate quota,
and is reported by `manifests_discovered` and `packs_discovered`.

GitHub's repository cache quota, eviction policy, and branch restrictions all
apply. At the time of writing, the default repository quota is 10 GB and caches
not accessed for seven days may be evicted. Pull-request caches are scoped, while
runs can restore caches from the default branch according to GitHub's cache
scope rules. Cache misses and eviction are normal and must never affect build
correctness.

Because GitHub evicts entries independently, an AC entry can outlive one of its
referenced CAS entries. The adapter checks the complete output closure before
serving or publishing an action result and degrades an incomplete closure to an
ordinary cache miss. In packed mode that check resolves each referenced blob
through the manifest view and the pack listing, without restoring the packs, and
renews their retention. One action result is
limited to 100,000 distinct validation operations to bound amplification from a
malformed cache entry.

- [GitHub dependency cache reference](https://docs.github.com/en/actions/reference/workflows-and-actions/dependency-caching)
- [GitHub cache limits](https://docs.github.com/en/actions/reference/limits#cache-limits)
- [GitHub cache scope restrictions](https://docs.github.com/en/actions/how-tos/manage-workflow-runs/manage-caches#restrictions-for-accessing-a-cache)
- [Bazel HTTP remote caching](https://bazel.build/remote/caching)

## Failure policy

The default is fail-open:

- backend GET/HEAD errors are logged and returned to Bazel as cache misses;
- backend PUT errors are logged after the request body was bounded, spooled,
  and validated, then returned as a soft success;
- incomplete or malformed action results are not served or published and are
  reported separately in the final statistics;
- invalid paths, sizes, encodings, and CAS digests are always rejected.

Set `fail-on-cache-error: true` to return backend failures as HTTP 502. The
default protects build availability because a remote cache is an optimization,
not a source of truth.

## Threat model

The server binds only to `127.0.0.1` on a dynamic port. Its shutdown token is
random, masked, stored only as action post-state, and never exposed as a normal
output. The GitHub runtime token is inherited by the Go process but never
logged. Uploads are bounded on disk, backend concurrency is limited, and CAS
content is verified in both directions.

Code running in the same job can still reach loopback, inspect its own runner
environment, exhaust the job's local disk, or submit valid AC objects. Do not
run untrusted code in a cache-writing job. Fork pull requests are forced
read-only, but GitHub's general guidance about untrusted workflows and
self-hosted runners still applies.

Serving Bazel's remote downloader means the server makes outbound requests to
URIs a caller supplies. Those are restricted to `https`, may not be redirected
off it, are bounded by `max-blob-size-mb`, and are discarded unless they match
the checksum the caller asked for. No credential is ever attached, so this
grants a caller nothing it could not already do by fetching the URI itself.

Action-cache values cannot be content-verified against the action digest (the
digest addresses the action, not the serialized result). Cache poisoning is
therefore controlled by restricting writes to trusted workflows. Their
serialized REAPI structure and referenced SHA-256 CAS closure are still
validated before use.

## Development and reproducible binaries

The repository pins Go in `.go-version` and pins `go-actions-cache` to a full
upstream commit through a Go pseudo-version.

REAPI messages are decoded with generated protobuf code (the opaque API of
`google.golang.org/protobuf`). The upstream `.proto` sources are vendored under
`proto/` and the generated Go code is committed under `internal/proto/gen`, so
the commands below need neither `buf` nor `protoc`. Run
`scripts/generate-proto.sh` after changing the vendored definitions or the
pinned generator versions.

```bash
go test -race ./...
go vet ./...
go run honnef.co/go/tools/cmd/staticcheck@v0.7.0 ./...
go run golang.org/x/vuln/cmd/govulncheck@v1.6.0 ./...
node --test action/*.test.js optimise/*.test.js
VERSION=dev scripts/build-dist.sh
(cd dist && sha256sum --check SHA256SUMS)
```

Release binaries are built with `CGO_ENABLED=0`, `-trimpath`, and an empty Go
build ID, with VCS stamping disabled, for deterministic Linux amd64/arm64
output. CI builds them twice and requires both builds to produce identical
checksums.

`dist/` is not tracked in Git, and neither is any other build output. Both
actions resolve their binary in two steps:

1. If `dist/<program>-linux-<arch>` exists in the checkout, it is used
   directly. This is how local development, CI, and the smoke workflow exercise
   the code under review rather than a published artifact.
2. Otherwise the binary is downloaded from the release the action itself was
   pinned to, and its build provenance is verified with
   `gh attestation verify` before it is allowed to run.

Nothing in the repository records which release to use. `GITHUB_ACTION_REF` is
the ref from your `uses:` line, so `@v0.4.0` names its release directly, and a
commit pin is mapped back to the tag released from it. `GITHUB_ACTION_REPOSITORY`
supplies the repository, so a fork downloads its own releases instead of binaries
built from somebody else's source, with nothing to edit after forking.

Integrity rests on
[artifact attestations](https://docs.github.com/en/actions/concepts/security/artifact-attestations):
the release workflows sign the binaries with `actions/attest-build-provenance`,
and the action refuses to run one that does not verify against the publishing
repository. Only someone who can add a workflow to that repository can produce a
passing attestation, so no checksum has to be committed alongside the action.

## Releases

Release tags point at the reviewed commit itself, so the tree a consumer checks
out is exactly the tree CI ran against. Enable
[immutable releases](https://docs.github.com/en/code-security/concepts/supply-chain-security/immutable-releases)
on the repository to lock those tags and assets after publication; the workflows
already draft, attach, then publish, which is the order that requires.

| Workflow | Trigger | Tag | Marked |
| --- | --- | --- | --- |
| `Pre-release` | push to `main` | `pre-<UTC timestamp>` | pre-release |
| `Release` | `workflow_dispatch` with a `version` input | the given `v*` tag | latest |

Neither workflow commits anything. A release is a tag, a set of assets, and an
attestation; the default branch is never rewritten by CI.

The smoke workflow has two modes. A default-branch push seeds a stable packed
object. A separate `workflow_dispatch` restore run downloads the manifest and
one CARv2 pack on a new runner and asserts the persistent hit statistics.

## License

Apache-2.0.
