import type { PrCommentSummary } from "@farrier/conductor";
import {
  APPROVED_LABEL,
  CHANGES_REQUESTED_LABEL,
  REVIEWING_LABEL,
  WORKING_LABEL,
} from "./config.js";

export type PrTaskKind = "fix" | "polish" | "review";

export type PrCandidate = PrCommentSummary & {
  taskKind: PrTaskKind;
  reasons: string[];
};

/** Why an open PR got no agent this pass. Reported, never silent. */
export type PrSkipReason =
  | "agent-in-flight"
  | "review-in-flight"
  | "draft"
  | "approved-and-clean"
  | "reviewed-at-head"
  | "reviews-disabled";

export type PrSkip = {
  number: number;
  url: string;
  reason: PrSkipReason;
};

export const SKIP_EXPLANATIONS: Record<PrSkipReason, string> = {
  "agent-in-flight": "an implementer/fixer is already working it",
  "review-in-flight": "a review is already in progress",
  draft: "draft — not ready for review",
  "approved-and-clean": "approved, no conflicts, nothing open — waiting on merge",
  "reviewed-at-head": "already reviewed at the current head commit",
  "reviews-disabled": "needs review, but reviews are fired by a trigger, not the conductor",
};

const ACTIVE_AGENT_STATUSES = new Set([
  "running",
  "creating",
  "queued",
  "pending",
  "in_progress",
  // Cursor cloud lifecycle (MCP / dashboard) — not idle/finished.
  "not_yet_started",
  "waiting_for_background_work",
]);

export function isAgentActive(status: string): boolean {
  return ACTIVE_AGENT_STATUSES.has(status.toLowerCase());
}

/** True when an in-flight agent already covers this PR (by number or branch). */
export function prHasAgentInFlight(
  pr: Pick<PrCommentSummary, "number" | "headRefName" | "title">,
  agents: Array<{ name: string; status: string }>,
): boolean {
  const active = agents.filter((a) => isAgentActive(a.status));
  if (active.length === 0) return false;

  const needles = [
    `#${pr.number}`,
    `PR ${pr.number}`,
    `PR#${pr.number}`,
    pr.headRefName,
    ...extractPrNumberFromTitle(pr.title),
  ].map((s) => s.toLowerCase());

  return active.some((agent) => {
    const hay = agent.name.toLowerCase();
    return needles.some((needle) => needle.length > 0 && hay.includes(needle));
  });
}

function extractPrNumberFromTitle(title: string): string[] {
  const match = /^#?(\d+)\b/.exec(title.trim());
  return match ? [`#${match[1]}`, match[1]!] : [];
}

export type PrRoute =
  | { kind: "assign"; candidate: PrCandidate }
  | { kind: "skip"; reason: PrSkipReason };

/**
 * GitHub's review decision, falling back to the conductor's verdict labels.
 *
 * A real review state always wins — if a human ever reviews a PR, or reviews
 * start coming from a separate identity, that is the truth and the labels are
 * a stale shadow of it. The labels only speak when GitHub has nothing to say,
 * which today is always (see `APPROVED_LABEL` in config.ts for why).
 *
 * `changes-requested` beats `approved` when both labels are somehow present:
 * the conservative reading is the one that keeps working on the PR.
 */
export function effectiveReviewDecision(
  pr: Pick<PrCommentSummary, "reviewDecision" | "labels">,
): string | null {
  if (pr.reviewDecision !== null) return pr.reviewDecision;
  const labels = new Set(pr.labels);
  if (labels.has(CHANGES_REQUESTED_LABEL)) return "CHANGES_REQUESTED";
  if (labels.has(APPROVED_LABEL)) return "APPROVED";
  return null;
}

/**
 * The routing table. Order matters and is the contract:
 *
 *   in-flight locks    → skip (never double-assign an agent to one PR)
 *   needs code changes → fix    (opus)
 *   approved + nits    → polish (opus)
 *   approved + clean   → skip   (done; waiting on a human to merge)
 *   draft              → skip   (drafts are not review-ready by definition)
 *   unreviewed head    → review (sonnet)
 *   reviewed head      → skip   (nothing changed since the last review)
 *
 * Fix rules run before the draft check on purpose: a draft with a merge
 * conflict or requested changes is still broken and still worth fixing. Only
 * *reviewing* a draft is wasted work.
 *
 * The review decision is read from GitHub when it exists and from the
 * conductor's verdict labels otherwise — see `effectiveReviewDecision`.
 */
