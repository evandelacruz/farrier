import { Agent } from "@cursor/sdk";
import { DEFAULT_MODEL, requireApiKey } from "./config.js";

export type FollowUpOptions = {
  agentId: string;
  prompt: string;
  model?: string;
  wait?: boolean;
};

export async function followUp(options: FollowUpOptions): Promise<{
  agentId: string;
  runId: string;
  status: string;
}> {
  const apiKey = requireApiKey();
  const agent = await Agent.resume(options.agentId, {
    apiKey,
    ...(options.model ? { model: { id: options.model } } : { model: { id: DEFAULT_MODEL } }),
  });

  const run = await agent.send(options.prompt);

  if (options.wait) {
    const result = await run.wait();
    return {
      agentId: agent.agentId,
      runId: run.id,
      status: result.status,
    };
  }

  return {
    agentId: agent.agentId,
    runId: run.id,
    status: "started",
  };
}
