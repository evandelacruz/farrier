import { dependenciesSatisfied } from "./backlog.js";

/**
 * Coherent work chunks — fleet assigns one chunk per implementer agent.
 *
 * Chunks are **not** a hand-maintained catalog. Each plan run derives the next
 * ready work from `docs/status.json` + functional-requirements order + the dependency gates
 * encoded in `dependenciesSatisfied`, then assigns **one ID per agent** by
 * default. Co-pack only for explicit pairs (paired drivers behind one interface) — not
 * greedy same-domain adjacency.
 */

export type WorkChunk = {
  /** Stable key for logs / reservation. */
  key: string;
  title: string;
  /** Requirement IDs that constitute this chunk — implement all of them fully. */
  ids: string[];
  /**
   * @deprecated Static catalog `requires` are gone; dependency gates + status drive readiness.
   * Kept optional so older tests/helpers typing still compile.
   */
  requires?: string[];
  /**
   * @deprecated Use `partial` in docs/status.json for completion passes.
   */
  reopen?: boolean;
};

/**
 * @deprecated Empty — dynamic planning replaced the static catalog.
 * Exported so older imports/tests that list keys fail closed on length 0.
 */
export const WORK_CHUNKS: WorkChunk[] = [];

/**
 * Soft cap on IDs in one chunk after co-ship packing / fold.
 * Default assign is 1; explicit pairs are 2.
 */
export const MAX_IDS_PER_CHUNK = 2;

/**
 * Undirected co-ship pairs — the only reason two IDs share an implementer.
 * Not “same domain prefix.”
 */
export const CO_SHIP_PAIRS: ReadonlyArray<readonly [string, string]> = [
  ["KEY-001", "KEY-002"], // both keystore drivers behind one interface
  ["DNS-001", "DNS-002"], // both DNS drivers behind one interface
  ["BLOB-001", "BLOB-002"], // both blob adapters behind one interface
  ["ACME-001", "ACME-002"], // issuance and renewal are one lego integration
  ["API-001", "API-002"], // the API surface and its event streaming
];

export function idDomain(id: string): string {
  return id.split("-")[0] ?? "";
}

export function areCoShipPartners(a: string, b: string): boolean {
  if (a === b) return false;
  return CO_SHIP_PAIRS.some(
    ([x, y]) => (x === a && y === b) || (x === b && y === a),
  );
}

/** Co-ship partners listed for `id` (usually 0 or 1). */
export function coShipPartnersOf(id: string): string[] {
  const out: string[] = [];
  for (const [x, y] of CO_SHIP_PAIRS) {
    if (x === id) out.push(y);
    else if (y === id) out.push(x);
  }
  return out;
}

export function synthesizeChunk(assignIds: string[]): WorkChunk & { assignIds: string[] } {
  const key = assignIds.map((id) => id.toLowerCase()).join("+");
  const title =
    assignIds.length === 1
      ? assignIds[0]!
      : `${assignIds[0]}… (${assignIds.length} IDs)`;
  return {
    key,
    title,
    ids: [...assignIds],
    assignIds: [...assignIds],
  };
}

/**
 * IDs still to implement: anything not in `landed`.
 * `partial` is excluded from landed by parseLandedIds, so completion passes queue.
 */
export function chunkUnlandedIds(chunk: WorkChunk, landed: Set<string>): string[] {
  if (chunk.reopen) return [...chunk.ids];
  return chunk.ids.filter((id) => !landed.has(id));
}

/**
 * @deprecated Prefer readiness via `listReadyIds` / `pickNextChunks`.
 * True when every id is still open/partial and dependency gates hold for the first id.
 */
export function chunkIsReady(
  chunk: WorkChunk,
  landed: Set<string>,
  reserved: Set<string>,
): boolean {
  const remaining = chunkUnlandedIds(chunk, landed);
  if (remaining.length === 0) return false;
  if (remaining.some((id) => reserved.has(id))) return false;
  const provisional = new Set(landed);
  for (const id of remaining) {
    if (!dependenciesSatisfied(id, provisional)) return false;
    provisional.add(id);
  }
  return true;
}

/**
 * Spec-order IDs that are not landed, not reserved, and pass the dependency gates
 * (treating `alsoLanding` as already satisfied for co-assigned packs).
 */
