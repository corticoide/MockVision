import {
  AlertTriangle,
  Ban,
  CheckCircle2,
  CircleDashed,
  CircleDot,
  CircleHelp,
  CircleSlash,
  Clock,
  Loader2,
  PauseCircle,
  XCircle,
} from "lucide-react";
import type { ReactNode } from "react";
import type { CameraState, JobStatus } from "@/api/client";
import { BadgeBase, type BadgeTone } from "@/components/ui/badge";
import { Tooltip } from "@/components/ui/tooltip";
import { useT } from "@/lib/i18n";
import { cn } from "@/lib/utils";

type Tone = BadgeTone;

/** A badge; with a title, a tooltip explains it on hover and focus. */
export function Badge({ tone = "muted", icon, children, title }: { tone?: Tone; icon?: ReactNode; children: ReactNode; title?: string }) {
  const badge = (
    <BadgeBase tone={tone} tabIndex={title ? 0 : undefined}>
      {icon}
      {children}
    </BadgeBase>
  );
  return title ? <Tooltip content={title}>{badge}</Tooltip> : badge;
}

// States always carry an icon and a text, never color alone.
const states: Record<CameraState, { tone: Tone; label: string; icon: ReactNode }> = {
  running: { tone: "ok", label: "Running", icon: <CheckCircle2 /> },
  degraded: { tone: "warn", label: "Degraded", icon: <AlertTriangle /> },
  provisioning: { tone: "info", label: "Provisioning", icon: <Loader2 className="animate-spin" /> },
  starting: { tone: "info", label: "Starting", icon: <Loader2 className="animate-spin" /> },
  restarting: { tone: "info", label: "Restarting", icon: <Loader2 className="animate-spin" /> },
  stopping: { tone: "info", label: "Stopping", icon: <Loader2 className="animate-spin" /> },
  stopped: { tone: "muted", label: "Stopped", icon: <PauseCircle /> },
  error: { tone: "error", label: "Error", icon: <XCircle /> },
};

export function StateBadge({ state, reason }: { state: CameraState; reason?: string }) {
  const t = useT();
  const s = states[state] ?? states.stopped;
  return (
    <Badge tone={s.tone} icon={s.icon} title={reason || undefined}>
      {t(s.label)}
    </Badge>
  );
}

const jobStates: Record<JobStatus, { tone: Tone; label: string; icon: ReactNode }> = {
  queued: { tone: "muted", label: "Queued", icon: <Clock /> },
  running: { tone: "info", label: "In progress", icon: <Loader2 className="animate-spin" /> },
  waiting: { tone: "warn", label: "Waiting for you", icon: <CircleHelp /> },
  completed: { tone: "ok", label: "Completed", icon: <CheckCircle2 /> },
  failed: { tone: "error", label: "Failed", icon: <XCircle /> },
  canceled: { tone: "muted", label: "Canceled", icon: <Ban /> },
  interrupted: { tone: "warn", label: "Interrupted", icon: <PauseCircle /> },
};

export function JobStatusBadge({ status }: { status: JobStatus }) {
  const t = useT();
  const s = jobStates[status] ?? jobStates.queued;
  return (
    <Badge tone={s.tone} icon={s.icon}>
      {t(s.label)}
    </Badge>
  );
}

const levels: Record<string, { tone: Tone; label: string }> = {
  draft: { tone: "muted", label: "Draft" },
  documented: { tone: "info", label: "Documented" },
  captured: { tone: "ok", label: "Captured" },
  verified: { tone: "ok", label: "Verified" },
};

export function LevelBadge({ level }: { level: string }) {
  const t = useT();
  const l = levels[level] ?? { tone: "muted" as Tone, label: level };
  return (
    <Badge tone={l.tone} icon={<CircleDot />}>
      {t(l.label)}
    </Badge>
  );
}

export function DeliveryBadge({ status }: { status: string }) {
  const t = useT();
  switch (status) {
    case "ok":
      return (
        <Badge tone="ok" icon={<CheckCircle2 />}>
          {t("Delivered")}
        </Badge>
      );
    case "failed":
      return (
        <Badge tone="error" icon={<XCircle />}>
          {t("Failed")}
        </Badge>
      );
    case "pending":
      return (
        <Badge tone="info" icon={<Clock />}>
          {t("Pending")}
        </Badge>
      );
    case "skipped":
      return (
        <Badge tone="muted" icon={<Ban />}>
          {t("Skipped")}
        </Badge>
      );
    default:
      return (
        <Badge tone="muted" icon={<CircleSlash />}>
          {t("No target")}
        </Badge>
      );
  }
}

export function Mono({ children, className }: { children: ReactNode; className?: string }) {
  return <span className={cn("font-mono text-[12px]", className)}>{children}</span>;
}

export function Dashed() {
  return <CircleDashed className="size-3 text-muted" />;
}
