/**
 * Heuristic touch zones for parallel fleet assignments.
 * Goal: avoid scheduling work in the same batch that is likely to collide
 * on merge (shared packages, the CLI command tree, go.mod, CLAUDE.md).
 *
 * Zones are derived from the Go layout in docs/tech-spec.md: one zone per
 * `internal/core/<pkg>`, plus global hotspots every writer touches.
 */

/** Global hotspots — any two writers here will usually conflict. */
export const HOTSPOT_ZONES = [
  "hot:go-mod",
  "hot:cli-commands",
  "hot:manifest-schema",
  "hot:status-json",
  "hot:claude-md",
] as const;

export function zonesFromPaths(paths: string[]): Set<string> {
  const zones = new Set<string>();
  for (const raw of paths) {
    const p = raw.replace(/\\/g, "/");
    if (p === "go.mod" || p === "go.sum" || p.endsWith("/go.mod") || p.endsWith("/go.sum")) {
      zones.add("hot:go-mod");
    }
    if (p.startsWith("cmd/farrier/")) {
      zones.add("hot:cli-commands");
    }
    // The manifest type is what every command reads and writes.
    if (p.startsWith("internal/core/bundle/")) {
      zones.add("hot:manifest-schema");
    }
    if (p === "docs/status.json" || p.endsWith("/docs/status.json")) {
      zones.add("hot:status-json");
    }
    if (p === "CLAUDE.md" || p.endsWith("/CLAUDE.md")) {
      zones.add("hot:claude-md");
    }

    const corePkg = /internal\/core\/([^/]+)\//.exec(p);
    if (corePkg) zones.add(`core:${corePkg[1]}`);

    const apiPkg = /internal\/api\/([^/]+)\//.exec(p);
    if (apiPkg) zones.add(`api:${apiPkg[1]}`);

    if (p.startsWith("web/")) zones.add("web:dashboard");
    if (p.startsWith("docs/")) zones.add("docs");
  }
  return zones;
}

/**
 * Zones used when claiming batch capacity after selecting a PR.
 * Omits `hot:claude-md` — nearly every PR edits §8 and treating it as a hard
 * exclusive lock empties the batch; agents can merge CLAUDE.md carefully.
 */
export function batchClaimZonesFromPrPaths(paths: string[]): Set<string> {
  const zones = zonesFromPaths(paths);
  zones.delete("hot:claude-md");
  return zones;
}

/**
 * Paths that nearly every feature PR touches and that agents routinely merge
 * (register a command, append a status line). Used by
 * path-hardness helpers (`pathZoneSet` / `prParallelZones`); open-PR fix
 * selection does **not** skip candidates for zone overlap.
 */
export function isSoftMergePath(path: string): boolean {
  const p = path.replace(/\\/g, "/");
  if (p === "CLAUDE.md" || p.endsWith("/CLAUDE.md")) return true;
  if (p === "AGENTS.md" || p.endsWith("/AGENTS.md")) return true;
  if (p.endsWith("README.md")) return true;
  // One line per requirement, so two PRs landing different IDs merge cleanly.
  if (p === "docs/status.json") return true;
  // Command registration is one additive line per command.
  if (p === "cmd/farrier/main.go") return true;
  // Dependency manifests merge additively far more often than not.
  if (p === "go.mod" || p === "go.sum") return true;
  return false;
}

/** Exact hard-path set (soft registry paths excluded). */
export function pathZoneSet(paths: string[]): Set<string> {
  return new Set(
    paths
      .map((p) => p.replace(/\\/g, "/"))
      .filter((p) => !isSoftMergePath(p))
      .map((p) => `path:${p}`),
  );
}

/**
 * Zones that *would* overlap if we serialized PR fixes (domain/module + hard
 * paths; soft registry files ignored). Fleet does **not** use this to skip
 * open-PR fix/polish — those PRs already exist and always get agents up to `n`.
 * Kept for tests / diagnostics of path hardness.
 */
export function prParallelZones(paths: string[]): Set<string> {
  const hardPaths = paths.filter((p) => !isSoftMergePath(p));
  const zones = batchClaimZonesFromPrPaths(hardPaths);
  // The CLI command tree is an additive registration point — drop it so two
  // PRs adding different commands can co-schedule. Exact path zones still catch
  // two PRs editing the same file.
  zones.delete("hot:cli-commands");
  for (const z of pathZoneSet(hardPaths)) zones.add(z);
  return zones;
}

/**
 * Static zones for a backlog chunk.
 *
 * Domain / package zones only — not universal hotspots like the CLI command
 * tree. Claiming those for every chunk serializes the whole backlog to one
 * agent and makes `n=6` meaningless. Cross-domain chunks may still touch
 * `cmd/farrier/main.go` or `docs/status.json`; agents merge those.
 * Same-domain work still conflicts and is folded or skipped.
 */
export function zonesForChunkIds(ids: string[]): Set<string> {
  const zones = new Set<string>();

  for (const id of ids) {
    const prefix = id.split("-")[0]!;
    zones.add(`domain:${prefix}`);

    switch (prefix) {
      case "CORE":
        zones.add("core:bundle");
        zones.add("core:events");
        break;
      case "KEY":
        zones.add("core:keystore");
        break;
      case "BLOB":
        zones.add("core:blob");
        break;
      case "DNS":
        zones.add("core:dns");
        break;
      case "ACME":
        zones.add("core:acme");
        break;
      case "ORCH":
      case "UP":
        zones.add("core:orchestrate");
        break;
      case "FORGE":
      case "IMPT":
        zones.add("core:forge");
        break;
      case "STATE":
        zones.add("core:state");
        break;
      case "BKUP":
        zones.add("core:backup");
        zones.add("core:state");
        break;
      case "RSTR":
      case "FAIL":
      case "UPGR":
      case "DRIL":
        zones.add("core:restore");
        break;
      case "API":
        zones.add("api:server");
        break;
      case "UI":
        zones.add("web:dashboard");
        break;
      default:
        break;
    }
  }

  return zones;
}

export function zonesOverlap(a: Set<string>, b: Set<string>): boolean {
  for (const z of a) {
    if (b.has(z)) return true;
  }
  return false;
}

/**
 * Greedy select items that do not overlap already-chosen touch zones.
 * Preserves input order (caller sorts by priority / age first).
 */
export function selectNonOverlapping<T>(options: {
  items: T[];
  limit: number;
  zonesOf: (item: T) => Set<string>;
}): { selected: T[]; skippedForConflict: T[] } {
  const selected: T[] = [];
  const skippedForConflict: T[] = [];
  const claimed = new Set<string>();

  for (const item of options.items) {
    if (selected.length >= options.limit) break;
    const zones = options.zonesOf(item);
    if (zonesOverlap(zones, claimed)) {
      skippedForConflict.push(item);
      continue;
    }
    selected.push(item);
    for (const z of zones) claimed.add(z);
  }

  return { selected, skippedForConflict };
}
