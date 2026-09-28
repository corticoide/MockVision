import type { Camera, CameraEventType, ManualEvent, Point, Rule } from "@/api/client";
import type { Translate } from "./i18n";

/** Canonical event types by what they mean, for someone new to analytics. */
const eventLabels: Record<string, string> = {
  line_crossing: "Line crossing",
  region_entrance: "Region entrance",
  region_exit: "Region exit",
  loitering: "Loitering",
  intrusion: "Intrusion",
  motion: "Motion",
  lpr: "License plate",
  speed: "Speed",
  tamper: "Tampering",
};

/** The panel lists canonical events in this order, vendor events after. */
const canonicalOrder = Object.keys(eventLabels);

/** An event type in words; a vendor event (custom:<name>) by its name. */
export function eventLabel(type: string, t: Translate) {
  const label = eventLabels[type];
  if (label) return t(label);
  const words = type
    .replace(/^custom:/, "")
    .replace(/[_.-]+/g, " ")
    .trim();
  return words ? words[0].toUpperCase() + words.slice(1) : type;
}

/** Sorts event types: canonical ones in the panel's order, then the rest. */
export function sortEvents(types: readonly string[]): string[] {
  const rank = (x: string) => {
    const i = canonicalOrder.indexOf(x);
    return i < 0 ? canonicalOrder.length : i;
  };
  return [...types].sort((a, b) => rank(a) - rank(b) || a.localeCompare(b));
}

/** The kind of rule an event type comes from, as the camera's profile
 * declares it; "" for the events no rule raises. */
export function ruleTypeFor(type: string, events: readonly CameraEventType[]): "line" | "region" | "" {
  return events.find((e) => e.type === type)?.rule ?? "";
}

/** The events a kind of rule can report on this camera. */
export function ruleChoices(kind: "line" | "region", events: readonly CameraEventType[]): string[] {
  return sortEvents(events.filter((e) => e.rule === kind).map((e) => e.type));
}

/** Whether an event is a report: it carries counts, not an object. */
export function isReport(type: string, events: readonly CameraEventType[]) {
  return events.some((e) => e.type === type && e.report);
}

/** The events a rule reports; a line listing none reports crossings. */
export function ruleEvents(rule: { type: Rule["type"]; events: readonly string[] }): string[] {
  return rule.type === "line" && rule.events.length === 0 ? ["line_crossing"] : [...rule.events];
}

export function directionLabel(direction: string, t: Translate) {
  if (direction === "A->B") return "A → B";
  if (direction === "B->A") return "B → A";
  return t("Both ways");
}

/** The event the quick button of a camera fires: the first event of its
 * first enabled rule, or else the first event that needs no rule. Null
 * when its profile gives it nothing to fire at once. */
export function quickEvent(camera: Camera): ManualEvent | null {
  const sendable = new Set(camera.event_types.map((e) => e.type));
  for (const r of camera.rules) {
    const type = r.enabled ? ruleEvents(r).find((x) => sendable.has(x)) : undefined;
    if (type) return { type, rule_id: r.id };
  }
  const free = sortEvents(camera.event_types.filter((e) => e.rule === "" && !e.report).map((e) => e.type))[0];
  return free ? { type: free } : null;
}

/** Picture size of the main stream, for the editor's coordinates. */
export function pictureSize(camera: Camera): { w: number; h: number } {
  const main = camera.streams.find((s) => s.name === "main") ?? camera.streams[0];
  const [w, h] = (main?.resolution ?? "").split("x").map(Number);
  return w > 0 && h > 0 ? { w, h } : { w: 1280, h: 720 };
}

/** The unit normal of the line from a to b pointing to its side B (the
 * right, walking from a to b on the picture). */
export function sideB(a: Point, b: Point, w: number, h: number): Point {
  const dx = (b.x - a.x) * w;
  const dy = (b.y - a.y) * h;
  const n = Math.hypot(dx, dy) || 1;
  return { x: -dy / n, y: dx / n };
}

export function centroid(points: Point[]): Point {
  const n = points.length || 1;
  return { x: points.reduce((s, p) => s + p.x, 0) / n, y: points.reduce((s, p) => s + p.y, 0) / n };
}

export const clamp01 = (v: number) => Math.min(1, Math.max(0, v));

/** Rounds a coordinate to four decimals: a tenth of a pixel on 4K. */
export const round4 = (v: number) => Math.round(v * 10000) / 10000;

function side(a: Point, b: Point, p: Point) {
  return (b.x - a.x) * (p.y - a.y) - (b.y - a.y) * (p.x - a.x);
}

function touch(p1: Point, p2: Point, q1: Point, q2: Point) {
  const d1 = side(q1, q2, p1);
  const d2 = side(q1, q2, p2);
  const d3 = side(p1, p2, q1);
  const d4 = side(p1, p2, q2);
  return ((d1 > 0 && d2 < 0) || (d1 < 0 && d2 > 0)) && ((d3 > 0 && d4 < 0) || (d3 < 0 && d4 > 0));
}

/** Whether two sides of a region cross, which the node refuses. */
export function crosses(points: Point[]): boolean {
  const n = points.length;
  for (let i = 0; i < n; i++) {
    for (let j = i + 2; j < n; j++) {
      if (i === 0 && j === n - 1) continue;
      if (touch(points[i], points[(i + 1) % n], points[j], points[(j + 1) % n])) return true;
    }
  }
  return false;
}

/** Colors of the rules on the picture, in turn: bright, as they are drawn
 * over any picture, with a dark halo. Text classes, as SVG shapes draw with
 * currentColor. */
export const ruleColors = ["text-[#fbbf24]", "text-[#22d3ee]", "text-[#a3e635]", "text-[#f472b6]", "text-[#a78bfa]", "text-[#fb923c]"];
