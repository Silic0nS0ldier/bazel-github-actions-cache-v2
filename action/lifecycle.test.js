"use strict";

const assert = require("node:assert/strict");
const crypto = require("node:crypto");
const fs = require("node:fs");
const os = require("node:os");
const path = require("node:path");
const { spawn, spawnSync } = require("node:child_process");
const { after, before, test } = require("node:test");
const { serverArchitecture } = require("./main");
const { processExists, waitForProcessExit } = require("./post");
const { assetName } = require("./release");

const repoRoot = path.resolve(__dirname, "..");
const architecture = serverArchitecture(
  process.platform,
  process.platform === "win32" && process.env.RUNNER_ARCH
    ? process.env.RUNNER_ARCH.toLowerCase()
    : process.arch,
);
let root;
let tools;
let action;
let catalogServer;
let catalogPort;

before(async () => {
  catalogServer = spawn(process.execPath, ["-e", `
    require("node:http").createServer((_request, response) => {
      response.writeHead(200, { "Content-Type": "application/json" });
      response.end('{"actions_caches":[]}');
    }).listen(0, "127.0.0.1", function () {
      process.stdout.write(String(this.address().port));
    });
  `], { stdio: ["ignore", "pipe", "inherit"] });
  catalogPort = await new Promise((resolve, reject) => {
    catalogServer.once("error", reject);
    catalogServer.stdout.once("data", (data) => resolve(Number(data.toString())));
  });
  root = fs.mkdtempSync(path.join(os.tmpdir(), "cache lifecycle "));
  tools = path.join(root, "tools");
  action = path.join(root, "action");
  fs.mkdirSync(action);
  for (const name of ["main.js", "post.js", "lib.js", "release.js"]) {
    fs.copyFileSync(path.join(__dirname, name), path.join(action, name));
  }
  const binary = path.join(
    tools, "bazel-gha-cache-server", "lifecycle", architecture,
    assetName("cache-server", architecture, process.platform),
  );
  fs.mkdirSync(path.dirname(binary), { recursive: true });
  const build = spawnSync("go", ["build", "-o", binary, "./cmd/cache-server"], {
    cwd: repoRoot,
    env: {
      ...process.env,
      CGO_ENABLED: "0",
      GOOS: process.platform === "win32" ? "windows" : process.platform,
      GOARCH: architecture,
    },
    encoding: "utf8",
    timeout: 120_000,
  });
  assert.ifError(build.error);
  assert.equal(build.status, 0, build.stdout + build.stderr);
});

after(async () => {
  if (catalogServer?.exitCode === null) {
    catalogServer.kill();
    await new Promise((resolve) => catalogServer.once("exit", resolve));
  }
  if (root) fs.rmSync(root, { recursive: true, force: true });
});

function values(file) {
  return Object.fromEntries(fs.readFileSync(file, "utf8").trim().split("\n").map((line) => {
    const at = line.indexOf("=");
    return [line.slice(0, at), line.slice(at + 1)];
  }));
}

function runtimeToken() {
  const encode = (value) => Buffer.from(JSON.stringify(value)).toString("base64url");
  const now = Math.floor(Date.now() / 1000);
  return `${encode({ alg: "HS256" })}.${encode({ ac: "[]", nbf: now - 60, exp: now + 3600 })}.test`;
}