export function routePr(pr: PrCommentSummary): PrRoute {
  const labels = new Set(pr.labels);
  if (labels.has(WORKING_LABEL)) return { kind: "skip", reason: "agent-in-flight" };
  if (labels.has(REVIEWING_LABEL)) return { kind: "skip", reason: "review-in-flight" };

  const decision = effectiveReviewDecision(pr);
  const reasons: string[] = [];
  let taskKind: PrTaskKind | null = null;

  if (pr.hasMergeConflict) {
    reasons.push("merge conflict");
    taskKind = "fix";
  }
  if (decision === "CHANGES_REQUESTED") {
    reasons.push("changes requested");
    taskKind = "fix";
  }
  if (pr.unresolvedReviewThreads > 0 && decision !== "APPROVED") {
    reasons.push(`${pr.unresolvedReviewThreads} unresolved review thread(s)`);
    taskKind = "fix";
  }
  // `null` means no checks for this commit, or checks still running — neither
  // is a failure, and neither must queue a fixer. Only an explicit `false`
  // does. See the gather step in .claude/skills/farrier-conductor/SKILL.md.
  if (pr.checksOk === false) {
    reasons.push("failing checks");
    taskKind = "fix";
  }
  if (pr.unresolvedReviewThreads > 0 && decision === "APPROVED" && taskKind === null) {
    reasons.push(`${pr.unresolvedReviewThreads} open thread(s) on approved PR`);
    taskKind = "polish";
  }

  if (taskKind !== null && reasons.length > 0) {
    // No round cap. Review → fix → push → review runs until it converges or
    // Evan steps in; a fixer is queued every pass for as long as something is
    // open. Deciding a PR is stuck is a human call, not a counter's.
    return { kind: "assign", candidate: { ...pr, taskKind, reasons } };
  }

  const headIsReviewed = pr.reviewedShas.includes(pr.headSha);

  // Approved *and* nothing has been pushed since: done, Evan merges.
  //
  // The head check is load-bearing. An approval is a statement about the
  // commit it was written against, so approving and then pushing more code
  // must re-open the PR for review rather than skip it — otherwise anything
  // added after approval ships unreviewed. This matters more with verdict
  // labels than it did with GitHub review states, because a label sits on the
  // PR indefinitely and does not know a new commit arrived.
  if (decision === "APPROVED" && headIsReviewed) {
    return { kind: "skip", reason: "approved-and-clean" };
  }
  if (pr.isDraft) return { kind: "skip", reason: "draft" };

  // Reviews are pinned to the commit they read, so a push naturally re-opens
  // the PR for review without any state of our own.
  if (headIsReviewed) {
    return { kind: "skip", reason: "reviewed-at-head" };
  }

  return {
    kind: "assign",
    candidate: { ...pr, taskKind: "review", reasons: ["no review at current head commit"] },
  };
}

/** Priority among same-age ties: conflicts → changes requested → fix → polish → review. */
function prCandidateRank(candidate: PrCandidate): number {
  if (candidate.hasMergeConflict) return 0;
  if (effectiveReviewDecision(candidate) === "CHANGES_REQUESTED") return 1;
  if (candidate.taskKind === "fix") return 2;
  if (candidate.taskKind === "polish") return 3;
  return 4;
}

export function selectPrCandidates(
  prs: PrCommentSummary[],
  agents: Array<{ name: string; status: string }>,
  limit: number,
): PrCandidate[] {
  return listPrCandidates(prs, agents).slice(0, limit);
}

/** All eligible PR candidates, oldest first (no limit). */
export function listPrCandidates(
  prs: PrCommentSummary[],
  agents: Array<{ name: string; status: string }>,
): PrCandidate[] {
  return routeOpenPrs(prs, agents).candidates;
}

/**
 * Route every open PR, returning both what to assign and why everything else
 * was passed over. The skips are reported rather than dropped — a conductor
 * that silently does nothing is indistinguishable from a broken one.
 */
export function routeOpenPrs(
  prs: PrCommentSummary[],
  agents: Array<{ name: string; status: string }>,
  options: {
    /**
     * False when a GitHub trigger fires the reviewer Routine directly, so the
     * conductor must not spend a slot on a review that is already coming.
     */
    includeReviews?: boolean;
  } = {},
): { candidates: PrCandidate[]; skips: PrSkip[] } {
  const includeReviews = options.includeReviews ?? true;
  const candidates: PrCandidate[] = [];
  const skips: PrSkip[] = [];

  for (const pr of prs) {
    // An external agent registry (Cursor cloud) is a second in-flight signal
    // alongside the label lock; either one claims the PR.
    if (prHasAgentInFlight(pr, agents)) {
      skips.push({ number: pr.number, url: pr.url, reason: "agent-in-flight" });
      continue;
    }
    const route = routePr(pr);
    if (route.kind === "skip") {
      skips.push({ number: pr.number, url: pr.url, reason: route.reason });
    } else if (route.candidate.taskKind === "review" && !includeReviews) {
      skips.push({ number: pr.number, url: pr.url, reason: "reviews-disabled" });
    } else {
      candidates.push(route.candidate);
    }
  }

  candidates.sort((a, b) => {
    const age = a.number - b.number;
    if (age !== 0) return age;
    return prCandidateRank(a) - prCandidateRank(b);
  });

  return { candidates, skips };
}

export function reservedIdsFromPrs(prs: PrCommentSummary[]): Set<string> {
  const reserved = new Set<string>();
  for (const pr of prs) {
    for (const id of extractRequirementIds(`${pr.title} ${pr.headRefName}`)) {
      reserved.add(id);
    }
  }
  return reserved;
}

function extractRequirementIds(text: string): string[] {
  const matches = text.matchAll(/\b([A-Z]{2,6}-\d{3})\b/g);
  return [...new Set([...matches].map((m) => m[1]!))];
}

export function reservedIdsFromAgents(
  agents: Array<{ name: string; status: string }>,
): Set<string> {
  const reserved = new Set<string>();
  for (const agent of agents.filter((a) => isAgentActive(a.status))) {
    for (const id of extractRequirementIds(agent.name)) {
      reserved.add(id);
    }
  }
  return reserved;
}
