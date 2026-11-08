"use strict";

const fs = require("node:fs");
const http = require("node:http");
const path = require("node:path");

function input(name, fallback = "") {
  const exact = `INPUT_${name.toUpperCase().replaceAll(" ", "_")}`;
  const normalized = exact.replaceAll("-", "_");
  return process.env[exact] ?? process.env[normalized] ?? fallback;
}

function parseBoolean(value, name) {
  switch (String(value).trim().toLowerCase()) {
    case "true":
      return true;
    case "false":
      return false;
    default:
      throw new Error(`${name} must be true or false`);
  }
}

function booleanFlag(name, value) {
  if (typeof value !== "boolean" || !/^[a-z][a-z-]*$/.test(name)) {
    throw new Error("invalid boolean flag");
  }
  // Go's flag package treats a bare bool flag as true and does not consume the
  // next argument. Always use = so later flags are still parsed.
  return `--${name}=${value}`;
}

function eventPayload() {
  const eventPath = process.env.GITHUB_EVENT_PATH;
  if (!eventPath) return {};
  try {
    return JSON.parse(fs.readFileSync(eventPath, "utf8"));
  } catch (error) {
    throw new Error(`cannot read GITHUB_EVENT_PATH: ${error.message}`);
  }
}

function isForkPullRequest(payload) {
  const head = payload.pull_request?.head?.repo?.full_name;
  const base = payload.pull_request?.base?.repo?.full_name;
  return Boolean(head && base && head !== base);
}

function resolveWriteMode(value, payload = eventPayload()) {
  const mode = String(value || "auto").trim().toLowerCase();
  if (!["auto", "true", "false"].includes(mode)) {
    throw new Error("write must be auto, true, or false");
  }
  if (isForkPullRequest(payload)) {
    return false;
  }
  if (mode === "true") return true;
  if (mode === "false") return false;
  const defaultBranch = payload.repository?.default_branch;
  return (
    process.env.GITHUB_EVENT_NAME === "push" &&
    Boolean(defaultBranch) &&
    process.env.GITHUB_REF === `refs/heads/${defaultBranch}`
  );
}

function appendCommand(file, name, value) {
  if (!file) return;
  const text = String(value);
  if (text.includes("\n") || text.includes("\r")) {
    const delimiter = `gha_cache_${process.pid}_${Date.now()}`;
    fs.appendFileSync(file, `${name}<<${delimiter}\n${text}\n${delimiter}\n`);
  } else {
    fs.appendFileSync(file, `${name}=${text}\n`);
  }
}

function setOutput(name, value) {
  appendCommand(process.env.GITHUB_OUTPUT, name, value);
}

function saveState(name, value) {
  appendCommand(process.env.GITHUB_STATE, name, value);
}

function mask(value) {
  process.stdout.write(`::add-mask::${String(value).replaceAll("\r", "").replaceAll("\n", "")}\n`);
}

function request(url, options = {}) {
  return new Promise((resolve, reject) => {
    const req = http.request(url, options, (res) => {
      const chunks = [];
      res.on("data", (chunk) => chunks.push(chunk));
      res.on("end", () =>
        resolve({ status: res.statusCode, body: Buffer.concat(chunks).toString("utf8") }),
      );
    });
    req.setTimeout(options.timeout ?? 2000, () => req.destroy(new Error("request timed out")));
    req.on("error", reject);
    req.end();
  });
}

function safeTemporaryDirectory(candidate) {
  const runnerTemp = path.resolve(process.env.RUNNER_TEMP || "");
  const resolved = path.resolve(candidate);
  if (!runnerTemp || resolved === runnerTemp || !resolved.startsWith(`${runnerTemp}${path.sep}`)) {
    throw new Error("refusing to use a temporary directory outside RUNNER_TEMP");
  }
  return resolved;
}

const BYTE_UNITS = [
  ["TiB", 1024 ** 4],
  ["GiB", 1024 ** 3],
  ["MiB", 1024 ** 2],
  ["KiB", 1024],
  ["B", 1],
];

// formatCount groups digits so a long number can be read at a glance. The
// locale is fixed so a job summary does not depend on the runner's.
function formatCount(value) {
  const number = Number(value);
  if (!Number.isFinite(number)) return String(value);
  return number.toLocaleString("en-US");
}

// formatBytes renders a size in the largest unit that fits, as "2.32GiB".
// Trailing zeros are dropped so a round number stays short.
function formatBytes(value) {
  const total = Number(value);
  if (!Number.isFinite(total)) return String(value);
  if (total < 0) return `-${formatBytes(-total)}`;
  for (const [unit, size] of BYTE_UNITS) {
    // Bytes are the floor, so anything under a kibibyte lands there.
    if (total < size && size > 1) continue;
    const scaled = (total / size).toFixed(3).replace(/0+$/, "").replace(/\.$/, "");
    const [integer, fraction] = scaled.split(".");
    return `${formatCount(integer)}${fraction ? `.${fraction}` : ""}${unit}`;
  }
  return "0B";
}

// The first line of a Go runtime crash: a panic, a fatal error, or the
// runtime's own diagnostic that precedes some fatal errors.
const crashLine = /^(?:panic: |fatal error: |runtime: )/m;

function omission(count) {
  return `[... ${formatCount(count)} characters omitted ...]\n`;
}

// logExcerpt keeps a log readable in the job output. Its head shows how the
// server started, its tail what it did last, and the start of a crash is kept
// in between because a runtime dump lists every goroutine and pushes the fatal
// error itself far out of the tail. Cuts fall on line boundaries.
function logExcerpt(text, { headChars = 4000, crashChars = 4000, tailChars = 16000 } = {}) {
  if (text.length <= headChars + crashChars + tailChars) {
    return text;
  }
  const cutBefore = (limit) => {
    const newline = text.lastIndexOf("\n", limit - 1);
    return newline >= 0 ? newline + 1 : limit;
  };
  const cutAfter = (start) => {
    const newline = text.indexOf("\n", start);
    return newline >= 0 ? newline + 1 : start;
  };

  const headEnd = cutBefore(headChars);
  const tailStart = Math.max(headEnd, cutAfter(text.length - tailChars));
  const ranges = [[0, headEnd]];
  const crash = crashLine.exec(text.slice(headEnd, tailStart));
  if (crash) {
    const start = headEnd + crash.index;
    ranges.push([start, Math.min(tailStart, cutBefore(start + crashChars))]);
  }
  ranges.push([tailStart, text.length]);

  let excerpt = "";
  let previousEnd = 0;
  for (const [start, end] of ranges) {
    if (start > previousEnd) {
      excerpt += omission(start - previousEnd);
    }
    excerpt += text.slice(start, end);
    previousEnd = end;
  }
  return excerpt;
}

module.exports = {
  appendCommand,
  booleanFlag,
  eventPayload,
  formatBytes,
  formatCount,
  input,
  isForkPullRequest,
  logExcerpt,
  mask,
  parseBoolean,
  request,
  resolveWriteMode,
  safeTemporaryDirectory,
  saveState,
  setOutput,
};
