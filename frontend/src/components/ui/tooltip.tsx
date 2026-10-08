import { Tooltip as TooltipPrimitive } from "radix-ui";
import type { ReactNode } from "react";

/** Wraps the panel once: tooltips share their delays. */
export function TooltipProvider({ children }: { children: ReactNode }) {
  return (
    <TooltipPrimitive.Provider delayDuration={300} skipDelayDuration={200}>
      {children}
    </TooltipPrimitive.Provider>
  );
}

/**
 * A tooltip on Radix's (shadcn/ui's Tooltip): it shows on hover and on
 * keyboard focus, and screen readers announce it with its trigger.
 * Positioning goes through the style object, never an inline style
 * attribute, so it runs under the panel's CSP.
 */
export function Tooltip({ content, children }: { content: ReactNode; children: ReactNode }) {
  return (
    <TooltipPrimitive.Root>
      <TooltipPrimitive.Trigger asChild>{children}</TooltipPrimitive.Trigger>
      <TooltipPrimitive.Portal>
        <TooltipPrimitive.Content
          data-slot="tooltip-content"
          side="top"
          sideOffset={4}
          collisionPadding={8}
          className="z-50 max-w-80 rounded-sm border border-border bg-surface-2 px-2 py-1 text-xs text-text shadow-lg"
        >
          {content}
        </TooltipPrimitive.Content>
      </TooltipPrimitive.Portal>
    </TooltipPrimitive.Root>
  );
}
