import { test } from "node:test";
import assert from "node:assert/strict";
import { anySpawnFailed, formatSpawnResults, type SpawnResult } from "./run.js";
import type { FleetAssignment } from "./plan.js";

function stubAssignment(name: string): FleetAssignment {
  return {
    kind: "implement",
    name,
    ids: ["LEDG-008"],
    prompt: "implement LEDG-008",
  };
}

test("formatSpawnResults includes successful and failed spawns", () => {
  const results: SpawnResult[] = [
    {
      assignment: stubAssignment("agent-a"),
      agentId: "bc-aaa",
      name: "agent-a",
      url: "https://cursor.com/agents/bc-aaa",
      status: "started",
    },
    {
      assignment: stubAssignment("agent-b"),
      error: "CURSOR_API_KEY missing",
    },
  ];
  const text = formatSpawnResults(results);
  assert.match(text, /agent-a/);
  assert.match(text, /bc-aaa/);
  assert.match(text, /FAILED: CURSOR_API_KEY missing/);
});

test("anySpawnFailed is true when any assignment failed", () => {
  const ok: SpawnResult = {
    assignment: stubAssignment("ok"),
    agentId: "bc-ok",
    url: "https://cursor.com/agents/bc-ok",
  };
  const fail: SpawnResult = {
    assignment: stubAssignment("fail"),
    error: "boom",
  };
  assert.equal(anySpawnFailed([ok]), false);
  assert.equal(anySpawnFailed([ok, fail]), true);
});