export function listReadyIds(options: {
  specIdsInOrder: string[];
  landed: Set<string>;
  reserved: Set<string>;
  alsoLanding?: Set<string>;
}): string[] {
  const effectiveLanded = new Set(options.landed);
  for (const id of options.alsoLanding ?? []) effectiveLanded.add(id);
  const ready: string[] = [];
  for (const id of options.specIdsInOrder) {
    if (options.landed.has(id)) continue;
    if (options.reserved.has(id)) continue;
    if (!dependenciesSatisfied(id, effectiveLanded)) continue;
    ready.push(id);
  }
  return ready;
}

/**
 * Build the assign list for a seed ID.
 *
 * Default: singleton. Optionally append ready co-ship partners (explicit pairs
 * only). Order follows `specIdsInOrder`.
 */
export function assignIdsForSeed(options: {
  seedId: string;
  specIdsInOrder: string[];
  landed: Set<string>;
  reserved: Set<string>;
  maxIds?: number;
}): string[] {
  const maxIds = options.maxIds ?? MAX_IDS_PER_CHUNK;
  const assignIds = [options.seedId];
  const alsoLanding = new Set<string>([options.seedId]);
  const specIndex = new Map(options.specIdsInOrder.map((id, i) => [id, i]));

  for (const partner of coShipPartnersOf(options.seedId)) {
    if (assignIds.length >= maxIds) break;
    if (options.landed.has(partner)) continue;
    if (options.reserved.has(partner)) continue;
    if (!specIndex.has(partner)) continue;
    if (!dependenciesSatisfied(partner, new Set([...options.landed, ...alsoLanding]))) {
      continue;
    }
    assignIds.push(partner);
    alsoLanding.add(partner);
  }

  assignIds.sort((a, b) => (specIndex.get(a) ?? 0) - (specIndex.get(b) ?? 0));
  return assignIds;
}

/**
 * Next ready chunks from status + spec order + dependency gates (dependency / priority order).
 * Skips chunks that overlap `claimedZones` (parallel merge-conflict avoidance).
 */
export function pickNextChunks(options: {
  count: number;
  landed: Set<string>;
  reserved: Set<string>;
  /** Spec-order IDs from functional-requirements.md — required for dynamic planning. */
  specIdsInOrder?: string[];
  /** Zones already claimed by PR fixes / earlier chunks in this batch. */
  claimedZones?: Set<string>;
  zonesOf?: (chunk: WorkChunk, assignIds: string[]) => Set<string>;
  maxIdsPerChunk?: number;
}): {
  chunks: Array<WorkChunk & { assignIds: string[] }>;
  skippedForConflict: Array<WorkChunk & { assignIds: string[] }>;
  /** Ready work exhausted; caller may fall back to spec-order singles. */
  catalogExhausted: boolean;
} {
  const chunks: Array<WorkChunk & { assignIds: string[] }> = [];
  const skippedForConflict: Array<WorkChunk & { assignIds: string[] }> = [];
  const reserved = options.reserved;
  const claimed = options.claimedZones ?? new Set<string>();
  const specIdsInOrder = options.specIdsInOrder ?? [];
  const maxIds = options.maxIdsPerChunk ?? MAX_IDS_PER_CHUNK;
  const zonesOf =
    options.zonesOf ??
    ((chunk: WorkChunk, assignIds: string[]) => {
      void chunk;
      return new Set(assignIds.map((id) => `domain:${idDomain(id)}`));
    });

  if (specIdsInOrder.length === 0) {
    return {
      chunks,
      skippedForConflict,
      catalogExhausted: true,
    };
  }

  const considered = new Set<string>();

  for (const seedId of specIdsInOrder) {
    if (chunks.length >= options.count) break;
    if (options.landed.has(seedId)) continue;
    if (reserved.has(seedId)) continue;
    if (considered.has(seedId)) continue;
    if (!dependenciesSatisfied(seedId, options.landed)) continue;

    const assignIds = assignIdsForSeed({
      seedId,
      specIdsInOrder,
      landed: options.landed,
      reserved,
      maxIds,
    });
    for (const id of assignIds) considered.add(id);

    const chunk = synthesizeChunk(assignIds);
    const zones = zonesOf(chunk, assignIds);
    let overlaps = false;
    for (const z of zones) {
      if (claimed.has(z)) {
        overlaps = true;
        break;
      }
    }
    if (overlaps) {
      skippedForConflict.push(chunk);
      continue;
    }

    chunks.push(chunk);
    for (const id of assignIds) reserved.add(id);
    for (const z of zones) claimed.add(z);
  }

  return {
    chunks,
    skippedForConflict,
    catalogExhausted: chunks.length < options.count,
  };
}

