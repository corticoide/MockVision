import type { Camera, ManualEvent, Point, Rule } from "@/api/client";
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

/** An event type in words; custom types show as they are. */
export function eventLabel(type: string, t: Translate) {
  const label = eventLabels[type];
  return label ? t(label) : type;
}

/** The events a region can report, in the order the panel lists them. */
export const regionEvents = ["region_entrance", "region_exit", "loitering", "intrusion"] as const;

/** The kind of rule an event type comes from; "" for events no rule raises. */
export function ruleTypeFor(type: string): "line" | "region" | "" {
  if (type === "line_crossing") return "line";
  return (regionEvents as readonly string[]).includes(type) ? "region" : "";
}

/** The events a rule reports. */
export function ruleEvents(rule: { type: Rule["type"]; events: readonly string[] }): string[] {
  return rule.type === "line" ? ["line_crossing"] : [...rule.events];
}

export function directionLabel(direction: string, t: Translate) {
  if (direction === "A->B") return "A → B";
  if (direction === "B->A") return "B → A";
  return t("Both ways");
}

/** The event the quick button of a camera fires: its first enabled rule's
 * first event, or a line crossing on the camera's default line. */
export function quickEvent(camera: Camera): ManualEvent {
  const rule = camera.rules.find((r) => r.enabled && ruleEvents(r).length > 0);
  return rule ? { type: ruleEvents(rule)[0], rule_id: rule.id } : { type: "line_crossing" };
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
