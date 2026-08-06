import { test } from "node:test";
import assert from "node:assert/strict";
import type { PrCommentSummary } from "@farrier/conductor";
import { IMPLEMENTER_MODEL, REVIEWER_MODEL, REVIEWING_LABEL, WORKING_LABEL } from "./config.js";
import { buildDispatch, implementBranchName } from "./dispatch.js";
import type { FleetPlan } from "./plan.js";

function candidate(
  number: number,
  taskKind: "fix" | "polish" | "review",
): PrCommentSummary & { taskKind: "fix" | "polish" | "review"; reasons: string[] } {
  return {
    number,
    title: `PR ${number}`,
    url: `https://github.com/evandelacruz/farrier/pull/${number}`,
    headRefName: `cursor/pr-${number}`,
    headSha: `sha-${number}`,
    isDraft: false,
    reviewDecision: null,
    mergeable: "MERGEABLE",
    mergeStateStatus: "CLEAN",
    hasMergeConflict: false,
    unresolvedReviewThreads: 0,
    issueComments: 0,
    checksOk: true,
    labels: [],
    reviewedShas: [],
    taskKind,
    reasons: ["because"],
  };
}

function planWith(assignments: FleetPlan["assignments"]): FleetPlan {
  return {
    requested: assignments.length,
    assignments,
    askPause: null,
    skipped: {
      prsWithAgentsInFlight: 0,
      noReadyBacklogItems: 0,
      conflictAvoided: 0,
      prs: [],
    },
  };
}

test("buildDispatch routes reviews to the reviewer model and fixes to the implementer", () => {
  const manifest = buildDispatch(
    planWith([
      {
        kind: "pr-review",
        pr: candidate(10, "review"),
        name: "Review PR #10",
        prompt: "review brief",
        prUrl: "https://github.com/evandelacruz/farrier/pull/10",
      },
      {
        kind: "pr-fix",
        pr: candidate(11, "fix"),
        name: "Fix PR #11",
        prompt: "fix brief",
        prUrl: "https://github.com/evandelacruz/farrier/pull/11",
      },
    ]),
  );

  assert.deepEqual(
    manifest.items.map((i) => [i.role, i.model, i.lockLabel]),
    [
      ["reviewer", REVIEWER_MODEL, REVIEWING_LABEL],
      ["implementer", IMPLEMENTER_MODEL, WORKING_LABEL],
    ],
  );
});

test("buildDispatch tells PR workers to check out the PR and drop the lock", () => {
  const manifest = buildDispatch(
    planWith([
      {
        kind: "pr-fix",
        pr: candidate(12, "fix"),
        name: "Fix PR #12",
        prompt: "fix brief",
        prUrl: "https://github.com/evandelacruz/farrier/pull/12",
      },
    ]),
  );

  const item = manifest.items[0]!;
  assert.equal(item.branch, "cursor/pr-12");
  // Plain git, not `gh` — a Claude Code session has GitHub MCP tools and no
  // `gh` binary, so a brief that shells out to it fails on the first step.
  assert.match(item.text, /git checkout cursor\/pr-12/);
  assert.match(item.text, new RegExp(`\\\`${WORKING_LABEL}\\\` label`));
});

test("buildDispatch gives implement work a branch to create and no lock label", () => {
  const manifest = buildDispatch(
    planWith([
      {
        kind: "implement",
        ids: ["LEDG-012", "LEDG-013"],
        name: "Implement LEDG-012, LEDG-013",
        prompt: "implement brief",
      },
    ]),
  );

  const item = manifest.items[0]!;
  assert.equal(item.prNumber, null);
  assert.equal(item.lockLabel, null);
  assert.deepEqual(item.ids, ["LEDG-012", "LEDG-013"]);
  assert.match(item.text, /claude\/ledg-012-ledg-013/);
});

test("implementBranchName slugs ids and survives junk", () => {
  assert.equal(implementBranchName(["KEY-001"]), "claude/key-001");
  assert.equal(implementBranchName(["A B/C"]), "claude/abc");
  assert.equal(implementBranchName([]), "claude/farrier-work");
});
