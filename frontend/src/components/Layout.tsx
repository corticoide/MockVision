import { Activity, Boxes, Camera, Cpu, Image, LogOut, MemoryStick, Send, Settings, Wifi, WifiOff } from "lucide-react";
import type { ReactNode } from "react";
import { useLiveStatus } from "@/api/live";
import { useLogout, useNode, useNodeMetrics } from "@/api/queries";
import { useT } from "@/lib/i18n";
import { Link, usePath } from "@/lib/router";
import { cn, formatBytes, formatPercent } from "@/lib/utils";
import { Badge } from "./badges";
import { LanguageSelect } from "./LanguageSelect";
import { Button } from "./ui/button";

const nav = [
  { href: "/cameras", label: "Cameras", icon: <Camera /> },
  { href: "/events", label: "Events", icon: <Activity /> },
  { href: "/profiles", label: "Profiles", icon: <Boxes /> },
  { href: "/assets", label: "Assets", icon: <Image /> },
  { href: "/targets", label: "Targets", icon: <Send /> },
  { href: "/settings", label: "Settings", icon: <Settings /> },
];

export function Logo() {
  return (
    <div className="flex items-center gap-2">
      <img src="/favicon.svg" alt="" className="size-6" />
      <span className="text-[15px] font-semibold tracking-tight">MockVision</span>
    </div>
  );
}

export function Layout({ username, children }: { username: string; children: ReactNode }) {
  const path = usePath();
  const t = useT();
  return (
    <div className="flex h-full">
      <aside className="flex w-52 shrink-0 flex-col border-r border-border bg-surface-1">
        <div className="flex h-12 items-center border-b border-border px-4">
          <Logo />
        </div>
        <nav className="flex flex-col gap-0.5 p-2">
          {nav.map((item) => {
            const active = path === item.href || path.startsWith(`${item.href}/`) || (path === "/" && item.href === "/cameras");
            return (
              <Link
                key={item.href}
                href={item.href}
                className={cn(
                  "flex h-8 items-center gap-2.5 rounded-sm px-2.5 text-[13px] text-muted hover:bg-surface-2 hover:text-text [&_svg]:size-4",
                  active && "bg-surface-2 text-text shadow-[inset_2px_0_0_var(--brand)]",
                )}
              >
                {item.icon}
                {t(item.label)}
              </Link>
            );
          })}
        </nav>
        <div className="mt-auto border-t border-border p-3 text-xs text-muted">
          <NodeFooter />
        </div>
      </aside>
      <div className="flex min-w-0 flex-1 flex-col">
        <TopBar username={username} />
        <main className="min-h-0 flex-1 overflow-y-auto p-5">{children}</main>
      </div>
    </div>
  );
}

function NodeFooter() {
  const { data: node } = useNode();
  const t = useT();
  if (!node) return null;
  return (
    <div className="flex flex-col gap-1">
      <span>
        {node.hostname} · v{node.version}
      </span>
      <span>
        {node.runtime === "local" ? t("local mode (127.0.0.1)") : t("parent {iface}", { iface: node.parent_interface || "—" })}
      </span>
    </div>
  );
}

function TopBar({ username }: { username: string }) {
  const { data: m } = useNodeMetrics();
  const { data: node } = useNode();
  const live = useLiveStatus();
  const logout = useLogout();
  const t = useT();
  const memPct = m && m.mem_total ? (m.mem_used / m.mem_total) * 100 : undefined;
  return (
    <header className="flex h-12 shrink-0 items-center gap-5 border-b border-border bg-surface-1 px-5 text-[13px]">
      <Metric icon={<Cpu />} label={t("CPU")} value={formatPercent(m?.cpu_percent)} warn={(m?.cpu_sustained_percent ?? 0) > (m?.limits.max_cpu_percent ?? 80)} />
      <Metric
        icon={<MemoryStick />}
        label={t("RAM")}
        value={m ? `${formatBytes(m.mem_used)} / ${formatBytes(m.mem_total)}` : "—"}
        warn={(memPct ?? 0) > (m?.limits.max_ram_percent ?? 85)}
      />
      <Metric
        icon={<Camera />}
        label={t("Cameras")}
        value={m ? t("{running} running / {total}", { running: m.camera_counts.running, total: m.camera_counts.total }) : "—"}
      />
      {node?.runtime === "local" && <Badge tone="warn">{t("local mode")}</Badge>}
      <div className="ml-auto flex items-center gap-3">
        {live === "open" ? (
          <Badge tone="ok" icon={<Wifi />}>
            {t("Live")}
          </Badge>
        ) : (
          <Badge tone="warn" icon={<WifiOff />}>
            {live === "connecting" ? t("Connecting") : t("Offline")}
          </Badge>
        )}
        <LanguageSelect />
        <span className="text-muted">{username}</span>
        <Button variant="ghost" size="sm" onClick={() => logout.mutate()}>
          <LogOut /> {t("Log out")}
        </Button>
      </div>
    </header>
  );
}

function Metric({ icon, label, value, warn }: { icon: ReactNode; label: string; value: string; warn?: boolean }) {
  return (
    <div className="flex items-center gap-1.5 text-muted [&_svg]:size-3.5">
      {icon}
      <span>{label}</span>
      <span className={cn("font-mono text-[12px] text-text", warn && "text-warn")}>{value}</span>
    </div>
  );
}
