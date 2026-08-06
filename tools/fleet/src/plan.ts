import { readFile } from "node:fs/promises";
import { join } from "node:path";
import {
  listCloudAgents,
  summarizeOpenPrs,
  type PrCommentSummary,
} from "@farrier/conductor";
import {
  parseLandedIds,
  parseRemainingNotes,
  parseSpecIdsInOrder,
  pickNextSpecFallbackId,
} from "./backlog.js";
import {
  chunkCombinedDueToConflict,
  foldConflictSkippedIntoSelected,
  idInCatalog,
  pickNextChunks,
} from "./chunks.js";
import {
  assignmentName,
  buildImplementerPrompt,
  buildPrFixPrompt,
  buildPrPolishPrompt,
  buildPrReviewPrompt,
} from "./briefs.js";
import { IMPLEMENTER_MODEL, REVIEWER_MODEL } from "./config.js";
import {
  batchClaimZonesFromPrPaths,
  zonesForChunkIds,
} from "./conflict-zones.js";
import {
  reservedIdsFromAgents,
  reservedIdsFromPrs,
  routeOpenPrs,
  SKIP_EXPLANATIONS,
  type PrCandidate,
  type PrSkip,
} from "./pr-candidates.js";
import { listPrFilesMany } from "./pr-files.js";
import { findRepoRoot } from "./repo-root.js";

export type ImplementAssignment = {
  kind: "implement";
  ids: string[];
  name: string;
  prompt: string;
  chunkKey?: string;
};

export type PrFixAssignment = {
  kind: "pr-fix";
  pr: PrCandidate;
  name: string;
  prompt: string;
  prUrl: string;
};

export type PrPolishAssignment = {
  kind: "pr-polish";
  pr: PrCandidate;
  name: string;
  prompt: string;
  prUrl: string;
};

export type PrReviewAssignment = {
  kind: "pr-review";
  pr: PrCandidate;
  name: string;
  prompt: string;
  prUrl: string;
};

export type FleetAssignment =
  | ImplementAssignment
  | PrFixAssignment
  | PrPolishAssignment
  | PrReviewAssignment;

/** Worker role and model for an assignment kind. Reviewers are read-only. */
export function workerFor(kind: FleetAssignment["kind"]): {
  role: "implementer" | "reviewer";
  model: string;
} {
  return kind === "pr-review"
    ? { role: "reviewer", model: REVIEWER_MODEL }
    : { role: "implementer", model: IMPLEMENTER_MODEL };
}

export type AskPause = {
  nextId: string;
  message: string;
};

export type FleetPlan = {
  requested: number;
  assignments: FleetAssignment[];
  /** Set when a fallback token is not a requirement ID — ask Evan. */
  askPause: AskPause | null;
  skipped: {
    prsWithAgentsInFlight: number;
    noReadyBacklogItems: number;
    /** Ready backlog chunks deferred so new implement work stays merge-conflict-light. */
    conflictAvoided: number;
    /** Every open PR that got no agent, with the routing reason. */
    prs: PrSkip[];
  };
};

async function readRepoDocs(repoRoot: string): Promise<{
  statusJson: string;
  functionalSpec: string;
}> {
  const [statusJson, functionalSpec] = await Promise.all([
    readFile(join(repoRoot, "docs/status.json"), "utf8"),
    readFile(join(repoRoot, "docs/functional-requirements.md"), "utf8"),
  ]);
  return { statusJson, functionalSpec };
}

