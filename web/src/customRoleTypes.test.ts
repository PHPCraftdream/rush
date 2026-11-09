import assert from "node:assert/strict";
import { test } from "node:test";
import { roleToolsMode, roleToolsPayload } from "./customRoleTypes.ts";

test("custom role tools distinguish omitted, empty and pattern lists", () => {
  assert.equal(roleToolsMode({ provider: "p", model: "m" }), "inherit");
  assert.equal(roleToolsMode({ provider: "p", model: "m", tools: [] }), "none");
  assert.equal(roleToolsMode({ provider: "p", model: "m", tools: ["mcp_*"] }), "patterns");
  assert.deepEqual(roleToolsPayload("inherit", "invalid ignored text"), { inherit_tools: true });
  assert.deepEqual(roleToolsPayload("none", "invalid ignored text"), { tools: [] });
  assert.deepEqual(roleToolsPayload("patterns", '["view", "mcp_*", "tool?"]'), { tools: ["view", "mcp_*", "tool?"] });
  assert.deepEqual(roleToolsPayload("patterns", "[]"), { tools: [] });
});

test("malformed custom role patterns never become unrestricted access", () => {
  for (const invalid of ["null", "{}", '"*"', "[null]", "[3]", '[""]', "not json"]) {
    assert.throws(() => roleToolsPayload("patterns", invalid));
  }
});
