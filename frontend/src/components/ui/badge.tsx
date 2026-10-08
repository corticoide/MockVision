import { cva, type VariantProps } from "class-variance-authority";
import type { HTMLAttributes } from "react";
import { cn } from "@/lib/utils";

export const badgeVariants = cva(
  "inline-flex h-5 items-center gap-1 rounded-sm border px-1.5 text-[11px] font-medium whitespace-nowrap [&_svg]:size-3 [&_svg]:shrink-0",
  {
    variants: {
      tone: {
        ok: "border-ok/35 bg-ok/10 text-ok",
        warn: "border-warn/35 bg-warn/10 text-warn",
        error: "border-error/35 bg-error/10 text-error",
        info: "border-info/35 bg-info/10 text-info",
        muted: "border-border bg-surface-2 text-muted",
      },
    },
    defaultVariants: { tone: "muted" },
  },
);

export type BadgeTone = NonNullable<VariantProps<typeof badgeVariants>["tone"]>;

/** shadcn/ui's Badge, in the panel's tones. */
export function BadgeBase({ tone, className, ...props }: HTMLAttributes<HTMLSpanElement> & VariantProps<typeof badgeVariants>) {
  return <span data-slot="badge" className={cn(badgeVariants({ tone }), className)} {...props} />;
}
