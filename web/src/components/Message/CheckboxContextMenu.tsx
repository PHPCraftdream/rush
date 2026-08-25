import { useEffect, useRef } from "react";
import { ChevronsUp, ChevronsDown } from "lucide-react";

export function CheckboxContextMenu({
  x, y, onSelectAbove, onSelectBelow, onClose,
}: {
  x: number; y: number; onSelectAbove: () => void; onSelectBelow: () => void; onClose: () => void;
}) {
  const ref = useRef<HTMLDivElement>(null);

  useEffect(() => {
    function onMouseDown(e: MouseEvent) {
      if (ref.current && !ref.current.contains(e.target as Node)) onClose();
    }
    function onKey(e: KeyboardEvent) {
      if (e.key === "Escape") onClose();
    }
    document.addEventListener("mousedown", onMouseDown);
    document.addEventListener("keydown", onKey);
    return () => {
      document.removeEventListener("mousedown", onMouseDown);
      document.removeEventListener("keydown", onKey);
    };
  }, [onClose]);

  return (
    <div
      ref={ref}
      className="fixed z-[9999] min-w-[200px] py-1 rounded-xl border border-surface bg-canvas shadow-xl chat-font"
      style={{ left: x, top: y }}
    >
      <button
        onClick={onSelectAbove}
        className="w-full flex items-center gap-2 px-3 py-1.5 text-sm text-text hover:bg-base-overlay transition-colors"
      >
        <ChevronsUp size={14} className="text-text-subtle" />
        + Select all above
      </button>
      <button
        onClick={onSelectBelow}
        className="w-full flex items-center gap-2 px-3 py-1.5 text-sm text-text hover:bg-base-overlay transition-colors"
      >
        <ChevronsDown size={14} className="text-text-subtle" />
        + Select all below
      </button>
    </div>
  );
}
