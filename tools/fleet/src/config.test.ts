import { test } from "node:test";
import assert from "node:assert/strict";
import { DEFAULT_FLEET_MODEL } from "./config.js";

test("DEFAULT_FLEET_MODEL is Cursor Grok 4.5 (SDK id grok-4.5) unless CURSOR_MODEL set", () => {
  if (process.env.CURSOR_MODEL) {
    assert.equal(DEFAULT_FLEET_MODEL, process.env.CURSOR_MODEL);
  } else {
    assert.equal(DEFAULT_FLEET_MODEL, "grok-4.5");
  }
});
