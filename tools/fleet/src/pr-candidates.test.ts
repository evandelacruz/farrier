import { test } from "node:test";
import assert from "node:assert/strict";
import type { PrCommentSummary } from "@farrier/conductor";
import {
  isAgentActive,
  prHasAgentInFlight,
  effectiveReviewDecision,
  routeOpenPrs,
  routePr,
  selectPrCandidates,
} from "./pr-candidates.js";
import {
  APPROVED_LABEL,
  CHANGES_REQUESTED_LABEL,
  REVIEWING_LABEL,
  WORKING_LABEL,
} from "./config.js";

function pr(overrides: Partial<PrCommentSummary> & Pick<PrCommentSummary, "number">): PrCommentSummary {
  return {
    title: `PR ${overrides.number}`,
    url: `https://github.com/evandelacruz/farrier/pull/${overrides.number}`,
    headRefName: `cursor/pr-${overrides.number}`,
    headSha: `sha-${overrides.number}`,
    isDraft: false,
    reviewDecision: null,
    mergeable: "MERGEABLE",
    mergeStateStatus: "CLEAN",
    hasMergeConflict: false,
    unresolvedReviewThreads: 0,
    issueComments: 0,
    checksOk: true,
    labels: [],
    // Default fixture is an unreviewed head, which now routes to `review`.
    // Tests asserting fix/polish set the reviewed sha explicitly.
    reviewedShas: [`sha-${overrides.number}`],
    ...overrides,
  };
}

test("isAgentActive recognizes running-like statuses", () => {
  assert.equal(isAgentActive("running"), true);
  assert.equal(isAgentActive("waiting_for_background_work"), true);
  assert.equal(isAgentActive("FINISHED"), false);
  assert.equal(isAgentActive("idle"), false);
  assert.equal(isAgentActive("unknown"), false);
});

test("prHasAgentInFlight matches PR number in agent name", () => {
  const candidate = pr({ number: 42, title: "CORE-008: agreements" });
  assert.equal(
    prHasAgentInFlight(candidate, [{ name: "Fix PR #42", status: "running" }]),
    true,
  );
  assert.equal(
    prHasAgentInFlight(candidate, [{ name: "Implement LEDG-009", status: "running" }]),
    false,
  );
});

test("selectPrCandidates prefers oldest PRs first", () => {
  const prs = [
    pr({
      number: 45,
      hasMergeConflict: true,
      mergeable: "CONFLICTING",
      mergeStateStatus: "DIRTY",
    }),
    pr({
      number: 37,
      reviewDecision: "CHANGES_REQUESTED",
      unresolvedReviewThreads: 2,
    }),
  ];
  const selected = selectPrCandidates(prs, [], 2);
  assert.equal(selected[0]?.number, 37);
  assert.equal(selected[1]?.number, 45);
});

test("selectPrCandidates never drops same-module PRs for zone overlap", () => {
  // Fleet must assign every needy open PR up to n — conflict avoidance is
  // for new implement chunks only. Two CORE PRs both get fix agents.
  const prs = [
    pr({
      number: 53,
      title: "CORE-009 + CORE-010",
      headRefName: "cursor/core-009-010",
      reviewDecision: "CHANGES_REQUESTED",
      hasMergeConflict: true,
      mergeable: "CONFLICTING",
      mergeStateStatus: "DIRTY",
    }),
    pr({
      number: 54,
      title: "CORE-011",
      headRefName: "cursor/core-011",
      reviewDecision: "CHANGES_REQUESTED",
      hasMergeConflict: true,
      mergeable: "CONFLICTING",
      mergeStateStatus: "DIRTY",
    }),
    pr({
      number: 55,
      title: "AUTH-010 / AUTH-011",
      headRefName: "cursor/auth-010-011",
      reviewDecision: "CHANGES_REQUESTED",
    }),
  ];
  const selected = selectPrCandidates(prs, [], 6);
  assert.deepEqual(
    selected.map((c) => c.number),
    [53, 54, 55],
  );
});

test("selectPrCandidates prioritizes merge conflicts when PR numbers match", () => {
  // Degenerate same-number case only used for rank tie-break coverage.
  const conflict = pr({
    number: 10,
    hasMergeConflict: true,
    mergeable: "CONFLICTING",
    mergeStateStatus: "DIRTY",
  });
  const changes = pr({
    number: 10,
    reviewDecision: "CHANGES_REQUESTED",
    unresolvedReviewThreads: 2,
  });
  const selected = selectPrCandidates([changes, conflict], [], 2);
  assert.equal(selected[0]?.hasMergeConflict, true);
});

