"use strict";

const test = require("node:test");
const assert = require("node:assert/strict");

const { artifactName, serverArchitecture, startupTimeoutMilliseconds } = require("./main");

test("the server supports Linux and Windows amd64/arm64 and Apple Silicon macOS", () => {
  assert.equal(serverArchitecture("linux", "x64"), "amd64");
  assert.equal(serverArchitecture("linux", "arm64"), "arm64");
  assert.equal(serverArchitecture("darwin", "arm64"), "arm64");
  assert.equal(serverArchitecture("win32", "x64"), "amd64");
  assert.equal(serverArchitecture("win32", "arm64"), "arm64");
});

test("Intel macOS and unsupported runner platforms or architectures are rejected", () => {
  assert.throws(() => serverArchitecture("darwin", "x64"), /macOS requires Apple Silicon/);
  assert.throws(() => serverArchitecture("darwin", "ia32"), /macOS requires Apple Silicon/);
  assert.throws(() => serverArchitecture("linux", "riscv64"), /unsupported Linux architecture/);
  assert.throws(() => serverArchitecture("win32", "ia32"), /unsupported Windows architecture/);
  for (const platform of ["freebsd", undefined]) {
    assert.throws(() => serverArchitecture(platform, "arm64"), /supports Linux, Apple Silicon macOS, and Windows/);
  }
});

test("startup waits for the configured manifest-discovery deadline", () => {
  assert.equal(startupTimeoutMilliseconds(300), 300_000);
});

function withInput(name, value, run) {
  const key = `INPUT_${name.toUpperCase()}`;
  const previous = process.env[key];
  process.env[key] = value;
  try {
    return run();
  } finally {
    if (previous === undefined) delete process.env[key];
    else process.env[key] = previous;
  }
}

test("artifact names are trimmed and may be empty to disable publishing", () => {
  assert.equal(withInput("log-artifact", "  bazel-cache-server-log ", () => artifactName("log-artifact")), "bazel-cache-server-log");
  assert.equal(withInput("log-artifact", "", () => artifactName("log-artifact")), "");
});

test("artifact names outside the safe character set are rejected", () => {
  assert.throws(
    () => withInput("log-artifact", "../server log", () => artifactName("log-artifact")),
    /log-artifact must match/,
  );
});
