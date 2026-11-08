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
  resolveBinary,
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
  return resolveBinary({
    program: "cache-server",
    architecture: "amd64",
    actionRoot: "/action",
    installPath: "/tmp/installed",
    toolCacheRoot: "/tmp/tools",
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
    releaseAssetUrl(REPOSITORY, "v1.2.3", assetName("cache-server", "arm64")),
    "https://github.com/cre4ture/bazel-github-actions-cache-v2/releases/download/v1.2.3/cache-server-linux-arm64",
  );
  assert.strictEqual(
    releaseAssetUrl(REPOSITORY, "v1.2.3", assetName("cache-optimiser", "amd64")),
    "https://github.com/cre4ture/bazel-github-actions-cache-v2/releases/download/v1.2.3/cache-optimiser-linux-amd64",
  );
  assert.strictEqual(
    releaseAssetUrl(REPOSITORY, "pre-20261108T120614Z", assetName("cache-server", "arm64", "darwin")),
    "https://github.com/cre4ture/bazel-github-actions-cache-v2/releases/download/pre-20261108T120614Z/cache-server-darwin-arm64",
  );
});

test("only the server is published for macOS, and only for Apple Silicon", () => {
  assert.throws(() => assetName("cache-server", "amd64", "darwin"), /unsupported platform/);
  for (const architecture of ["amd64", "arm64"]) {
    assert.throws(() => assetName("cache-optimiser", architecture, "darwin"), /unsupported platform/);
    for (const program of ["cache-server", "cache-optimiser"]) {
      assert.equal(assetName(program, architecture, "linux"), `${program}-linux-${architecture}`);
    }
  }
  for (const platform of ["freebsd", "../linux", ""]) {
    assert.throws(() => assetName("cache-server", "arm64", platform), /unsupported platform/);
  }
});

test("Windows publishes only amd64/arm64 server executables", () => {
  for (const architecture of ["amd64", "arm64"]) {
    const asset = assetName("cache-server", architecture, "win32");
    assert.equal(asset, `cache-server-windows-${architecture}.exe`);
    assert.equal(
      releaseAssetUrl(REPOSITORY, "pre-20261108T120614Z", asset),
      `https://github.com/${REPOSITORY}/releases/download/pre-20261108T120614Z/${asset}`,
    );
    assert.throws(() => assetName("cache-optimiser", architecture, "win32"), /unsupported platform/);
  }
  assert.throws(() => assetName("cache-server", "386", "win32"), /unknown architecture/);
});

// The program name reaches a download URL and an install path, so only names
// this action actually publishes may be used. An argument left undefined has to
// fail here rather than as a path error further down.
test("an unknown program or architecture is rejected", () => {
  for (const program of ["", "sh", "../cache-server", undefined]) {
    assert.throws(() => assetName(program, "amd64"), /unknown program/);
  }
  for (const architecture of ["", "x64", "../../etc", undefined]) {
    assert.throws(() => assetName("cache-server", architecture), /unknown architecture/);
  }
});

test("an option that would become part of a path is required", async () => {
  const complete = {
    program: "cache-server",
    actionRoot: "/action",
    architecture: "amd64",
    installPath: "/tmp/installed",
    repository: REPOSITORY,
    ref: "v1.2.3",
    token: "",
    toolCacheRoot: "/tmp/tools",
    log: () => {},
  };
  for (const name of ["actionRoot", "installPath", "toolCacheRoot"]) {
    await assert.rejects(
      resolveBinary({ ...complete, [name]: undefined }),
      new RegExp(`resolveBinary needs a ${name}`),
    );
  }
  await assert.rejects(
    resolveBinary({ ...complete, architecture: undefined }),
    /unknown architecture/,
  );
});

// Both binaries are cached under the same root, so sharing a directory would
// let one be served in place of the other.
test("each program has its own tool-cache directory", async (t) => {
  const root = workspace(t);
  const toolCacheRoot = path.join(root, "tools");
  const optimiser = path.join(
    toolCacheRoot,
    "bazel-gha-cache-optimiser",
    "v1.2.3",
    "amd64",
    assetName("cache-optimiser", "amd64"),
  );
  fs.mkdirSync(path.dirname(optimiser), { recursive: true });
  fs.writeFileSync(optimiser, "cached optimiser");

  const installPath = path.join(root, "installed");
  const resolved = await resolveBinary({
    program: "cache-optimiser",
    actionRoot: path.join(root, "checkout"),
    architecture: "amd64",
    installPath,
    repository: REPOSITORY,
    ref: "v1.2.3",
    token: "",
    toolCacheRoot,
    log: () => {},
  });
  assert.strictEqual(resolved, installPath);
  assert.strictEqual(fs.readFileSync(installPath, "utf8"), "cached optimiser");
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
  const local = path.join(actionRoot, "dist", assetName("cache-server", "amd64"));
  fs.mkdirSync(path.dirname(local), { recursive: true });
  fs.writeFileSync(local, "locally built");

  assert.strictEqual(
    await resolve({ actionRoot, toolCacheRoot: path.join(root, "tools") }),
    local,
  );
});

