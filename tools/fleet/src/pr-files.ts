import { execFile } from "node:child_process";
import { promisify } from "node:util";

const execFileAsync = promisify(execFile);

/** Paths changed on an open PR (best-effort via gh). */
export async function listPrFiles(prNumber: number): Promise<string[]> {
  try {
    const { stdout } = await execFileAsync(
      "gh",
      ["pr", "view", String(prNumber), "--json", "files", "--jq", ".files[].path"],
      { maxBuffer: 5 * 1024 * 1024, env: process.env },
    );
    return stdout
      .split("\n")
      .map((s) => s.trim())
      .filter(Boolean);
  } catch {
    return [];
  }
}

export async function listPrFilesMany(
  prNumbers: number[],
): Promise<Map<number, string[]>> {
  const entries = await Promise.all(
    prNumbers.map(async (n) => [n, await listPrFiles(n)] as const),
  );
  return new Map(entries);
}
