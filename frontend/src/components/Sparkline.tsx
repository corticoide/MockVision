import { cn } from "@/lib/utils";

export interface Series {
  values: number[];
  /** A text color class; the line and its area take it. */
  className: string;
}

/**
 * A small line chart drawn as SVG paths: attributes only, so it works under
 * the panel's strict CSP. Every series shares the vertical scale, from zero
 * to max (or the largest value).
 */
export function Sparkline({ series, max, label, className }: { series: Series[]; max?: number; label: string; className?: string }) {
  const w = 120;
  const h = 32;
  const top = max ?? Math.max(1, ...series.flatMap((s) => s.values));
  const points = (values: number[]) =>
    values.map((v, i) => {
      const x = values.length === 1 ? w : (i / (values.length - 1)) * w;
      const y = h - (Math.min(Math.max(v, 0), top) / top) * (h - 2) - 1;
      return `${x.toFixed(2)},${y.toFixed(2)}`;
    });
  return (
    <svg viewBox={`0 0 ${w} ${h}`} preserveAspectRatio="none" role="img" aria-label={label} className={cn("h-8 w-full", className)}>
      {series.map((s, i) => {
        if (s.values.length === 0) return null;
        const p = points(s.values);
        return (
          <g key={i} className={s.className}>
            {i === 0 && <path d={`M0,${h} L${p.join(" L")} L${w},${h} Z`} fill="currentColor" fillOpacity={0.12} />}
            <path d={`M${p.join(" L")}`} fill="none" stroke="currentColor" strokeWidth={1.5} vectorEffect="non-scaling-stroke" />
          </g>
        );
      })}
    </svg>
  );
}

export interface Segment {
  value: number;
  className: string;
  label: string;
}

/** A horizontal bar split in proportional segments, such as cameras by state. */
export function StackedBar({ segments, label }: { segments: Segment[]; label: string }) {
  const total = segments.reduce((n, s) => n + s.value, 0);
  let x = 0;
  return (
    <svg viewBox="0 0 100 6" preserveAspectRatio="none" role="img" aria-label={label} className="h-1.5 w-full overflow-hidden rounded-sm">
      <rect x={0} y={0} width={100} height={6} className="fill-surface-2" />
      {total > 0 &&
        segments.map((s) => {
          const width = (s.value / total) * 100;
          const r = <rect key={s.label} x={x} y={0} width={width} height={6} fill="currentColor" className={s.className}><title>{s.label}</title></rect>;
          x += width;
          return r;
        })}
    </svg>
  );
}

/** A progress bar from 0 to 1, drawn as SVG like the charts. */
export function ProgressBar({ value, label, className }: { value: number; label: string; className?: string }) {
  const width = Math.min(Math.max(value, 0), 1) * 100;
  return (
    <svg
      viewBox="0 0 100 6"
      preserveAspectRatio="none"
      role="progressbar"
      aria-label={label}
      aria-valuemin={0}
      aria-valuemax={100}
      aria-valuenow={Math.round(width)}
      className={cn("h-1.5 w-full overflow-hidden rounded-sm", className)}
    >
      <rect x={0} y={0} width={100} height={6} className="fill-surface-2" />
      <rect x={0} y={0} width={width} height={6} fill="currentColor" />
    </svg>
  );
}
