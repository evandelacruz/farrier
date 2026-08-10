/** Extract requirement IDs like BKUP-002 from free text. */
export function extractRequirementIds(text: string): string[] {
  const matches = text.matchAll(/\b([A-Z]{2,6}-\d{3})\b/g);
  return [...new Set([...matches].map((m) => m[1]!))];
}

/** Expand `BKUP-001`–`BKUP-005` style ranges (same prefix). */
export function expandIdRange(startId: string, endId: string): string[] {
  const startMatch = /^([A-Z]+)-(\d+)$/.exec(startId);
  const endMatch = /^([A-Z]+)-(\d+)$/.exec(endId);
  if (!startMatch || !endMatch || startMatch[1] !== endMatch[1]) {
    return [startId, endId];
  }
  const prefix = startMatch[1]!;
  const from = Number(startMatch[2]);
  const to = Number(endMatch[2]);
  if (!Number.isFinite(from) || !Number.isFinite(to) || from > to) {
    return [startId, endId];
  }
  const ids: string[] = [];
  for (let n = from; n <= to; n++) {
    ids.push(`${prefix}-${String(n).padStart(3, "0")}`);
  }
  return ids;
}

/** Delivery state of one requirement in `docs/status.json`. */
export type RequirementState = "landed" | "partial" | "open";

const REQUIREMENT_STATES: readonly RequirementState[] = ["landed", "partial", "open"];

/**
 * A `partial` entry, which must say what is left.
 *
 * The note is the one thing a later pass cannot recover by reading the repo.
 * Code and tests already describe what a requirement *does*; nothing in them
 * describes what was deliberately left out, or what was left out on purpose
 * and must stay that way. The agent that sliced the ID knows both for free —
 * the one picking it up would have to re-derive them from the spec.
 */
export type PartialRequirement = {
  state: "partial";
  /** One line: what remains before this ID is `landed`. */
  remaining: string;
};

export type RequirementStatusValue = RequirementState | PartialRequirement;

export type RequirementStatusFile = {
  requirements: Record<string, RequirementStatusValue>;
};

export class StatusFileError extends Error {
  override readonly name = "StatusFileError";
}

/**
 * Parse `docs/status.json` — the authoritative record of what has shipped.
 *
 * This used to scrape backticked IDs out of CLAUDE.md status prose, which made
 * paragraph wording load-bearing: a forward reference like "future `AUTH-014`
 * elevation" inside the Landed section marked AUTH-014 shipped even though the
 * same section listed it as unbuilt, so fleet would never queue it. Status is
 * data now, and there is no prose companion to drift from it.
 *
 * Two accepted value shapes per ID:
 *
 * - `"landed"` / `"open"` — nothing more to say. Code and tests describe what
 *   landed work does better than a paragraph would, and keep describing it
 *   after the paragraph goes stale.
 * - `{ "state": "partial", "remaining": "..." }` — a deliberate slice, and the
 *   one line saying what is left.
 *
 * A bare `"partial"` is rejected on purpose. The note is the whole reason the
 * object form exists, so allowing the state without it would let the useful
 * half be dropped silently — the same reasoning that puts constraints in the
 * database rather than in a convention.
 *
 * Unknown states throw rather than defaulting: silently treating a typo as
 * `open` re-queues shipped work, and as `landed` strands real work forever.
 */
export function parseRequirementStatus(json: string): Map<string, RequirementState> {
  let parsed: unknown;
  try {
    parsed = JSON.parse(json);
  } catch (error) {
    throw new StatusFileError(
      `docs/status.json is not valid JSON: ${error instanceof Error ? error.message : error}`,
    );
  }

  const requirements = (parsed as Partial<RequirementStatusFile> | null)?.requirements;
  if (typeof requirements !== "object" || requirements === null) {
    throw new StatusFileError('docs/status.json must have a "requirements" object.');
  }

  const statuses = new Map<string, RequirementState>();
  for (const [id, value] of Object.entries(requirements)) {
    if (!/^[A-Z]{2,6}-\d{3}$/.test(id)) {
      throw new StatusFileError(`docs/status.json has a malformed requirement id: ${id}`);
    }
    statuses.set(id, parseStatusValue(id, value));
  }
  return statuses;
}

