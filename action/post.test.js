"use strict";

const assert = require("node:assert/strict");
const fs = require("node:fs");
const path = require("node:path");
const test = require("node:test");

const os = require("node:os");

const { buildSummary, publishLog } = require("./post");

const statsSource = fs.readFileSync(
  path.join(__dirname, "..", "internal", "server", "stats.go"),
  "utf8",
);

function statsFields() {
  return [...statsSource.matchAll(/json:"([a-z_]+)"/g)].map((match) => match[1]);
}

// Every field gets a distinct value, so a row showing the wrong one is visible.
function everyField() {
  const stats = {};
  statsFields().forEach((field, index) => {
    stats[field] = (index + 1) * 1000;
  });
  return stats;
}

function headline(summary) {
  return summary.slice(0, summary.indexOf("<details>"));
}

function detail(summary) {
  return summary.slice(summary.indexOf("<details>"));
}

// A summary that leaves a counter out looks healthy when it is not, which
// matters most for the error counts.
test("every statistic the server reports reaches one of the two tables", () => {
  const summary = buildSummary(everyField());
  const labelled = new Set(
    [...summary.matchAll(/^\| ([^|]+?) \| /gm)].map((match) => match[1]),
  );
  // Counted by label rather than field name, since the tables are what a
  // person reads.
  assert.ok(labelled.size >= statsFields().length, `only ${labelled.size} rows for ${statsFields().length} fields`);
  for (const expected of ["Asset fetch errors", "Manifest load errors", "Backend save errors"]) {
    assert.ok(labelled.has(expected), `${expected} is in neither table`);
  }
});

test("the headline stays short and the rest is collapsed", () => {
  const summary = buildSummary(everyField());
  const rows = (text) => [...text.matchAll(/^\| /gm)].length - 2;
  assert.ok(rows(headline(summary)) <= 10, "the headline table has grown past a glance");
  assert.ok(rows(detail(summary)) > 20, "the detail table is missing rows");
  assert.match(summary, /<details>\n<summary>All statistics<\/summary>\n\n\| Metric/);
  assert.match(summary, /\n<\/details>/);
});

// The cue to open the detail is a non-zero error count, so it has to total the
// failures rather than report any single one.
test("the headline totals the error counters", () => {
  const clean = buildSummary({ hits: 1 });
  assert.match(headline(clean), /\| Errors \| 0 \|/);

  const failing = buildSummary({
    backend_load_errors: 2,
    backend_save_errors: 3,
    manifest_discovery_errors: 4,
    manifest_load_errors: 5,
    asset_fetch_errors: 6,
  });
  assert.match(headline(failing), /\| Errors \| 20 \|/);
});

// Objects mode never touches a pack, so those rows would all read zero.
test("pack rows are promoted only when packs are in use", () => {
  const objects = buildSummary({ hits: 1 });
  assert.doesNotMatch(headline(objects), /Pack yield/);

  const packs = buildSummary({
    packs_discovered: 4,
    pack_bytes_declared: 1000,
    pack_bytes_used: 250,
  });
  assert.match(headline(packs), /\| Pack yield \| 25\.0% \|/);
});

test("yield is unavailable rather than wrong when nothing was restored", () => {
  const summary = buildSummary({ packs_discovered: 1, pack_bytes_used: 500 });
  assert.match(headline(summary), /\| Pack yield \| n\/a \|/);
});

// Byte counts and plain counts are easy to confuse, and the wrong one reads as
// a nonsense size.
test("byte statistics render as sizes and the rest as counts", () => {
  const summary = buildSummary({ bytes_served: 2 * 1024 ** 3, hits: 1024 });
  assert.match(summary, /\| Served \| 2GiB \|/);
  assert.match(summary, /\| Hits \| 1,024 \|/);
});

const uploadCommandSource = fs.readFileSync(
  path.join(__dirname, "..", "cmd", "cache-server", "upload_artifact.go"),
  "utf8",
);

