"use strict";

const assert = require("node:assert/strict");
const fs = require("node:fs");
const os = require("node:os");
const path = require("node:path");
const test = require("node:test");
const { spawnSync } = require("node:child_process");

const repoRoot = path.resolve(__dirname, "..");
const architecture = { x64: "amd64", arm64: "arm64" }[process.arch];
const asset = `cache-optimiser-linux-${architecture}`;

// A stub that records how it was invoked and produces the summary the wrapper
// reads back.
const STUB = `#!/usr/bin/env node
const fs = require("node:fs");
const args = process.argv.slice(2);
fs.writeFileSync(process.env.STUB_ARGS_FILE, JSON.stringify(args));
const at = args.indexOf("--summary-file");
fs.writeFileSync(args[at + 1], JSON.stringify({ Records: 4, Rebuilt: 0, Deleted: 0, DryRun: true }));
`;

function runWrapper(t, inputs) {
  const distBinary = path.join(repoRoot, "dist", asset);
  if (fs.existsSync(distBinary)) {
    // A real build is present, so the wrapper would run the actual optimiser
    // against a live cache rather than the stub.
    t.skip(`dist/${asset} exists; run this before scripts/build-dist.sh`);
    return null;
  }
  fs.mkdirSync(path.dirname(distBinary), { recursive: true });
  fs.writeFileSync(distBinary, STUB, { mode: 0o755 });
  t.after(() => fs.rmSync(distBinary, { force: true }));

  const runnerTemp = fs.mkdtempSync(path.join(os.tmpdir(), "optimise-wrapper-"));
  t.after(() => fs.rmSync(runnerTemp, { recursive: true, force: true }));
  const argsFile = path.join(runnerTemp, "args.json");

  const result = spawnSync(process.execPath, [path.join(__dirname, "main.js")], {
    encoding: "utf8",
    // Deliberately not inheriting the environment: on a runner the ambient
    // GITHUB_* variables describe the job running the test, and the wrapper
    // would read them as its own context.
    env: {
      PATH: process.env.PATH,
      HOME: process.env.HOME,
      STUB_ARGS_FILE: argsFile,
      RUNNER_TEMP: runnerTemp,
      RUNNER_TOOL_CACHE: path.join(runnerTemp, "tools"),
      GITHUB_OUTPUT: path.join(runnerTemp, "output"),
      GITHUB_STEP_SUMMARY: path.join(runnerTemp, "summary.md"),
      ACTIONS_CACHE_SERVICE_V2: "true",
      ACTIONS_RESULTS_URL: "https://results.example.invalid/",
      ACTIONS_RUNTIME_TOKEN: "header.payload.signature",
      "INPUT_GITHUB-TOKEN": "test-token",
      ...inputs,
    },
  });
  return { result, argsFile, runnerTemp };
}

// The wrapper had no coverage at all, and shipped without passing the
// architecture through: the asset name became cache-optimiser-linux-undefined,
// which surfaced as a path error several frames away.
test("the wrapper runs the binary for this architecture with the parsed inputs", (t) => {
  const run = runWrapper(t, {
    "INPUT_KEY-PREFIX": "custom-prefix",
    "INPUT_USAGE-ARTIFACT": "custom-usage",
    "INPUT_MIN-RUNS": "2",
    "INPUT_PACK-SIZE-MB": "4",
    "INPUT_DRY-RUN": "true",
  });
  if (!run) return;

  assert.equal(run.result.status, 0, run.result.stdout + run.result.stderr);
  const args = JSON.parse(fs.readFileSync(run.argsFile, "utf8"));
  const valueOf = (flag) => args[args.indexOf(flag) + 1];
  assert.equal(valueOf("--key-prefix"), "custom-prefix");
  assert.equal(valueOf("--usage-artifact"), "custom-usage");
  assert.equal(valueOf("--min-runs"), "2");
  assert.equal(valueOf("--pack-size"), String(4 * 1024 * 1024));
  assert.ok(args.includes("--dry-run"));
});

test("a pass that applies does not ask for a dry run", (t) => {
  const run = runWrapper(t, { "INPUT_DRY-RUN": "false" });
  if (!run) return;

  assert.equal(run.result.status, 0, run.result.stdout + run.result.stderr);
  const args = JSON.parse(fs.readFileSync(run.argsFile, "utf8"));
  assert.ok(!args.includes("--dry-run"));
});

