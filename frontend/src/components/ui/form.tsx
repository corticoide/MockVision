import { Check } from "lucide-react";
import { Checkbox as CheckboxPrimitive, Label as LabelPrimitive } from "radix-ui";
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

interface CheckboxProps {
  label: ReactNode;
  checked: boolean;
  onCheckedChange: (checked: boolean) => void;
  disabled?: boolean;
  className?: string;
  title?: string;
  "aria-label"?: string;
}

/** A checkbox on Radix's (shadcn/ui's Checkbox), with its label. */
export function Checkbox({ label, checked, onCheckedChange, disabled, className, title, "aria-label": ariaLabel }: CheckboxProps) {
  return (
    <LabelPrimitive.Root
      data-slot="checkbox-label"
      title={title}
      className={cn("inline-flex cursor-pointer items-center gap-2 text-[13px] has-[button:disabled]:cursor-default has-[button:disabled]:opacity-50", className)}
    >
      <CheckboxPrimitive.Root
        data-slot="checkbox"
        checked={checked}
        disabled={disabled}
        aria-label={ariaLabel}
        onCheckedChange={(checked) => onCheckedChange(checked === true)}
        className={cn(
          "grid size-3.5 shrink-0 place-content-center rounded-[3px] border border-border bg-surface-2 outline-none",
          "focus-visible:ring-2 focus-visible:ring-info/60 data-[state=checked]:border-brand data-[state=checked]:bg-brand",
        )}
      >
        <CheckboxPrimitive.Indicator className="text-text [&_svg]:size-3">
          <Check strokeWidth={3} />
        </CheckboxPrimitive.Indicator>
      </CheckboxPrimitive.Root>
      {label}
    </LabelPrimitive.Root>
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
        <LabelPrimitive.Root data-slot="label" className="flex flex-col gap-1">
          {caption}
          {children}
        </LabelPrimitive.Root>
      )}
      {error ? <span className="text-xs text-error">{error}</span> : hint ? <span className="text-xs text-muted/80">{hint}</span> : null}
    </div>
  );
}