function existingBinary(t) {
  const directory = fs.mkdtempSync(path.join(os.tmpdir(), "post-test-"));
  t.after(() => fs.rmSync(directory, { recursive: true, force: true }));
  const binary = path.join(directory, "cache-server");
  fs.writeFileSync(binary, "");
  return binary;
}

function capture() {
  const output = { stdout: "", stderr: "" };
  return {
    output,
    write: (text) => {
      output.stdout += text;
    },
    warn: (text) => {
      output.stderr += text;
    },
  };
}

test("the server log is published through the server binary", (t) => {
  const binary = existingBinary(t);
  const calls = [];
  const { output, write, warn } = capture();
  const published = publishLog({
    binary,
    logFile: "/tmp/server.log",
    name: "bazel-cache-server-log",
    spawn: (command, args, options) => {
      calls.push({ command, args, options });
      return { status: 0, stdout: 'published cache-server.log as artifact "bazel-cache-server-log"\n' };
    },
    write,
    warn,
  });
  assert.equal(published, true);
  assert.equal(calls.length, 1);
  assert.equal(calls[0].command, binary);
  assert.deepEqual(calls[0].args.slice(0, 5), [
    "upload-artifact",
    "--name",
    "bazel-cache-server-log",
    "--file",
    "/tmp/server.log",
  ]);
  assert.ok(calls[0].options.timeout > 0, "the upload must not be able to hang the post step");
  assert.match(output.stdout, /published cache-server\.log/);
  assert.equal(output.stderr, "");
});

// The binary is invoked by name and flag, so a rename on either side would
// silently stop logs from being published.
test("the upload uses the subcommand and flags the server binary defines", (t) => {
  let args;
  publishLog({
    binary: existingBinary(t),
    logFile: "/tmp/server.log",
    name: "log",
    spawn: (_command, given) => {
      args = given;
      return { status: 0, stdout: "" };
    },
    write: () => {},
  });
  const command = uploadCommandSource.match(/uploadArtifactCommandName = "([^"]+)"/);
  assert.equal(args[0], command?.[1]);
  const defined = new Set(
    [...uploadCommandSource.matchAll(/flags\.[A-Za-z0-9]+\("([a-z-]+)"/g)].map((match) => match[1]),
  );
  for (const flag of args.filter((arg) => arg.startsWith("--"))) {
    assert.ok(defined.has(flag.slice(2)), `${flag} is not a flag of ${args[0]}`);
  }
});

test("a failed log upload warns rather than failing the job", (t) => {
  const { output, write, warn } = capture();
  const published = publishLog({
    binary: existingBinary(t),
    logFile: "/tmp/server.log",
    name: "log",
    spawn: () => ({ status: 1, stdout: "", stderr: "upload-artifact: FinalizeArtifact returned HTTP 409\n" }),
    write,
    warn,
  });
  assert.equal(published, false);
  assert.match(output.stderr, /^::warning::cannot publish the cache server log: .*HTTP 409\n$/);
});

test("a log upload that cannot start warns", (t) => {
  const { output, write, warn } = capture();
  const published = publishLog({
    binary: existingBinary(t),
    logFile: "/tmp/server.log",
    name: "log",
    spawn: () => ({ error: new Error("spawnSync ETIMEDOUT"), status: null }),
    write,
    warn,
  });
  assert.equal(published, false);
  assert.match(output.stderr, /ETIMEDOUT/);
});

test("an empty log-artifact disables publishing", () => {
  const published = publishLog({
    binary: "/nonexistent",
    logFile: "/tmp/server.log",
    name: "",
    spawn: () => assert.fail("nothing should be spawned"),
  });
  assert.equal(published, false);
});

test("a missing server binary warns instead of spawning", () => {
  const { output, write, warn } = capture();
  const published = publishLog({
    binary: "/nonexistent/cache-server",
    logFile: "/tmp/server.log",
    name: "log",
    spawn: () => assert.fail("nothing should be spawned"),
    write,
    warn,
  });
  assert.equal(published, false);
  assert.match(output.stderr, /server binary is gone/);
});
