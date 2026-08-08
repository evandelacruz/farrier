import { test } from "node:test";
import assert from "node:assert/strict";
import {
  SYNC_WITH_MAIN,
  buildImplementerPrompt,
  buildPrFixPrompt,
  buildPrReviewPrompt,
  buildPrPolishPrompt,
} from "./briefs.js";

test("SYNC_WITH_MAIN requires merge origin/main before other work", () => {
  assert.match(SYNC_WITH_MAIN, /git fetch origin main/);
  assert.match(SYNC_WITH_MAIN, /git merge origin\/main/);
  assert.match(SYNC_WITH_MAIN, /do not push/i);
  assert.doesNotMatch(SYNC_WITH_MAIN, /push origin HEAD.*merge/s);
});

test("implementer brief claims the ID with a draft PR before coding", () => {
  const prompt = buildImplementerPrompt(["BKUP-002"]);
  assert.match(prompt, /When to push/i);
  // The claim happens before implementation, and it is a draft — a draft is
  // not reviewed, so it costs nothing and makes the ID visible to the next
  // conductor pass, which reads open PRs rather than running sessions.
  assert.match(prompt, /claim the ID with a draft PR \(mandatory, before coding\)/i);
  assert.match(prompt, /branch-notes/);
  assert.match(prompt, /only signal that this ID is claimed/i);
  assert.match(prompt, /mark the PR ready for review/i);
  assert.match(prompt, /draft: false/);
  // The claiming push must come before the "When to push" section, so an agent
  // reading top-to-bottom claims first and implements second.
  assert.ok(
    prompt.indexOf("claim the ID with a draft PR") < prompt.indexOf("## When to push"),
    "claim step comes before the push section",
  );
  assert.match(prompt, /start from latest main/i);
  // One branch, named by the assignment — never a copy of another PR's.
  assert.match(prompt, /stay on it and bring main in/i);
  assert.match(prompt, /never reuse or copy another PR's branch name/i);
  assert.match(prompt, /git merge origin\/main/);
  assert.doesNotMatch(prompt, /-3e62/);
  assert.doesNotMatch(prompt, /Branch off main/i);
  assert.match(
    prompt,
    /Use the branch named in the Branch section below; do not open a parallel one\./,
  );
  assert.match(prompt, /thin stub/i);
  assert.match(prompt, /If unsure/i);
  assert.doesNotMatch(prompt, /review feedback is addressed/i);
});

test("no writer brief asks the agent to review its own diff", () => {
  // Reviewing is the reviewer Routine's job, and it reads every push. An
  // implementer re-reading its whole diff in a loop paid opus rates for a
  // second opinion the pipeline was already going to produce — and did not
  // stop reviewers requesting changes anyway.
  for (const prompt of [
    buildImplementerPrompt(["BKUP-002"]),
    buildPrFixPrompt({
      number: 1,
      url: "https://github.com/evandelacruz/farrier/pull/1",
      title: "Fix things",
      reasons: ["changes requested"],
    }),
    buildPrPolishPrompt({
      number: 2,
      url: "https://github.com/evandelacruz/farrier/pull/2",
      title: "Polish things",
      reasons: ["approved nit"],
    }),
  ]) {
    assert.doesNotMatch(prompt, /self-review/i);
    assert.doesNotMatch(prompt, /hostile reviewer/i);
    assert.doesNotMatch(prompt, /Repeat steps 1–3/i);
    // Pushing must not be gated on a review pass that no longer exists.
    assert.doesNotMatch(prompt, /internal self-review cycle is done/i);
    // The old rule this replaced — never post your critique as PR comments —
    // is obsolete here, but writers must still not review their own PR.
    assert.doesNotMatch(prompt, /gh api.*pulls.*comments/i);
  }
});

test("the implementer is told the reviewer will read the push", () => {
  // Removing self-review only works if the agent knows review still happens;
  // otherwise "mark it ready when done" reads as "nobody checks this".
  const prompt = buildImplementerPrompt(["BKUP-002"]);
  assert.match(prompt, /An automated reviewer reads the ready PR/i);
  assert.match(prompt, /not\*\* your job/i);
});

test("combinedDueToConflict brief tells agent to do co-ship partners in listed order", () => {
  const prompt = buildImplementerPrompt(["KEY-001", "KEY-002"], {
    chunkTitle: "KEY-001 file driver + KEY-002 command driver",
    chunkKey: "key-001+key-002",
    combinedDueToConflict: true,
  });
  assert.match(prompt, /combined into one agent/i);
  assert.match(prompt, /co-ship pair/i);
  assert.match(prompt, /conflict-skipped/i);
  assert.match(prompt, /listed order/i);
  assert.match(prompt, /one PR/i);
  // Same-domain adjacency is no longer the fold rationale (KEY-003 stays out).
  assert.doesNotMatch(prompt, /because they are the \*\*same domain\*\*/i);
  assert.doesNotMatch(prompt, /migrations \/ app\.module/);
});

test("PR fix brief pushes only after review fixes", () => {
  const prompt = buildPrFixPrompt({
    number: 1,
    url: "https://github.com/evandelacruz/farrier/pull/1",
    title: "Fix things",
    reasons: ["changes requested"],
  });
  assert.match(prompt, /do not push yet/i);
  assert.match(prompt, /When to push/i);
  assert.match(prompt, /review feedback is addressed/i);
  const whenToPushSection = prompt.indexOf("## When to push");
  const task = prompt.indexOf("## Task");
  assert.ok(whenToPushSection > task, "When to push section comes after task");
});

test("all fleet briefs include main-sync guidance as first work section", () => {
  for (const [label, prompt] of [
    ["implement", buildImplementerPrompt(["BKUP-002"])],
    [
      "fix",
      buildPrFixPrompt({
        number: 1,
        url: "https://github.com/evandelacruz/farrier/pull/1",
        title: "Fix things",
        reasons: ["changes requested"],
      }),
    ],
    [
      "polish",
      buildPrPolishPrompt({
        number: 2,
        url: "https://github.com/evandelacruz/farrier/pull/2",
        title: "Polish things",
        reasons: ["approved nit"],
      }),
    ],
  ] as const) {
    const syncIdx = prompt.indexOf("First step");
    const nextSection = Math.min(
      prompt.includes("## Requirements") ? prompt.indexOf("## Requirements") : Infinity,
      prompt.includes("## Task") ? prompt.indexOf("## Task") : Infinity,
    );
    assert.ok(syncIdx !== -1, `${label}: missing sync section`);
    assert.ok(Number.isFinite(nextSection), `${label}: missing task/requirements section`);
    assert.ok(syncIdx < nextSection, `${label}: sync must appear before task/requirements`);
    assert.ok(prompt.includes("git fetch origin main"));
  }
});

test("implementer brief permits a declared serial slice but still bans stubs", () => {
  const prompt = buildImplementerPrompt(["KEY-003"]);

  // The escape hatch exists and is reachable from the Requirements section.
  assert.match(prompt, /If the ID is too large for one PR/);
  assert.match(prompt, /that is the only sanctioned way to deliver less than the whole ID/i);

  // Capability cut, not a layer cut — a layer split lands dead code on main.
  assert.match(prompt, /Split by capability, never by layer/i);
  assert.match(prompt, /vertical\s+cut/i);

  // The three obligations that separate a slice from a stub.
  assert.match(prompt, /before implementing/i);
  assert.match(prompt, /No stubs, no TODOs standing in for the/i);
  assert.match(prompt, /Record the ID in docs\/status\.json as/);
  assert.match(prompt, /"state":"partial","remaining"/);

  // Unsliceable means implement whole or halt — not "slice anyway".
  assert.match(prompt, /the ID is not sliceable/i);

  // Serial only. Parallel PRs on one ID are still forbidden.
  assert.match(prompt, /never two in parallel/i);
  // Several PRs under one ID is the expected shape now, not an escape hatch —
  // otherwise a foundation and the feature that needs it land in one diff.
  assert.match(prompt, /Several small PRs under one ID is the expected shape/i);
  assert.match(prompt, /One ID may take as many PRs as it honestly needs/i);
  // The prerequisite carve-out, and the halt rule it replaces.
  assert.match(prompt, /a prerequisite that is decided but unbuilt/i);
  assert.match(prompt, /only\s+sanctioned layer cut/i);
  assert.match(prompt, /"Decided but not built" is not a stop condition/i);
  // It must sit inside the slicing section, which only the implementer brief has.
  assert.ok(
    prompt.indexOf("If the ID is too large for one PR") <
      prompt.indexOf('"Decided but not built"'),
    "the slicing section must precede the sentence that refers to it",
  );

  // The old absolute prohibitions must be gone, or the brief contradicts itself.
  assert.doesNotMatch(prompt, /Do not split the chunk across PRs/i);
  assert.doesNotMatch(prompt, /Do not invent a thinner slice/i);
  assert.doesNotMatch(prompt, /slice-of-a-slice/i);

  // ...but the anti-stub rule it was protecting survives.
  assert.match(prompt, /thin stub/i);
  assert.match(prompt, /is a stub, not a slice/i);
});

test("writer briefs ask for a scoped test run, not the full suite", () => {
  // CI runs the build + the whole suite on every PR into main, so an agent
  // repeating it pays for an answer already on its way — mostly rebuilding
  // packages it never touched. The scoped run stays, because that is
  // the one CI cannot make cheap: a break caught in-session costs seconds,
  // the same break caught by CI costs a red build plus a fresh fixer session.
  for (const prompt of [
    buildImplementerPrompt(["BKUP-002"]),
    buildPrFixPrompt({
      number: 1,
      url: "https://github.com/evandelacruz/farrier/pull/1",
      title: "Fix things",
      reasons: ["changes requested"],
    }),
  ]) {
    assert.match(prompt, /packages you touched/i);
    assert.match(prompt, /go test \.\/internal\/core\/backup\/\.\.\./);
    assert.match(prompt, /Do \*\*not\*\* run the full/i);
    assert.match(prompt, /CI runs the whole suite/i);
    // Writing tests is untouched — CI is what executes them.
    assert.match(prompt, /test coverage for behavior you add or change/i);
  }
});

test("fix and polish briefs do not carry the slicing-dependent stop-condition note", () => {
  // STOP_CONDITIONS is shared by three prompts, but "build it as its own slice,
  // on the terms above" only resolves where SLICING_BLOCK is present. A fixer
  // working an existing PR is not slicing a fresh ID, and pointing it at a
  // section that is not in its brief is worse than saying nothing.
  for (const prompt of [
    buildPrFixPrompt({
      number: 1,
      url: "https://github.com/evandelacruz/farrier/pull/1",
      title: "Fix things",
      reasons: ["changes requested"],
    }),
    buildPrPolishPrompt({
      number: 2,
      url: "https://github.com/evandelacruz/farrier/pull/2",
      title: "Polish things",
      reasons: ["approved nit"],
    }),
  ]) {
    assert.doesNotMatch(prompt, /Decided but not built/i);
    assert.doesNotMatch(prompt, /its own slice/i);
    // The ordinary stop conditions still apply to them.
    assert.match(prompt, /Stop conditions/i);
  }
});

test("briefs point agents at the decision record before they decide or ask", () => {
  // CLAUDE.md alone is not the whole record: docs/spec.md settles the state
  // model, the identity model, and what the operator owns. Without a pointer to
  // it, agents re-litigate settled questions and ask humans to choose things
  // already chosen.
  const impl = buildImplementerPrompt(["BKUP-002"]);
  assert.match(impl, /docs\/spec\.md/);
  assert.match(impl, /A settled decision is not yours to re-open/i);
  assert.match(impl, /raise it with Evan rather than deviating/i);

  const review = buildPrReviewPrompt({
    number: 1,
    url: "https://github.com/evandelacruz/farrier/pull/1",
    title: "t",
    headSha: "abc123",
  });
  assert.match(review, /docs\/spec\.md/);
  // A reviewer that does not know the settled decisions cannot catch a change
  // that contradicts one.
  assert.match(review, /contradicts a settled decision\s+in docs\/ is a finding/i);
  // Renegotiating in a PR is the specific failure the rule exists to stop.
  assert.match(review, /renegotiable with Evan, never unilaterally in a PR/i);
});
