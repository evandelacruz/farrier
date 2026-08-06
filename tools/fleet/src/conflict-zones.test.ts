import { test } from "node:test";
import assert from "node:assert/strict";
import {
  isSoftMergePath,
  pathZoneSet,
  prParallelZones,
  selectNonOverlapping,
  zonesForChunkIds,
  zonesFromPaths,
  zonesOverlap,
} from "./conflict-zones.js";

test("zonesFromPaths marks go.mod, the command tree and CLAUDE.md hotspots", () => {
  const zones = zonesFromPaths([
    "go.mod",
    "cmd/farrier/backup.go",
    "internal/core/backup/snapshot.go",
    "CLAUDE.md",
  ]);
  assert.ok(zones.has("hot:go-mod"));
  assert.ok(zones.has("hot:cli-commands"));
  assert.ok(zones.has("hot:claude-md"));
  assert.ok(zones.has("core:backup"));
});

test("the manifest package is a hotspot every command reads and writes", () => {
  const zones = zonesFromPaths(["internal/core/bundle/manifest.go"]);
  assert.ok(zones.has("hot:manifest-schema"));
  assert.ok(zones.has("core:bundle"));
});

test("same-domain chunks overlap; cross-domain chunks do not", () => {
  const keyA = zonesForChunkIds(["KEY-001"]);
  const keyB = zonesForChunkIds(["KEY-002"]);
  const dns = zonesForChunkIds(["DNS-001"]);
  assert.ok(zonesOverlap(keyA, keyB), "KEY shares core:keystore");
  assert.ok(!zonesOverlap(keyA, dns), "KEY vs DNS can run in parallel");
  assert.ok(!keyA.has("hot:cli-commands"), "chunks no longer claim universal hotspots");
});

test("restore-family chunks share a package and serialize", () => {
  // Promote, upgrade, and drill are all restore plus a wrapper, so two of them
  // in one batch would collide in internal/core/restore.
  const promote = zonesForChunkIds(["FAIL-001"]);
  const drill = zonesForChunkIds(["DRIL-001"]);
  assert.ok(zonesOverlap(promote, drill));
});

test("selectNonOverlapping keeps oldest non-overlapping items", () => {
  const items = [
    { id: 1, paths: ["internal/core/backup/foo.go"] },
    { id: 2, paths: ["internal/core/backup/bar.go"] }, // overlaps core:backup
    { id: 3, paths: ["internal/core/dns/baz.go"] },
  ];
  const { selected, skippedForConflict } = selectNonOverlapping({
    items,
    limit: 3,
    zonesOf: (i) => zonesFromPaths(i.paths),
  });
  assert.deepEqual(
    selected.map((s) => s.id),
    [1, 3],
  );
  assert.deepEqual(
    skippedForConflict.map((s) => s.id),
    [2],
  );
});

test("batchClaimZonesFromPrPaths ignores CLAUDE.md exclusive lock", async () => {
  const { batchClaimZonesFromPrPaths } = await import("./conflict-zones.js");
  const zones = batchClaimZonesFromPrPaths([
    "CLAUDE.md",
    "internal/core/backup/x.go",
  ]);
  assert.ok(!zones.has("hot:claude-md"));
  assert.ok(zones.has("core:backup"));
});

test("soft merge paths are excluded from PR↔PR path zones", () => {
  const soft = [
    "CLAUDE.md",
    "AGENTS.md",
    "tools/fleet/README.md",
    "docs/status.json",
    "cmd/farrier/main.go",
    "go.mod",
    "go.sum",
  ];
  for (const p of soft) assert.ok(isSoftMergePath(p), p);
  assert.ok(!isSoftMergePath("internal/core/backup/snapshot.go"));
  assert.ok(!isSoftMergePath("cmd/farrier/backup.go"));

  const paths = pathZoneSet([
    ...soft,
    "internal/core/backup/snapshot.go",
  ]);
  assert.equal(paths.size, 1);
  assert.ok(paths.has("path:internal/core/backup/snapshot.go"));
});

test("prParallelZones allows cross-domain PRs; same package still conflicts", () => {
  const dnsPr = prParallelZones([
    "cmd/farrier/main.go",
    "go.mod",
    "internal/core/dns/cloudflare.go",
  ]);
  const blobPr = prParallelZones([
    "cmd/farrier/main.go",
    "go.sum",
    "internal/core/blob/s3.go",
  ]);
  const backupA = prParallelZones(["internal/core/backup/snapshot.go"]);
  const backupB = prParallelZones(["internal/core/backup/verify.go"]);
  assert.ok(!zonesOverlap(dnsPr, blobPr), "DNS vs BLOB can fix in parallel");
  assert.ok(!dnsPr.has("hot:cli-commands"), "command registration does not serialize via hotspot");
  assert.ok(!dnsPr.has("hot:go-mod"));
  assert.ok(zonesOverlap(backupA, backupB), "two backup PRs still conflict");
});

test("prParallelZones soft-filters the command tree before zoning", () => {
  // Regression: soft paths must be dropped *before* zonesFromPaths, or
  // cmd/farrier/main.go still emits hot:cli-commands and serializes every PR
  // that registers a command.
  const softRegistry = [
    "cmd/farrier/main.go",
    "docs/status.json",
    "go.mod",
  ];
  const dnsPr = prParallelZones([
    ...softRegistry,
    "internal/core/dns/cloudflare.go",
  ]);
  const blobPr = prParallelZones([
    ...softRegistry,
    "internal/core/blob/s3.go",
  ]);
  assert.ok(!zonesOverlap(dnsPr, blobPr), "DNS vs BLOB sharing main.go can still parallelize");
  assert.ok(!dnsPr.has("hot:cli-commands"));
  assert.ok(!dnsPr.has("hot:status-json"));
  assert.ok(!dnsPr.has("hot:go-mod"));
});
