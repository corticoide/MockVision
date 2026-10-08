import { X } from "lucide-react";
import { Dialog as DialogPrimitive } from "radix-ui";
import type { ReactNode } from "react";
import { useT } from "@/lib/i18n";
import { cn } from "@/lib/utils";
import { Button } from "./button";

interface DialogProps {
  open: boolean;
  onClose: () => void;
  title: string;
  description?: ReactNode;
  children: ReactNode;
  footer?: ReactNode;
  className?: string;
}

/**
 * A modal dialog on Radix's (shadcn/ui's Dialog): focus trapped, Escape and
 * a click outside close it, the page behind does not scroll. The scroll lock
 * adds a style element, which carries the page's nonce (lib/nonce).
 */
export function Dialog({ open, onClose, title, description, children, footer, className }: DialogProps) {
  const t = useT();
  return (
    <DialogPrimitive.Root open={open} onOpenChange={(o) => !o && onClose()}>
      <DialogPrimitive.Portal>
        <DialogPrimitive.Overlay data-slot="dialog-overlay" className="fixed inset-0 z-50 bg-black/60" />
        <DialogPrimitive.Content
          data-slot="dialog-content"
          className={cn(
            "fixed top-1/2 left-1/2 z-50 flex max-h-[85vh] w-[min(640px,calc(100vw-32px))] -translate-x-1/2 -translate-y-1/2 flex-col",
            "rounded-sm border border-border bg-surface-1 text-text shadow-2xl outline-none",
            className,
          )}
        >
          <div className="flex items-start justify-between gap-4 border-b border-border px-4 py-3">
            <div>
              <DialogPrimitive.Title className="text-sm font-semibold">{title}</DialogPrimitive.Title>
              {description ? (
                <DialogPrimitive.Description className="mt-0.5 text-xs text-muted">{description}</DialogPrimitive.Description>
              ) : (
                <DialogPrimitive.Description className="sr-only">{title}</DialogPrimitive.Description>
              )}
            </div>
            <DialogPrimitive.Close asChild>
              <Button variant="ghost" size="icon" aria-label={t("Close")}>
                <X />
              </Button>
            </DialogPrimitive.Close>
          </div>
          <div className="overflow-y-auto px-4 py-4">{children}</div>
          {footer && <div className="flex justify-end gap-2 border-t border-border px-4 py-3">{footer}</div>}
        </DialogPrimitive.Content>
      </DialogPrimitive.Portal>
    </DialogPrimitive.Root>
  );
}
