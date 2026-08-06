import { REVIEWING_LABEL, WORKING_LABEL } from "./config.js";
import { workerFor, type FleetAssignment, type FleetPlan } from "./plan.js";

/**
 * One fired Routine. The conductor reads this manifest and, per item:
 *
 *   1. adds `lockLabel` to the PR (PR tasks only) so the next conductor pass
 *      does not re-assign work already in flight,
 *   2. calls `fire_trigger(routineId, text)` — which spawns a fresh Claude
 *      Code session running that brief.
 *
 * The manifest is plain JSON so the planning half stays headless and testable:
 * it needs `gh` and the repo, but nothing that can spawn an agent. Firing is
 * the only step that needs Routine access, and it is a loop over this array.
 */
export type DispatchItem = {
  index: number;
  kind: FleetAssignment["kind"];
  role: "implementer" | "reviewer";
  model: string;
  name: string;
  /** PR number for PR-bound work; null for a new implement chunk. */
  prNumber: number | null;
  prUrl: string | null;
  /** Requirement IDs (implement work); empty for PR tasks. */
  ids: string[];
  /** Branch the worker must be on. Null means "create one" — see `text`. */
  branch: string | null;
  /** Label the conductor adds before firing. Null when there is no PR yet. */
  lockLabel: string | null;
  /** Verbatim payload for fire_trigger's `text`. */
  text: string;
};

export type DispatchManifest = {
  requested: number;
  items: DispatchItem[];
};

export function buildDispatch(plan: FleetPlan): DispatchManifest {
  return {
    requested: plan.requested,
    items: plan.assignments.map((assignment, index) =>
      buildDispatchItem(assignment, index),
    ),
  };
}

function buildDispatchItem(assignment: FleetAssignment, index: number): DispatchItem {
  const worker = workerFor(assignment.kind);
  const common = {
    index,
    kind: assignment.kind,
    role: worker.role,
    model: worker.model,
    name: assignment.name,
  };

  if (assignment.kind === "implement") {
    return {
      ...common,
      prNumber: null,
      prUrl: null,
      ids: assignment.ids,
      branch: null,
      lockLabel: null,
      text: [
        assignment.prompt,
        "",
        "## Branch",
        `Create and stay on a branch named \`${implementBranchName(assignment.ids)}\`.`,
        "If that branch already exists on the remote, append `-2`, `-3`, … until",
        "the name is free. Never reuse another PR's branch.",
        "",
        "Open the PR yourself when the work is done — ready for review, not a",
        "draft, with the requirement IDs in the title.",
      ].join("\n"),
    };
  }

  const lockLabel = assignment.kind === "pr-review" ? REVIEWING_LABEL : WORKING_LABEL;
  return {
    ...common,
    prNumber: assignment.pr.number,
    prUrl: assignment.prUrl,
    ids: [],
    branch: assignment.pr.headRefName,
    lockLabel,
    text: [
      assignment.prompt,
      "",
      "## Branch",
      "Check out the PR branch before doing anything else:",
      "",
      "```bash",
      `git fetch origin ${assignment.pr.headRefName}`,
      `git checkout ${assignment.pr.headRefName}`,
      "```",
      "",
      "## Release the lock when you finish",
      "",
      `This PR carries the \`${lockLabel}\` label so the conductor does not assign`,
      "a second agent to it. Remove that label as your **last** action — whether",
      "you succeeded, failed, or halted with a question — using your GitHub tools.",
      "Leaving it on parks the PR until a human clears it. Label writes go through",
      "the same GitHub identity as everything else this Routine does (see",
      "`tools/fleet/README.md` — Claude Code egress replaces the auth header with",
      "an App installation token), the same channel a reviewer already uses to post",
      "its `conductor:approved` / `conductor:changes-requested` verdict label — so",
      "a reviewer that can post a verdict can drop this lock the same way.",
    ].join("\n"),
  };
}

/** `BKUP-002` + `BKUP-003` → `claude/bkup-002-bkup-003`. Stable, collision-free per batch. */
export function implementBranchName(ids: string[]): string {
  const slug = ids
    .map((id) => id.toLowerCase())
    .join("-")
    .replace(/[^a-z0-9-]/g, "");
  return `claude/${slug || "farrier-work"}`;
}