function start(t, inputs = {}) {
  const runnerTemp = fs.mkdtempSync(path.join(root, "runner temp "));
  const stateFile = path.join(runnerTemp, "state");
  const outputFile = path.join(runnerTemp, "output");
  const env = {
    // Use only synthetic credentials: these tests never contact GitHub.
    PATH: process.env.PATH,
    SystemRoot: process.env.SystemRoot,
    RUNNER_ARCH: process.env.RUNNER_ARCH,
    RUNNER_TEMP: runnerTemp,
    RUNNER_TOOL_CACHE: tools,
    GITHUB_ACTION_REPOSITORY: "cre4ture/bazel-github-actions-cache-v2",
    GITHUB_ACTION_REF: "lifecycle",
    GITHUB_REPOSITORY: "cre4ture/bazel-github-actions-cache-v2",
    GITHUB_API_URL: `http://127.0.0.1:${catalogPort}`,
    "INPUT_GITHUB-TOKEN": "synthetic-token",
    GITHUB_STATE: stateFile,
    GITHUB_OUTPUT: outputFile,
    ACTIONS_CACHE_SERVICE_V2: "true",
    ACTIONS_RESULTS_URL: "https://results.example.invalid/",
    ACTIONS_RUNTIME_TOKEN: runtimeToken(),
    "INPUT_WRITE": "false",
    "INPUT_USAGE-ARTIFACT": "",
    "INPUT_LOG-ARTIFACT": "",
    ...inputs,
  };
  t.after(async () => {
    if (!fs.existsSync(stateFile)) return;
    const pid = Number(values(stateFile).pid);
    if (Number.isSafeInteger(pid) && pid > 0 && processExists(pid)) {
      process.kill(pid, "SIGKILL");
      assert.ok(await waitForProcessExit(pid, 10_000), "test server did not exit");
    }
  });
  const result = spawnSync(process.execPath, [path.join(action, "main.js")], {
    env, encoding: "utf8", timeout: 25_000,
  });
  assert.ifError(result.error);
  const state = values(stateFile);
  return { result, state, env, outputFile };
}

function finish(run, overrides = {}) {
  const result = spawnSync(process.execPath, [path.join(action, "post.js")], {
    env: {
      ...run.env,
      ...Object.fromEntries(Object.entries(run.state).map(([key, value]) => [`STATE_${key}`, value])),
      ...overrides,
    },
    encoding: "utf8",
    timeout: 20_000,
  });
  assert.ifError(result.error);
  assert.equal(result.status, 0, result.stdout + result.stderr);
  assert.doesNotMatch(result.stderr, /::warning::/);
  return result;
}

test("the detached server survives the main step and shuts down before cleanup", async (t) => {
  const run = start(t);
  assert.equal(run.result.status, 0, run.result.stdout + run.result.stderr);
  assert.ok(processExists(Number(run.state.pid)));
  if (process.platform === "win32") assert.ok(run.state.binary.endsWith(".exe"));
  const payload = Buffer.from("cross-platform cache server lifecycle");
  const digest = crypto.createHash("sha256").update(payload).digest("hex");
  assert.equal((await fetch(`${run.state.url}/ready`)).status, 200);
  const upload = await fetch(`${run.state.url}/cas/${digest}`, { method: "PUT", body: payload });
  assert.equal(upload.status, 204);
  const download = await fetch(`${run.state.url}/cas/${digest}`);
  assert.equal(download.status, 200);
  assert.deepEqual(Buffer.from(await download.arrayBuffer()), payload);
  finish(run);
  assert.ok(!processExists(Number(run.state.pid)));
  assert.ok(!fs.existsSync(run.state.temp_dir), "post-step left its temporary directory behind");
  const stats = JSON.parse(values(run.outputFile)["final-stats"]);
  assert.equal(stats.discarded_uploads, 1);
  assert.equal(stats.hits, 1);
});

test("a server without readiness state is terminated before cleanup", (t) => {
  const run = start(t);
  assert.equal(run.result.status, 0, run.result.stdout + run.result.stderr);
  const result = spawnSync(process.execPath, [path.join(action, "post.js")], {
    env: {
      ...run.env,
      ...Object.fromEntries(Object.entries(run.state).map(([key, value]) => [`STATE_${key}`, value])),
      STATE_url: "",
    },
    encoding: "utf8",
    timeout: 20_000,
  });
  assert.ifError(result.error);
  assert.equal(result.status, 0, result.stdout + result.stderr);
  assert.match(result.stderr, /did not stop gracefully; terminating it/);
  assert.doesNotMatch(result.stderr, /cannot remove|still running|cannot terminate/);
  assert.ok(!processExists(Number(run.state.pid)));
  assert.ok(!fs.existsSync(run.state.temp_dir));
});

test("failed server startup retains cleanup state and leaves no temporary directory", (t) => {
  const run = start(t, { ACTIONS_RUNTIME_TOKEN: "invalid-token" });
  assert.equal(run.result.status, 1);
  assert.match(run.result.stderr, /cache server did not become ready/);
  assert.ok(run.state.pid);
  assert.ok(!run.state.url);
  finish(run);
  assert.ok(!fs.existsSync(run.state.temp_dir));
});
