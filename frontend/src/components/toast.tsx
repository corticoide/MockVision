import { CheckCircle2, Info, XCircle } from "lucide-react";
import { useSyncExternalStore } from "react";
import { cn } from "@/lib/utils";

type Tone = "ok" | "error" | "info";
interface Toast {
  id: number;
  tone: Tone;
  text: string;
}

let toasts: Toast[] = [];
let next = 1;
const listeners = new Set<() => void>();
const emit = () => listeners.forEach((fn) => fn());

export function toast(text: string, tone: Tone = "info") {
  const t = { id: next++, tone, text };
  toasts = [...toasts, t].slice(-4);
  emit();
  setTimeout(() => {
    toasts = toasts.filter((x) => x.id !== t.id);
    emit();
  }, tone === "error" ? 8000 : 4000);
}

export function Toaster() {
  const list = useSyncExternalStore(
    (fn) => {
      listeners.add(fn);
      return () => listeners.delete(fn);
    },
    () => toasts,
  );
  return (
    <div className="pointer-events-none fixed right-4 bottom-4 z-50 flex w-80 flex-col gap-2">
      {list.map((t) => (
        <div
          key={t.id}
          role="status"
          className={cn(
            "pointer-events-auto flex items-start gap-2 rounded-sm border bg-surface-1 px-3 py-2 text-[13px] shadow-lg [&_svg]:mt-0.5 [&_svg]:size-4 [&_svg]:shrink-0",
            t.tone === "ok" && "border-ok/40 [&_svg]:text-ok",
            t.tone === "error" && "border-error/40 [&_svg]:text-error",
            t.tone === "info" && "border-info/40 [&_svg]:text-info",
          )}
        >
          {t.tone === "ok" ? <CheckCircle2 /> : t.tone === "error" ? <XCircle /> : <Info />}
          <span>{t.text}</span>
        </div>
      ))}
    </div>
  );
}