// Reaping removes data without publishing a replacement, so it has to stay off
// unless it is asked for.
test("reaping is off unless it is asked for", (t) => {
  const off = runWrapper(t, { "INPUT_DRY-RUN": "true" });
  if (!off) return;
  assert.equal(off.result.status, 0, off.result.stdout + off.result.stderr);
  assert.ok(!JSON.parse(fs.readFileSync(off.argsFile, "utf8")).includes("--reap"));
});

test("reaping passes its age threshold through", (t) => {
  const run = runWrapper(t, {
    "INPUT_DRY-RUN": "true",
    "INPUT_REAP-UNMANIFESTED-PACKS": "true",
    "INPUT_REAP-OLDER-THAN-HOURS": "48",
  });
  if (!run) return;

  assert.equal(run.result.status, 0, run.result.stdout + run.result.stderr);
  const args = JSON.parse(fs.readFileSync(run.argsFile, "utf8"));
  assert.ok(args.includes("--reap"));
  assert.equal(args[args.indexOf("--reap-older-than") + 1], "48h");
});

test("the publish budget and upload rate reach the binary", (t) => {
  const run = runWrapper(t, {
    "INPUT_DRY-RUN": "true",
    "INPUT_MAX-NEW-MB": "512",
    "INPUT_UPLOADS-PER-MINUTE": "120",
  });
  if (!run) return;

  assert.equal(run.result.status, 0, run.result.stdout + run.result.stderr);
  const args = JSON.parse(fs.readFileSync(run.argsFile, "utf8"));
  const valueOf = (flag) => args[args.indexOf(flag) + 1];
  assert.equal(valueOf("--max-new-bytes"), String(512 * 1024 * 1024));
  assert.equal(valueOf("--uploads-per-minute"), "120");
});

test("an upload rate at or above GitHub's limit is refused", (t) => {
  const run = runWrapper(t, { "INPUT_UPLOADS-PER-MINUTE": "200" });
  if (!run) return;

  assert.equal(run.result.status, 1);
  assert.match(run.result.stdout, /uploads-per-minute must be an integer between 1 and 199/);
});

test("the result reaches the job summary and the step outputs", (t) => {
  const run = runWrapper(t, { "INPUT_DRY-RUN": "true" });
  if (!run) return;

  assert.equal(run.result.status, 0, run.result.stdout + run.result.stderr);
  const summary = fs.readFileSync(path.join(run.runnerTemp, "summary.md"), "utf8");
  assert.match(summary, /Usage records read \| 4/);
  const outputs = fs.readFileSync(path.join(run.runnerTemp, "output"), "utf8");
  assert.match(outputs, /^rebuilt=0$/m);
  assert.match(outputs, /^deleted=0$/m);
  assert.match(outputs, /^summary=\{.*"Records":4.*\}$/m);
});

test("a token is required, since the pass deletes what it replaces", (t) => {
  const run = runWrapper(t, { "INPUT_GITHUB-TOKEN": "" });
  if (!run) return;

  assert.equal(run.result.status, 1);
  assert.match(run.result.stdout, /github-token is required/);
});

test("an input outside its range stops the pass", (t) => {
  const run = runWrapper(t, { "INPUT_MIN-WASTE-FRACTION": "2" });
  if (!run) return;

  assert.equal(run.result.status, 1);
  assert.match(run.result.stdout, /min-waste-fraction must be between 0 and 1/);
});

// Deleting spans every ref, but a replacement published from a branch is only
// visible to that branch.
test("applying from a branch other than the default is refused", (t) => {
  const eventFile = path.join(os.tmpdir(), `optimise-event-${process.pid}.json`);
  fs.writeFileSync(eventFile, JSON.stringify({ repository: { default_branch: "main" } }));
  t.after(() => fs.rmSync(eventFile, { force: true }));

  const applying = runWrapper(t, {
    "INPUT_DRY-RUN": "false",
    GITHUB_EVENT_PATH: eventFile,
    GITHUB_REF_NAME: "some-feature",
  });
  if (!applying) return;
  assert.equal(applying.result.status, 1);
  assert.match(applying.result.stdout, /refusing to apply from some-feature/);
});

test("planning from a branch is allowed, since it deletes nothing", (t) => {
  const eventFile = path.join(os.tmpdir(), `optimise-event-dry-${process.pid}.json`);
  fs.writeFileSync(eventFile, JSON.stringify({ repository: { default_branch: "main" } }));
  t.after(() => fs.rmSync(eventFile, { force: true }));

  const run = runWrapper(t, {
    "INPUT_DRY-RUN": "true",
    GITHUB_EVENT_PATH: eventFile,
    GITHUB_REF_NAME: "some-feature",
  });
  if (!run) return;
  assert.equal(run.result.status, 0, run.result.stdout + run.result.stderr);
});
