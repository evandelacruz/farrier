import { test } from "node:test";
import assert from "node:assert/strict";
import { mkdtemp, mkdir, writeFile, rm } from "node:fs/promises";
import { tmpdir } from "node:os";
import { join } from "node:path";
import { findRepoRoot } from "./repo-root.js";

/**
 * Builds a throwaway repo root rather than asserting a literal path. The old
 * fixture hardcoded `/workspace` — the Cursor cloud checkout — so it passed
 * only inside that one runner and failed everywhere else, including locally
 * and in a Claude Code cloud session.
 */
async function makeRepoRoot(): Promise<string> {
  const root = await mkdtemp(join(tmpdir(), "pm1-repo-root-"));
  await mkdir(join(root, "docs"), { recursive: true });
  await writeFile(join(root, "CLAUDE.md"), "# CLAUDE.md\n");
  await writeFile(join(root, "docs/functional-requirements.md"), "# spec\n");
  return root;
}

test("findRepoRoot walks up from a nested package directory", async () => {
  const root = await makeRepoRoot();
  try {
    const nested = join(root, "tools", "fleet");
    await mkdir(nested, { recursive: true });
    assert.equal(await findRepoRoot(nested), root);
  } finally {
    await rm(root, { recursive: true, force: true });
  }
});

test("findRepoRoot returns the start directory when it is already the root", async () => {
  const root = await makeRepoRoot();
  try {
    assert.equal(await findRepoRoot(root), root);
  } finally {
    await rm(root, { recursive: true, force: true });
  }
});

test("findRepoRoot throws when no repo root is above the start directory", async () => {
  const bare = await mkdtemp(join(tmpdir(), "pm1-no-root-"));
  try {
    await assert.rejects(() => findRepoRoot(bare), /Could not find repo root/);
  } finally {
    await rm(bare, { recursive: true, force: true });
  }
});