test("macOS resolves its local server binary rather than a Linux ARM64 binary", async (t) => {
  const root = workspace(t);
  const actionRoot = path.join(root, "checkout");
  const dist = path.join(actionRoot, "dist");
  fs.mkdirSync(dist, { recursive: true });
  fs.writeFileSync(path.join(dist, assetName("cache-server", "arm64", "linux")), "Linux server");
  const local = path.join(dist, assetName("cache-server", "arm64", "darwin"));
  fs.writeFileSync(local, "macOS server");
  stubFetch(t, () => {
    throw new Error("a local binary must not require a release download");
  });

  assert.strictEqual(
    await resolve({ actionRoot, platform: "darwin", architecture: "arm64", repository: undefined }),
    local,
  );
});

for (const architecture of ["amd64", "arm64"]) {
  test(`Windows resolves its local ${architecture} executable rather than a Linux binary`, async (t) => {
    const root = workspace(t);
    const actionRoot = path.join(root, "checkout with spaces");
    const dist = path.join(actionRoot, "dist");
    fs.mkdirSync(dist, { recursive: true });
    fs.writeFileSync(path.join(dist, assetName("cache-server", architecture)), "Linux server");
    const local = path.join(dist, assetName("cache-server", architecture, "win32"));
    fs.writeFileSync(local, "Windows server");
    stubFetch(t, () => {
      throw new Error("a local binary must not require a release download");
    });
    assert.equal(
      await resolve({ actionRoot, platform: "win32", architecture, repository: undefined }),
      local,
    );
  });
}

test("Linux, macOS, and Windows ARM64 server binaries do not collide in the tool cache", async (t) => {
  const root = workspace(t);
  const toolCacheRoot = path.join(root, "tools");
  const cacheDir = path.join(toolCacheRoot, "bazel-gha-cache-server", "v1.2.3", "arm64");
  fs.mkdirSync(cacheDir, { recursive: true });
  for (const platform of ["linux", "darwin", "win32"]) {
    fs.writeFileSync(path.join(cacheDir, assetName("cache-server", "arm64", platform)), platform);
  }
  stubFetch(t, () => {
    throw new Error("a cached binary must not require a release download");
  });

  for (const platform of ["linux", "darwin", "win32"]) {
    const installPath = path.join(root, platform === "win32" ? "cache-server.exe" : "cache-server");
    const resolved = await resolve({
      actionRoot: path.join(root, "checkout"),
      architecture: "arm64",
      platform,
      installPath,
      toolCacheRoot,
    });
    assert.strictEqual(resolved, installPath);
    assert.strictEqual(fs.readFileSync(resolved, "utf8"), platform);
    if (process.platform !== "win32") {
      assert.strictEqual(fs.statSync(resolved).mode & 0o777, 0o700);
    }
  }
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
    assetName("cache-server", "amd64"),
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
    download(`http://${address}/${assetName("cache-server", "amd64")}`),
    /refusing to download/,
  );
  assert.ok(served, "the endpoint should have been reached and then rejected");
});

for (const platform of ["linux", "darwin", "win32"]) {
  test(`a ${platform} binary whose provenance cannot be verified is discarded`, async (t) => {
    const root = workspace(t);
    const toolCacheRoot = path.join(root, "tools");
    const actionRoot = path.join(root, "checkout");
    if (platform !== "linux") {
      fs.mkdirSync(path.join(actionRoot, "dist"), { recursive: true });
      fs.writeFileSync(path.join(actionRoot, "dist", assetName("cache-server", "arm64")), "Linux server");
    }
    const address = await localServer(t, (_, response) => {
      response.writeHead(200).end("an unattested binary");
    });
    // Serve the asset locally while reporting the HTTPS origin the download
    // requires, so that verification rather than transport is what fails.
    stubFetch(t, async (url) => {
      assert.strictEqual(
        url,
        releaseAssetUrl(REPOSITORY, "v1.2.3", assetName("cache-server", "arm64", platform)),
      );
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
        actionRoot,
        architecture: "arm64",
        platform,
        installPath: path.join(root, "cache-server"),
        toolCacheRoot,
      }),
      /provenance|GitHub CLI/,
    );
    const cacheDir = path.join(toolCacheRoot, "bazel-gha-cache-server", "v1.2.3", "arm64");
    const leftovers = fs.existsSync(cacheDir) ? fs.readdirSync(cacheDir) : [];
    assert.deepStrictEqual(leftovers, [], "an unverified download must not be left behind");
    assert.ok(!fs.existsSync(path.join(root, "cache-server")), "nothing must be installed");
  });
}
