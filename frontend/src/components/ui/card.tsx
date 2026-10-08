import type { HTMLAttributes, ReactNode } from "react";
import { cn } from "@/lib/utils";

// shadcn/ui's Card and its parts, in the panel's design (D47).

export function Card({ className, ...props }: HTMLAttributes<HTMLDivElement>) {
  return <div data-slot="card" className={cn("rounded-sm border border-border bg-surface-1", className)} {...props} />;
}

export function CardTitle({ className, ...props }: HTMLAttributes<HTMLHeadingElement>) {
  return <h2 data-slot="card-title" className={cn("text-sm font-semibold", className)} {...props} />;
}

export function CardDescription({ className, ...props }: HTMLAttributes<HTMLParagraphElement>) {
  return <p data-slot="card-description" className={cn("mt-0.5 text-xs text-muted", className)} {...props} />;
}

export function CardContent({ className, ...props }: HTMLAttributes<HTMLDivElement>) {
  return <div data-slot="card-content" className={cn("p-4", className)} {...props} />;
}

/** The header of a card: its title, what it is for and its actions. */
export function CardHeader({ title, description, actions }: { title: ReactNode; description?: ReactNode; actions?: ReactNode }) {
  return (
    <div data-slot="card-header" className="flex items-center justify-between gap-4 border-b border-border px-4 py-3">
      <div className="min-w-0">
        <CardTitle>{title}</CardTitle>
        {description && <CardDescription>{description}</CardDescription>}
      </div>
      {actions && (
        <div data-slot="card-action" className="flex shrink-0 items-center gap-2">
          {actions}
        </div>
      )}
    </div>
  );
}

export function PageHeader({ title, description, actions }: { title: string; description?: ReactNode; actions?: ReactNode }) {
  return (
    <div className="mb-4 flex items-end justify-between gap-4">
      <div>
        <h1 className="text-lg font-semibold tracking-tight">{title}</h1>
        {description && <p className="mt-0.5 text-[13px] text-muted">{description}</p>}
      </div>
      {actions && <div className="flex items-center gap-2">{actions}</div>}
    </div>
  );
}

export function Empty({ icon, title, children }: { icon?: ReactNode; title: string; children?: ReactNode }) {
  return (
    <div className="flex flex-col items-center justify-center gap-2 px-6 py-12 text-center">
      {icon && <div className="text-muted [&_svg]:size-6">{icon}</div>}
      <p className="text-sm font-medium">{title}</p>
      {children && <div className="max-w-md text-[13px] text-muted">{children}</div>}
    </div>
  );
}

export function Notice({ tone = "info", children }: { tone?: "info" | "error" | "warn" | "ok"; children: ReactNode }) {
  const tones = {
    info: "border-info/40 bg-info/10",
    error: "border-error/40 bg-error/10",
    warn: "border-warn/40 bg-warn/10",
    ok: "border-ok/40 bg-ok/10",
  };
  return <div className={cn("rounded-sm border px-3 py-2 text-[13px]", tones[tone])}>{children}</div>;
}
