"use strict";

const fs = require("node:fs");
const path = require("node:path");
const { execFile } = require("node:child_process");

const PROGRAMS = new Set(["cache-server", "cache-optimiser"]);
const ARCHITECTURES = new Set(["amd64", "arm64"]);
const USER_AGENT = "bazel-github-actions-cache-v2";
const DOWNLOAD_TIMEOUT_MS = 120_000;
const VERIFY_TIMEOUT_MS = 60_000;
const API_TIMEOUT_MS = 30_000;
const REPOSITORY_PATTERN = /^[A-Za-z0-9._-]+\/[A-Za-z0-9._-]+$/;
const TAG_PATTERN = /^[A-Za-z0-9][A-Za-z0-9._-]{0,63}$/;
const COMMIT_PATTERN = /^[0-9a-f]{40}$/;

// Only names this action publishes are accepted, so a caller cannot steer the
// download or the install path towards something else, and an argument left
// undefined fails here rather than as a path error further down.
function assetName(program, architecture) {
  if (!PROGRAMS.has(program)) {
    throw new Error(`unknown program ${program}`);
  }
  if (!ARCHITECTURES.has(architecture)) {
    throw new Error(`unknown architecture ${architecture}`);
  }
  return `${program}-linux-${architecture}`;
}

function releaseAssetUrl(repository, version, asset) {
  return `https://github.com/${repository}/releases/download/${version}/${asset}`;
}

async function download(url) {
  const response = await fetch(url, {
    headers: { "user-agent": USER_AGENT },
    signal: AbortSignal.timeout(DOWNLOAD_TIMEOUT_MS),
  });
  if (!response.ok) {
    throw new Error(`GET ${url} returned HTTP ${response.status}`);
  }
  // Release downloads redirect to object storage, and fetch follows that itself.
  // The body is still unread here, so a redirect chain that ended up on plain
  // HTTP is rejected before any of the binary travels over it.
  if (!response.url.startsWith("https://")) {
    throw new Error(`refusing to download ${url} over ${new URL(response.url).protocol}`);
  }
  return Buffer.from(await response.arrayBuffer());
}

async function githubJson(url, token) {
  const headers = { "user-agent": USER_AGENT, accept: "application/vnd.github+json" };
  if (token) {
    headers.authorization = `Bearer ${token}`;
  }
  const response = await fetch(url, { headers, signal: AbortSignal.timeout(API_TIMEOUT_MS) });
  if (!response.ok) {
    throw new Error(`GET ${url} returned HTTP ${response.status}`);
  }
  return response.json();
}

// requireTag rejects anything that could not be a release tag. The value becomes
// both a URL segment and a path segment on the way to an executable, so it is
// checked here rather than at each use.
function requireTag(tag, problem) {
  if (!TAG_PATTERN.test(String(tag ?? ""))) {
    throw new Error(problem);
  }
  return String(tag);
}

// releaseVersion works out which release publishes the binary for the running
// copy of the action. `uses: owner/repo@v1.2.3` names its release directly; a
// commit pin has to be mapped back to the tag that was released from it.
async function releaseVersion({ repository, ref, token }) {
  if (COMMIT_PATTERN.test(String(ref ?? ""))) {
    const refs = await githubJson(
      `https://api.github.com/repos/${repository}/git/matching-refs/tags/`,
      token,
    );
    // Release tags are lightweight, so they point straight at the release commit.
    const match = refs.find((entry) => entry.object?.type === "commit" && entry.object.sha === ref);
    if (!match) {
      throw new Error(`no release tag points at ${ref}; pin a released tag or commit`);
    }
    const tag = String(match.ref).replace(/^refs\/tags\//, "");
    return requireTag(tag, `${repository} has an unusable release tag at ${ref}`);
  }
  return requireTag(
    ref,
    "cannot tell which release to use; pin this action to a release tag or commit",
  );
}

// verifyAttestation proves the binary came out of a workflow in the repository
// that published it. Only someone who can add a workflow there can produce a
// passing attestation, which is what makes a committed checksum unnecessary.
function verifyAttestation({ file, repository, token }) {
  return new Promise((resolve, reject) => {
    execFile(
      "gh",
      ["attestation", "verify", file, "--repo", repository],
      { env: { ...process.env, GH_TOKEN: token }, timeout: VERIFY_TIMEOUT_MS },
      (error, _stdout, stderr) => {
        if (!error) {
          resolve();
          return;
        }
        if (error.code === "ENOENT") {
          reject(new Error("the GitHub CLI is required to verify the server binary's provenance"));
          return;
        }
        reject(
          new Error(
            `build provenance for ${path.basename(file)} could not be verified: ` +
              `${String(stderr).trim() || error.message}`,
          ),
        );
      },
    );
  });
}

// install places a verified binary at a caller-chosen constant path. The value
// that reaches the process spawner is then built entirely from constants, so a
// release name cannot influence which program runs.
function install(source, destination) {
  fs.rmSync(destination, { force: true });
  try {
    fs.linkSync(source, destination);
  } catch {
    fs.copyFileSync(source, destination);
  }
  fs.chmodSync(destination, 0o700);
  return destination;
}

// resolveBinary returns one of this action's binaries for this runner. A
// locally built dist/ always wins so that CI and the smoke workflow exercise
// the code under review rather than a published artifact.
async function resolveBinary({
  program,
  actionRoot,
  architecture,
  installPath,
  repository,
  ref,
  token,
  toolCacheRoot,
  log,
}) {
  // Every one of these becomes part of a filesystem path. Checking them here
  // turns a caller that omits one into a clear error rather than a path fault
  // several frames away.
  for (const [name, value] of Object.entries({ actionRoot, installPath, toolCacheRoot })) {
    if (typeof value !== "string" || value === "") {
      throw new Error(`resolveBinary needs a ${name}`);
    }
  }
  const asset = assetName(program, architecture);
  const localBinary = path.join(actionRoot, "dist", asset);
  if (fs.existsSync(localBinary)) {
    log(`using locally built dist/${asset}`);
    return localBinary;
  }
  // The repository comes from whichever copy of the action is running, so a fork
  // downloads its own releases rather than binaries built from other sources.
  if (!REPOSITORY_PATTERN.test(String(repository ?? ""))) {
    throw new Error(
      "cannot tell which repository published this action; run scripts/build-dist.sh to use a local build",
    );
  }

  const version = await releaseVersion({ repository, ref, token });
  const cached = path.join(toolCacheRoot, `bazel-gha-${program}`, version, architecture, asset);
  if (fs.existsSync(cached)) {
    log(`using cached ${asset} from ${version}`);
    return install(cached, installPath);
  }

  const url = releaseAssetUrl(repository, version, asset);
  log(`downloading ${url}`);
  const bytes = await download(url);
  fs.mkdirSync(path.dirname(cached), { recursive: true });
  const spool = `${cached}.${process.pid}.part`;
  fs.writeFileSync(spool, bytes, { mode: 0o700 });
  try {
    await verifyAttestation({ file: spool, repository, token });
  } catch (error) {
    fs.rmSync(spool, { force: true });
    throw error;
  }
  log(`verified the build provenance of ${asset} from ${version}`);
  fs.renameSync(spool, cached);
  return install(cached, installPath);
}

module.exports = {
  assetName,
  download,
  releaseAssetUrl,
  releaseVersion,
  resolveBinary,
};
