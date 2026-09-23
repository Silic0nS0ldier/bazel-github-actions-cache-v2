"use strict";

const fs = require("node:fs");
const os = require("node:os");
const path = require("node:path");
const { spawn } = require("node:child_process");
const {
  eventPayload,
  formatBytes,
  formatCount,
  input,
  mask,
  parseBoolean,
  safeTemporaryDirectory,
  setOutput,
} = require("../action/lib");
const { resolveBinary } = require("../action/release");

function positiveInteger(name, fallback, maximum = Number.MAX_SAFE_INTEGER) {
  const value = Number.parseInt(input(name, String(fallback)), 10);
  if (!Number.isSafeInteger(value) || value <= 0 || value > maximum) {
    throw new Error(`${name} must be an integer between 1 and ${maximum}`);
  }
  return value;
}

function fraction(name, fallback) {
  const value = Number.parseFloat(input(name, String(fallback)));
  if (!Number.isFinite(value) || value < 0 || value > 1) {
    throw new Error(`${name} must be between 0 and 1`);
  }
  return value;
}

// Fractions are allowed so that a test can ask for a threshold shorter than an
// hour. Anything that short cannot tell a lost pack from one a running job is
// still publishing, so it warns.
function reapAge(name, fallback, maximumHours) {
  const hours = Number.parseFloat(input(name, String(fallback)));
  if (!Number.isFinite(hours) || hours <= 0 || hours > maximumHours) {
    throw new Error(
      `${name} must be greater than 0 and at most ${maximumHours}; fractions of an hour are allowed`,
    );
  }
  if (hours < 1) {
    process.stdout.write(
      `::warning::${name} is under an hour, so a pack a running job has published but not yet ` +
        `committed a manifest for can be reaped; use this for testing only${os.EOL}`,
    );
  }
  return Math.max(1, Math.round(hours * 3600));
}

function run(binary, args, env) {
  return new Promise((resolve, reject) => {
    const child = spawn(binary, args, { stdio: ["ignore", "inherit", "inherit"], env });
    child.on("error", reject);
    child.on("exit", (code, signal) => {
      if (signal) {
        reject(new Error(`the optimiser was killed by ${signal}`));
        return;
      }
      if (code !== 0) {
        reject(new Error(`the optimiser exited with status ${code}`));
        return;
      }
      resolve();
    });
  });
}

function writeJobSummary(result) {
  const file = process.env.GITHUB_STEP_SUMMARY;
  if (!file) {
    return;
  }
  const count = (value) => formatCount(value ?? 0);
  const bytes = (value) => formatBytes(value ?? 0);
  const yesNo = (value) => (value ? "yes" : "no");
  const rows = [
    ["Usage records read", count(result.Records)],
    ["Packs before", count(result.Packs)],
    ["Live entries", count(result.Entries)],
    ["Manifests not restorable here", count(result.Unreadable)],
    ["Packs no manifest names", count(result.Unmanifested)],
    ["Packs old enough to reap", count(result.Reapable)],
    ["Packs rebuilt", count(result.Rebuilt)],
    ["Packs deleted", count(result.Deleted)],
    ["Packs reaped", count(result.Reaped)],
    ["Published", bytes(result.PublishedBytes)],
    ["Stopped at the byte budget", yesNo(result.Incomplete)],
    ["Wasted per restore", bytes(result.WastedBytes)],
    ["Reclaimed", bytes(result.ReclaimedBytes)],
    ["Dry run", yesNo(result.DryRun)],
  ];
  const table = [
    "### Bazel cache layout",
    "",
    ...(result.Skipped ? [`Nothing to do: ${result.Skipped}.`, ""] : []),
    "| Measure | Value |",
    "| --- | ---: |",
    ...rows.map(([label, value]) => `| ${label} | ${value} |`),
    "",
  ].join(os.EOL);
  fs.appendFileSync(file, table + os.EOL);
}

// A pass reads, rebuilds and deletes entirely within the reference it runs on.
// Running it from a branch would therefore rebuild that branch's own caches,
// which is almost always a mistake: the entries jobs actually read belong to the
// default branch.
function requireDefaultBranch() {
  const defaultBranch = eventPayload().repository?.default_branch;
  const ref = process.env.GITHUB_REF_NAME;
  if (!defaultBranch || !ref || ref === defaultBranch) {
    return;
  }
  throw new Error(
    `refusing to apply from ${ref}: a pass only ever touches its own reference's caches, ` +
      `so this would rebuild ${ref} rather than what jobs read; run from ${defaultBranch} or set dry-run`,
  );
}