test("selectPrCandidates classifies approved open threads as polish", () => {
  const prs = [
    pr({
      number: 3,
      reviewDecision: "APPROVED",
      unresolvedReviewThreads: 1,
    }),
  ];
  const selected = selectPrCandidates(prs, [], 1);
  assert.equal(selected[0]?.taskKind, "polish");
});

test("selectPrCandidates skips PRs with agents in flight", () => {
  const prs = [
    pr({
      number: 4,
      reviewDecision: "CHANGES_REQUESTED",
      unresolvedReviewThreads: 1,
    }),
  ];
  const selected = selectPrCandidates(
    prs,
    [{ name: "Fix PR #4", status: "running" }],
    1,
  );
  assert.equal(selected.length, 0);
});

// --- routing table ---------------------------------------------------------

test("routePr queues a review when the head commit has no submitted review", () => {
  const route = routePr(pr({ number: 20, reviewedShas: ["sha-old"] }));
  assert.equal(route.kind, "assign");
  assert.equal(route.kind === "assign" && route.candidate.taskKind, "review");
});

test("routePr skips a head commit that was already reviewed", () => {
  const route = routePr(pr({ number: 21, reviewedShas: ["sha-21"] }));
  assert.equal(route.kind, "skip");
  assert.equal(route.kind === "skip" && route.reason, "reviewed-at-head");
});

test("routePr skips an approved PR with nothing outstanding", () => {
  // Approved *at the current head* — nothing pushed since. Only then is it
  // terminal; an approval plus a later push goes back for review.
  const route = routePr(
    pr({ number: 22, reviewDecision: "APPROVED", reviewedShas: ["sha-22"] }),
  );
  assert.equal(route.kind, "skip");
  assert.equal(route.kind === "skip" && route.reason, "approved-and-clean");
});

test("routePr fixes an approved PR that has a merge conflict", () => {
  const route = routePr(
    pr({
      number: 23,
      reviewDecision: "APPROVED",
      hasMergeConflict: true,
      mergeable: "CONFLICTING",
    }),
  );
  assert.equal(route.kind, "assign");
  assert.equal(route.kind === "assign" && route.candidate.taskKind, "fix");
});

test("routePr skips either in-flight lock label before anything else", () => {
  // Conflicting *and* locked: the lock still wins, or the conductor would
  // stack a second agent onto a PR already being fixed.
  const working = routePr(
    pr({ number: 24, labels: [WORKING_LABEL], hasMergeConflict: true }),
  );
  assert.equal(working.kind === "skip" && working.reason, "agent-in-flight");

  const reviewing = routePr(pr({ number: 25, labels: [REVIEWING_LABEL] }));
  assert.equal(reviewing.kind === "skip" && reviewing.reason, "review-in-flight");
});

test("routePr does not review a draft but still fixes one", () => {
  const idle = routePr(pr({ number: 26, isDraft: true, reviewedShas: ["sha-old"] }));
  assert.equal(idle.kind === "skip" && idle.reason, "draft");

  const broken = routePr(
    pr({ number: 27, isDraft: true, reviewDecision: "CHANGES_REQUESTED" }),
  );
  assert.equal(broken.kind === "assign" && broken.candidate.taskKind, "fix");
});

test("routePr queues a fixer on red checks but ignores absent checks", () => {
  const red = routePr(pr({ number: 28, checksOk: false }));
  assert.equal(red.kind === "assign" && red.candidate.taskKind, "fix");
  assert.ok(red.kind === "assign" && red.candidate.reasons.includes("failing checks"));

  // No CI configured yet — `null` must not read as failure.
  const none = routePr(pr({ number: 29, checksOk: null, reviewedShas: ["sha-29"] }));
  assert.equal(none.kind === "skip" && none.reason, "reviewed-at-head");
});

