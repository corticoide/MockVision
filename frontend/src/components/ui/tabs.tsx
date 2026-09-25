import { type KeyboardEvent, type ReactNode, useRef } from "react";
import { cn } from "@/lib/utils";

export interface TabItem {
  id: string;
  label: string;
  /** A small marker after the label, e.g. pending changes. */
  badge?: boolean;
}

/**
 * Tabs renders an accessible tab list. The selection lives in the caller
 * (usually the URL), so tabs can be linked to and survive a reload.
 */
export function Tabs({
  items,
  value,
  onChange,
  label,
  className,
}: {
  items: TabItem[];
  value: string;
  onChange: (id: string) => void;
  label: string;
  className?: string;
}) {
  const refs = useRef<Record<string, HTMLButtonElement | null>>({});

  const onKey = (e: KeyboardEvent) => {
    const i = items.findIndex((t) => t.id === value);
    let next = -1;
    if (e.key === "ArrowRight") next = (i + 1) % items.length;
    if (e.key === "ArrowLeft") next = (i - 1 + items.length) % items.length;
    if (e.key === "Home") next = 0;
    if (e.key === "End") next = items.length - 1;
    if (next < 0) return;
    e.preventDefault();
    onChange(items[next].id);
    refs.current[items[next].id]?.focus();
  };

  return (
    <div role="tablist" aria-label={label} onKeyDown={onKey} className={cn("flex gap-1 border-b border-border", className)}>
      {items.map((t) => {
        const selected = t.id === value;
        return (
          <button
            key={t.id}
            ref={(el) => {
              refs.current[t.id] = el;
            }}
            type="button"
            role="tab"
            id={`tab-${t.id}`}
            aria-selected={selected}
            aria-controls={`panel-${t.id}`}
            tabIndex={selected ? 0 : -1}
            onClick={() => onChange(t.id)}
            className={cn(
              "-mb-px flex h-9 cursor-pointer items-center gap-1.5 border-b-2 border-transparent px-3 text-[13px] text-muted hover:text-text",
              selected && "border-brand text-text",
            )}
          >
            {t.label}
            {t.badge && <span className="size-1.5 rounded-full bg-warn" aria-label="pending changes" />}
          </button>
        );
      })}
    </div>
  );
}

export function TabPanel({ id, children }: { id: string; children: ReactNode }) {
  return (
    <div role="tabpanel" id={`panel-${id}`} aria-labelledby={`tab-${id}`} className="pt-4">
      {children}
    </div>
  );
}
