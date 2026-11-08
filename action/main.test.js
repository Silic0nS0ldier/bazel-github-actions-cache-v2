"use strict";

const test = require("node:test");
const assert = require("node:assert/strict");

const { artifactName, startupTimeoutMilliseconds } = require("./main");

test("packed startup waits for its configured manifest-discovery deadline", () => {
  assert.equal(startupTimeoutMilliseconds("packs", 300), 300_000);
});

test("object startup retains the short local-server readiness deadline", () => {
  assert.equal(startupTimeoutMilliseconds("objects", 300), 15_000);
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
