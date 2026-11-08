import { useState } from "react";
import { Plus, Trash2 } from "lucide-react";
import type { PeakHoursWindowSpec, ProviderPeakHoursPolicy } from "../types";

// ── Shared weekly peak-hours editor (task #56) ───────────────────────────────
//
// Used by BOTH the custom-provider form (idPrefix "provider-form-peak") and
// the built-in peak-hours-only editor (idPrefix "peak-hours-only"). The
// editor owns no provider identity and never talks to the wire — parents
// submit via set_provider_peak_hours / add_custom_provider /
// update_custom_provider — and it never decides whether a provider is
// currently blocked: there are deliberately no Date.now()-based active
// checks here. Status display is PeakHoursStatus's job, backed by the
// server's get_peak_hours_status reply.
//
// data-test-ids are parameterised by idPrefix so the existing Playwright
// specs keep working unchanged: provider-form-peak-toggle / -inputs /
// -start / -end / -message on the custom form, peak-hours-only-toggle /
// -inputs / -start / -end / -message on the built-in editor. The start/end
// test-ids land on the FIRST window row only.

const HH_MM_RE = /^([01]\d|2[0-3]):[0-5]\d$/;
// End additionally accepts "24:00" (exclusive end-of-day boundary).
const END_RE = /^(24:00|([01]\d|2[0-3]):[0-5]\d)$/;

const DAYS = ["mon", "tue", "wed", "thu", "fri", "sat", "sun"];
const MAX_WINDOWS = 64;

// canonicalDays normalises a days spec to one of the wire-canonical forms:
// "" (every day), "mon-fri" (a contiguous mon..sun run, wrap allowed e.g.
// "fri-mon"), or a comma list in mon..sun order ("mon,wed,fri"). This is a
// pure string helper — no clock is consulted.
export function canonicalDays(days: string): string {
  const set = new Set<string>();
  for (const token of days.split(",")) {
    const t = token.trim().toLowerCase();
    if (!t) continue;
    const range = t.split("-");
    if (range.length === 2 && DAYS.includes(range[0]) && DAYS.includes(range[1])) {
      // Expand a (possibly wrapping) contiguous run into its day set.
      let i = DAYS.indexOf(range[0]);
      const j = DAYS.indexOf(range[1]);
      while (true) {
        set.add(DAYS[i]);
        if (i === j) break;
        i = (i + 1) % 7;
      }
    } else if (DAYS.includes(t)) {
      set.add(t);
    }
  }
  if (set.size === 0 || set.size === 7) return "";
  // Contiguous (with wrap) runs collapse to "x-y"; anything else lists in
  // mon..sun order.
  const ordered = DAYS.filter((d) => set.has(d));
  const startIdx = DAYS.indexOf(ordered[0]);
  let contiguous = true;
  for (let k = 0; k < ordered.length; k++) {
    if (ordered[k] !== DAYS[(startIdx + k) % 7]) { contiguous = false; break; }
  }
  if (contiguous) {
    return ordered.length === 1 ? ordered[0] : `${ordered[0]}-${ordered[ordered.length - 1]}`;
  }
  return ordered.join(",");
}

// peakDaysSet expands a days spec into the full set of days it covers.
// "" means every day; each comma token is a single day or a contiguous
// range that may wrap ("fri-mon" walks forward through DAYS modulo 7).
// Unknown tokens are ignored so malformed wire data can't crash the editor.
export function peakDaysSet(days: string): Set<string> {
  const set = new Set<string>();
  if (!days) {
    DAYS.forEach((d) => set.add(d));
    return set;
  }
  for (const token of days.split(",")) {
    const t = token.trim().toLowerCase();
    const range = t.split("-");
    if (range.length === 2 && DAYS.includes(range[0]) && DAYS.includes(range[1])) {
      let i = DAYS.indexOf(range[0]);
      const j = DAYS.indexOf(range[1]);
      while (true) {
        set.add(DAYS[i]);
        if (i === j) break;
        i = (i + 1) % 7;
      }
    } else if (DAYS.includes(t)) {
      set.add(t);
    }
  }
  return set;
}

