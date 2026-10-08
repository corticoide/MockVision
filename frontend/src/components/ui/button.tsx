import { cva, type VariantProps } from "class-variance-authority";
import { Slot } from "radix-ui";
import type { ButtonHTMLAttributes } from "react";
import { cn } from "@/lib/utils";

export const buttonVariants = cva(
  "inline-flex shrink-0 cursor-pointer items-center rounded-sm font-medium whitespace-nowrap transition-colors outline-none " +
    "focus-visible:ring-2 focus-visible:ring-info/60 disabled:pointer-events-none disabled:opacity-40 aria-disabled:pointer-events-none " +
    "aria-disabled:opacity-40 [&_svg]:size-3.5 [&_svg]:shrink-0",
  {
    variants: {
      variant: {
        primary: "bg-brand text-text hover:bg-brand-hover",
        secondary: "border border-border bg-surface-2 text-text hover:bg-[#262a2f]",
        ghost: "text-muted hover:bg-surface-2 hover:text-text",
        danger: "border border-error/40 bg-transparent text-error hover:bg-error/10",
      },
      size: {
        sm: "h-7 gap-1.5 px-2.5 text-xs",
        md: "h-8 gap-2 px-3 text-[13px]",
        icon: "h-7 w-7 justify-center",
      },
    },
    defaultVariants: { variant: "secondary", size: "md" },
  },
);

export interface ButtonProps extends ButtonHTMLAttributes<HTMLButtonElement>, VariantProps<typeof buttonVariants> {
  /** Renders its child, such as a link, with the button's look (shadcn/ui's asChild). */
  asChild?: boolean;
}

export function Button({ variant, size, className, asChild = false, type = "button", ...props }: ButtonProps) {
  const cls = cn(buttonVariants({ variant, size }), className);
  if (asChild) return <Slot.Root data-slot="button" className={cls} {...props} />;
  return <button data-slot="button" type={type} className={cls} {...props} />;
}
