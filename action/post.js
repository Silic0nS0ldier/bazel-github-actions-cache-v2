"use strict";

const fs = require("node:fs");
const {
  formatBytes,
  formatCount,
  request,
  safeTemporaryDirectory,
  setOutput,
} = require("./lib");

const sleep = (milliseconds) => new Promise((resolve) => setTimeout(resolve, milliseconds));

function processExists(pid) {
  try {
    process.kill(pid, 0);
    return true;
  } catch {
    return false;
  }
}

// buildSummary is separated from the post step so the rendered tables can
// be tested without standing a server up.
function buildSummary(stats) {
  const count = (name) => formatCount(stats[name] ?? 0);
  const bytes = (name) => formatBytes(stats[name] ?? 0);
  const declared = Number(stats.pack_bytes_declared ?? 0);
  const packed = ["packs_discovered", "pack_downloads", "pack_uploads"].some(
    (name) => Number(stats[name] ?? 0) > 0,
  );
  // Collapsing the detail hides an error count behind a click, so the total
  // is surfaced here: a non-zero value is the cue to expand.
  const failures = [
    "backend_load_errors",
    "backend_save_errors",
    "manifest_discovery_errors",
    "manifest_load_errors",
    "asset_fetch_errors",
  ].reduce((total, name) => total + Number(stats[name] ?? 0), 0);

  const headline = [
    ["Hits", count("hits")],
    ["Misses", count("misses")],
    ["Published uploads", count("uploads")],
    ...(packed
      ? [
          ["CARv2 pack downloads", count("pack_downloads")],
          ["Pack bytes transferred", bytes("pack_bytes_restored")],
          // Used over declared, since a transferred total is compressed and
          // so is not comparable with either.
          [
            "Pack yield",
            declared > 0
              ? `${((100 * Number(stats.pack_bytes_used ?? 0)) / declared).toFixed(1)}%`
              : "n/a",
          ],
        ]
      : []),
    ["Served", bytes("bytes_served")],
    ["Received", bytes("bytes_received")],
    ["Errors", formatCount(failures)],
  ];

  // Everything the JSON carries that is not in the headline, so expanding
  // always answers the question rather than sending you to the raw output.
  const detail = [
    ["Requests", count("requests")],
    ["Operations", count("operations")],
    ["Missing action results", count("misses_ac")],
    ["Missing CAS objects", count("misses_cas")],
    ["Failed presence checks", count("misses_presence")],
    ["Unusable action results", count("misses_rejected")],
    ["Misses from degraded backend", count("misses_degraded")],
    ["Rejected requests", count("rejected_requests")],
    ["Deduplicated uploads", count("deduplicated_uploads")],
    ["Read-only discarded uploads", count("discarded_uploads")],
    ["Upload throttle waits", count("throttle_waits")],
    ["CARv2 pack uploads", count("pack_uploads")],
    ["Manifest uploads", count("manifest_uploads")],
    ["Pack retention renewals", count("pack_renewals")],
    ["Unavailable packs skipped", count("pack_loads_skipped")],
    ["Compressed blocks", count("compressed_blocks")],
    ["Saved by compression", bytes("compression_saved_bytes")],
    ["Packs discovered", count("packs_discovered")],
    ["Manifests discovered", count("manifests_discovered")],
    ["Manifests skipped", count("manifests_skipped")],
    ["Orphaned manifests", count("manifests_orphaned")],
    ["Manifest discovery errors", count("manifest_discovery_errors")],
    ["Manifest load errors", count("manifest_load_errors")],
    ["Action-digest conflicts", count("action_digest_conflicts")],
    ["Validated action results", count("validated_action_results")],
    ["Incomplete action results", count("incomplete_action_results")],
    ["Invalid action results", count("invalid_action_results")],
    ["Skipped action-result uploads", count("skipped_action_result_uploads")],
    ["Asset requests", count("asset_requests")],
    ["Asset hits", count("asset_hits")],
    ["Asset downloads", count("asset_downloads")],
    ["Asset requests rejected", count("asset_rejected")],
    ["Asset fetch errors", count("asset_fetch_errors")],
    ["Backend requests", count("backend_requests")],
    ["Backend downloads", count("backend_downloads")],
    ["Backend existence checks", count("backend_existence_checks")],
    ["Backend load errors", count("backend_load_errors")],
    ["Backend save errors", count("backend_save_errors")],
    ["Pack content restored", bytes("pack_bytes_declared")],
    ["Pack content used", bytes("pack_bytes_used")],
    ...(packed ? [] : [["Pack bytes transferred", bytes("pack_bytes_restored")]]),
  ];

  const table = (rows) => [
    "| Metric | Value |",
    "|---|---:|",
    ...rows.map(([label, value]) => `| ${label} | ${value} |`),
  ];
  const summary = [
    "### Bazel GitHub Actions cache v2",
    "",
    ...table(headline),
    "",
    "<details>",
    "<summary>All statistics</summary>",
    // A table needs a blank line after the summary tag to render at all.
    "",
    ...table(detail),
    "",
    "</details>",
    "",
  ].join("\n");
  return summary;
}