function toggleDay(days: string, day: string): string {
  // Expand the CURRENT spec (not the canonical form) so ranges like
  // "mon-fri" toggle correctly instead of being re-expanded from a lossy
  // list. A window must keep at least one day — dropping the last day
  // would leave a window with no active time at all, so keep the previous
  // set instead.
  const set = peakDaysSet(days);
  if (set.has(day) && set.size === 1) return canonicalDays([...set].join(","));
  if (set.has(day)) set.delete(day); else set.add(day);
  return canonicalDays([...set].join(","));
}

// peakHoursProblems returns non-blocking UX-hint problems (malformed HH:MM,
// start == end, too many windows). The parent consumes its length for
// canSubmit; the editor also renders the strings inline as hints.
export function peakHoursProblems(value: ProviderPeakHoursPolicy | null): string[] {
  if (!value) return [];
  const problems: string[] = [];
  const windows = value.windows ?? [];
  if (windows.length > MAX_WINDOWS) problems.push(`At most ${MAX_WINDOWS} windows are allowed`);
  windows.forEach((w, i) => {
    const label = windows.length > 1 ? `Window ${i + 1}: ` : "";
    if (!HH_MM_RE.test(w.start) || !END_RE.test(w.end)) problems.push(`${label}times must be in 24-hour HH:MM format (end may be 24:00)`);
    else if (w.start === w.end) problems.push(`${label}start and end must differ`);
  });
  return problems;
}

function defaultPolicy(): ProviderPeakHoursPolicy {
  return { windows: [{ days: "", start: "09:00", end: "18:00" }], message: "" };
}

// legacyToPolicy maps the pre-#56 wire shape { start, end, message? } onto a
// policy so old data edits fine and round-trips back as a policy.
export function legacyToPolicy(peak: { start?: string; end?: string; message?: string; windows?: PeakHoursWindowSpec[] } | null | undefined): ProviderPeakHoursPolicy | null {
  if (!peak) return null;
  if (Array.isArray(peak.windows)) return peak as ProviderPeakHoursPolicy;
  if (peak.start && peak.end) return { windows: [{ days: "", start: peak.start, end: peak.end }], message: peak.message ?? "" };
  return null;
}

// peakSummaryText renders the wire summary of either a policy (windows
// list) or the legacy { start, end } shape, joined with ", ". Pure string
// helper — used by ProviderRow where the peakHours union must narrow
// without a type cast.
export function peakSummaryText(peak: { windows?: { start: string; end: string }[]; start?: string; end?: string } | null | undefined): string {
  if (peak?.windows && peak.windows.length > 0) {
    return peak.windows.filter((w) => w.start && w.end).map((w) => `${w.start}–${w.end}`).join(", ");
  }
  if (peak?.start && peak?.end) return `${peak.start}–${peak.end}`;
  return "";
}

function peakSummary(value: ProviderPeakHoursPolicy | null): string {
  return (value?.windows ?? []).map((w) => `${w.start}–${w.end}`).join(", ");
}