test("routePr keeps assigning fixers however long a PR has been round-tripping", () => {
  // There is no round cap: the loop runs until the PR converges or Evan
  // intervenes. Nothing here counts reviews, so a PR on its twentieth round
  // routes exactly like one on its first.
  const stuck = pr({
    number: 40,
    reviewDecision: "CHANGES_REQUESTED",
    unresolvedReviewThreads: 2,
  });
  const route = routePr(stuck);
  assert.equal(route.kind === "assign" && route.candidate.taskKind, "fix");
});

test("verdict labels stand in for a review decision GitHub cannot record", () => {
  // Approved by label, head already reviewed → terminal, Evan merges.
  const done = routePr(
    pr({ number: 50, labels: [APPROVED_LABEL], reviewedShas: ["sha-50"] }),
  );
  assert.equal(done.kind === "skip" && done.reason, "approved-and-clean");

  // Changes requested by label → fixer, even with no threads recorded.
  const blocked = routePr(
    pr({ number: 51, labels: [CHANGES_REQUESTED_LABEL], reviewedShas: ["sha-51"] }),
  );
  assert.equal(blocked.kind === "assign" && blocked.candidate.taskKind, "fix");
  assert.ok(
    blocked.kind === "assign" && blocked.candidate.reasons.includes("changes requested"),
  );

  // Approved label + open threads → polish, same as a real APPROVED.
  const nits = routePr(
    pr({
      number: 52,
      labels: [APPROVED_LABEL],
      unresolvedReviewThreads: 2,
      reviewedShas: ["sha-52"],
    }),
  );
  assert.equal(nits.kind === "assign" && nits.candidate.taskKind, "polish");
});

test("a real review decision outranks the labels", () => {
  const route = routePr(
    pr({
      number: 53,
      reviewDecision: "CHANGES_REQUESTED",
      labels: [APPROVED_LABEL],
      reviewedShas: ["sha-53"],
    }),
  );
  assert.equal(route.kind === "assign" && route.candidate.taskKind, "fix");

  // Both labels present: the conservative reading wins.
  assert.equal(
    effectiveReviewDecision({
      reviewDecision: null,
      labels: [APPROVED_LABEL, CHANGES_REQUESTED_LABEL],
    }),
    "CHANGES_REQUESTED",
  );
});

test("pushing after approval re-opens the PR for review", () => {
  // The label persists across pushes and cannot know a new commit landed, so
  // the head check is what stops post-approval code shipping unreviewed.
  const route = routePr(
    pr({ number: 54, labels: [APPROVED_LABEL], reviewedShas: ["sha-old"] }),
  );
  assert.equal(route.kind, "assign");
  assert.equal(route.kind === "assign" && route.candidate.taskKind, "review");
});

test("routeOpenPrs skips reviews when a trigger owns them", () => {
  const needsReview = pr({ number: 60, reviewedShas: ["sha-old"] });
  const needsFix = pr({ number: 61, hasMergeConflict: true });

  const on = routeOpenPrs([needsReview, needsFix], []);
  assert.deepEqual(
    on.candidates.map((c) => c.taskKind),
    ["review", "fix"],
  );

  const off = routeOpenPrs([needsReview, needsFix], [], { includeReviews: false });
  assert.deepEqual(
    off.candidates.map((c) => c.taskKind),
    ["fix"],
  );
  // Reported, not silently dropped — that is how you see the webhook lagging.
  assert.deepEqual(
    off.skips.map((s) => [s.number, s.reason]),
    [[60, "reviews-disabled"]],
  );
});

test("routePr polishes an approved PR with a leftover open thread", () => {
  const route = routePr(
    pr({
      number: 41,
      reviewDecision: "APPROVED",
      unresolvedReviewThreads: 1,
    }),
  );
  assert.equal(route.kind === "assign" && route.candidate.taskKind, "polish");
});

test("routeOpenPrs reports a reason for every PR it passes over", () => {
  const routed = routeOpenPrs(
    [
      pr({ number: 30, reviewDecision: "APPROVED", reviewedShas: ["sha-30"] }),
      pr({ number: 31, reviewedShas: ["sha-31"] }),
      pr({ number: 32, hasMergeConflict: true }),
    ],
    [],
  );
  assert.deepEqual(
    routed.candidates.map((c) => [c.number, c.taskKind]),
    [[32, "fix"]],
  );
  assert.deepEqual(
    routed.skips.map((s) => [s.number, s.reason]),
    [
      [30, "approved-and-clean"],
      [31, "reviewed-at-head"],
    ],
  );
});