function parseStatusValue(id: string, value: unknown): RequirementState {
  if (typeof value === "string") {
    if (value === "partial") {
      throw new StatusFileError(
        `docs/status.json marks ${id} "partial" with no note. Use ` +
          `{"state":"partial","remaining":"<what is left>"} — the note is the ` +
          `only part a later pass cannot work out by reading the repo.`,
      );
    }
    if (!REQUIREMENT_STATES.includes(value as RequirementState)) {
      throw new StatusFileError(
        `docs/status.json has an unknown state for ${id}: ${JSON.stringify(value)}. ` +
          `Expected one of ${REQUIREMENT_STATES.join(", ")}.`,
      );
    }
    return value as RequirementState;
  }

  if (typeof value === "object" && value !== null && !Array.isArray(value)) {
    const obj = value as Record<string, unknown>;
    if (obj["state"] !== "partial") {
      throw new StatusFileError(
        `docs/status.json has an unknown state for ${id}: ${JSON.stringify(value)}. ` +
          `Only "partial" takes the object form.`,
      );
    }
    const remaining = obj["remaining"];
    if (typeof remaining !== "string" || remaining.trim() === "") {
      throw new StatusFileError(
        `docs/status.json marks ${id} partial without a non-empty "remaining" note.`,
      );
    }
    return "partial";
  }

  throw new StatusFileError(
    `docs/status.json has an unknown state for ${id}: ${JSON.stringify(value)}. ` +
      `Expected one of ${REQUIREMENT_STATES.join(", ")}.`,
  );
}

/**
 * What is left on each `partial` ID, keyed by requirement ID.
 *
 * Threaded into the implementer brief for a completion pass, so the agent
 * picking the ID up is told what remains instead of re-deriving it.
 */
export function parseRemainingNotes(statusJson: string): Map<string, string> {
  const parsed = JSON.parse(statusJson) as Partial<RequirementStatusFile> | null;
  const requirements = parsed?.requirements ?? {};
  const notes = new Map<string, string>();
  for (const [id, value] of Object.entries(requirements)) {
    if (typeof value === "object" && value !== null && "remaining" in value) {
      notes.set(id, String((value as PartialRequirement).remaining));
    }
  }
  return notes;
}

/**
 * IDs fleet must not queue. `partial` is deliberately excluded so a thin slice
 * gets picked back up for its completion pass, carrying its `remaining` note.
 */
export function parseLandedIds(statusJson: string): Set<string> {
  const statuses = parseRequirementStatus(statusJson);
  const landed = new Set<string>();
  for (const [id, state] of statuses) {
    if (state === "landed") landed.add(id);
  }
  return landed;
}

/**
 * Requirement IDs in docs/functional-requirements.md order (first occurrence wins).
 *
 * Matches the doc's definition form — a list item whose first bold token is the
 * ID (`- **CORE-001** · …`) — and the table-cell form (`| CORE-001 |`), so a
 * later reformat of the doc does not silently empty the backlog.
 *
 * Anchoring on the definition form is what keeps prose non-load-bearing: a
 * cross-reference like "precedes FAIL-004" in a dependency note is not a
 * definition and must not enter the backlog, or spec order becomes whatever
 * order the prose happens to mention IDs in.
 */
export function parseSpecIdsInOrder(functionalSpec: string): string[] {
  const seen = new Set<string>();
  const ordered: string[] = [];
  const definitionForms = [
    /^\s*[-*]\s*\*\*([A-Z]{2,6}-\d{3})\*\*/gm,
    /^\s*\|\s*([A-Z]{2,6}-\d{3})\s*\|/gm,
  ];
  for (const pattern of definitionForms) {
    for (const match of functionalSpec.matchAll(pattern)) {
      const id = match[1]!;
      if (!seen.has(id)) {
        seen.add(id);
        ordered.push(id);
      }
    }
    if (ordered.length > 0) return ordered;
  }
  return ordered;
}

/**
 * The engine foundation. The bundle/manifest, the job+event model, and the
 * driver protocol are what every other requirement is written against, so
 * nothing else is pickable until all three land.
 */
const CORE_FOUNDATION = ["CORE-001", "CORE-002", "CORE-003"] as const;

function coreFoundationSatisfied(landed: Set<string>): boolean {
  return CORE_FOUNDATION.every((id) => landed.has(id));
}

/**
 * Capture prerequisites. A snapshot is key material + database + git + blobs,
 * so backup has nothing to capture and nothing to encrypt with until the
 * keystore, the blob adapters, and the state exporters exist.
 */
const CAPTURE_PREREQS = [
  "KEY-001",
  "BLOB-001",
  "STATE-001",
  "STATE-002",
  "STATE-003",
  "STATE-004",
] as const;

function capturePrereqsSatisfied(landed: Set<string>): boolean {
  return CAPTURE_PREREQS.every((id) => landed.has(id));
}

/** Snapshot creation. Restore reads what backup writes, so it gates restore. */
const BACKUP_CORE = ["BKUP-001", "BKUP-003", "BKUP-004"] as const;

function backupCoreSatisfied(landed: Set<string>): boolean {
  return BACKUP_CORE.every((id) => landed.has(id));
}

/**
 * Rebuild-from-snapshot. Promote, upgrade, and drill are each restore plus a
 * wrapper — none has anything to do until restore works.
 */
const RESTORE_CORE = ["RSTR-001", "RSTR-002", "RSTR-003"] as const;

function restoreCoreSatisfied(landed: Set<string>): boolean {
  return RESTORE_CORE.every((id) => landed.has(id));
}