export type ReadyChunk = WorkChunk & { assignIds: string[] };

/** Requirement-prefix domain for a chunk (CORE, BKUP, …). */
export function chunkDomain(chunk: Pick<ReadyChunk, "assignIds" | "ids">): string {
  const id = chunk.assignIds?.[0] ?? chunk.ids[0] ?? "";
  return idDomain(id);
}

function chunkIsCoShipOf(primary: ReadyChunk, skipped: ReadyChunk): boolean {
  return skipped.assignIds.every((sid) =>
    primary.assignIds.some((pid) => areCoShipPartners(pid, sid)),
  );
}

/**
 * Fold co-ship conflict-skipped chunks into each selected agent.
 *
 * Only absorbs a skip when it is an explicit co-ship partner of the selected
 * chunk (e.g. AUTH-007 selected, AUTH-008 skipped). Does not glue arbitrary
 * same-domain neighbors — that re-inflates PRs.
 *
 * Conservative: at most one extra chunk per selected agent, hard ID cap.
 */
export function foldConflictSkippedIntoSelected(options: {
  selected: ReadyChunk[];
  skippedForConflict: ReadyChunk[];
  /** Extra chunks to fold into each selected agent (default 1). */
  maxExtraChunks?: number;
  /** Cap on total requirement IDs after folding (default MAX_IDS_PER_CHUNK). */
  maxTotalIds?: number;
}): {
  selected: ReadyChunk[];
  skippedForConflict: ReadyChunk[];
  /** Keys of skipped chunks that were absorbed. */
  foldedKeys: string[];
  /** Keys of *selected* chunks that absorbed at least one fold (not every `+` pack). */
  combinedChunkKeys: string[];
} {
  const maxExtra = options.maxExtraChunks ?? 1;
  const maxIds = options.maxTotalIds ?? MAX_IDS_PER_CHUNK;
  if (options.selected.length === 0 || options.skippedForConflict.length === 0 || maxExtra <= 0) {
    return {
      selected: options.selected,
      skippedForConflict: options.skippedForConflict,
      foldedKeys: [],
      combinedChunkKeys: [],
    };
  }

  const remainingSkipped = [...options.skippedForConflict];
  const foldedKeys: string[] = [];
  const combinedChunkKeys: string[] = [];
  const nextSelected: ReadyChunk[] = [];

  for (const sel of options.selected) {
    const primary = { ...sel, assignIds: [...sel.assignIds] };
    const titles = [primary.title];
    const localFolded: string[] = [];

    while (localFolded.length < maxExtra) {
      const idx = remainingSkipped.findIndex(
        (s) =>
          chunkIsCoShipOf(primary, s) &&
          primary.assignIds.length + s.assignIds.length <= maxIds,
      );
      if (idx < 0) break;
      const [next] = remainingSkipped.splice(idx, 1);
      if (!next) break;
      primary.assignIds.push(...next.assignIds);
      localFolded.push(next.key);
      titles.push(next.title);
    }

    if (localFolded.length === 0) {
      nextSelected.push(sel);
    } else {
      foldedKeys.push(...localFolded);
      const combinedKey = [primary.key, ...localFolded].join("+");
      combinedChunkKeys.push(combinedKey);
      nextSelected.push({
        ...primary,
        key: combinedKey,
        title: titles.join(" + "),
      });
    }
  }

  return {
    selected: nextSelected,
    skippedForConflict: remainingSkipped,
    foldedKeys,
    combinedChunkKeys,
  };
}

/** True when this chunk's key is one that absorbed a conflict fold (not merely packed). */
export function chunkCombinedDueToConflict(
  chunkKey: string | undefined,
  combinedChunkKeys: readonly string[],
): boolean {
  if (!chunkKey || combinedChunkKeys.length === 0) return false;
  return combinedChunkKeys.includes(chunkKey);
}

/**
 * Formerly “is this ID in the static catalog?” — always true for well-formed
 * requirement IDs. Fleet no longer refuses work for missing catalog rows.
 */
export function idInCatalog(id: string): boolean {
  return /^[A-Z]{2,6}-\d{3}$/.test(id);
}
