#!/usr/bin/env node
import { flagBool, flagString, parseArgs, readPrompt } from "./args.js";
import { DEFAULT_ENV_NAME, DEFAULT_MODEL } from "./config.js";
import { followUp } from "./follow-up.js";
import { summarizeOpenPrs } from "./gh.js";
import { spawnImplementer } from "./spawn.js";
import { listCloudAgents } from "./status.js";

function printHelp(): void {
  console.log(`farrier-conductor — spawn and watch Farrier implementer agents

Usage:
  pnpm --filter @farrier/conductor spawn [options] -- <prompt>
  pnpm --filter @farrier/conductor follow-up --agent <bc-…> -- <prompt>
  pnpm --filter @farrier/conductor status
  pnpm --filter @farrier/conductor prs

spawn options:
  --ids BKUP-002,BKUP-003   Requirement IDs (comma-separated)
  --name "Backup capture"   Agent display name
  --model composer-2.5      Model id (default: ${DEFAULT_MODEL})
  --env ${DEFAULT_ENV_NAME} Named cloud environment (default)
  --repo <url>              Use explicit repo instead of named env
  --ref main                startingRef when using --repo
  --pr <url>                Attach to an existing PR
  --no-pr                   Do not auto-create a PR
  --wait                    Block until the run finishes

follow-up options:
  --agent <bc-…>            Required agent id
  --model <id>              Optional model override
  --wait                    Block until the run finishes

Environment:
  CURSOR_API_KEY            Required for spawn / follow-up / status
  CURSOR_MODEL              Default model override
`);
}

function parseIds(raw: string | undefined): string[] {
  if (!raw?.trim()) return [];
  return raw
    .split(",")
    .map((s) => s.trim())
    .filter(Boolean);
}

async function cmdSpawn(argv: string[]): Promise<void> {
  const { flags, positionals } = parseArgs(argv);
  const prompt = await readPrompt(positionals);
  if (!prompt) {
    throw new Error("spawn requires a prompt (args after -- or stdin)");
  }

  const noPr = flagBool(flags, "no-pr", false);
  const name = flagString(flags, "name");
  const model = flagString(flags, "model");
  const envName = flagString(flags, "env");
  const repoUrl = flagString(flags, "repo");
  const startingRef = flagString(flags, "ref");
  const prUrl = flagString(flags, "pr");
  const result = await spawnImplementer({
    prompt,
    ids: parseIds(flagString(flags, "ids")),
    ...(name !== undefined ? { name } : {}),
    ...(model !== undefined ? { model } : {}),
    ...(envName !== undefined ? { envName } : {}),
    ...(repoUrl !== undefined ? { repoUrl } : {}),
    ...(startingRef !== undefined ? { startingRef } : {}),
    ...(prUrl !== undefined ? { prUrl } : {}),
    autoCreatePR: !noPr,
    wait: flagBool(flags, "wait", false),
  });

  console.log(JSON.stringify(result, null, 2));
  console.log(`\nAgent: https://cursor.com/agents/${result.agentId}`);
}

async function cmdFollowUp(argv: string[]): Promise<void> {
  const { flags, positionals } = parseArgs(argv);
  const agentId = flagString(flags, "agent");
  if (!agentId) {
    throw new Error("follow-up requires --agent <bc-…>");
  }
  const prompt = await readPrompt(positionals);
  if (!prompt) {
    throw new Error("follow-up requires a prompt (args after -- or stdin)");
  }

  const model = flagString(flags, "model");
  const result = await followUp({
    agentId,
    prompt,
    ...(model !== undefined ? { model } : {}),
    wait: flagBool(flags, "wait", false),
  });

  console.log(JSON.stringify(result, null, 2));
  console.log(`\nAgent: https://cursor.com/agents/${result.agentId}`);
}

async function cmdStatus(argv: string[]): Promise<void> {
  const { flags } = parseArgs(argv);
  const limitRaw = flagString(flags, "limit");
  const limit = limitRaw ? Number(limitRaw) : 20;
  const agents = await listCloudAgents(Number.isFinite(limit) ? limit : 20);
  if (agents.length === 0) {
    console.log("No cloud agents found (SDK list is empty).");
    return;
  }
  for (const a of agents) {
    console.log([a.agentId, a.name, a.status, a.lastModified, a.url].join("\t"));
  }
}

async function cmdPrs(): Promise<void> {
  const summaries = await summarizeOpenPrs();
  if (summaries.length === 0) {
    console.log("No open PRs.");
    return;
  }

  for (const s of summaries) {
    const checks =
      s.checksOk === null ? "checks:?" : s.checksOk ? "checks:ok" : "checks:fail";
    const review = s.reviewDecision ?? "review:none";
    const draft = s.isDraft ? "draft" : "ready";
    console.log(
      [
        `#${s.number}`,
        draft,
        review,
        checks,
        s.hasMergeConflict ? "merge:conflict" : "merge:ok",
        `unresolved:${s.unresolvedReviewThreads}`,
        `comments:${s.issueComments}`,
        s.headRefName,
        s.title,
        s.url,
      ].join("\t"),
    );
  }

  const needsFix = summaries.filter(
    (s) =>
      s.hasMergeConflict ||
      s.reviewDecision === "CHANGES_REQUESTED" ||
      (s.unresolvedReviewThreads > 0 && s.reviewDecision !== "APPROVED"),
  );
  if (needsFix.length > 0) {
    console.log("\nNeeds fixer follow-up:");
    for (const s of needsFix) {
      console.log(`  #${s.number} (${s.unresolvedReviewThreads} unresolved) ${s.url}`);
    }
  }

  // An open thread on an approved PR is a nit by the reviewer's own verdict,
  // so it takes the skill's polish path — never a `--pr` fixer spawn.
  const needsPolish = summaries.filter(
    (s) => s.unresolvedReviewThreads > 0 && s.reviewDecision === "APPROVED",
  );
  if (needsPolish.length > 0) {
    console.log("\nApproved with open threads — polish, not a fixer spawn:");
    for (const s of needsPolish) {
      console.log(`  #${s.number} (${s.unresolvedReviewThreads} open) ${s.url}`);
    }
  }

  const mergeReady = summaries.filter(
    (s) =>
      !s.isDraft &&
      s.unresolvedReviewThreads === 0 &&
      s.reviewDecision === "APPROVED" &&
      s.checksOk === true,
  );
  if (mergeReady.length > 0) {
    // This filter cannot see the nits an approving reviewer left behind, so
    // the heading defers to the skill's polish pass rather than reading clean.
    console.log(
      "\nPossibly ready for Evan to merge, after an approved-PR polish check (conductor never merges):",
    );
    for (const s of mergeReady) {
      console.log(`  #${s.number} ${s.title} ${s.url}`);
    }
  }
}

async function main(): Promise<void> {
  const [command, ...rest] = process.argv.slice(2);
  if (!command || command === "-h" || command === "--help" || command === "help") {
    printHelp();
    return;
  }

  switch (command) {
    case "spawn":
      await cmdSpawn(rest);
      break;
    case "follow-up":
      await cmdFollowUp(rest);
      break;
    case "status":
      await cmdStatus(rest);
      break;
    case "prs":
      await cmdPrs();
      break;
    default:
      throw new Error(`Unknown command: ${command}\n\nRun with --help for usage.`);
  }
}

main().catch((err) => {
  const message = err instanceof Error ? err.message : String(err);
  console.error(message);
  process.exitCode = 1;
});