function pushPrAssignment(assignments: FleetAssignment[], pr: PrCandidate): void {
  const base = { pr, prUrl: pr.url };
  const summary = {
    number: pr.number,
    url: pr.url,
    title: pr.title,
    reasons: pr.reasons,
  };

  switch (pr.taskKind) {
    case "fix":
      assignments.push({
        ...base,
        kind: "pr-fix",
        name: assignmentName("pr-fix", String(pr.number)),
        prompt: buildPrFixPrompt(summary),
      });
      return;
    case "polish":
      assignments.push({
        ...base,
        kind: "pr-polish",
        name: assignmentName("pr-polish", String(pr.number)),
        prompt: buildPrPolishPrompt(summary),
      });
      return;
    case "review":
      assignments.push({
        ...base,
        kind: "pr-review",
        name: assignmentName("pr-review", String(pr.number)),
        prompt: buildPrReviewPrompt({
          number: pr.number,
          url: pr.url,
          title: pr.title,
          headSha: pr.headSha,
        }),
      });
      return;
  }
}

/** `remaining` notes for the IDs in one chunk, in listed order. */
export function remainingFor(
  ids: string[],
  notes: Map<string, string>,
): Array<{ id: string; remaining: string }> {
  const out: Array<{ id: string; remaining: string }> = [];
  for (const id of ids) {
    const note = notes.get(id);
    if (note) out.push({ id, remaining: note });
  }
  return out;
}

/** Push full-scope single-ID implement assignments (fallback / remainder fills). */
export function appendImplementAssignments(options: {
  assignIds: string[];
  assignments: FleetAssignment[];
  n: number;
  /** Updated with zones after each push (for callers that continue planning). */
  claimedZones?: Set<string>;
  /** `partial` completion notes, keyed by ID. */
  remainingNotes?: Map<string, string>;
}): number {
  let pushed = 0;
  for (const id of options.assignIds) {
    if (options.assignments.length >= options.n) break;
    options.assignments.push({
      kind: "implement",
      ids: [id],
      chunkKey: id.toLowerCase(),
      name: assignmentName("implement", id),
      prompt: buildImplementerPrompt([id], {
        chunkTitle: id,
        chunkKey: id.toLowerCase(),
        remaining: remainingFor([id], options.remainingNotes ?? new Map()),
      }),
    });
    pushed += 1;
    if (options.claimedZones) {
      for (const z of zonesForChunkIds([id])) options.claimedZones.add(z);
    }
  }
  return pushed;
}

