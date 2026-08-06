import { test } from "node:test";
import assert from "node:assert/strict";
import { readFile } from "node:fs/promises";
import { join } from "node:path";
import { fileURLToPath } from "node:url";
import {
  expandIdRange,
  extractRequirementIds,
  parseLandedIds,
  parseRemainingNotes,
  parseRequirementStatus,
  parseSpecIdsInOrder,
  pickNextRequirementIds,
  dependenciesSatisfied,
  StatusFileError,
} from "./backlog.js";
import { findRepoRoot } from "./repo-root.js";

function statusFile(requirements: Record<string, unknown>): string {
  return JSON.stringify({ requirements });
}

async function readRepoStatusJson(): Promise<string> {
  const repoRoot = await findRepoRoot(join(fileURLToPath(new URL(".", import.meta.url)), ".."));
  return readFile(join(repoRoot, "docs/status.json"), "utf8");
}

test("expandIdRange expands same-prefix numeric ranges", () => {
  assert.deepEqual(expandIdRange("BKUP-001", "BKUP-003"), [
    "BKUP-001",
    "BKUP-002",
    "BKUP-003",
  ]);
});

test("extractRequirementIds deduplicates", () => {
  assert.deepEqual(extractRequirementIds("BKUP-002 and BKUP-002 again"), ["BKUP-002"]);
});

test("parseLandedIds reads landed IDs from the status file", () => {
  const landed = parseLandedIds(
    statusFile({ "BKUP-001": "landed", "CORE-001": "landed", "RSTR-001": "open" }),
  );
  assert.ok(landed.has("BKUP-001"));
  assert.ok(landed.has("CORE-001"));
  assert.ok(!landed.has("RSTR-001"));
});

test("parseLandedIds treats partial as not landed so the completion pass gets queued", () => {
  const landed = parseLandedIds(statusFile({
      "BKUP-001": "landed",
      "BKUP-002": { state: "partial", remaining: "push-hold release on capture failure" },
    }));
  assert.ok(landed.has("BKUP-001"));
  assert.ok(!landed.has("BKUP-002"));
});

test("status is unaffected by how requirement IDs are mentioned in prose", () => {
  // The prose parser this replaced marked an ID landed because a Landed
  // section merely referred to it in passing. Status is data now.
  const landed = parseLandedIds(statusFile({ "RSTR-001": "landed", "RSTR-004": "open" }));
  assert.ok(landed.has("RSTR-001"));
  assert.ok(!landed.has("RSTR-004"));
});

test("parseRequirementStatus rejects an unknown state rather than guessing", () => {
  assert.throws(
    () => parseRequirementStatus(statusFile({ "BKUP-001": "shipped" })),
    (error: unknown) => error instanceof StatusFileError && /unknown state/.test(error.message),
  );
});

test("parseRequirementStatus rejects a malformed requirement id", () => {
  assert.throws(
    () => parseRequirementStatus(statusFile({ "ledger-1": "landed" })),
    (error: unknown) => error instanceof StatusFileError && /malformed/.test(error.message),
  );
});

test("parseRequirementStatus rejects invalid JSON and a missing requirements object", () => {
  assert.throws(() => parseRequirementStatus("{nope"), StatusFileError);
  assert.throws(() => parseRequirementStatus("{}"), StatusFileError);
});

test("repo status.json covers exactly the requirement IDs in the functional spec", async () => {
  const repoRoot = await findRepoRoot(join(fileURLToPath(new URL(".", import.meta.url)), ".."));
  const spec = await readFile(join(repoRoot, "docs/functional-requirements.md"), "utf8");
  const statuses = parseRequirementStatus(await readRepoStatusJson());

  const specIds = parseSpecIdsInOrder(spec);
  const missing = specIds.filter((id) => !statuses.has(id));
  const extra = [...statuses.keys()].filter((id) => !specIds.includes(id));

  assert.deepEqual(missing, [], "every spec requirement needs a status entry");
  assert.deepEqual(extra, [], "status entries must correspond to a spec requirement");
});

test("parseSpecIdsInOrder preserves first-seen document order", () => {
  const spec = `
- **BKUP-001** · snapshot contents
- **BKUP-002** · live capture
- **BKUP-001** · duplicate mention
- **RSTR-001** · rebuild
`;
  assert.deepEqual(parseSpecIdsInOrder(spec), ["BKUP-001", "BKUP-002", "RSTR-001"]);
});

const CORE_FOUNDATION = ["CORE-001", "CORE-002", "CORE-003"] as const;
const CAPTURE_PREREQS = [
  "KEY-001",
  "BLOB-001",
  "STATE-001",
  "STATE-002",
  "STATE-003",
  "STATE-004",
] as const;
const BACKUP_CORE = ["BKUP-001", "BKUP-003", "BKUP-004"] as const;
const RESTORE_CORE = ["RSTR-001", "RSTR-002", "RSTR-003"] as const;
const ORCH_CORE = ["ORCH-001", "ORCH-002"] as const;

test("dependenciesSatisfied blocks everything until the core foundation lands", () => {
  assert.equal(dependenciesSatisfied("STATE-001", new Set(["CORE-001"])), false);
  assert.equal(dependenciesSatisfied("STATE-001", new Set(CORE_FOUNDATION)), true);
  // The foundation IDs are never gated on themselves.
  for (const id of CORE_FOUNDATION) {
    assert.equal(dependenciesSatisfied(id, new Set()), true, `${id} must be pickable first`);
  }
});

test("dependenciesSatisfied blocks backup until there is something to capture", () => {
  const base = new Set<string>(CORE_FOUNDATION);
  assert.equal(dependenciesSatisfied("BKUP-001", base), false);
  const partial = new Set([...base, "KEY-001", "BLOB-001", "STATE-001"]);
  assert.equal(dependenciesSatisfied("BKUP-001", partial), false);
  const ready = new Set([...base, ...CAPTURE_PREREQS]);
  assert.equal(dependenciesSatisfied("BKUP-001", ready), true);
});

