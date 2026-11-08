"use strict";

const assert = require("node:assert/strict");
const fs = require("node:fs");
const os = require("node:os");
const path = require("node:path");
const test = require("node:test");
const {
  appendCommand,
  formatBytes,
  formatCount,
  booleanFlag,
  isForkPullRequest,
  logExcerpt,
  resolveWriteMode,
  safeTemporaryDirectory,
} = require("./lib");

test("fork pull requests are always read-only", () => {
  const payload = {
    pull_request: {
      head: { repo: { full_name: "fork/repo" } },
      base: { repo: { full_name: "owner/repo" } },
    },
  };
  assert.equal(isForkPullRequest(payload), true);
  assert.equal(resolveWriteMode("true", payload), false);
});

test("auto writes only on a default-branch push", () => {
  const previousEvent = process.env.GITHUB_EVENT_NAME;
  const previousRef = process.env.GITHUB_REF;
  process.env.GITHUB_EVENT_NAME = "push";
  process.env.GITHUB_REF = "refs/heads/main";
  assert.equal(resolveWriteMode("auto", { repository: { default_branch: "main" } }), true);
  process.env.GITHUB_REF = "refs/heads/feature";
  assert.equal(resolveWriteMode("auto", { repository: { default_branch: "main" } }), false);
  process.env.GITHUB_EVENT_NAME = previousEvent;
  process.env.GITHUB_REF = previousRef;
});

test("command writer uses multiline syntax", () => {
  const directory = fs.mkdtempSync(path.join(os.tmpdir(), "command-test-"));
  const file = path.join(directory, "output");
  appendCommand(file, "value", "a\nb");
  assert.match(fs.readFileSync(file, "utf8"), /^value<<gha_cache_/);
  fs.rmSync(directory, { recursive: true });
});

test("temporary directory must be below RUNNER_TEMP", () => {
  const directory = fs.mkdtempSync(path.join(os.tmpdir(), "runner-temp-"));
  const previous = process.env.RUNNER_TEMP;
  process.env.RUNNER_TEMP = directory;
  assert.equal(safeTemporaryDirectory(path.join(directory, "child")), path.join(directory, "child"));
  assert.throws(() => safeTemporaryDirectory(directory));
  assert.throws(() => safeTemporaryDirectory(path.dirname(directory)));
  process.env.RUNNER_TEMP = previous;
  fs.rmSync(directory, { recursive: true });
});

test("Go boolean flags use equals syntax", () => {
  assert.equal(booleanFlag("write-enabled", true), "--write-enabled=true");
  assert.equal(booleanFlag("fail-open", false), "--fail-open=false");
  assert.throws(() => booleanFlag("unsafe flag", true));
});

test("byte sizes use the largest unit that fits", () => {
  assert.equal(formatBytes(0), "0B");
  assert.equal(formatBytes(12), "12B");
  assert.equal(formatBytes(1023), "1,023B");
  assert.equal(formatBytes(1024), "1KiB");
  assert.equal(formatBytes(2491416576), "2.32GiB");
  assert.equal(formatBytes(241432494), "230.248MiB");
  assert.equal(formatBytes(1024 ** 4 * 3), "3TiB");
  // Trailing zeros go, so a round size stays short.
  assert.equal(formatBytes(2 * 1024 ** 2), "2MiB");
  assert.equal(formatBytes(-1024), "-1KiB");
});

test("long counts are grouped so they can be read", () => {
  assert.equal(formatCount(0), "0");
  assert.equal(formatCount(999), "999");
  assert.equal(formatCount(999999), "999,999");
  assert.equal(formatCount(1000000), "1,000,000");
});

function crashLog({ before = 2000, goroutines = 5000 } = {}) {
  const lines = ["bazel-gha-cache: ready on http://127.0.0.1:1234"];
  for (let i = 0; i < before; i += 1) lines.push(`bazel-gha-cache: request ${i}`);
  lines.push("runtime: program exceeds 10000-thread limit", "fatal error: thread exhaustion", "");
  for (let i = 0; i < goroutines; i += 1) lines.push(`goroutine ${i} gp=0x1 m=nil [select]:`);
  return `${lines.join("\n")}\n`;
}

test("a short log is printed whole", () => {
  const log = "ready\nstopped\n";
  assert.equal(logExcerpt(log), log);
});

// The tail alone held only goroutine traces when the server crashed in CI, so
// neither its start nor the fatal error was visible.
test("a long log keeps its head, the start of a crash, and its tail", () => {
  const log = crashLog();
  const excerpt = logExcerpt(log);
  assert.ok(excerpt.length < 25_000, `excerpt is ${excerpt.length} characters`);
  assert.ok(excerpt.startsWith("bazel-gha-cache: ready on http://127.0.0.1:1234\n"));
  assert.match(excerpt, /^runtime: program exceeds 10000-thread limit\nfatal error: thread exhaustion\n/m);
  assert.ok(excerpt.endsWith("goroutine 4999 gp=0x1 m=nil [select]:\n"));
  assert.equal([...excerpt.matchAll(/^\[\.\.\. [\d,]+ characters omitted \.\.\.\]$/gm)].length, 2);
});

test("excerpts only cut on line boundaries", () => {
  const excerpt = logExcerpt(crashLog());
  const original = new Set(crashLog().split("\n"));
  for (const line of excerpt.split("\n")) {
    assert.ok(original.has(line) || /^\[\.\.\. /.test(line), `partial line ${JSON.stringify(line)}`);
  }
});

test("omitted counts account for every character left out", () => {
  const log = crashLog();
  const excerpt = logExcerpt(log);
  const omitted = [...excerpt.matchAll(/^\[\.\.\. ([\d,]+) characters omitted \.\.\.\]\n/gm)];
  const kept = omitted.reduce((length, match) => length - match[0].length, excerpt.length);
  const dropped = omitted.reduce((total, match) => total + Number(match[1].replaceAll(",", "")), 0);
  assert.equal(kept + dropped, log.length);
});

test("a crash already inside the head or tail is not repeated", () => {
  const early = logExcerpt(crashLog({ before: 10 }));
  assert.equal(early.match(/fatal error: thread exhaustion/g).length, 1);
  assert.equal([...early.matchAll(/characters omitted/g)].length, 1);
  const late = logExcerpt(crashLog({ goroutines: 10 }));
  assert.equal(late.match(/fatal error: thread exhaustion/g).length, 1);
  assert.equal([...late.matchAll(/characters omitted/g)].length, 1);
});

test("a long log without a crash keeps only its head and tail", () => {
  const lines = [];
  for (let i = 0; i < 5000; i += 1) lines.push(`bazel-gha-cache: request ${i}`);
  const excerpt = logExcerpt(`${lines.join("\n")}\n`);
  assert.equal([...excerpt.matchAll(/characters omitted/g)].length, 1);
  assert.ok(excerpt.endsWith("bazel-gha-cache: request 4999\n"));
});