export async function buildFleetPlan(options: {
  n: number;
  repoRoot?: string;
  /** False when a GitHub trigger owns reviews (see routeOpenPrs). */
  includeReviews?: boolean;
  /**
   * Open PRs to plan against. Supply these when the caller already has GitHub
   * access — a Claude Code session has MCP tools but no `gh` binary, so it
   * gathers the PRs itself and hands them in. Omit to fall back to shelling
   * out to `gh`, which is what a developer laptop does.
   */
  prs?: PrCommentSummary[];
  /** Changed paths per PR, for conflict-zone claiming. Omit to fetch via `gh`. */
  prFiles?: Map<number, string[]>;
  /** In-flight external agents (Cursor cloud). Omit to query the SDK. */
  agents?: Array<{ name: string; status: string }>;
}): Promise<FleetPlan> {
  const repoRoot = options.repoRoot ?? (await findRepoRoot());
  const n = Math.max(0, options.n);

  // Deliberately not read up front: when open PRs fill every slot there is no
  // backlog planning to do, and draining the PR queue before opening new work
  // is the intended priority. A saturated run touches GitHub and nothing else.
  const [prs, agents] = await Promise.all([
    options.prs ?? summarizeOpenPrs(),
    options.agents ??
      listCloudAgents(50).catch((err: unknown) => {
        const message = err instanceof Error ? err.message : String(err);
        console.warn(
          `Warning: could not list cloud agents (${message}); assuming none in flight.`,
        );
        return [];
      }),
  ]);

  // Open PRs already exist — never skip a fix/polish for zone overlap. Agents
  // resolve merge conflicts on their own branches; serializing needy PRs only
  // leaves review feedback rotting. Conflict avoidance applies to *new*
  // implement chunks below.
  const routed = routeOpenPrs(prs, agents, {
    includeReviews: options.includeReviews ?? true,
  });
  const prsInFlight = routed.skips.filter(
    (s) => s.reason === "agent-in-flight" || s.reason === "review-in-flight",
  ).length;
  const prSelected = routed.candidates.slice(0, n);
  // Reviewers write no code, so they need no file list and claim no zones.
  const writers = prSelected.filter((p) => p.taskKind !== "review");
  const prFiles =
    options.prFiles ?? (await listPrFilesMany(writers.map((p) => p.number)));

  const assignments: FleetAssignment[] = [];
  const claimedZones = new Set<string>();
  for (const pr of prSelected) {
    pushPrAssignment(assignments, pr);
  }
  for (const pr of writers) {
    // Claim zones so backlog implementers do not pile onto modules already
    // being fixed in this batch.
    for (const z of batchClaimZonesFromPrPaths(prFiles.get(pr.number) ?? [])) {
      claimedZones.add(z);
    }
  }

  const remaining = n - assignments.length;
  let noReadyBacklogItems = 0;
  let askPause: AskPause | null = null;
  let chunksSkippedConflict = 0;

  if (remaining > 0) {
    const docs = await readRepoDocs(repoRoot);
    const landed = parseLandedIds(docs.statusJson);
    // What each `partial` ID still owes. Handed to the completion-pass agent so
    // it is told what is left rather than re-deriving it from spec vs code.
    const remainingNotes = parseRemainingNotes(docs.statusJson);
    const specIdsInOrder = parseSpecIdsInOrder(docs.functionalSpec);
    const reserved = new Set<string>([
      ...reservedIdsFromPrs(prs),
      ...reservedIdsFromAgents(agents),
    ]);

    const picked = pickNextChunks({
      count: remaining,
      landed,
      reserved,
      specIdsInOrder,
      claimedZones,
      zonesOf: (_chunk, assignIds) => zonesForChunkIds(assignIds),
    });

    // Parallel slots empty only due to shared hotspots → fold one skipped
    // ready chunk into the first implementer (sequentialize, don't abandon).
    const folded = foldConflictSkippedIntoSelected({
      selected: picked.chunks,
      skippedForConflict: picked.skippedForConflict,
    });
    const chunks = folded.selected;
    // Post-fold remainder = still deferred after co-ship absorb (reporting).
    chunksSkippedConflict = folded.skippedForConflict.length;
    // Pre-fold skips gate fallback/ASK_PAUSE: fold assigns deferred co-ship
    // partners; it must not reopen fallback assign when it clears the skip list.
    const deferredForConflict = picked.skippedForConflict.length;

    for (const chunk of chunks) {
      for (const id of chunk.assignIds) reserved.add(id);
      assignments.push({
        kind: "implement",
        ids: chunk.assignIds,
        chunkKey: chunk.key,
        name: assignmentName("implement", chunk.assignIds.join(", ")),
        prompt: buildImplementerPrompt(chunk.assignIds, {
          chunkTitle: chunk.title,
          chunkKey: chunk.key,
          remaining: remainingFor(chunk.assignIds, remainingNotes),
          ...(chunkCombinedDueToConflict(chunk.key, folded.combinedChunkKeys)
            ? { combinedDueToConflict: true }
            : {}),
        }),
      });
      for (const z of zonesForChunkIds(chunk.assignIds)) claimedZones.add(z);
    }

    const slotsLeft = n - assignments.length;
    if (slotsLeft > 0) {
      // Only consult spec-order when packing has no further *ready* work
      // (ignoring conflict skips). If ready chunks were deferred solely for
      // merge-conflict avoidance, leave slots empty and report via conflictAvoided.
      const fallback =
        deferredForConflict > 0
          ? {}
          : pickNextSpecFallbackId({
              specIdsInOrder,
              landed,
              reserved,
            });

      const remainder = resolveUnfilledBacklogSlots({
        slotsLeft,
        skippedForConflictCount: deferredForConflict,
        fallback,
      });
      askPause = remainder.askPause;
      noReadyBacklogItems = remainder.noReadyBacklogItems;

      appendImplementAssignments({
        assignIds: remainder.assignIds,
        assignments,
        n,
        claimedZones,
        remainingNotes,
      });
    }
  }

  return {
    requested: n,
    assignments,
    askPause,
    skipped: {
      prsWithAgentsInFlight: prsInFlight,
      noReadyBacklogItems,
      conflictAvoided: chunksSkippedConflict,
      prs: routed.skips,
    },
  };
}

