import { test } from "node:test";
import assert from "node:assert/strict";
import {
  areCoShipPartners,
  chunkCombinedDueToConflict,
  foldConflictSkippedIntoSelected,
  assignIdsForSeed,
  pickNextChunks,
  synthesizeChunk,
  WORK_CHUNKS,
} from "./chunks.js";
import { zonesForChunkIds } from "./conflict-zones.js";

const FOUNDATION = ["CORE-001", "CORE-002", "CORE-003"];

/** Minimal spec-order slice used by dynamic planner tests. */
const SPEC_SLICE = [
  "KEY-001",
  "KEY-002",
  "KEY-003",
  "DNS-001",
  "DNS-002",
  "BLOB-001",
  "BLOB-002",
  "ACME-001",
  "ACME-002",
  "ORCH-001",
];

test("default assign is a singleton (no greedy same-domain pack)", () => {
  assert.deepEqual(
    assignIdsForSeed({
      seedId: "STATE-001",
      specIdsInOrder: ["STATE-001", "STATE-002", "STATE-003", "STATE-004"],
      landed: new Set(FOUNDATION),
      reserved: new Set(),
    }),
    ["STATE-001"],
  );
});

test("KEY-001 co-ships KEY-002 only; KEY-003 stays separate", () => {
  const { chunks } = pickNextChunks({
    count: 3,
    landed: new Set(FOUNDATION),
    reserved: new Set(),
    specIdsInOrder: SPEC_SLICE,
    claimedZones: new Set(),
    zonesOf: (_chunk, assignIds) => zonesForChunkIds(assignIds),
  });
  assert.deepEqual(chunks[0]?.assignIds, ["KEY-001", "KEY-002"]);
  assert.ok(
    !chunks.some((c) => c.assignIds.includes("KEY-003")),
    "KEY-003 conflict-skips (same domain) rather than packing into the driver pair",
  );
  assert.ok(chunks.some((c) => c.assignIds[0]?.startsWith("DNS")));
  assert.ok(chunks.some((c) => c.assignIds[0]?.startsWith("BLOB")));
});

test("pickNextChunks fills parallel domains; DNS-001 co-ships DNS-002", () => {
  const landed = new Set(FOUNDATION.concat(["KEY-001", "KEY-002", "KEY-003"]));
  const { chunks } = pickNextChunks({
    count: 3,
    landed,
    reserved: new Set(),
    specIdsInOrder: SPEC_SLICE,
    claimedZones: new Set(),
    zonesOf: (_chunk, assignIds) => zonesForChunkIds(assignIds),
  });
  assert.deepEqual(
    chunks.map((c) => c.assignIds[0]?.split("-")[0]),
    ["DNS", "BLOB", "ACME"],
  );
  assert.deepEqual(chunks[0]?.assignIds, ["DNS-001", "DNS-002"]);
  assert.deepEqual(chunks[1]?.assignIds, ["BLOB-001", "BLOB-002"]);
});

test("a co-ship partner already landed leaves a singleton", () => {
  assert.deepEqual(
    assignIdsForSeed({
      seedId: "BLOB-001",
      specIdsInOrder: ["BLOB-001", "BLOB-002"],
      landed: new Set([...FOUNDATION, "BLOB-002"]),
      reserved: new Set(),
    }),
    ["BLOB-001"],
  );
});

test("a gated partner does not co-ship", () => {
  // ACME-002 needs ACME-001 landed; seeding ACME-002 alone cannot pull ACME-001
  // backwards, so it stays a singleton.
  assert.deepEqual(
    assignIdsForSeed({
      seedId: "ACME-002",
      specIdsInOrder: ["ACME-001", "ACME-002"],
      landed: new Set([...FOUNDATION, "ACME-001"]),
      reserved: new Set(),
    }),
    ["ACME-002"],
  );
});

test("unpaired IDs are singletons", () => {
  assert.deepEqual(
    assignIdsForSeed({
      seedId: "STATE-002",
      specIdsInOrder: ["STATE-001", "STATE-002", "STATE-003"],
      landed: new Set(FOUNDATION),
      reserved: new Set(),
    }),
    ["STATE-002"],
  );
});

