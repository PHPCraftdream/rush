import assert from "node:assert/strict";
import { test } from "node:test";
import { customRoleAt } from "./customRole/customRoleAt.ts";
import { roleToolsMode } from "./customRole/roleToolsMode.ts";
import { roleToolsPayload } from "./customRole/roleToolsPayload.ts";

test("custom role lookup distinguishes missing prototype names from own definitions", () => {
  const role = { provider: "p", model: "m" };
  const empty = JSON.parse("{}") as Record<string, typeof role>;
  for (const name of ["constructor", "toString", "__proto__"]) {
    assert.equal(customRoleAt(empty, name), undefined, `${name} is not a role in an empty map`);
    const roles = JSON.parse(JSON.stringify({ [name]: role })) as Record<string, typeof role>;
    assert.deepEqual(customRoleAt(roles, name), role);
  }
  assert.equal(customRoleAt(undefined, "constructor"), undefined);
});

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
