import type { ButtonHTMLAttributes } from "react";
import { cn } from "@/lib/utils";

const variants = {
  primary: "bg-brand text-text hover:bg-brand-hover",
  secondary: "bg-surface-2 text-text border border-border hover:bg-[#262a2f]",
  ghost: "text-muted hover:text-text hover:bg-surface-2",
  danger: "bg-transparent text-error border border-error/40 hover:bg-error/10",
} as const;

const sizes = {
  sm: "h-7 px-2.5 text-xs gap-1.5",
  md: "h-8 px-3 text-[13px] gap-2",
  icon: "h-7 w-7 justify-center",
} as const;

export interface ButtonProps extends ButtonHTMLAttributes<HTMLButtonElement> {
  variant?: keyof typeof variants;
  size?: keyof typeof sizes;
}

export function Button({ variant = "secondary", size = "md", className, type = "button", ...props }: ButtonProps) {
  return (
    <button
      type={type}
      className={cn(
        "inline-flex shrink-0 cursor-pointer items-center rounded-sm font-medium whitespace-nowrap transition-colors",
        "disabled:pointer-events-none disabled:opacity-40 [&_svg]:size-3.5",
        variants[variant],
        sizes[size],
        className,
      )}
      {...props}
    />
  );
}
