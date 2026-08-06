/** Fleet default model — Cursor Grok 4.5 (SDK id). Override with --model or CURSOR_MODEL. */
export const DEFAULT_FLEET_MODEL = process.env.CURSOR_MODEL ?? "grok-4.5";

/**
 * In-flight locks, as PR labels.
 *
 * A label rather than a process registry because the conductor is stateless:
 * it re-derives the whole plan from GitHub on every run, and a worker in its
 * own cloud session has no other durable way to say "mine". The conductor
 * takes the lock before firing; the worker drops it as its last action.
 */
export const WORKING_LABEL = "conductor:working";
export const REVIEWING_LABEL = "conductor:reviewing";

/**
 * Review verdicts, as PR labels.
 *
 * Agent reviews post under the repo owner's identity and GitHub forbids
 * approving your own PR, so `reviewDecision` is permanently null (see "Review
 * identity" in the README). Labels are not subject to that restriction — an
 * author can label their own PR — so the reviewer records its verdict here
 * instead and the conductor reads it as if it were the review state.
 *
 * This is our state machine, not GitHub's: branch protection still cannot gate
 * on it. It exists so the conductor knows what to do next, which is the only
 * thing that needed the signal.
 */
export const APPROVED_LABEL = "conductor:approved";
export const CHANGES_REQUESTED_LABEL = "conductor:changes-requested";

/**
 * Worker models. Aliases, not pinned ids: "opus" always resolves to the latest
 * Opus, which is what the implementer wants. Pin a full model id via these env
 * vars when a run needs to be reproducible.
 */
export const IMPLEMENTER_MODEL = process.env.FARRIER_IMPLEMENTER_MODEL ?? "opus";
export const REVIEWER_MODEL = process.env.FARRIER_REVIEWER_MODEL ?? "sonnet";
