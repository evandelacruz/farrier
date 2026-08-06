import { access } from "node:fs/promises";
import { dirname, join } from "node:path";

async function hasRepoDocs(dir: string): Promise<boolean> {
  try {
    await access(join(dir, "CLAUDE.md"));
    await access(join(dir, "docs/functional-requirements.md"));
    return true;
  } catch {
    return false;
  }
}

/** Walk up from startDir until CLAUDE.md and docs/functional-requirements.md are found. */
export async function findRepoRoot(startDir = process.cwd()): Promise<string> {
  let dir = startDir;
  for (;;) {
    if (await hasRepoDocs(dir)) return dir;
    const parent = dirname(dir);
    if (parent === dir) {
      throw new Error(
        `Could not find repo root (CLAUDE.md + docs/functional-requirements.md) from ${startDir}`,
      );
    }
    dir = parent;
  }
}
