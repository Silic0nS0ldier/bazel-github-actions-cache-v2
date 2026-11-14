"use strict";

const crypto = require("node:crypto");
const fs = require("node:fs");
const os = require("node:os");
const path = require("node:path");
const { spawn } = require("node:child_process");
const {
  input,
  booleanFlag,
  logExcerpt,
  mask,
  parseBoolean,
  request,
  resolveWriteMode,
  safeTemporaryDirectory,
  saveState,
  setOutput,
} = require("./lib");
const { resolveBinary } = require("./release");

const sleep = (milliseconds) => new Promise((resolve) => setTimeout(resolve, milliseconds));

function startupTimeoutMilliseconds(backendTimeoutSeconds) {
  // The server restores every discoverable manifest before accepting Bazel
  // requests, so startup may take as long as backend discovery.
  return backendTimeoutSeconds * 1000;
}

function artifactName(name) {
  const value = input(name, "").trim();
  if (value && !/^[A-Za-z0-9._-]{1,180}$/.test(value)) {
    throw new Error(`${name} must match [A-Za-z0-9._-]{1,180}`);
  }
  return value;
}

function positiveInteger(name, fallback, maximum = Number.MAX_SAFE_INTEGER) {
  const value = Number.parseInt(input(name, String(fallback)), 10);
  if (!Number.isSafeInteger(value) || value <= 0 || value > maximum) {
    throw new Error(`${name} must be an integer between 1 and ${maximum}`);
  }
  return value;
}

function portNumber(name) {
  const value = Number.parseInt(input(name, "0"), 10);
  if (!Number.isSafeInteger(value) || value < 0 || value > 65535) {
    throw new Error(`${name} must be an integer between 0 and 65535`);
  }
  return value;
}

function serverArchitecture(platform, arch) {
  if (!["linux", "darwin", "win32"].includes(platform)) {
    throw new Error("this release supports Linux, Apple Silicon macOS, and Windows GitHub Actions runners only");
  }
  if (platform === "darwin" && arch !== "arm64") {
    throw new Error(`macOS requires Apple Silicon (arm64); unsupported architecture: ${arch}`);
  }
  const architecture = { x64: "amd64", arm64: "arm64" }[arch];
  if (!architecture) {
    throw new Error(`unsupported ${platform === "win32" ? "Windows" : "Linux"} architecture: ${arch}`);
  }
  return architecture;
}