/** Prefixes whose whole surface is built on restore. */
const RESTORE_DEPENDENT_PREFIXES = ["FAIL-", "UPGR-", "DRIL-"];

/** Host reach. Nothing can be deployed or restored onto a host without it. */
const ORCH_CORE = ["ORCH-001", "ORCH-002"] as const;

function orchCoreSatisfied(landed: Set<string>): boolean {
  return ORCH_CORE.every((id) => landed.has(id));
}

/**
 * Dependency gates from docs/functional-requirements.md § Dependency order.
 * Returns false when an ID must not be picked yet.
 */
export function dependenciesSatisfied(id: string, landed: Set<string>): boolean {
  if (
    !(CORE_FOUNDATION as readonly string[]).includes(id) &&
    !coreFoundationSatisfied(landed)
  ) {
    return false;
  }
  // Backup captures what the keystore, blob adapters, and exporters provide.
  if (id.startsWith("BKUP-") && !capturePrereqsSatisfied(landed)) {
    return false;
  }
  // Restore reads the snapshot format backup defines.
  if (id.startsWith("RSTR-") && !backupCoreSatisfied(landed)) {
    return false;
  }
  // Promote, upgrade, and drill are restore plus a wrapper.
  if (
    RESTORE_DEPENDENT_PREFIXES.some((prefix) => id.startsWith(prefix)) &&
    !restoreCoreSatisfied(landed)
  ) {
    return false;
  }
  // Deploying and restoring both need a host to reach.
  if (
    (id.startsWith("UP-") || id.startsWith("RSTR-")) &&
    !orchCoreSatisfied(landed)
  ) {
    return false;
  }
  // The forge cannot be brought up before its configuration renders.
  if (id.startsWith("UP-") && !landed.has("FORGE-001")) {
    return false;
  }
  // Import has nothing to import into until the forge is deployable.
  if (id.startsWith("IMPT-") && !landed.has("UP-001")) {
    return false;
  }
  // Zone proof at init is an ACME DNS-01 challenge.
  if (id === "INIT-002" && !landed.has("ACME-001")) {
    return false;
  }
  // Nothing can push to an `origin` the instance does not serve.
  if (id === "IMPT-004" && !landed.has("UP-005")) {
    return false;
  }
  // A nameless instance must exist before it can be served...
  if (id === "UP-006" && !landed.has("INIT-005")) {
    return false;
  }
  // ...and be served before it can be given a name.
  if (id === "UP-007" && !landed.has("UP-006")) {
    return false;
  }
  // A driver-applied flip needs a driver; the print fallback (DNS-003) does not.
  if (id === "FAIL-004" && !landed.has("DNS-001")) {
    return false;
  }
  // CI reconciliation at promote is FORGE-004's mechanism.
  if (id === "FAIL-003" && !landed.has("FORGE-004")) {
    return false;
  }
  // The dashboard is a client of the local API.
  if (id.startsWith("UI-") && !landed.has("API-001")) {
    return false;
  }
  // SSE streaming carries the CORE-002 event model over the API surface.
  if (id === "API-002" && !landed.has("API-001")) {
    return false;
  }
  // Renewal extends issuance.
  if (id === "ACME-002" && !landed.has("ACME-001")) {
    return false;
  }
  // The s3 adapter implements the interface the local adapter establishes.
  if (id === "BLOB-002" && !landed.has("BLOB-001")) {
    return false;
  }
  // Both shipped drivers implement the CORE-003 protocol.
  if (
    (id.startsWith("DNS-") || id.startsWith("KEY-") || id.startsWith("BLOB-")) &&
    !landed.has("CORE-003")
  ) {
    return false;
  }
  return true;
}

/**
 * Spec-order fallback for a single full-scope ID when dynamic packing has
 * nothing ready. Returns at most one ID (full piece, not a micro-slice pack).
 */
export function pickNextSpecFallbackId(options: {
  specIdsInOrder: string[];
  landed: Set<string>;
  reserved: Set<string>;
}): { id?: string } {
  for (const id of options.specIdsInOrder) {
    if (options.landed.has(id)) continue;
    if (options.reserved.has(id)) continue;
    if (!dependenciesSatisfied(id, options.landed)) continue;
    return { id };
  }
  return {};
}

/** @deprecated Prefer pickNextChunks + pickNextSpecFallbackId. Kept for tests. */
export function pickNextRequirementIds(options: {
  count: number;
  specIdsInOrder: string[];
  landed: Set<string>;
  reserved: Set<string>;
}): { ids: string[] } {
  const picked: string[] = [];
  for (const id of options.specIdsInOrder) {
    if (picked.length >= options.count) break;
    if (options.landed.has(id)) continue;
    if (options.reserved.has(id)) continue;
    if (!dependenciesSatisfied(id, options.landed)) continue;
    picked.push(id);
    options.reserved.add(id);
  }
  return { ids: picked };
}

export function oneIdPerAssignment(ids: string[]): string[][] {
  return ids.map((id) => [id]);
}
