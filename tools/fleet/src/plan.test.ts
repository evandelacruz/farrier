import { test } from "node:test";
import assert from "node:assert/strict";
import { readFile } from "node:fs/promises";
import { join } from "node:path";
import { fileURLToPath } from "node:url";
import { foldConflictSkippedIntoSelected, pickNextChunks } from "./chunks.js";
import { findRepoRoot } from "./repo-root.js";
import {
  appendImplementAssignments,
  formatPlan,
  resolveUnfilledBacklogSlots,
  type FleetAssignment,
  type FleetPlan,
} from "./plan.js";
import { batchClaimZonesFromPrPaths, zonesForChunkIds } from "./conflict-zones.js";

test("formatPlan surfaces ASK_PAUSE for uncertain chunks", () => {
  const plan: FleetPlan = {
    requested: 1,
    assignments: [],
    askPause: {
      nextId: "BLOB-001",
      message: "ASK_PAUSE: next ready backlog ID is PLAT-009",
    },
    skipped: {
      prsWithAgentsInFlight: 0,
      noReadyBacklogItems: 0,
      conflictAvoided: 0,
      prs: [],
    },
  };
  assert.match(formatPlan(plan), /ASK_PAUSE/);
  assert.match(formatPlan(plan), /PLAT-009/);
});

test("one chunk assigned + others conflict-skipped ⇒ no ASK_PAUSE", () => {
  const remainder = resolveUnfilledBacklogSlots({
    slotsLeft: 2,
    skippedForConflictCount: 10,
    // Would otherwise look like uncatalogued-frontier ambiguity — must not ASK_PAUSE.
    fallback: { id: "BLOB-001" },
  });
  assert.equal(remainder.askPause, null);
  assert.equal(remainder.noReadyBacklogItems, 0);
  assert.deepEqual(remainder.assignIds, []);
});

test("pick→fold that clears all skips still gates on pre-fold conflict count", () => {
  // KEY-001 packed alone then KEY-002 conflict-skipped → fold absorbs it.
  // Empty slots must stay quiet (conflictAvoided), not fall through to assign
  // an unrelated ID when pre-fold deferred work existed.
  const landed = new Set(["CORE-001", "CORE-002", "CORE-003"]);
  const reserved = new Set<string>();
  const claimedZones = new Set<string>();
  const picked = pickNextChunks({
    count: 1,
    landed,
    reserved,
    claimedZones,
    specIdsInOrder: ["KEY-001", "KEY-002", "DNS-001"],
    zonesOf: (_chunk, assignIds) => zonesForChunkIds(assignIds),
  });
  // Dynamic pack already includes KEY-002 with KEY-001 when both ready.
  assert.deepEqual(picked.chunks[0]?.assignIds, ["KEY-001", "KEY-002"]);

  // Simulate the old select-then-skip shape for the fold gate regression.
  const selected = [
    {
      key: "key-001",
      title: "KEY-001",
      ids: ["KEY-001"],
      assignIds: ["KEY-001"],
    },
  ];
  const skippedForConflict = [
    {
      key: "key-002",
      title: "KEY-002",
      ids: ["KEY-002"],
      assignIds: ["KEY-002"],
    },
  ];
  const folded = foldConflictSkippedIntoSelected({
    selected,
    skippedForConflict,
  });
  assert.ok(folded.foldedKeys.includes("key-002"));
  assert.equal(folded.skippedForConflict.length, 0);

  const slotsLeft = 2;
  // Post-fold length 0 would incorrectly open the fallback assign path.
  const buggy = resolveUnfilledBacklogSlots({
    slotsLeft,
    skippedForConflictCount: folded.skippedForConflict.length,
    fallback: { id: "DNS-001" },
  });
  assert.deepEqual(buggy.assignIds, ["DNS-001"], "post-fold 0 opens fallback assign");

  // Plan must gate on pre-fold deferred count — leave slots empty.
  const remainder = resolveUnfilledBacklogSlots({
    slotsLeft,
    skippedForConflictCount: skippedForConflict.length,
    fallback: { id: "DNS-001" },
  });
  assert.equal(remainder.askPause, null);
  assert.deepEqual(remainder.assignIds, []);
});

test("ASK_PAUSE only for malformed non-requirement fallback tokens", () => {
  const remainder = resolveUnfilledBacklogSlots({
    slotsLeft: 1,
    skippedForConflictCount: 0,
    fallback: { id: "NOT-A-REQ" },
  });
  assert.ok(remainder.askPause);
  assert.equal(remainder.askPause?.nextId, "NOT-A-REQ");
  assert.match(remainder.askPause!.message, /ASK_PAUSE/);
  assert.deepEqual(remainder.assignIds, []);
});

