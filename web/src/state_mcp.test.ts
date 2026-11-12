// Native-Node tests for the MCP snapshot store.
import assert from "node:assert/strict";
import { test } from "node:test";
import { flush } from "@react-carburetor/core/testing";
import { McpCarburetor } from "./state/McpCarburetor.ts";
import type { IMcpState } from "./state/McpCarburetor.ts";
import type { MCPServerInfo, MCPState } from "./types.ts";

const nameOf = (server: { name: string }): string => server.name;
const toServer = (name: string): MCPServerInfo => ({ name, status: "connected", disabled: false, toolCount: 1 });
const selectServerNames = (state: IMcpState): string[] => (state.mcp?.servers ?? []).map(nameOf);
const snapshot = (...names: string[]): MCPState => ({ servers: names.map(toServer) });

test("starts without a snapshot and setMcp replaces the whole snapshot", () => {
  const store = new McpCarburetor({ mcp: null });
  assert.equal(store.getData().mcp, null);
  store.setMcp(snapshot("a", "b"));
  assert.deepEqual(store.getData().mcp?.servers.map(nameOf), ["a", "b"]);
  store.setMcp(snapshot("c"));
  assert.deepEqual(store.getData().mcp?.servers.map(nameOf), ["c"]);
  store.setMcp(null);
  assert.equal(store.getData().mcp, null);
});

test("watchers receive each new server list", () => {
  const store = new McpCarburetor({ mcp: null });
  const seen: string[][] = [];
  const stop = store.watch(selectServerNames, (next) => {
    seen.push([...next]);
  });
  try {
    store.setMcp(snapshot("a"));
    flush(store);
    store.setMcp(snapshot("a", "b"));
    flush(store);
    store.setMcp(null);
    flush(store);
    assert.deepEqual(seen, [["a"], ["a", "b"], []]);
  } finally {
    stop();
  }
});
