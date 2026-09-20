"use strict";

const assert = require("node:assert");
const fs = require("node:fs");
const http = require("node:http");
const os = require("node:os");
const path = require("node:path");
const { test } = require("node:test");

const {
  assetName,
  download,
  releaseAssetUrl,
  releaseVersion,
  resolveServerBinary,
} = require("./release");

const REPOSITORY = "cre4ture/bazel-github-actions-cache-v2";
const COMMIT = "b".repeat(40);
const realFetch = globalThis.fetch.bind(globalThis);

function workspace(t) {
  const root = fs.mkdtempSync(path.join(os.tmpdir(), "release-test-"));
  t.after(() => fs.rmSync(root, { recursive: true, force: true }));
  return root;
}

function resolve(overrides) {
  return resolveServerBinary({
    architecture: "amd64",
    repository: REPOSITORY,
    ref: "v1.2.3",
    token: "",
    log: () => {},
    ...overrides,
  });
}

function stubFetch(t, handler) {
  globalThis.fetch = handler;
  t.after(() => {
    globalThis.fetch = realFetch;
  });
}

function jsonResponse(body) {
  return Promise.resolve({ ok: true, url: "https://api.github.com/", json: async () => body });
}

async function localServer(t, handler) {
  const server = http.createServer(handler);
  await new Promise((ready) => server.listen(0, "127.0.0.1", ready));
  t.after(() => server.close());
  return `127.0.0.1:${server.address().port}`;
}

test("release assets are addressed by tag, not by latest", () => {
  assert.strictEqual(
    releaseAssetUrl(REPOSITORY, "v1.2.3", assetName("arm64")),
    "https://github.com/cre4ture/bazel-github-actions-cache-v2/releases/download/v1.2.3/cache-server-linux-arm64",
  );
});

test("a tag ref names its own release", async () => {
  assert.strictEqual(await releaseVersion({ repository: REPOSITORY, ref: "v1.2.3" }), "v1.2.3");
});

test("a commit pin resolves to the tag released from it", async (t) => {
  stubFetch(t, (url) => {
    assert.match(String(url), /git\/matching-refs\/tags\//);
    return jsonResponse([
      { ref: "refs/tags/v1.0.0", object: { type: "commit", sha: "a".repeat(40) } },
      { ref: "refs/tags/v1.2.3", object: { type: "commit", sha: COMMIT } },
    ]);
  });
  assert.strictEqual(await releaseVersion({ repository: REPOSITORY, ref: COMMIT }), "v1.2.3");
});

test("a commit that was never released is reported as such", async (t) => {
  stubFetch(t, () => jsonResponse([]));
  await assert.rejects(
    releaseVersion({ repository: REPOSITORY, ref: COMMIT }),
    /no release tag points at/,
  );
});

// A tag name reaches both a download URL and the path of the binary that gets
// executed, so a hostile one must never be taken at face value.
test("a tag name that could escape its directory is rejected", async (t) => {
  for (const ref of [
    "refs/tags/../../../../tmp/evil",
    "refs/tags/v1.2.3/../../..",
    "refs/tags/v1;rm -rf /",
    "refs/tags/",
  ]) {
    stubFetch(t, () => jsonResponse([{ ref, object: { type: "commit", sha: COMMIT } }]));
    await assert.rejects(
      releaseVersion({ repository: REPOSITORY, ref: COMMIT }),
      /unusable release tag/,
    );
  }
});

test("a ref that names no release at all is rejected", async () => {
  for (const ref of [undefined, "", "refs/heads/main", "feature branch"]) {
    await assert.rejects(
      releaseVersion({ repository: REPOSITORY, ref }),
      /pin this action to a release tag or commit/,
    );
  }
});

test("a locally built binary is preferred over any published release", async (t) => {
  const root = workspace(t);
  const actionRoot = path.join(root, "checkout");
  const local = path.join(actionRoot, "dist", assetName("amd64"));
  fs.mkdirSync(path.dirname(local), { recursive: true });
  fs.writeFileSync(local, "locally built");

  assert.strictEqual(
    await resolve({ actionRoot, toolCacheRoot: path.join(root, "tools") }),
    local,
  );
});

test("an action running from an unknown repository will not guess where to download", async (t) => {
  const root = workspace(t);
  await assert.rejects(
    resolve({
      actionRoot: path.join(root, "checkout"),
      repository: undefined,
      toolCacheRoot: path.join(root, "tools"),
    }),
    /cannot tell which repository/,
  );
});

test("an already verified binary is installed at the fixed path that gets executed", async (t) => {
  const root = workspace(t);
  const toolCacheRoot = path.join(root, "tools");
  const version = "v1.2.3";
  const cached = path.join(
    toolCacheRoot,
    "bazel-gha-cache-server",
    version,
    "amd64",
    assetName("amd64"),
  );
  fs.mkdirSync(path.dirname(cached), { recursive: true });
  fs.writeFileSync(cached, "previously verified");
  const installPath = path.join(root, "cache-server");

  const resolved = await resolve({
    actionRoot: path.join(root, "checkout"),
    installPath,
    toolCacheRoot,
  });
  assert.strictEqual(resolved, installPath);
  assert.strictEqual(fs.readFileSync(resolved, "utf8"), "previously verified");
  // The executed path must carry no trace of the release it came from.
  assert.ok(!resolved.includes(version), `${resolved} still names the release`);
});

test("a download that lands on plain HTTP is refused before the body is read", async (t) => {
  let served = false;
  const address = await localServer(t, (_, response) => {
    served = true;
    response.writeHead(200).end("a binary that must never be trusted");
  });

  await assert.rejects(
    download(`http://${address}/${assetName("amd64")}`),
    /refusing to download/,
  );
  assert.ok(served, "the endpoint should have been reached and then rejected");
});

test("a binary whose provenance cannot be verified is discarded", async (t) => {
  const root = workspace(t);
  const toolCacheRoot = path.join(root, "tools");
  const address = await localServer(t, (_, response) => {
    response.writeHead(200).end("an unattested binary");
  });
  // Serve the asset locally while reporting the HTTPS origin the download
  // requires, so that verification rather than transport is what fails.
  stubFetch(t, async (url) => {
    const served = await realFetch(String(url).replace(/^https:\/\/github\.com/, `http://${address}`));
    return { ok: true, url: "https://github.com/", arrayBuffer: () => served.arrayBuffer() };
  });
  // Hide the GitHub CLI so verification fails locally instead of reaching out.
  const realPath = process.env.PATH;
  process.env.PATH = path.join(root, "empty");
  t.after(() => {
    process.env.PATH = realPath;
  });

  await assert.rejects(
    resolve({
      actionRoot: path.join(root, "checkout"),
      installPath: path.join(root, "cache-server"),
      toolCacheRoot,
    }),
    /provenance|GitHub CLI/,
  );
  const cacheDir = path.join(toolCacheRoot, "bazel-gha-cache-server", "v1.2.3", "amd64");
  const leftovers = fs.existsSync(cacheDir) ? fs.readdirSync(cacheDir) : [];
  assert.deepStrictEqual(leftovers, [], "an unverified download must not be left behind");
  assert.ok(!fs.existsSync(path.join(root, "cache-server")), "nothing must be installed");
});
