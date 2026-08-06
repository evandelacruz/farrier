#!/usr/bin/env node
import { mkdir, readFile, writeFile } from "node:fs/promises";
import { join, resolve } from "node:path";
import {
  flagBool,
  flagString,
  parseArgs,
  type PrCommentSummary,
} from "@farrier/conductor";
import { DEFAULT_FLEET_MODEL } from "./config.js";
import { buildDispatch, type DispatchManifest } from "./dispatch.js";
import { buildFleetPlan, formatPlan } from "./plan.js";
import { executeFleetPlan, anySpawnFailed, formatSpawnResults } from "./run.js";

function printHelp(): void {
  console.log(`farrier-fleet — plan n agent assignments across open PRs and work chunks

Usage:
  pnpm --filter @farrier/fleet plan -- --n <count> [--out <dir>]
  pnpm --filter @farrier/fleet fleet -- --n <count>

Options:
  --n <count>       Number of agents to assign (required)
  --prs <file>      JSON array of open PRs (skips \`gh\`; see README)
  --out <dir>       Write the dispatch manifest for the conductor to fire
  --no-reviews      Do not queue reviews (a GitHub trigger fires them instead)
  --model <id>      Cursor cloud agent model (default: ${DEFAULT_FLEET_MODEL})
  --json            Also print the plan as JSON (plan command)
  --dry-run         Plan only; do not spawn (run command only; plan always dry)
  --repo-root <p>   Repo root for docs/ (default: cwd)

Environment:
  CURSOR_API_KEY    Required only by \`fleet\` (legacy Cursor spawn path)
  gh                Authenticated for PR inspection (all commands)

The Routine-based conductor uses \`plan --out\`; see tools/fleet/README.md.
`);
}

/**
 * Open PRs supplied by the caller instead of fetched with `gh`.
 *
 * A Claude Code session has GitHub MCP tools and no `gh` binary, so the
 * conductor skill gathers PRs itself and passes them here. The shape is
 * `PrCommentSummary[]` — the same thing `summarizeOpenPrs()` returns.
 */
async function readPrsFile(path: string): Promise<PrCommentSummary[]> {
  const raw = await readFile(resolve(path), "utf8");
  const parsed: unknown = JSON.parse(raw);
  if (!Array.isArray(parsed)) {
    throw new Error(`--prs ${path} must contain a JSON array of open PRs`);
  }
  return parsed as PrCommentSummary[];
}

async function writeManifest(dir: string, manifest: DispatchManifest): Promise<string> {
  const outDir = resolve(dir);
  await mkdir(outDir, { recursive: true });
  const path = join(outDir, "dispatch.json");
  await writeFile(path, `${JSON.stringify(manifest, null, 2)}\n`, "utf8");
  return path;
}

function parseCount(flags: Map<string, string | boolean>): number {
  const raw = flagString(flags, "n");
  if (!raw?.trim()) {
    throw new Error("--n <count> is required");
  }
  const n = Number(raw);
  if (!Number.isInteger(n) || n < 1) {
    throw new Error("--n must be a positive integer");
  }
  return n;
}

function stripPassThrough(argv: string[]): string[] {
  return argv[0] === "--" ? argv.slice(1) : argv;
}

async function cmdPlan(argv: string[]): Promise<void> {
  const { flags } = parseArgs(stripPassThrough(argv));
  const n = parseCount(flags);
  const repoRoot = flagString(flags, "repo-root");
  const out = flagString(flags, "out");
  const prsFile = flagString(flags, "prs");

  const plan = await buildFleetPlan({
    n,
    ...(repoRoot !== undefined ? { repoRoot } : {}),
    ...(prsFile !== undefined
      ? { prs: await readPrsFile(prsFile), agents: [] }
      : {}),
    includeReviews: !flagBool(flags, "no-reviews", false),
  });
  console.log(formatPlan(plan));
  if (plan.askPause) {
    console.warn(`\n${plan.askPause.message}`);
  } else if (plan.skipped.noReadyBacklogItems > 0) {
    console.warn(
      `Note: could not fill ${plan.skipped.noReadyBacklogItems} slot(s) — no ready backlog IDs`,
    );
  }

  if (out !== undefined) {
    const path = await writeManifest(out, buildDispatch(plan));
    console.log(`\nDispatch manifest: ${path}`);
  }

  if (flagBool(flags, "json", false)) {
    console.log("\n" + JSON.stringify(plan, null, 2));
  }
}

async function cmdRun(argv: string[]): Promise<void> {
  const { flags } = parseArgs(stripPassThrough(argv));
  const n = parseCount(flags);
  const dryRun = flagBool(flags, "dry-run", false);
  const repoRoot = flagString(flags, "repo-root");
  const model = flagString(flags, "model") ?? DEFAULT_FLEET_MODEL;

  const plan = await buildFleetPlan({
    n,
    ...(repoRoot !== undefined ? { repoRoot } : {}),
  });
  console.log(formatPlan(plan));

  if (plan.askPause) {
    console.warn(`\n${plan.askPause.message}`);
    if (plan.assignments.length === 0) {
      console.warn("Nothing to spawn until you answer in chat.");
      process.exitCode = 3;
      return;
    }
    console.warn(
      `Spawning ${plan.assignments.length} clear assignment(s); asking before further backlog.`,
    );
  }

  if (plan.assignments.length === 0) {
    console.log("\nNothing to spawn.");
    return;
  }

  if (dryRun) {
    console.log(`\nDry run — no agents spawned (would use model: ${model}).`);
    if (plan.askPause) process.exitCode = 3;
    return;
  }

  console.log(`\nSpawning agents with model ${model}…`);
  const results = await executeFleetPlan(plan, { model });
  console.log("\n" + formatSpawnResults(results));
  if (anySpawnFailed(results)) {
    process.exitCode = 1;
  } else if (plan.askPause) {
    process.exitCode = 3;
  }
}

async function main(): Promise<void> {
  const [command, ...rest] = process.argv.slice(2);
  if (!command || command === "-h" || command === "--help" || command === "help") {
    printHelp();
    return;
  }

  switch (command) {
    case "plan":
      await cmdPlan(rest);
      break;
    case "run":
      await cmdRun(rest);
      break;
    default:
      throw new Error(`Unknown command: ${command}\n\nRun with --help for usage.`);
  }
}

main()
  .catch((err) => {
    const message = err instanceof Error ? err.message : String(err);
    console.error(message);
    process.exitCode = 1;
  })
  .finally(() => {
    // Cloud Agent SDK can leave open handles after spawn; force exit once work is done.
    process.exit(process.exitCode ?? 0);
  });