async function main() {
  // An ARM64 Windows runner may execute the action's Node runtime under x64
  // emulation. Select the native server from the runner's architecture.
  const architecture = serverArchitecture(
    process.platform,
    process.platform === "win32" && process.env.RUNNER_ARCH
      ? process.env.RUNNER_ARCH.toLowerCase()
      : process.arch,
  );
  if (process.env.ACTIONS_CACHE_SERVICE_V2?.toLowerCase() !== "true") {
    throw new Error("GitHub Actions cache v2 is unavailable (ACTIONS_CACHE_SERVICE_V2 != true)");
  }
  if (!process.env.ACTIONS_RESULTS_URL || !process.env.ACTIONS_RUNTIME_TOKEN) {
    throw new Error("GitHub Actions cache v2 credentials are unavailable in this step");
  }

  const writeEnabled = resolveWriteMode(input("write", "auto"));
  const failOpen = !parseBoolean(input("fail-on-cache-error", "false"), "fail-on-cache-error");
  const maxBlobSizeMB = positiveInteger("max-blob-size-mb", 512, 10_240);
  const maxConcurrent = positiveInteger("max-concurrent-operations", 4, 32);
  const uploadsPerMinute = positiveInteger("max-uploads-per-minute", 180, 199);
  const backendTimeoutSeconds = positiveInteger("backend-timeout-seconds", 300, 3600);
  const packSizeMB = positiveInteger("pack-size-mb", 8, 32);
  const packFlushSeconds = positiveInteger("pack-flush-seconds", 30, 3600);
  const packRenewSeconds = positiveInteger("pack-renew-seconds", 15, 3600);
  const packCompression = parseBoolean(input("pack-compression", "true"), "pack-compression");
  const packCompressionLevel = positiveInteger("pack-compression-level", 3, 19);
  const maxManifests = positiveInteger("max-manifests", 2048, 10_000);
  const githubToken = input("github-token", "");
  if (!githubToken) {
    throw new Error("github-token with actions: read is required to discover cache manifests");
  }
  if (githubToken) mask(githubToken);
  const port = portNumber("port");
  const grpcPort = portNumber("grpc-port");
  if (port !== 0 && port === grpcPort) {
    throw new Error("port and grpc-port must differ");
  }
  // Artifact names must be unique within a job, so two instances of this action
  // cannot both publish under the same one.
  const usageArtifact = artifactName("usage-artifact");
  const logArtifact = artifactName("log-artifact");
  if (usageArtifact && usageArtifact === logArtifact) {
    throw new Error("usage-artifact and log-artifact must differ");
  }
  // Validated by the server, which is what decides whether a credential is sent.
  const assetHeaderRoutes = input("asset-header-routes", "").trim();

  const tempDir = safeTemporaryDirectory(
    fs.mkdtempSync(path.join(path.resolve(process.env.RUNNER_TEMP), "bazel-gha-cache-v2-")),
  );
  saveState("temp_dir", tempDir);
  const readyFile = path.join(tempDir, "ready.json");
  const statsFile = path.join(tempDir, "stats.json");
  const logFile = path.join(tempDir, "server.log");
  const spoolDir = path.join(tempDir, "spool");
  const shutdownToken = crypto.randomBytes(32).toString("hex");
  mask(shutdownToken);

  const binary = await resolveBinary({
    program: "cache-server",
    actionRoot: path.resolve(__dirname, ".."),
    architecture,
    platform: process.platform,
    // Installing under a fixed name keeps every part of the spawned path a
    // constant, so no release name can influence which program runs.
    installPath: path.join(tempDir, process.platform === "win32" ? "cache-server.exe" : "cache-server"),
    repository: process.env.GITHUB_ACTION_REPOSITORY,
    ref: process.env.GITHUB_ACTION_REF,
    token: githubToken || process.env.GITHUB_TOKEN || "",
    toolCacheRoot: path.resolve(process.env.RUNNER_TOOL_CACHE || process.env.RUNNER_TEMP || os.tmpdir()),
    log: (message) => process.stdout.write(`${message}${os.EOL}`),
  });

  const args = [
    "--port",
    String(port),
    "--grpc-port",
    String(grpcPort),
    "--cache-dir",
    spoolDir,
    "--key-prefix",
    input("key-prefix", "bazel-http-v1"),
    "--pack-size",
    String(packSizeMB * 1024 * 1024),
    "--pack-flush-interval",
    `${packFlushSeconds}s`,
    "--pack-renew-interval",
    `${packRenewSeconds}s`,
    booleanFlag("pack-compression", packCompression),
    "--pack-compression-level",
    String(packCompressionLevel),
    "--max-manifests",
    String(maxManifests),
    booleanFlag("write-enabled", writeEnabled),
    booleanFlag("fail-open", failOpen),
    "--max-blob-size",
    String(maxBlobSizeMB * 1024 * 1024),
    "--max-concurrent",
    String(maxConcurrent),
    "--uploads-per-minute",
    String(uploadsPerMinute),
    "--backend-timeout",
    `${backendTimeoutSeconds}s`,
    "--ready-file",
    readyFile,
    "--stats-file",
    statsFile,
    "--usage-artifact",
    usageArtifact,
    "--asset-header-routes",
    assetHeaderRoutes,
  ];
  // Saved before startup so that the post step can publish the log of a server
  // that never became ready.
  saveState("binary", binary);
  saveState("log_file", logFile);
  saveState("log_artifact", logArtifact);
  const logDescriptor = fs.openSync(logFile, "a", 0o600);
  const child = spawn(binary, args, {
    detached: true,
    windowsHide: true,
    env: {
      ...process.env,
      BAZEL_GHA_CACHE_SHUTDOWN_TOKEN: shutdownToken,
      GITHUB_TOKEN: githubToken || process.env.GITHUB_TOKEN || "",
    },
    stdio: ["ignore", logDescriptor, logDescriptor],
  });
  fs.closeSync(logDescriptor);
  await new Promise((resolve, reject) => {
    child.once("spawn", resolve);
    child.once("error", reject);
  });
  child.unref();
  saveState("pid", String(child.pid));
  saveState("shutdown_token", shutdownToken);
  saveState("stats_file", statsFile);
  saveState("shutdown_wait_seconds", String(Math.max(60, backendTimeoutSeconds + 30)));

  const readyDeadline = Date.now() + startupTimeoutMilliseconds(backendTimeoutSeconds);
  let ready;
  while (Date.now() < readyDeadline) {
    if (fs.existsSync(readyFile)) {
      ready = JSON.parse(fs.readFileSync(readyFile, "utf8"));
      break;
    }
    if (child.exitCode !== null) break;
    await sleep(100);
  }
  if (!ready) {
    const log = fs.existsSync(logFile)
      ? logExcerpt(fs.readFileSync(logFile, "utf8"), { headChars: 2000, crashChars: 2000, tailChars: 4000 })
      : "";
    throw new Error(`cache server did not become ready\n${log}`);
  }
  const health = await request(`${ready.url}/ready`, { method: "GET", timeout: 2000 });
  if (health.status !== 200) {
    throw new Error(`cache server readiness check returned HTTP ${health.status}`);
  }

  saveState("url", ready.url);
  setOutput("url", ready.url);
  setOutput("grpc-url", ready.grpc_url);
  setOutput("stats-url", ready.stats_url);
  setOutput("writable", String(writeEnabled));
  setOutput(
    "bazel-args",
    `--remote_cache=${ready.url} --remote_upload_local_results=${writeEnabled}`,
  );
  setOutput(
    "grpc-bazel-args",
    `--remote_cache=${ready.grpc_url} --remote_upload_local_results=${writeEnabled}`,
  );
  setOutput("initial-stats", '{"requests":0,"hits":0,"misses":0,"uploads":0}');
  process.stdout.write(
    `Bazel cache adapter ready at ${ready.url} and ${ready.grpc_url} (write=${writeEnabled}, fail_open=${failOpen}, pid=${ready.pid})${os.EOL}`,
  );
}

if (require.main === module) {
  main().catch((error) => {
    process.stderr.write(`::error::${String(error.message).replaceAll("\r", "").replaceAll("\n", "%0A")}\n`);
    process.exitCode = 1;
  });
}

module.exports = { artifactName, serverArchitecture, startupTimeoutMilliseconds };