async function post() {
  const url = process.env.STATE_url;
  const token = process.env.STATE_shutdown_token;
  const pid = Number.parseInt(process.env.STATE_pid || "", 10);
  const statsFile = process.env.STATE_stats_file;
  const logFile = process.env.STATE_log_file;
  const tempDir = process.env.STATE_temp_dir;
  const shutdownWaitSeconds = Number.parseInt(process.env.STATE_shutdown_wait_seconds || "330", 10);
  if (!url || !token || !Number.isSafeInteger(pid)) {
    process.stdout.write("Cache server state is absent; no cleanup is needed.\n");
    return;
  }

  try {
    const response = await request(`${url}/shutdown`, {
      method: "POST",
      headers: { "X-Shutdown-Token": token },
      timeout: 3000,
    });
    if (response.status !== 202) {
      process.stderr.write(`::warning::cache shutdown returned HTTP ${response.status}\n`);
    }
  } catch (error) {
    process.stderr.write(`::warning::cache shutdown request failed: ${error.message}\n`);
  }

  const shutdownAttempts = Number.isSafeInteger(shutdownWaitSeconds) && shutdownWaitSeconds > 0
    ? shutdownWaitSeconds * 10
    : 3300;
  for (let attempt = 0; attempt < shutdownAttempts && processExists(pid); attempt += 1) {
    await sleep(100);
  }
  if (processExists(pid)) {
    process.stderr.write("::warning::cache server did not stop gracefully; sending SIGTERM\n");
    try {
      process.kill(pid, "SIGTERM");
    } catch {}
  }

  let stats = {};
  if (statsFile && fs.existsSync(statsFile)) {
    try {
      stats = JSON.parse(fs.readFileSync(statsFile, "utf8"));
      setOutput("final-stats", JSON.stringify(stats));
    } catch (error) {
      process.stderr.write(`::warning::cannot read final cache statistics: ${error.message}\n`);
    }
  }
  if (process.env.GITHUB_STEP_SUMMARY) {
    fs.appendFileSync(process.env.GITHUB_STEP_SUMMARY, buildSummary(stats));
  }

  if (logFile && fs.existsSync(logFile)) {
    const log = fs.readFileSync(logFile, "utf8");
    process.stdout.write(`Cache server log:\n${log.slice(-16000)}`);
  }
  if (tempDir) {
    try {
      fs.rmSync(safeTemporaryDirectory(tempDir), { recursive: true, force: true });
    } catch (error) {
      process.stderr.write(`::warning::cannot remove cache temporary directory: ${error.message}\n`);
    }
  }
}

if (require.main === module) {
  post().catch((error) => {
    process.stderr.write(`::warning::cache post-step failed: ${error.message}\n`);
  });
}

module.exports = { buildSummary };
