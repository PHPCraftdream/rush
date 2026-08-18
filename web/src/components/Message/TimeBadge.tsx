// Timestamp badge shown next to event headers, with its local-time formatters.
// Pure code move from the former components/Message.tsx.

import { memo } from "react";

// formatEventTime returns "HH:MM:SS" in the operator's local timezone
// (Intl picks the system TZ automatically). The value is shown next to
// every event header — message, tool group, tool row, thinking row — so
// the operator can correlate the chat against logs / external runs.
// Epoch seconds; we keep 1s precision intentionally (matches the int64
// column in SQLite).
function formatEventTime(epochSec: number | undefined): string {
  if (!epochSec || epochSec <= 0) return "";
  const d = new Date(epochSec * 1000);
  // Force 24h + seconds; locale may otherwise drop seconds in toLocaleTimeString.
  const hh = String(d.getHours()).padStart(2, "0");
  const mm = String(d.getMinutes()).padStart(2, "0");
  const ss = String(d.getSeconds()).padStart(2, "0");
  return `${hh}:${mm}:${ss}`;
}

// formatEventDateTime — for tooltips: full local "YYYY-MM-DD HH:MM:SS".
function formatEventDateTime(epochSec: number | undefined): string {
  if (!epochSec || epochSec <= 0) return "";
  const d = new Date(epochSec * 1000);
  const yyyy = d.getFullYear();
  const mo = String(d.getMonth() + 1).padStart(2, "0");
  const dd = String(d.getDate()).padStart(2, "0");
  const hh = String(d.getHours()).padStart(2, "0");
  const mm = String(d.getMinutes()).padStart(2, "0");
  const ss = String(d.getSeconds()).padStart(2, "0");
  const tz = -d.getTimezoneOffset();
  const tzSign = tz >= 0 ? "+" : "-";
  const tzAbs = Math.abs(tz);
  const tzH = String(Math.floor(tzAbs / 60)).padStart(2, "0");
  const tzM = String(tzAbs % 60).padStart(2, "0");
  return `${yyyy}-${mo}-${dd} ${hh}:${mm}:${ss} UTC${tzSign}${tzH}:${tzM}`;
}

export const TimeBadge = memo(function TimeBadge({ epochSec }: { epochSec: number | undefined }) {
  const text = formatEventTime(epochSec);
  if (!text) return null;
  return (
    <span
      className="text-xs text-text-subtle font-mono tabular-nums"
      title={formatEventDateTime(epochSec)}
    >
      {text}
    </span>
  );
});
