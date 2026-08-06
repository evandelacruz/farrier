import { spawnImplementer } from "@farrier/conductor";
import { DEFAULT_FLEET_MODEL } from "./config.js";
import type { FleetAssignment, FleetPlan } from "./plan.js";

export type SpawnResult = {
  assignment: FleetAssignment;
  agentId?: string;
  name?: string;
  runId?: string;
  status?: string;
  url?: string;
  error?: string;
  /** Set when the assignment was deliberately not spawned by this path. */
  skipped?: string;
};

export async function executeFleetPlan(
  plan: FleetPlan,
  options: { model?: string } = {},
): Promise<SpawnResult[]> {
  const model = options.model ?? DEFAULT_FLEET_MODEL;
  const results: SpawnResult[] = [];

  for (const assignment of plan.assignments) {
    // This legacy path spawns one kind of agent: write-capable, on the fleet
    // model. A reviewer is neither — it must be read-only and on
    // REVIEWER_MODEL. Spawning one here would hand a full write agent a brief
    // that says "do not push", which is a guardrail made of hope. Reviews are
    // fired as Routines (see .claude/skills/farrier-conductor), so refuse instead.
    if (assignment.kind === "pr-review") {
      results.push({
        assignment,
        skipped:
          "review assignments are fired via the reviewer Routine, not this spawn path",
      });
      continue;
    }

    const spawnOpts =
      assignment.kind === "implement"
        ? {
            prompt: assignment.prompt,
            ids: assignment.ids,
            name: assignment.name,
            model,
            autoCreatePR: true,
          }
        : {
            prompt: assignment.prompt,
            ids: [],
            name: assignment.name,
            model,
            prUrl: assignment.prUrl,
            autoCreatePR: false,
          };

    try {
      const result = await spawnImplementer(spawnOpts);
      results.push({
        assignment,
        agentId: result.agentId,
        name: result.name,
        ...(result.runId !== undefined ? { runId: result.runId } : {}),
        ...(result.status !== undefined ? { status: result.status } : {}),
        url: `https://cursor.com/agents/${result.agentId}`,
      });
    } catch (err) {
      results.push({
        assignment,
        error: err instanceof Error ? err.message : String(err),
      });
    }
  }

  return results;
}

export function anySpawnFailed(results: SpawnResult[]): boolean {
  return results.some((r) => r.error !== undefined);
}

export function formatSpawnResults(results: SpawnResult[]): string {
  if (results.length === 0) return "No agents spawned.";
  return results
    .map((r, i) => {
      if (r.error !== undefined) {
        return `${i + 1}. ${r.assignment.name}\n   FAILED: ${r.error}`;
      }
      if (r.skipped !== undefined) {
        return `${i + 1}. ${r.assignment.name}\n   SKIPPED: ${r.skipped}`;
      }
      return `${i + 1}. ${r.name ?? r.assignment.name}\n   ${r.url}\n   status: ${r.status ?? "started"}`;
    })
    .join("\n\n");
}