/**
 * Decide what to do when pickNextChunks did not fill all remaining slots.
 *
 * ASK_PAUSE is reserved for malformed / non-requirement fallback IDs.
 * Normal open work is planned dynamically from status.json + spec order + dependency gates
 * — there is no static chunk catalog to be “missing” from.
 * It must not fire when ready chunks were only deferred for merge-conflict
 * avoidance in this batch — those leave slots empty and are counted under
 * conflictAvoided (no exit code 3).
 */
export function resolveUnfilledBacklogSlots(options: {
  slotsLeft: number;
  skippedForConflictCount: number;
  fallback: { id?: string };
}): {
  askPause: AskPause | null;
  noReadyBacklogItems: number;
  /** Full-scope single-ID implement assignments. */
  assignIds: string[];
} {
  if (options.slotsLeft <= 0) {
    return {
      askPause: null,
      noReadyBacklogItems: 0,
      assignIds: [],
    };
  }

  if (options.skippedForConflictCount > 0) {
    return {
      askPause: null,
      noReadyBacklogItems: 0,
      assignIds: [],
    };
  }

  const { fallback } = options;
  if (fallback.id === undefined) {
    return {
      askPause: null,
      noReadyBacklogItems: options.slotsLeft,
      assignIds: [],
    };
  }

  if (!idInCatalog(fallback.id)) {
    return {
      askPause: {
        nextId: fallback.id,
        message: [
          `ASK_PAUSE: next ready backlog token ${fallback.id} is not a requirement ID.`,
          "Paused rather than inventing work.",
          "Tell fleet how to proceed.",
        ].join(" "),
      },
      noReadyBacklogItems: 0,
      assignIds: [],
    };
  }

  // Dynamic planner already walked ready work; a remaining valid ID is a
  // full-scope single-ID implement (same bar as a one-ID packed chunk).
  return {
    askPause: null,
    noReadyBacklogItems: 0,
    assignIds: [fallback.id],
  };
}

export function formatPlan(plan: FleetPlan): string {
  const lines: string[] = [
    `Fleet plan: ${plan.assignments.length}/${plan.requested} assignments`,
  ];

  for (const skip of plan.skipped.prs) {
    lines.push(`Skipped PR #${skip.number} — ${SKIP_EXPLANATIONS[skip.reason]}`);
  }
  if (plan.skipped.conflictAvoided > 0) {
    lines.push(
      `Skipped ${plan.skipped.conflictAvoided} backlog chunk(s) to avoid likely merge conflicts in this batch`,
    );
  }
  if (plan.askPause) {
    lines.push(plan.askPause.message);
  }
  if (!plan.askPause && plan.skipped.noReadyBacklogItems > 0) {
    lines.push(
      `Could not fill ${plan.skipped.noReadyBacklogItems} slot(s) — no ready backlog IDs`,
    );
  }

  lines.push("");
  for (const [i, a] of plan.assignments.entries()) {
    const worker = workerFor(a.kind);
    lines.push(`${i + 1}. [${a.kind}] ${a.name}  (${worker.role} / ${worker.model})`);
    if (a.kind === "implement") {
      lines.push(`   IDs: ${a.ids.join(", ")}`);
      if (a.chunkKey) lines.push(`   Chunk: ${a.chunkKey}`);
    } else {
      lines.push(`   PR: ${a.prUrl}`);
      lines.push(`   Why: ${a.pr.reasons.join("; ")}`);
    }
  }

  if (plan.askPause) {
    lines.push("");
    lines.push(`Next backlog (ask): ${plan.askPause.nextId}`);
  }

  return lines.join("\n");
}
