import type { HTMLAttributes, TdHTMLAttributes, ThHTMLAttributes } from "react";
import { cn } from "@/lib/utils";

export function Table({ className, ...props }: HTMLAttributes<HTMLTableElement>) {
  return (
    <div data-slot="table-container" className="overflow-x-auto">
      <table data-slot="table" className={cn("w-full border-collapse text-[13px]", className)} {...props} />
    </div>
  );
}

export function THead(props: HTMLAttributes<HTMLTableSectionElement>) {
  return <thead data-slot="table-header" className="border-b border-border bg-surface-1" {...props} />;
}

export function TBody(props: HTMLAttributes<HTMLTableSectionElement>) {
  return <tbody data-slot="table-body" {...props} />;
}

export function TR({ className, ...props }: HTMLAttributes<HTMLTableRowElement>) {
  return <tr data-slot="table-row" className={cn("h-8 border-b border-border/70 last:border-b-0 hover:bg-surface-2/60", className)} {...props} />;
}

export function TH({ className, ...props }: ThHTMLAttributes<HTMLTableCellElement>) {
  return (
    <th
      data-slot="table-head"
      className={cn("h-8 px-3 text-left text-[11px] font-medium tracking-wide text-muted uppercase whitespace-nowrap", className)}
      {...props}
    />
  );
}

export function TD({ className, ...props }: TdHTMLAttributes<HTMLTableCellElement>) {
  return <td data-slot="table-cell" className={cn("px-3 py-1 align-middle whitespace-nowrap", className)} {...props} />;
}