export function PeakHoursEditor({
  idPrefix,
  value,
  onChange,
  pausedUntil,
  onPause,
  onResume,
}: {
  idPrefix: string;
  value: ProviderPeakHoursPolicy | null;
  onChange: (next: ProviderPeakHoursPolicy | null) => void;
  pausedUntil?: string;
  onPause?: (duration?: string) => Promise<unknown> | unknown;
  onResume?: () => Promise<unknown> | unknown;
}) {
  const enabled = value !== null;
  const windows = value?.windows ?? [];
  const problems = peakHoursProblems(value);
  const [pauseDuration, setPauseDuration] = useState("");

  function update(i: number, field: keyof PeakHoursWindowSpec, v: string) {
    const next = windows.map((w, idx) => (idx === i ? { ...w, [field]: v } : w));
    onChange({ ...(value ?? defaultPolicy()), windows: next });
  }

  function addRow() {
    onChange({ ...(value ?? defaultPolicy()), windows: [...windows, { days: "", start: "09:00", end: "18:00" }] });
  }

  function removeRow(i: number) {
    const next = windows.filter((_, idx) => idx !== i);
    onChange(next.length > 0 ? { ...(value ?? defaultPolicy()), windows: next } : null);
  }

  function setDays(i: number, days: string) {
    update(i, "days", canonicalDays(days));
  }

  const inputCls = "w-full text-xs font-mono bg-canvas border border-surface rounded-lg px-2.5 py-1.5 outline-none focus:border-accent/50 text-text";

  return (
    <div className="space-y-2 border-t border-surface pt-3">
      <label className="flex items-center gap-2 text-[11px] text-text-subtle cursor-pointer select-none">
        <input
          type="checkbox"
          checked={enabled}
          onChange={(e) => onChange(e.target.checked ? (value ?? defaultPolicy()) : null)}
          className="accent-accent"
          data-test-id={`${idPrefix}-toggle`}
        />
        Restrict to hours
      </label>
      {enabled && (
        <div className="space-y-2" data-test-id={`${idPrefix}-inputs`}>
          {windows.map((w, i) => (
            <div key={i} className="space-y-1.5 p-2.5 bg-base-overlay border border-surface rounded-xl">
              <div className="flex items-center gap-1 flex-wrap">
                {DAYS.map((d) => (
                  <button
                    key={d}
                    type="button"
                    onClick={() => setDays(i, toggleDay(w.days ?? "", d))}
                    className={`px-1.5 py-0.5 text-[10px] uppercase rounded-md border transition-colors ${
                      peakDaysSet(w.days ?? "").has(d)
                        ? "bg-accent-fill text-white/90 border-accent/30"
                        : "text-text-subtle border-surface hover:text-text"
                    }`}
                  >
                    {d.slice(0, 2)}
                  </button>
                ))}
                <span className="text-[10px] text-text-subtle ml-1">|</span>
                <button type="button" onClick={() => setDays(i, "")} className="px-1.5 py-0.5 text-[10px] text-text-subtle hover:text-text rounded-md border border-surface">All</button>
                <button type="button" onClick={() => setDays(i, "mon-fri")} className="px-1.5 py-0.5 text-[10px] text-text-subtle hover:text-text rounded-md border border-surface">Mon–Fri</button>
                <button type="button" onClick={() => setDays(i, "sat,sun")} className="px-1.5 py-0.5 text-[10px] text-text-subtle hover:text-text rounded-md border border-surface">Weekend</button>
                {windows.length > 1 && (
                  <button
                    type="button"
                    onClick={() => removeRow(i)}
                    title="Remove window"
                    className="ml-auto text-text-subtle hover:text-red transition-colors"
                    data-test-id={`${idPrefix}-remove-row`}
                  >
                    <Trash2 size={12} />
                  </button>
                )}
              </div>
              <div className="grid grid-cols-2 gap-2">
                <div>
                  <label className="block text-[11px] text-text-subtle mb-1">Start {i === 0 ? "" : `(${i + 1})`}</label>
                  <input
                    type="text"
                    // A plain HH:MM text field, not <input type="time"> — the
                    // native time picker's AM/PM vs 24h display follows the OS
                    // locale in most non-Chromium browsers, but peak_hours is
                    // always 24h HH:MM server-side, so we own the format.
                    inputMode="numeric"
                    pattern="([01]\d|2[0-3]):[0-5]\d"
                    placeholder="HH:MM"
                    maxLength={5}
                    value={w.start}
                    onChange={(e) => update(i, "start", e.target.value)}
                    className={inputCls}
                    data-test-id={i === 0 ? `${idPrefix}-start` : undefined}
                  />
                </div>
                <div>
                  <label className="block text-[11px] text-text-subtle mb-1">End {i === 0 ? "" : `(${i + 1})`} (exclusive)</label>
                  <input
                    type="text"
                    inputMode="numeric"
                    pattern="([01]\d|2[0-3]):[0-5]\d|24:00"
                    placeholder="HH:MM"
                    maxLength={5}
                    value={w.end}
                    onChange={(e) => update(i, "end", e.target.value)}
                    className={inputCls}
                    data-test-id={i === 0 ? `${idPrefix}-end` : undefined}
                  />
                </div>
              </div>
            </div>
          ))}
          <div className="flex items-center gap-3">
            <button
              type="button"
              onClick={addRow}
              className="flex items-center gap-1.5 text-xs text-accent hover:text-accent/80 transition-colors"
              data-test-id={`${idPrefix}-add-row`}
            >
              <Plus size={12} />
              Add window
            </button>
            <button
              type="button"
              onClick={() => onChange(null)}
              className="text-xs text-text-subtle hover:text-text transition-colors"
              data-test-id={`${idPrefix}-clear-all`}
            >
              Clear all
            </button>
            {windows.length > MAX_WINDOWS && (
              <span className="text-[10px] text-red">At most {MAX_WINDOWS} windows</span>
            )}
          </div>
          <div>
            <label htmlFor={`${idPrefix}-timezone`} className="block text-[11px] text-text-subtle mb-1">Timezone (optional)</label>
            <input
              id={`${idPrefix}-timezone`}
              type="text"
              value={value?.timezone ?? ""}
              onChange={(e) => onChange({ ...(value ?? defaultPolicy()), timezone: e.target.value })}
              placeholder="local, UTC, Asia/Shanghai, UTC+08:00"
              className={inputCls}
              data-test-id={`${idPrefix}-timezone`}
            />
          </div>
          <div>
            <label htmlFor={`${idPrefix}-message`} className="block text-[11px] text-text-subtle mb-1">
              Exit message (optional)
            </label>
            <textarea
              id={`${idPrefix}-message`}
              rows={2}
              placeholder="Shown after a blank line when rush exits because of this window"
              value={value?.message ?? ""}
              onChange={(e) => onChange({ ...(value ?? defaultPolicy()), message: e.target.value })}
              className="w-full text-xs bg-canvas border border-surface rounded-lg px-2.5 py-1.5 outline-none focus:border-accent/50 text-text placeholder:text-text-muted/50 resize-none"
              data-test-id={`${idPrefix}-message`}
            />
          </div>
          {problems.length > 0 && (
            <p className="text-[10px] text-red">
              {problems.map((p) => (
                <span key={p} className="block">{p}</span>
              ))}
            </p>
          )}
          {windows.length > 0 && (
            <p className="text-[10px] text-text-muted">
              Weekly windows · {peakSummary(value)} · whether it's active right now is decided by the server
            </p>
          )}
        </div>
      )}

      {(onPause || onResume) && (
        <div className="flex items-center gap-2 flex-wrap">
          {pausedUntil && onResume && (
            <>
              {/* pausedUntil is a backend RFC3339 string — formatted for
                  display only; the server decides whether the pause is still
                  in effect. */}
              <span className="text-[11px] text-text-subtle">Paused until {new Date(pausedUntil).toLocaleString()}</span>
              <button
                type="button"
                onClick={onResume}
                className="px-2 py-0.5 text-xs text-text-subtle hover:text-text rounded-lg border border-surface"
                data-test-id={`${idPrefix}-resume`}
              >
                Resume
              </button>
            </>
          )}
          {onPause && !pausedUntil && (
            <>
              <input
                type="text"
                value={pauseDuration}
                onChange={(e) => setPauseDuration(e.target.value)}
                placeholder="2h / 7d / empty = until the current window ends"
                className="flex-1 min-w-0 text-xs font-mono bg-canvas border border-surface rounded-lg px-2.5 py-1.5 outline-none focus:border-accent/50 text-text placeholder:text-text-muted/50"
                data-test-id={`${idPrefix}-pause-duration`}
              />
              <button
                type="button"
                // Empty input → undefined duration → the server picks the
                // end of the currently active band.
                onClick={() => { onPause?.(pauseDuration.trim() || undefined); setPauseDuration(""); }}
                className="px-2.5 py-1.5 text-xs text-text-subtle hover:text-text rounded-lg border border-surface"
                data-test-id={`${idPrefix}-pause`}
              >
                Pause
              </button>
            </>
          )}
        </div>
      )}
    </div>
  );
}
