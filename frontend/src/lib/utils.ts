import { type ClassValue, clsx } from "clsx";
import { twMerge } from "tailwind-merge";

export function cn(...inputs: ClassValue[]) {
  return twMerge(clsx(inputs));
}

// Locale for number and date formatting. i18n keeps it in sync with the panel
// language via setFormatLocale; it defaults to English until then.
let formatLocale = "en";
let timeFormat = buildTimeFormat(formatLocale);
let dateTimeFormat = buildDateTimeFormat(formatLocale);

function buildTimeFormat(locale: string) {
  return new Intl.DateTimeFormat(locale, { hour: "2-digit", minute: "2-digit", second: "2-digit" });
}
function buildDateTimeFormat(locale: string) {
  return new Intl.DateTimeFormat(locale, {
    month: "short",
    day: "2-digit",
    hour: "2-digit",
    minute: "2-digit",
    second: "2-digit",
  });
}

export function setFormatLocale(locale: string) {
  if (locale === formatLocale) return;
  formatLocale = locale;
  timeFormat = buildTimeFormat(locale);
  dateTimeFormat = buildDateTimeFormat(locale);
}

export function formatBytes(n: number | null | undefined): string {
  if (n == null) return "—";
  const units = ["B", "KiB", "MiB", "GiB", "TiB"];
  let v = n;
  let i = 0;
  while (v >= 1024 && i < units.length - 1) {
    v /= 1024;
    i++;
  }
  const digits = i === 0 ? 0 : 1;
  const value = new Intl.NumberFormat(formatLocale, { minimumFractionDigits: digits, maximumFractionDigits: digits }).format(v);
  return `${value} ${units[i]}`;
}

/** Formats a byte rate as bits per second, the unit links are sold in. */
export function formatBitRate(bytesPerSecond: number | null | undefined): string {
  if (bytesPerSecond == null) return "—";
  const units = ["bit/s", "kbit/s", "Mbit/s", "Gbit/s"];
  let v = bytesPerSecond * 8;
  let i = 0;
  while (v >= 1000 && i < units.length - 1) {
    v /= 1000;
    i++;
  }
  const digits = i === 0 || v >= 100 ? 0 : 1;
  const value = new Intl.NumberFormat(formatLocale, { minimumFractionDigits: digits, maximumFractionDigits: digits }).format(v);
  return `${value} ${units[i]}`;
}

export function formatPercent(n: number | null | undefined, digits = 1): string {
  if (n == null) return "—";
  const value = new Intl.NumberFormat(formatLocale, { minimumFractionDigits: digits, maximumFractionDigits: digits }).format(n);
  return `${value}%`;
}

export function formatTime(iso: string | null | undefined): string {
  if (!iso) return "—";
  const d = new Date(iso);
  const today = new Date();
  return d.toDateString() === today.toDateString() ? timeFormat.format(d) : dateTimeFormat.format(d);
}

export function sinceText(iso: string | null | undefined): string {
  if (!iso) return "—";
  const s = Math.max(0, Math.round((Date.now() - new Date(iso).getTime()) / 1000));
  if (s < 60) return `${s}s`;
  const m = Math.floor(s / 60);
  if (m < 60) return `${m}m ${s % 60}s`;
  const h = Math.floor(m / 60);
  if (h < 24) return `${h}h ${m % 60}m`;
  return `${Math.floor(h / 24)}d ${h % 24}h`;
}

/** A duration between two times, such as 850 ms, 12 s or 3m 5s. */
export function formatDuration(from: string | null | undefined, to: string | null | undefined): string {
  if (!from) return "—";
  const ms = Math.max(0, (to ? new Date(to).getTime() : Date.now()) - new Date(from).getTime());
  if (ms < 1000) return `${ms} ms`;
  const s = Math.round(ms / 1000);
  if (s < 60) return `${s} s`;
  const m = Math.floor(s / 60);
  if (m < 60) return `${m}m ${s % 60}s`;
  return `${Math.floor(m / 60)}h ${m % 60}m`;
}