test("valid requirement fallback becomes a full-scope single-ID assign", () => {
  const remainder = resolveUnfilledBacklogSlots({
    slotsLeft: 1,
    skippedForConflictCount: 0,
    fallback: { id: "KEY-001" },
  });
  assert.equal(remainder.askPause, null);
  assert.equal(remainder.noReadyBacklogItems, 0);
  assert.deepEqual(remainder.assignIds, ["KEY-001"]);
});

test("UI fallback ID is a normal full-scope assign (no pause)", () => {
  const remainder = resolveUnfilledBacklogSlots({
    slotsLeft: 1,
    skippedForConflictCount: 0,
    fallback: { id: "KEY-003" },
  });
  assert.equal(remainder.askPause, null);
  assert.equal(remainder.noReadyBacklogItems, 0);
  assert.deepEqual(remainder.assignIds, ["KEY-003"]);

  const claimedZones = batchClaimZonesFromPrPaths([
    "internal/core/backup/snapshot.go",
  ]);
  assert.ok(claimedZones.has("core:backup"));
  const uiZones = zonesForChunkIds(["KEY-003"]);
  assert.ok(![...uiZones].some((z) => claimedZones.has(z)));

  const assignments: FleetAssignment[] = [];
  const pushed = appendImplementAssignments({
    assignIds: remainder.assignIds,
    assignments,
    n: 2,
    claimedZones,
  });
  assert.equal(pushed, 1);
  assert.equal(assignments.length, 1);
  const impl = assignments[0];
  assert.ok(impl && impl.kind === "implement");
  assert.deepEqual(impl.ids, ["KEY-003"]);
  assert.equal(impl.chunkKey, "key-003");
});

test("skill + README say dynamic planning; default one ID; explicit co-ship", async () => {
  const repoRoot = await findRepoRoot(
    join(fileURLToPath(new URL(".", import.meta.url)), ".."),
  );
  const skill = await readFile(join(repoRoot, ".cursor/skills/farrier-fleet/SKILL.md"), "utf8");
  const readme = await readFile(join(repoRoot, "tools/fleet/README.md"), "utf8");
  assert.match(skill, /work chunk/i);
  assert.match(readme, /work chunk/i);
  assert.match(skill, /No static chunk catalog/i);
  assert.match(readme, /hand-maintained/);
  assert.match(readme, /chunk catalog/);
  assert.match(skill, /one ID per agent/i);
  assert.match(readme, /one ID per agent/i);
  assert.match(skill, /KEY-001\+002/);
  assert.match(readme, /KEY-001\+002/);
  assert.doesNotMatch(skill, /allow-ui/i);
  assert.doesNotMatch(readme, /allow-ui/i);
  assert.doesNotMatch(skill, /UI_PAUSE/);
  assert.doesNotMatch(readme, /UI_PAUSE/);
  assert.doesNotMatch(skill, /≤4 IDs/);
  assert.doesNotMatch(readme, /≤4 IDs/);
});

test("skill + README never skip open PR fixes for zone overlap", async () => {
  const repoRoot = await findRepoRoot(
    join(fileURLToPath(new URL(".", import.meta.url)), ".."),
  );
  const skill = await readFile(join(repoRoot, ".cursor/skills/farrier-fleet/SKILL.md"), "utf8");
  const readme = await readFile(join(repoRoot, "tools/fleet/README.md"), "utf8");
  assert.match(readme, /never skipped for zone overlap/i);
  assert.match(skill, /never skipped for zone overlap/i);
  assert.match(readme, /new work only/i);
});

test("planning docs point --repo-root at the main worktree, and clean it with git", async () => {
  const repoRoot = await findRepoRoot(
    join(fileURLToPath(new URL(".", import.meta.url)), ".."),
  );
  const skills = await Promise.all(
    [".cursor/skills/farrier-fleet/SKILL.md", ".claude/skills/farrier-conductor/SKILL.md"].map(
      async (p) => [p, await readFile(join(repoRoot, p), "utf8")] as const,
    ),
  );

  for (const [path, skill] of skills) {
    // The backlog is read off disk, so the worktree only helps if the plan is
    // actually told to read from it. Both skills must pass the flag.
    assert.match(skill, /--repo-root/, `${path} must pass --repo-root`);
    assert.match(skill, /git worktree add --detach/, `${path} must build a worktree`);

    // rm -rf leaves .git/worktrees metadata behind and breaks the next add.
    assert.doesNotMatch(skill, /rm -rf\s+\S*main-wt/, `${path} must not rm -rf the worktree`);
    assert.match(skill, /git worktree remove --force/, `${path} must remove via git`);
    assert.match(skill, /git worktree prune/, `${path} must prune stale metadata`);

    // Undefined placeholders are unrunnable; <scratch> is the only one the
    // harness defines.
    assert.doesNotMatch(skill, /<repo>/, `${path} uses an undefined <repo> placeholder`);
  }
});
