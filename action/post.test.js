"use strict";

const assert = require("node:assert/strict");
const fs = require("node:fs");
const path = require("node:path");
const test = require("node:test");

const { buildSummary } = require("./post");

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
