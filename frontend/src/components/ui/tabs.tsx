import { type KeyboardEvent, type ReactNode, useRef } from "react";
import { useT } from "@/lib/i18n";
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
  const t = useT();

  const onKey = (e: KeyboardEvent) => {
    const i = items.findIndex((item) => item.id === value);
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
      {items.map((item) => {
        const selected = item.id === value;
        return (
          <button
            key={item.id}
            ref={(el) => {
              refs.current[item.id] = el;
            }}
            type="button"
            role="tab"
            id={`tab-${item.id}`}
            aria-selected={selected}
            aria-controls={`panel-${item.id}`}
            tabIndex={selected ? 0 : -1}
            onClick={() => onChange(item.id)}
            className={cn(
              "-mb-px flex h-9 cursor-pointer items-center gap-1.5 border-b-2 border-transparent px-3 text-[13px] whitespace-nowrap text-muted hover:text-text",
              selected && "border-brand text-text",
            )}
          >
            {item.label}
            {item.badge && <span className="size-1.5 rounded-full bg-warn" aria-label={t("pending changes")} />}
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
