import { Agent } from "@cursor/sdk";
import { requireApiKey } from "./config.js";

export type CloudAgentSummary = {
  agentId: string;
  name: string;
  status: string;
  createdAt: string | undefined;
  lastModified: string;
  url: string;
};

/** Minimal listRuns shape used for status hydration (injectable in tests). */
export type ListRunsFn = (
  agentId: string,
  opts: { runtime: "cloud"; apiKey: string; limit: number },
) => Promise<{ items: Array<{ status?: string }> }>;

/**
 * Cloud `Agent.list` often omits `status` even though the type allows it.
 * When missing, resolve from the agent's latest run (`Agent.listRuns`).
 * That is what fleet/conductor use for in-flight dedupe — without it, every
 * finished agent looks the same as a running one (`unknown`) and fleet
 * re-spawns Fix PR agents on every pass.
 */
export async function resolveAgentStatus(options: {
  agentId: string;
  listedStatus: string | undefined;
  apiKey: string;
  /** Override for tests — defaults to `Agent.listRuns`. */
  listRuns?: ListRunsFn;
}): Promise<string> {
  if (options.listedStatus !== undefined && options.listedStatus.length > 0) {
    return options.listedStatus;
  }
  const listRuns = options.listRuns ?? Agent.listRuns;
  try {
    const { items } = await listRuns(options.agentId, {
      runtime: "cloud",
      apiKey: options.apiKey,
      limit: 1,
    });
    const latest = items[0];
    if (latest?.status) return latest.status;
  } catch {
    // Fall through — caller treats unknown as not-in-flight.
  }
  return "unknown";
}

export async function listCloudAgents(limit = 20): Promise<CloudAgentSummary[]> {
  const apiKey = requireApiKey();
  const { items } = await Agent.list({
    runtime: "cloud",
    apiKey,
    limit,
  });

  return Promise.all(
    items.map(async (a) => {
      const status = await resolveAgentStatus({
        agentId: a.agentId,
        listedStatus: a.status,
        apiKey,
      });
      return {
        agentId: a.agentId,
        name: a.name,
        status,
        createdAt:
          a.createdAt !== undefined ? new Date(a.createdAt).toISOString() : undefined,
        lastModified: new Date(a.lastModified).toISOString(),
        url: `https://cursor.com/agents/${a.agentId}`,
      };
    }),
  );
}
