import { Tabs as TabsPrimitive } from "radix-ui";
import type { ReactNode } from "react";
import { useT } from "@/lib/i18n";
import { cn } from "@/lib/utils";

export interface TabItem {
  id: string;
  label: string;
  /** A small marker after the label, e.g. pending changes. */
  badge?: boolean;
}

/**
 * Tabs on Radix's (shadcn/ui's Tabs): arrow keys, Home and End move between
 * them. The selection lives in the caller (usually the URL), so tabs can
 * be linked to and survive a reload; the panel of the selected tab is the
 * TabPanel among the children.
 */
export function Tabs({
  items,
  value,
  onChange,
  label,
  className,
  children,
}: {
  items: TabItem[];
  value: string;
  onChange: (id: string) => void;
  label: string;
  className?: string;
  children?: ReactNode;
}) {
  const t = useT();
  return (
    <TabsPrimitive.Root value={value} onValueChange={onChange} data-slot="tabs">
      <TabsPrimitive.List aria-label={label} className={cn("flex gap-1 border-b border-border", className)}>
        {items.map((item) => (
          <TabsPrimitive.Trigger
            key={item.id}
            value={item.id}
            className={cn(
              "-mb-px flex h-9 cursor-pointer items-center gap-1.5 border-b-2 border-transparent px-3 text-[13px] whitespace-nowrap text-muted",
              "outline-none hover:text-text focus-visible:text-text data-[state=active]:border-brand data-[state=active]:text-text",
            )}
          >
            {item.label}
            {item.badge && <span className="size-1.5 rounded-full bg-warn" aria-label={t("pending changes")} />}
          </TabsPrimitive.Trigger>
        ))}
      </TabsPrimitive.List>
      {children}
    </TabsPrimitive.Root>
  );
}

/** The panel of a tab; only the selected one renders. */
export function TabPanel({ id, children }: { id: string; children: ReactNode }) {
  return (
    <TabsPrimitive.Content value={id} className="pt-4 outline-none" data-slot="tabs-content">
      {children}
    </TabsPrimitive.Content>
  );
}
