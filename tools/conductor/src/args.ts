export type FlagMap = Map<string, string | boolean>;

/** Parse `--flag`, `--flag=value`, `--flag value`, and collect `--` rest / trailing. */
export function parseArgs(argv: string[]): { flags: FlagMap; positionals: string[] } {
  const flags: FlagMap = new Map();
  const positionals: string[] = [];

  for (let i = 0; i < argv.length; i++) {
    const arg = argv[i]!;
    if (arg === "--") {
      positionals.push(...argv.slice(i + 1));
      break;
    }
    if (arg.startsWith("--")) {
      const eq = arg.indexOf("=");
      if (eq !== -1) {
        flags.set(arg.slice(2, eq), arg.slice(eq + 1));
        continue;
      }
      const name = arg.slice(2);
      const next = argv[i + 1];
      if (next !== undefined && !next.startsWith("-")) {
        flags.set(name, next);
        i++;
      } else {
        flags.set(name, true);
      }
      continue;
    }
    if (arg.startsWith("-") && arg.length === 2) {
      const name = arg.slice(1);
      const next = argv[i + 1];
      if (next !== undefined && !next.startsWith("-")) {
        flags.set(name, next);
        i++;
      } else {
        flags.set(name, true);
      }
      continue;
    }
    positionals.push(arg);
  }

  return { flags, positionals };
}

export function flagString(flags: FlagMap, name: string): string | undefined {
  const v = flags.get(name);
  if (v === undefined || typeof v === "boolean") return undefined;
  return v;
}

export function flagBool(flags: FlagMap, name: string, defaultValue = false): boolean {
  const v = flags.get(name);
  if (v === undefined) return defaultValue;
  if (v === true) return true;
  if (v === "false" || v === "0") return false;
  return true;
}

export async function readPrompt(positionals: string[]): Promise<string> {
  const fromArgs = positionals.join(" ").trim();
  if (fromArgs) return fromArgs;

  if (process.stdin.isTTY) {
    return "";
  }

  const chunks: Buffer[] = [];
  for await (const chunk of process.stdin) {
    chunks.push(Buffer.isBuffer(chunk) ? chunk : Buffer.from(chunk));
  }
  return Buffer.concat(chunks).toString("utf8").trim();
}