test("areCoShipPartners is undirected for listed pairs only", () => {
  assert.equal(areCoShipPartners("KEY-001", "KEY-002"), true);
  assert.equal(areCoShipPartners("KEY-002", "KEY-001"), true);
  assert.equal(areCoShipPartners("KEY-001", "KEY-003"), false);
  assert.equal(areCoShipPartners("STATE-001", "STATE-002"), false);
});

test("fold absorbs co-ship KEY-002 into KEY-001; not KEY-003", () => {
  const folded = foldConflictSkippedIntoSelected({
    selected: [synthesizeChunk(["KEY-001"])],
    skippedForConflict: [
      synthesizeChunk(["KEY-002"]),
      synthesizeChunk(["KEY-003"]),
    ],
  });
  assert.deepEqual(folded.selected[0]?.assignIds, ["KEY-001", "KEY-002"]);
  assert.ok(folded.foldedKeys.includes("key-002"));
  assert.ok(
    folded.skippedForConflict.some((c) => c.assignIds.includes("KEY-003")),
  );
});

test("fold does not glue cross-domain neighbors", () => {
  const folded = foldConflictSkippedIntoSelected({
    selected: [synthesizeChunk(["KEY-001"])],
    skippedForConflict: [synthesizeChunk(["DNS-001", "DNS-002"])],
  });
  assert.equal(folded.foldedKeys.length, 0);
  assert.deepEqual(folded.selected[0]?.assignIds, ["KEY-001"]);
});

test("foldConflictSkippedIntoSelected is a no-op when nothing was selected", () => {
  const skipped = [synthesizeChunk(["KEY-001"])];
  const folded = foldConflictSkippedIntoSelected({
    selected: [],
    skippedForConflict: skipped,
  });
  assert.deepEqual(folded.selected, []);
  assert.equal(folded.foldedKeys.length, 0);
  assert.equal(folded.skippedForConflict.length, 1);
});

test("fold does not absorb arbitrary same-domain skips", () => {
  const folded = foldConflictSkippedIntoSelected({
    selected: [synthesizeChunk(["KEY-001", "KEY-002"])],
    skippedForConflict: [synthesizeChunk(["KEY-003"])],
  });
  assert.equal(folded.foldedKeys.length, 0);
  assert.deepEqual(folded.selected[0]?.assignIds, ["KEY-001", "KEY-002"]);
});

test("combinedDueToConflict only marks chunks that absorbed a fold", () => {
  const folded = foldConflictSkippedIntoSelected({
    selected: [
      synthesizeChunk(["KEY-001", "KEY-002"]),
      synthesizeChunk(["DNS-001"]),
    ],
    skippedForConflict: [synthesizeChunk(["DNS-002"])],
  });
  assert.deepEqual(folded.combinedChunkKeys, ["dns-001+dns-002"]);
  assert.equal(
    chunkCombinedDueToConflict("key-001+key-002", folded.combinedChunkKeys),
    false,
  );
  assert.equal(
    chunkCombinedDueToConflict("dns-001+dns-002", folded.combinedChunkKeys),
    true,
  );
});

test("KEY-001+002 claims domain; parallel DNS/BLOB/ACME still fill", () => {
  const landed = new Set(FOUNDATION);
  const picked = pickNextChunks({
    count: 6,
    landed,
    reserved: new Set(),
    specIdsInOrder: [
      "KEY-001",
      "KEY-002",
      "KEY-003",
      "DNS-001",
      "BLOB-001",
      "ACME-001",
    ],
    claimedZones: new Set(),
    zonesOf: (_chunk, assignIds) => zonesForChunkIds(assignIds),
  });
  assert.deepEqual(picked.chunks[0]?.assignIds, ["KEY-001", "KEY-002"]);
  assert.ok(
    picked.skippedForConflict.some((c) => c.assignIds.includes("KEY-003")),
  );
  const domains = picked.chunks.map((c) => c.assignIds[0]?.split("-")[0]);
  assert.ok(domains.includes("DNS"));
  assert.ok(domains.includes("BLOB"));
  assert.ok(domains.includes("ACME"));
});

test("WORK_CHUNKS is empty — planning is dynamic", () => {
  assert.equal(WORK_CHUNKS.length, 0);
});
