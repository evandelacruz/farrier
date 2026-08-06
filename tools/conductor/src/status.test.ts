import { test } from "node:test";
import assert from "node:assert/strict";
import { resolveAgentStatus, type ListRunsFn } from "./status.js";

test("resolveAgentStatus uses listed status when present", async () => {
  let listRunsCalled = false;
  const listRuns: ListRunsFn = async () => {
    listRunsCalled = true;
    return { items: [{ status: "running" }] };
  };
  const status = await resolveAgentStatus({
    agentId: "bc-test",
    listedStatus: "finished",
    apiKey: "test-key",
    listRuns,
  });
  assert.equal(status, "finished");
  assert.equal(listRunsCalled, false);
});

test("resolveAgentStatus hydrates from latest run when listed status missing", async () => {
  const listRuns: ListRunsFn = async (agentId, opts) => {
    assert.equal(agentId, "bc-hydrate");
    assert.equal(opts.limit, 1);
    assert.equal(opts.runtime, "cloud");
    return { items: [{ status: "waiting_for_background_work" }] };
  };
  const status = await resolveAgentStatus({
    agentId: "bc-hydrate",
    listedStatus: undefined,
    apiKey: "test-key",
    listRuns,
  });
  assert.equal(status, "waiting_for_background_work");
});

test("resolveAgentStatus treats empty listed status as missing", async () => {
  const listRuns: ListRunsFn = async () => ({
    items: [{ status: "running" }],
  });
  const status = await resolveAgentStatus({
    agentId: "bc-empty",
    listedStatus: "",
    apiKey: "test-key",
    listRuns,
  });
  assert.equal(status, "running");
});

test("resolveAgentStatus returns unknown when listRuns throws", async () => {
  const listRuns: ListRunsFn = async () => {
    throw new Error("network");
  };
  const status = await resolveAgentStatus({
    agentId: "bc-err",
    listedStatus: undefined,
    apiKey: "test-key",
    listRuns,
  });
  assert.equal(status, "unknown");
});

test("resolveAgentStatus returns unknown when latest run has no status", async () => {
  const listRuns: ListRunsFn = async () => ({ items: [{}] });
  const status = await resolveAgentStatus({
    agentId: "bc-nostatus",
    listedStatus: undefined,
    apiKey: "test-key",
    listRuns,
  });
  assert.equal(status, "unknown");
});