test("dependenciesSatisfied blocks restore until backup writes a snapshot", () => {
  const base = new Set([...CORE_FOUNDATION, ...CAPTURE_PREREQS, ...ORCH_CORE]);
  assert.equal(dependenciesSatisfied("RSTR-001", base), false);
  const ready = new Set([...base, ...BACKUP_CORE]);
  assert.equal(dependenciesSatisfied("RSTR-001", ready), true);
});

test("dependenciesSatisfied blocks promote, upgrade, and drill until restore works", () => {
  const base = new Set([
    ...CORE_FOUNDATION,
    ...CAPTURE_PREREQS,
    ...ORCH_CORE,
    ...BACKUP_CORE,
    "FORGE-004",
    "DNS-001",
  ]);
  for (const id of ["FAIL-001", "UPGR-001", "DRIL-001"]) {
    assert.equal(dependenciesSatisfied(id, base), false, `${id} needs restore`);
  }
  const ready = new Set([...base, ...RESTORE_CORE]);
  for (const id of ["FAIL-001", "UPGR-001", "DRIL-001"]) {
    assert.equal(dependenciesSatisfied(id, ready), true, `${id} ready once restore lands`);
  }
});

test("dependenciesSatisfied blocks up until the host is reachable and config renders", () => {
  const base = new Set<string>(CORE_FOUNDATION);
  assert.equal(dependenciesSatisfied("UP-001", base), false);
  assert.equal(dependenciesSatisfied("UP-001", new Set([...base, ...ORCH_CORE])), false);
  assert.equal(
    dependenciesSatisfied("UP-001", new Set([...base, ...ORCH_CORE, "FORGE-001"])),
    true,
  );
});

test("dependenciesSatisfied blocks import until the forge is deployable", () => {
  const base = new Set([...CORE_FOUNDATION, ...ORCH_CORE, "FORGE-001"]);
  assert.equal(dependenciesSatisfied("IMPT-001", base), false);
  assert.equal(dependenciesSatisfied("IMPT-001", new Set([...base, "UP-001"])), true);
});

test("dependenciesSatisfied blocks the driver-applied flip but not the print fallback", () => {
  const base = new Set([
    ...CORE_FOUNDATION,
    ...CAPTURE_PREREQS,
    ...ORCH_CORE,
    ...BACKUP_CORE,
    ...RESTORE_CORE,
    "FORGE-004",
  ]);
  assert.equal(dependenciesSatisfied("FAIL-004", base), false);
  assert.equal(dependenciesSatisfied("FAIL-004", new Set([...base, "DNS-001"])), true);
  // DNS-003 is the no-driver path, so it never waits on a driver.
  assert.equal(dependenciesSatisfied("DNS-003", new Set(CORE_FOUNDATION)), true);
});

test("dependenciesSatisfied blocks CI reconciliation until its mechanism exists", () => {
  const base = new Set([
    ...CORE_FOUNDATION,
    ...CAPTURE_PREREQS,
    ...ORCH_CORE,
    ...BACKUP_CORE,
    ...RESTORE_CORE,
  ]);
  assert.equal(dependenciesSatisfied("FAIL-003", base), false);
  assert.equal(dependenciesSatisfied("FAIL-003", new Set([...base, "FORGE-004"])), true);
});

test("dependenciesSatisfied blocks the dashboard until the local API exists", () => {
  const base = new Set<string>(CORE_FOUNDATION);
  assert.equal(dependenciesSatisfied("UI-001", base), false);
  assert.equal(dependenciesSatisfied("UI-002", base), false);
  const ready = new Set([...base, "API-001"]);
  assert.equal(dependenciesSatisfied("UI-001", ready), true);
  assert.equal(dependenciesSatisfied("API-002", ready), true);
  assert.equal(dependenciesSatisfied("API-002", base), false);
});

test("dependenciesSatisfied blocks drivers until the driver protocol lands", () => {
  const withoutProtocol = new Set(["CORE-001", "CORE-002"]);
  for (const id of ["DNS-001", "KEY-001", "BLOB-001"]) {
    assert.equal(dependenciesSatisfied(id, withoutProtocol), false, `${id} needs CORE-003`);
  }
  const ready = new Set(CORE_FOUNDATION);
  for (const id of ["DNS-001", "KEY-001", "BLOB-001"]) {
    assert.equal(dependenciesSatisfied(id, ready), true, `${id} ready with CORE-003`);
  }
});

test("dependenciesSatisfied orders the paired driver and cert requirements", () => {
  const base = new Set(CORE_FOUNDATION);
  assert.equal(dependenciesSatisfied("BLOB-002", base), false);
  assert.equal(dependenciesSatisfied("BLOB-002", new Set([...base, "BLOB-001"])), true);
  assert.equal(dependenciesSatisfied("ACME-002", base), false);
  assert.equal(dependenciesSatisfied("ACME-002", new Set([...base, "ACME-001"])), true);
  assert.equal(dependenciesSatisfied("INIT-002", base), false);
  assert.equal(dependenciesSatisfied("INIT-002", new Set([...base, "ACME-001"])), true);
});

test("pickNextRequirementIds skips landed and reserved IDs in spec order", () => {
  const specIdsInOrder = ["CORE-001", "CORE-002", "CORE-003"];
  const { ids } = pickNextRequirementIds({
    count: 2,
    specIdsInOrder,
    landed: new Set(["CORE-001"]),
    reserved: new Set(["CORE-002"]),
  });
  assert.deepEqual(ids, ["CORE-003"]);
});
