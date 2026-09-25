import type { InputHTMLAttributes, ReactNode, SelectHTMLAttributes, TextareaHTMLAttributes } from "react";
import { cn } from "@/lib/utils";

const control =
  "h-8 w-full rounded-sm border border-border bg-surface-2 px-2.5 text-[13px] text-text placeholder:text-muted/60 " +
  "focus:border-info focus:outline-none disabled:opacity-50";

export function Input({ className, ...props }: InputHTMLAttributes<HTMLInputElement>) {
  return <input className={cn(control, className)} {...props} />;
}

export function Textarea({ className, ...props }: TextareaHTMLAttributes<HTMLTextAreaElement>) {
  return <textarea className={cn(control, "h-auto min-h-16 py-1.5", className)} {...props} />;
}

export function Select({ className, children, ...props }: SelectHTMLAttributes<HTMLSelectElement>) {
  return (
    <select className={cn(control, "pr-7", className)} {...props}>
      {children}
    </select>
  );
}

export function Checkbox({ label, className, ...props }: InputHTMLAttributes<HTMLInputElement> & { label: ReactNode }) {
  return (
    <label className={cn("inline-flex cursor-pointer items-center gap-2 text-[13px]", className)}>
      <input type="checkbox" className="size-3.5 accent-brand" {...props} />
      {label}
    </label>
  );
}

interface FieldProps {
  label: string;
  hint?: ReactNode;
  error?: string;
  children: ReactNode;
  className?: string;
  /** Renders a group of controls that carry their own labels (checkboxes). */
  group?: boolean;
}

export function Field({ label, hint, error, children, className, group }: FieldProps) {
  const caption = <span className="text-xs font-medium text-muted">{label}</span>;
  return (
    <div className={cn("flex flex-col gap-1", className)}>
      {group ? (
        <div role="group" aria-label={label} className="flex flex-col gap-1">
          {caption}
          {children}
        </div>
      ) : (
        <label className="flex flex-col gap-1">
          {caption}
          {children}
        </label>
      )}
      {error ? <span className="text-xs text-error">{error}</span> : hint ? <span className="text-xs text-muted/80">{hint}</span> : null}
    </div>
  );
}
