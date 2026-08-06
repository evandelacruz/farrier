import { Agent } from "@cursor/sdk";
import {
  DEFAULT_ENV_NAME,
  DEFAULT_MODEL,
  DEFAULT_REPO_URL,
  DEFAULT_STARTING_REF,
  requireApiKey,
} from "./config.js";

export type SpawnOptions = {
  prompt: string;
  ids: string[];
  name?: string;
  model?: string;
  /** Named Cursor cloud environment (default: evandelacruz/farrier). */
  envName?: string;
  /** If set, use explicit repos instead of a named environment. */
  repoUrl?: string;
  startingRef?: string;
  autoCreatePR?: boolean;
  /** Attach to an existing PR (uses repos + prUrl; ignores named env). */
  prUrl?: string;
  wait?: boolean;
};

function buildName(ids: string[], name: string | undefined): string {
  if (name?.trim()) return name.trim();
  if (ids.length > 0) return `Implement ${ids.join(", ")}`;
  return "Farrier implementer";
}

function prependIds(prompt: string, ids: string[]): string {
  if (ids.length === 0) return prompt;
  const header = [
    `Requirement IDs: ${ids.join(", ")}`,
    "Cite these IDs in commits and the PR body.",
    "Read CLAUDE.md and docs/functional-requirements.md for these IDs before coding.",
    "If blocked by an open architecture or stack question — halt and print why. Do not invent, and do not renegotiate a decision in docs/ on your own.",
    "",
  ].join("\n");
  return `${header}${prompt}`;
}

export async function spawnImplementer(options: SpawnOptions): Promise<{
  agentId: string;
  name: string;
  runId?: string;
  status?: string;
}> {
  const apiKey = requireApiKey();
  const modelId = options.model ?? DEFAULT_MODEL;
  const name = buildName(options.ids, options.name);
  const prompt = prependIds(options.prompt, options.ids);
  const autoCreatePR = options.autoCreatePR ?? true;

  // Named Cursor environments already bind the repo; repos + env.name are mutually exclusive.
  const useRepo = options.prUrl !== undefined || options.repoUrl !== undefined;

  const cloud = useRepo
    ? {
        repos: [
          {
            url: options.repoUrl ?? DEFAULT_REPO_URL,
            startingRef: options.startingRef ?? DEFAULT_STARTING_REF,
            ...(options.prUrl ? { prUrl: options.prUrl } : {}),
          },
        ],
        autoCreatePR: options.prUrl ? false : autoCreatePR,
      }
    : {
        env: { type: "cloud" as const, name: options.envName ?? DEFAULT_ENV_NAME },
        autoCreatePR,
      };

  const agent = await Agent.create({
    apiKey,
    name,
    model: { id: modelId },
    cloud,
  });

  const run = await agent.send(prompt);

  if (options.wait) {
    const result = await run.wait();
    try {
      agent.close();
    } catch {
      // Best-effort; cloud run already completed.
    }
    return {
      agentId: agent.agentId,
      name,
      runId: run.id,
      status: result.status,
    };
  }

  // Detach: drop local SDK handles so the Node process can exit; cloud continues.
  try {
    agent.close();
  } catch {
    // Best-effort; spawn already succeeded.
  }

  return {
    agentId: agent.agentId,
    name,
    runId: run.id,
    status: "started",
  };
}