async function main() {
  if (process.platform !== "linux") {
    throw new Error("this release supports Linux GitHub Actions runners only");
  }
  if (process.env.ACTIONS_CACHE_SERVICE_V2?.toLowerCase() !== "true") {
    throw new Error("GitHub Actions cache v2 is unavailable (ACTIONS_CACHE_SERVICE_V2 != true)");
  }
  if (!process.env.ACTIONS_RESULTS_URL || !process.env.ACTIONS_RUNTIME_TOKEN) {
    throw new Error("GitHub Actions cache v2 credentials are unavailable in this step");
  }

  const architecture = { x64: "amd64", arm64: "arm64" }[process.arch];
  if (!architecture) {
    throw new Error(`unsupported Linux architecture: ${process.arch}`);
  }

  const githubToken = input("github-token", "");
  if (!githubToken) {
    throw new Error("github-token is required; the pass deletes the cache entries it replaces");
  }
  mask(githubToken);

  const keyPrefix = input("key-prefix", "bazel-http-v1").trim();
  if (!/^[A-Za-z0-9._-]{1,128}$/.test(keyPrefix)) {
    throw new Error("key-prefix must match [A-Za-z0-9._-]{1,128}");
  }
  const usageArtifact = input("usage-artifact", "bazel-cache-usage").trim();
  if (!/^[A-Za-z0-9._-]{1,180}$/.test(usageArtifact)) {
    throw new Error("usage-artifact must match [A-Za-z0-9._-]{1,180}");
  }
  const maxRecords = positiveInteger("max-records", 25, 1000);
  const minRuns = positiveInteger("min-runs", 3, 1000);
  const packSizeMB = positiveInteger("pack-size-mb", 8, 32);
  const maxBlobSizeMB = positiveInteger("max-blob-size-mb", 512, 10_240);
  const uploadsPerMinute = positiveInteger("uploads-per-minute", 180, 199);
  const maxNewMB = Number.parseInt(input("max-new-mb", "2048"), 10);
  if (!Number.isSafeInteger(maxNewMB) || maxNewMB < 0) {
    throw new Error("max-new-mb must be a non-negative integer");
  }
  const reap = parseBoolean(input("reap-unmanifested-packs", "false"), "reap-unmanifested-packs");
  const reapOlderThanSeconds = reapAge("reap-older-than-hours", 24, 8760);
  const backendTimeoutSeconds = positiveInteger("backend-timeout-seconds", 300, 3600);
  const minWasteFraction = fraction("min-waste-fraction", 0.25);
  const dryRun = parseBoolean(input("dry-run", "false"), "dry-run");
  const jobSummary = parseBoolean(input("job-summary", "true"), "job-summary");
  if (!dryRun) {
    requireDefaultBranch();
  }

  const tempDir = safeTemporaryDirectory(
    fs.mkdtempSync(path.join(path.resolve(process.env.RUNNER_TEMP), "bazel-gha-optimise-")),
  );
  const summaryFile = path.join(tempDir, "summary.json");
  const spoolDir = path.join(tempDir, "spool");

  const binary = await resolveBinary({
    program: "cache-optimiser",
    actionRoot: path.resolve(__dirname, ".."),
    architecture,
    // Installing under a fixed name keeps every part of the spawned path a
    // constant, so no release name can influence which program runs.
    installPath: path.join(tempDir, "cache-optimiser"),
    repository: process.env.GITHUB_ACTION_REPOSITORY,
    ref: process.env.GITHUB_ACTION_REF,
    token: githubToken,
    toolCacheRoot: path.resolve(
      process.env.RUNNER_TOOL_CACHE || process.env.RUNNER_TEMP || os.tmpdir(),
    ),
    log: (message) => process.stdout.write(`${message}${os.EOL}`),
  });

  const args = [
    "--cache-dir",
    spoolDir,
    "--key-prefix",
    keyPrefix,
    "--usage-artifact",
    usageArtifact,
    "--max-records",
    String(maxRecords),
    "--min-runs",
    String(minRuns),
    "--pack-size",
    String(packSizeMB * 1024 * 1024),
    "--min-waste-fraction",
    String(minWasteFraction),
    "--max-blob-size",
    String(maxBlobSizeMB * 1024 * 1024),
    "--uploads-per-minute",
    String(uploadsPerMinute),
    "--max-new-bytes",
    String(maxNewMB * 1024 * 1024),
    "--reap-older-than",
    `${reapOlderThanSeconds}s`,
    "--backend-timeout",
    `${backendTimeoutSeconds}s`,
    "--summary-file",
    summaryFile,
  ];
  if (dryRun) {
    args.push("--dry-run");
  }
  if (reap) {
    args.push("--reap");
  }

  try {
    // The binary reads its credentials from the environment, so the token has to
    // arrive that way rather than on a command line other processes can see.
    await run(binary, args, { ...process.env, GITHUB_TOKEN: githubToken });
  } finally {
    // The summary is written even on failure, and is the only record of how far
    // the pass got.
    if (fs.existsSync(summaryFile)) {
      const result = JSON.parse(fs.readFileSync(summaryFile, "utf8"));
      setOutput("summary", JSON.stringify(result));
      setOutput("rebuilt", String(result.Rebuilt ?? 0));
      setOutput("deleted", String(result.Deleted ?? 0));
      setOutput("reaped", String(result.Reaped ?? 0));
      if (jobSummary) {
        writeJobSummary(result);
      }
    }
    fs.rmSync(tempDir, { recursive: true, force: true });
  }
}

main().catch((error) => {
  process.stdout.write(`::error::${error.message}${os.EOL}`);
  process.exitCode = 1;
});
