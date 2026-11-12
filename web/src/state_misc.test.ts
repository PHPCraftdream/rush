// Native-Node tests for the small data stores: busy, selection, config, skills, signals, sitter.
import assert from "node:assert/strict";
import { test } from "node:test";
import { BusyCarburetor } from "./state/BusyCarburetor.ts";
import { ConfigCarburetor } from "./state/ConfigCarburetor.ts";
import { SelectionCarburetor } from "./state/SelectionCarburetor.ts";
import { SignalsCarburetor } from "./state/SignalsCarburetor.ts";
import { SitterCarburetor } from "./state/SitterCarburetor.ts";
import { SkillsCarburetor } from "./state/SkillsCarburetor.ts";
import type { SkillInfo } from "./types.ts";

/** In-memory localStorage stand-in. */
class MemoryStorage {
  public readonly items = new Map<string, string>();

  /**
   * Reads an item.
   *
   * @param key - storage key
   */
  public getItem(key: string): string | null {
    return this.items.get(key) ?? null;
  }

  /**
   * Writes an item.
   *
   * @param key - storage key
   * @param value - the value
   */
  public setItem(key: string, value: string): void {
    this.items.set(key, value);
  }

  /**
   * Removes an item.
   *
   * @param key - storage key
   */
  public removeItem(key: string): void {
    this.items.delete(key);
  }
}

/**
 * Installs a fresh in-memory localStorage and returns it.
 */
const installStorage = (): MemoryStorage => {
  const storage = new MemoryStorage();
  (globalThis as unknown as { localStorage: MemoryStorage }).localStorage = storage;
  return storage;
};

test("busy: setBusy toggles one session, setBusySessions replaces, summarize flags are independent", () => {
  const store = new BusyCarburetor({ busySessions: [], summarizeQueued: [] });
  store.setBusy("a", true);
  store.setBusy("b", true);
  store.setBusy("a", true);
  assert.deepEqual(store.getData().busySessions, ["a", "b"]);
  store.setBusy("a", false);
  assert.equal(store.isBusy("a"), false);
  assert.equal(store.isBusy("b"), true);
  store.setSummarizeQueued("b", true);
  assert.deepEqual(store.getData().summarizeQueued, ["b"]);
  store.setBusySessions(new Set(["z"]));
  assert.deepEqual(store.getData().busySessions, ["z"]);
  assert.deepEqual(store.getData().summarizeQueued, ["b"]);
  store.setSummarizeQueued("b", false);
  assert.deepEqual(store.getData().summarizeQueued, []);
});

test("selection: toggle flips, selectAll only adds, clear empties", () => {
  const store = new SelectionCarburetor({ selectedMessageIDs: [] });
  store.toggle("a");
  store.toggle("b");
  store.toggle("a");
  assert.deepEqual(store.getData().selectedMessageIDs, ["b"]);
  store.selectAll(["b", "c", "d"]);
  assert.deepEqual(store.getData().selectedMessageIDs, ["b", "c", "d"]);
  store.clear();
  assert.deepEqual(store.getData().selectedMessageIDs, []);
});

test("config: trackRecent moves to front, dedupes and caps at five; removeRecent drops one", () => {
  const store = new ConfigCarburetor({ config: null, recent: { smart: [], fast: [], worker: [], reviewer: [] } });
  for (const key of ["a", "b", "c", "d", "e", "f"]) store.trackRecent("smart", key);
  assert.deepEqual(store.getData().recent.smart, ["f", "e", "d", "c", "b"]);
  store.trackRecent("smart", "c");
  assert.deepEqual(store.getData().recent.smart, ["c", "f", "e", "d", "b"]);
  store.removeRecent("smart", "e");
  assert.deepEqual(store.getData().recent.smart, ["c", "f", "d", "b"]);
  assert.deepEqual(store.getData().recent.fast, []);
  store.setRecent("fast", ["x"]);
  assert.deepEqual(store.getData().recent.fast, ["x"]);
});

test("skills: the last used skill is persisted to localStorage exactly as before", () => {
  const storage = installStorage();
  assert.equal(SkillsCarburetor.readStoredSkill(), "");
  const store = new SkillsCarburetor({ skills: [], lastUsedSkill: SkillsCarburetor.readStoredSkill() });
  store.setLastUsedSkill("review");
  assert.equal(store.getData().lastUsedSkill, "review");
  assert.equal(storage.getItem("rush_last_skill"), "review");
  assert.equal(SkillsCarburetor.readStoredSkill(), "review");
  store.setSkills([{ name: "review" } as unknown as SkillInfo]);
  assert.equal(store.getData().skills.length, 1);
});

test("signals: collapseAll ticks the counter and every expand request gets a fresh nonce", () => {
  const store = new SignalsCarburetor({ collapseAllNonce: 0, expandToolCallRequest: null });
  store.collapseAll();
  store.collapseAll();
  assert.equal(store.getData().collapseAllNonce, 2);
  store.requestExpandToolCall("tc1");
  const first = store.getData().expandToolCallRequest;
  store.requestExpandToolCall("tc1");
  const second = store.getData().expandToolCallRequest;
  assert.equal(first?.toolCallID, "tc1");
  assert.equal(second?.nonce, (first?.nonce ?? 0) + 1);
});

test("sitter: a running status is saved to localStorage and a stopped one forgets it", () => {
  const storage = installStorage();
  const store = new SitterCarburetor({ sitter: { running: false } });
  store.setStatus({ running: true, sessionID: "s1", intervalMin: 5, startedAt: 1 });
  assert.deepEqual(SitterCarburetor.readSaved(), { sessionID: "s1", intervalMin: 5 });
  store.setStatus({ running: false });
  assert.equal(storage.getItem("rushSitter"), null);
  assert.equal(SitterCarburetor.readSaved(), null);
  storage.setItem("rushSitter", "{not json");
  assert.throws(SitterCarburetor.readSaved);
  SitterCarburetor.clearSaved();
  assert.equal(storage.getItem("rushSitter"), null);
});
