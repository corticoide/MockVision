import { type KeyboardEvent, type PointerEvent, useEffect, useRef, useState } from "react";
import type { Analytics, Camera, Point } from "@/api/client";
import { useT } from "@/lib/i18n";
import { cn } from "@/lib/utils";
import { centroid, clamp01, pictureSize, round4, ruleColors, sideB } from "@/lib/vca";
import { isRunning } from "./parts";

/** A rule as the editor draws it. */
export interface CanvasRule {
  key: string;
  name: string;
  type: "line" | "region";
  points: Point[];
  direction: string;
  enabled: boolean;
}

/** What is being drawn: a new rule, or new points for an existing one. */
export interface Drawing {
  type: "line" | "region";
  index: number | null;
}

type Drag = { kind: "vertex"; index: number } | { kind: "shape"; start: Point; origin: Point[] };

/**
 * RuleCanvas draws a camera's rules over its picture and edits them: click
 * to draw a line or a region, drag a corner or the whole shape, or move a
 * focused corner with the arrow keys. Coordinates are fractions of the
 * picture, as the node stores them. Everything is SVG attributes, which the
 * panel's CSP allows.
 */
export function RuleCanvas({
  camera,
  rules,
  selected,
  drawing,
  onSelect,
  onChange,
  onDrawn,
  onCancel,
  heat,
}: {
  camera: Camera;
  rules: CanvasRule[];
  selected: number | null;
  drawing: Drawing | null;
  onSelect: (index: number | null) => void;
  onChange: (index: number, points: Point[]) => void;
  onDrawn: (points: Point[]) => void;
  onCancel: () => void;
  /** A heat map to draw under the rules: objects seen in each cell. */
  heat?: Analytics["heat"];
}) {
  const t = useT();
  const svg = useRef<SVGSVGElement>(null);
  const drag = useRef<Drag | null>(null);
  const { w, h } = pictureSize(camera);
  const u = w / 100; // one percent of the width, the unit of sizes
  const [points, setPoints] = useState<Point[]>([]);
  const [pointer, setPointer] = useState<Point | null>(null);

  // A snapshot of the main stream, refreshed every 5 s while the camera runs.
  const running = isRunning(camera);
  const [tick, setTick] = useState(0);
  useEffect(() => {
    if (!running) return;
    const timer = setInterval(() => setTick((n) => n + 1), 5000);
    return () => clearInterval(timer);
  }, [running]);
  const main = camera.streams.find((s) => s.name === "main") ?? camera.streams[0];
  const picture = main?.rendition_status === "ready";

  useEffect(() => {
    setPoints([]);
    setPointer(null);
    if (drawing) svg.current?.focus();
  }, [drawing]);

  const at = (e: { clientX: number; clientY: number }): Point => {
    const r = svg.current!.getBoundingClientRect();
    return { x: round4(clamp01((e.clientX - r.left) / r.width)), y: round4(clamp01((e.clientY - r.top) / r.height)) };
  };
  // Closer than 10 px on the screen.
  const near = (a: Point, b: Point) => {
    const r = svg.current!.getBoundingClientRect();
    return Math.hypot((a.x - b.x) * r.width, (a.y - b.y) * r.height) < 10;
  };

  const down = (e: PointerEvent<SVGSVGElement>) => {
    if (e.button !== 0) return;
    const p = at(e);
    if (drawing?.type === "line") {
      if (points.length === 0) setPoints([p]);
      else if (!near(points[0], p)) onDrawn([points[0], p]);
      return;
    }
    if (drawing?.type === "region") {
      if (points.length >= 3 && near(points[0], p)) onDrawn(points);
      else if (points.length === 0 || !near(points[points.length - 1], p)) setPoints([...points, p]);
      return;
    }
    onSelect(null);
  };

  const move = (e: PointerEvent<SVGSVGElement>) => {
    const p = at(e);
    if (drawing) {
      setPointer(p);
      return;
    }
    const d = drag.current;
    if (!d || selected == null) return;
    const rule = rules[selected];
    if (d.kind === "vertex") {
      onChange(selected, rule.points.map((q, i) => (i === d.index ? p : q)));
      return;
    }
    // The whole shape moves, as far as the picture's edges allow.
    let dx = p.x - d.start.x;
    let dy = p.y - d.start.y;
    for (const q of d.origin) {
      dx = Math.min(Math.max(dx, -q.x), 1 - q.x);
      dy = Math.min(Math.max(dy, -q.y), 1 - q.y);
    }
    onChange(selected, d.origin.map((q) => ({ x: round4(q.x + dx), y: round4(q.y + dy) })));
  };

  const key = (e: KeyboardEvent<SVGSVGElement>) => {
    if (e.key === "Escape") {
      e.preventDefault();
      if (drawing) onCancel();
      else onSelect(null);
    } else if (drawing?.type === "region" && e.key === "Enter" && points.length >= 3) {
      e.preventDefault();
      onDrawn(points);
    } else if (drawing && e.key === "Backspace") {
      e.preventDefault();
      setPoints(points.slice(0, -1));
    }
  };

  const grab = (e: PointerEvent<SVGElement>, index: number, d: Drag) => {
    if (drawing || e.button !== 0) return;
    e.stopPropagation();
    onSelect(index);
    if (index === selected || d.kind === "vertex") {
      (e.currentTarget as Element).setPointerCapture(e.pointerId);
      drag.current = d;
    }
  };

  const nudge = (e: KeyboardEvent<SVGCircleElement>, vertex: number) => {
    const step = e.shiftKey ? 0.05 : 0.005;
    const d = { ArrowLeft: [-step, 0], ArrowRight: [step, 0], ArrowUp: [0, -step], ArrowDown: [0, step] }[e.key];
    if (!d || selected == null) return;
    e.preventDefault();
    e.stopPropagation();
    const rule = rules[selected];
    onChange(selected, rule.points.map((q, i) => (i === vertex ? { x: round4(clamp01(q.x + d[0])), y: round4(clamp01(q.y + d[1])) } : q)));
  };

  const xy = (p: Point) => `${p.x * w},${p.y * h}`;
  const label = (x: number, y: number, text: string, anchor: "start" | "middle" = "middle") => (
    <text
      x={x}
      y={y}
      fontSize={u * 1.9}
      fontWeight={600}
      textAnchor={anchor}
      dominantBaseline="central"
      fill="currentColor"
      stroke="#000"
      strokeWidth={u * 0.35}
      paintOrder="stroke"
      pointerEvents="none"
    >
      {text}
    </text>
  );
  const arrow = (from: Point, to: Point, heads: boolean[]) => {
    const dx = to.x - from.x;
    const dy = to.y - from.y;
    const n = Math.hypot(dx, dy) || 1;
    const ux = dx / n;
    const uy = dy / n;
    const s = u * 1.2;
    const head = (tip: Point, dir: number) =>
      `${tip.x},${tip.y} ${tip.x - dir * ux * s - uy * s * 0.6},${tip.y - dir * uy * s + ux * s * 0.6} ${tip.x - dir * ux * s + uy * s * 0.6},${tip.y - dir * uy * s - ux * s * 0.6}`;
    return (
      <g pointerEvents="none">
        <line x1={from.x} y1={from.y} x2={to.x} y2={to.y} stroke="#000" strokeOpacity={0.7} strokeWidth={5} vectorEffect="non-scaling-stroke" />
        <line x1={from.x} y1={from.y} x2={to.x} y2={to.y} stroke="currentColor" strokeWidth={2} vectorEffect="non-scaling-stroke" />
        {heads[0] && <polygon points={head(from, -1)} fill="currentColor" stroke="#000" strokeOpacity={0.7} strokeWidth={1} vectorEffect="non-scaling-stroke" />}
        {heads[1] && <polygon points={head(to, 1)} fill="currentColor" stroke="#000" strokeOpacity={0.7} strokeWidth={1} vectorEffect="non-scaling-stroke" />}
      </g>
    );
  };

  const shape = (rule: CanvasRule, i: number) => {
    const isSelected = i === selected;
    const color = ruleColors[i % ruleColors.length];
    const width = isSelected ? 3.5 : 2.5;
    const stroke = { stroke: "currentColor", strokeWidth: width, vectorEffect: "non-scaling-stroke" as const };
    // A dark halo keeps the shape legible over any picture.
    const halo = { stroke: "#000", strokeOpacity: 0.7, strokeWidth: width + 3, vectorEffect: "non-scaling-stroke" as const, pointerEvents: "none" as const };
    const dash = rule.enabled ? undefined : "8 6";
    const hit = (e: PointerEvent<SVGElement>) => grab(e, i, { kind: "shape", start: at(e), origin: rule.points });
    if (rule.type === "line" && rule.points.length === 2) {
      const [a, b] = rule.points;
      const mid = { x: ((a.x + b.x) / 2) * w, y: ((a.y + b.y) / 2) * h };
      const nb = sideB(a, b, w, h);
      const off = (k: number) => ({ x: mid.x + nb.x * k * u, y: mid.y + nb.y * k * u });
      // Arrows go from the side objects come from to the side they reach.
      const heads: boolean[] = rule.direction === "A->B" ? [false, true] : rule.direction === "B->A" ? [true, false] : [true, true];
      return (
        <g key={rule.key} className={cn(color, !rule.enabled && "opacity-60")}>
          <title>{rule.name}</title>
          <line x1={a.x * w} y1={a.y * h} x2={b.x * w} y2={b.y * h} {...halo} />
          <line x1={a.x * w} y1={a.y * h} x2={b.x * w} y2={b.y * h} {...stroke} strokeDasharray={dash} />
          <line
            x1={a.x * w}
            y1={a.y * h}
            x2={b.x * w}
            y2={b.y * h}
            stroke="transparent"
            strokeWidth={16}
            vectorEffect="non-scaling-stroke"
            className={cn(!drawing && "cursor-move")}
            onPointerDown={hit}
          />
          {arrow(off(-3.5), off(3.5), heads)}
          {label(off(-6).x, off(-6).y, "A")}
          {label(off(6).x, off(6).y, "B")}
          {label(a.x * w + u, a.y * h - u * 2, rule.name, "start")}
        </g>
      );
    }
    const c = centroid(rule.points);
    return (
      <g key={rule.key} className={cn(color, !rule.enabled && "opacity-60")}>
        <title>{rule.name}</title>
        <polygon points={rule.points.map(xy).join(" ")} fill="none" strokeLinejoin="round" {...halo} />
        <polygon
          points={rule.points.map(xy).join(" ")}
          fill="currentColor"
          fillOpacity={isSelected ? 0.22 : 0.12}
          {...stroke}
          strokeDasharray={dash}
          strokeLinejoin="round"
          className={cn(!drawing && "cursor-move")}
          onPointerDown={hit}
        />
        {label(c.x * w, c.y * h, rule.name)}
      </g>
    );
  };

  const current = selected != null && !drawing ? rules[selected] : null;
  const preview = drawing ? [...points, ...(pointer ? [pointer] : [])] : [];

  return (
    <svg
      ref={svg}
      viewBox={`0 0 ${w} ${h}`}
      tabIndex={0}
      role="group"
      aria-label={t("Rules on the picture of {name}", { name: camera.name })}
      className={cn(
        "block h-auto w-full touch-none rounded-sm border border-border bg-black select-none focus:outline-none focus-visible:border-info",
        drawing && "cursor-crosshair",
      )}
      onPointerDown={down}
      onPointerMove={move}
      onPointerUp={() => (drag.current = null)}
      onPointerLeave={() => setPointer(null)}
      onDoubleClick={() => drawing?.type === "region" && points.length >= 3 && onDrawn(points)}
      onKeyDown={key}
    >
      {picture ? (
        <image
          href={`/api/v1/cameras/${camera.id}/snapshot?stream=${encodeURIComponent(main.name)}&v=${main.rendition_id ?? ""}-${tick}`}
          x={0}
          y={0}
          width={w}
          height={h}
          preserveAspectRatio="none"
        />
      ) : (
        <text x={w / 2} y={h / 2} fontSize={u * 2} textAnchor="middle" dominantBaseline="central" className="fill-muted">
          {t("Encoding the stream…")}
        </text>
      )}
      {heat && <HeatLayer heat={heat} w={w} h={h} />}
      {rules.map(shape)}
      {current &&
        current.points.map((p, vi) => (
          <circle
            key={vi}
            cx={p.x * w}
            cy={p.y * h}
            r={u * 0.9}
            tabIndex={0}
            role="button"
            aria-label={t("Corner {n} of {name}: {x} % across, {y} % down; arrows move it", {
              n: vi + 1,
              name: current.name,
              x: Math.round(p.x * 100),
              y: Math.round(p.y * 100),
            })}
            className={cn(ruleColors[selected! % ruleColors.length], "cursor-grab fill-bg focus:outline-none focus-visible:fill-text")}
            stroke="currentColor"
            strokeWidth={2}
            vectorEffect="non-scaling-stroke"
            onPointerDown={(e) => grab(e, selected!, { kind: "vertex", index: vi })}
            onKeyDown={(e) => nudge(e, vi)}
          />
        ))}
      {drawing && preview.length > 0 && (
        <g className="text-text" pointerEvents="none">
          {drawing.type === "region" && preview.length > 2 && (
            <polygon points={preview.map(xy).join(" ")} fill="currentColor" fillOpacity={0.08} stroke="none" />
          )}
          <polyline
            points={preview.map(xy).join(" ")}
            fill="none"
            stroke="currentColor"
            strokeWidth={2}
            strokeDasharray="6 4"
            vectorEffect="non-scaling-stroke"
          />
          {points.map((p, i) => (
            <circle
              key={i}
              cx={p.x * w}
              cy={p.y * h}
              r={i === 0 && drawing.type === "region" && points.length >= 3 ? u * 1.1 : u * 0.6}
              className="fill-text"
            />
          ))}
        </g>
      )}
    </svg>
  );
}

/** HeatLayer shades each cell of a heat map by how many objects were seen
 * there, relative to the busiest cell. */
function HeatLayer({ heat, w, h }: { heat: NonNullable<Analytics["heat"]>; w: number; h: number }) {
  const top = Math.max(0, ...heat.cells);
  if (top === 0) return null;
  const cw = w / heat.cols;
  const ch = h / heat.rows;
  return (
    <g className="pointer-events-none text-[#ef4444]" aria-hidden>
      {heat.cells.map((n, i) =>
        n > 0 ? (
          <rect
            key={i}
            x={(i % heat.cols) * cw}
            y={Math.floor(i / heat.cols) * ch}
            width={cw}
            height={ch}
            fill="currentColor"
            fillOpacity={0.12 + 0.5 * (n / top)}
          />
        ) : null,
      )}
    </g>
  );
}
