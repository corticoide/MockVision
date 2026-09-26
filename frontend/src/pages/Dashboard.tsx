import { AlertTriangle, ArrowDown, ArrowUp, CheckCircle2, CircleHelp, KeyRound, RotateCw, XCircle } from "lucide-react";
import type { ReactNode } from "react";
import type { Camera, CameraState, EventItem } from "@/api/client";
import { useCameras, useEvents, useFailedEvents, useJobs, useNode, useNodeHistory, useNodeMetrics, useTokens } from "@/api/queries";
import { Badge, DeliveryBadge, JobStatusBadge, Mono, StateBadge } from "@/components/badges";
import { CopyButton } from "@/components/CopyButton";
import { type Segment, Sparkline, StackedBar } from "@/components/Sparkline";
import { Card, CardHeader, Empty, PageHeader } from "@/components/ui/card";
import { Table, TBody, TD, TH, THead, TR } from "@/components/ui/table";
import { plural, type Translate, useT } from "@/lib/i18n";
import { Link } from "@/lib/router";
import { cn, formatBitRate, formatBytes, formatPercent, formatTime, sinceText } from "@/lib/utils";

// The order states are listed in, and the color of each in the bar.
const stateOrder: { state: CameraState; className: string }[] = [
  { state: "running", className: "text-ok" },
  { state: "degraded", className: "text-warn" },
  { state: "provisioning", className: "text-info" },
  { state: "starting", className: "text-info" },
  { state: "restarting", className: "text-info" },
  { state: "stopping", className: "text-info" },
  { state: "error", className: "text-error" },
  { state: "stopped", className: "text-muted" },
];

export function DashboardPage() {
  const t = useT();
  const { data: node } = useNode();
  return (
    <>
      <PageHeader
        title={t("Dashboard")}
        description={node ? t("{host} · MockVision {version} · up for {uptime}", { host: node.hostname, version: node.version, uptime: sinceText(node.started_at) }) : undefined}
      />
      <div className="flex flex-col gap-4">
        <NodeTiles />
        <div className="grid grid-cols-[minmax(0,2fr)_minmax(0,3fr)] items-start gap-4">
          <CamerasByState />
          <Attention />
        </div>
        <div className="grid grid-cols-[minmax(0,3fr)_minmax(0,2fr)] items-start gap-4">
          <LatestEvents />
          <PanelAccess />
        </div>
      </div>
    </>
  );
}

function NodeTiles() {
  const t = useT();
  const { data: m } = useNodeMetrics();
  const { data: history = [] } = useNodeHistory();
  const memPct = m && m.mem_total ? (m.mem_used / m.mem_total) * 100 : undefined;
  const maxCameras = m?.limits.max_cameras ?? 0;
  const counts = m?.camera_counts;
  return (
    <div className="grid grid-cols-4 gap-4">
      <Tile
        label={t("CPU")}
        value={formatPercent(m?.cpu_percent)}
        warn={(m?.cpu_sustained_percent ?? 0) > (m?.limits.max_cpu_percent ?? 80)}
        detail={m && t("{avg} over 1 min · limit {limit}", { avg: formatPercent(m.cpu_sustained_percent), limit: formatPercent(m.limits.max_cpu_percent, 0) })}
      >
        <Sparkline label={t("CPU, last 10 minutes")} max={100} series={[{ values: history.map((s) => s.cpu_percent), className: "text-info" }]} />
      </Tile>
      <Tile
        label={t("Memory")}
        value={formatPercent(memPct)}
        warn={(memPct ?? 0) > (m?.limits.max_ram_percent ?? 85)}
        detail={m && t("{used} of {total} · limit {limit}", { used: formatBytes(m.mem_used), total: formatBytes(m.mem_total), limit: formatPercent(m.limits.max_ram_percent, 0) })}
      >
        <Sparkline
          label={t("Memory, last 10 minutes")}
          max={100}
          series={[{ values: history.map((s) => (s.mem_total ? (s.mem_used / s.mem_total) * 100 : 0)), className: "text-info" }]}
        />
      </Tile>
      <Tile
        label={m?.net_interface ? t("Network ({iface})", { iface: m.net_interface }) : t("Network")}
        value={
          <span className="flex items-center gap-3 text-base whitespace-nowrap [&_svg]:size-3.5">
            <span className="flex items-center gap-0.5" title={t("Received")}>
              <ArrowDown className="text-info" />
              {formatBitRate(m?.net_rx_bps)}
            </span>
            <span className="flex items-center gap-0.5" title={t("Sent")}>
              <ArrowUp className="text-ok" />
              {formatBitRate(m?.net_tx_bps)}
            </span>
          </span>
        }
        detail={t("Received and sent, per second")}
      >
        <Sparkline
          label={t("Network traffic, last 10 minutes")}
          series={[
            { values: history.map((s) => s.net_rx_bps), className: "text-info" },
            { values: history.map((s) => s.net_tx_bps), className: "text-ok" },
          ]}
        />
      </Tile>
      <Tile
        label={t("Cameras")}
        value={counts ? t("{running} running / {total}", { running: counts.running, total: counts.total }) : "—"}
        warn={(counts?.error ?? 0) > 0}
        detail={counts && t("{total} of {max} allowed on this node", { total: counts.total, max: maxCameras })}
      >
        <div className="flex h-8 items-end">
          <StackedBar label={t("Cameras by state")} segments={stateSegments(counts?.by_state ?? {}, t)} />
        </div>
      </Tile>
    </div>
  );
}

function stateSegments(byState: Record<string, number>, t: Translate): Segment[] {
  return stateOrder
    .filter((s) => (byState[s.state] ?? 0) > 0)
    .map((s) => ({ value: byState[s.state], className: s.className, label: `${t(capitalize(s.state))}: ${byState[s.state]}` }));
}

const capitalize = (s: string) => s.charAt(0).toUpperCase() + s.slice(1);

function Tile({ label, value, detail, warn, children }: { label: string; value: ReactNode; detail?: ReactNode; warn?: boolean; children: ReactNode }) {
  return (
    <Card className="flex flex-col gap-2 p-4">
      <span className="text-[11px] font-medium tracking-wide text-muted uppercase">{label}</span>
      <span className={cn("font-mono text-lg leading-none", warn && "text-warn")}>{value}</span>
      {children}
      <span className="min-h-4 truncate text-xs text-muted">{detail}</span>
    </Card>
  );
}

function CamerasByState() {
  const t = useT();
  const { data: m } = useNodeMetrics();
  const byState = m?.camera_counts.by_state ?? {};
  const rows = stateOrder.filter((s) => (byState[s.state] ?? 0) > 0 || s.state === "running" || s.state === "stopped" || s.state === "error");
  return (
    <Card>
      <CardHeader
        title={t("Cameras by state")}
        actions={
          <Link href="/cameras" className="text-xs text-muted hover:text-text">
            {t("All cameras")}
          </Link>
        }
      />
      <ul className="divide-y divide-border/70">
        {rows.map((s) => (
          <li key={s.state}>
            <Link href={`/cameras?state=${s.state}`} className="flex h-9 items-center justify-between px-4 hover:bg-surface-2/60">
              <StateBadge state={s.state} />
              <Mono className={cn((byState[s.state] ?? 0) === 0 && "text-muted")}>{byState[s.state] ?? 0}</Mono>
            </Link>
          </li>
        ))}
      </ul>
    </Card>
  );
}

function Attention() {
  const t = useT();
  const { data: cameras } = useCameras();
  const { data: failed } = useFailedEvents();
  const { data: activeJobs } = useJobs({ status: "active" });
  const troubled = (cameras ?? []).filter((c) => c.status.state === "error" || c.status.state === "degraded" || c.status.pending_restart.length > 0);
  const deliveries = failed?.items ?? [];
  const jobs = (activeJobs?.items ?? []).filter((j) => j.status === "waiting" || j.status === "interrupted");
  const nothing = troubled.length === 0 && deliveries.length === 0 && jobs.length === 0;
  return (
    <Card>
      <CardHeader title={t("Needs attention")} description={t("Cameras in error or degraded, changes waiting for a restart, deliveries that gave up and jobs that need you.")} />
      {nothing ? (
        <Empty icon={<CheckCircle2 className="text-ok" />} title={t("Nothing needs attention")} />
      ) : (
        <ul className="divide-y divide-border/70 text-[13px]">
          {jobs.map((j) => (
            <li key={j.id} className="flex items-center gap-3 px-4 py-2">
              <CircleHelp className="size-4 shrink-0 text-warn" />
              <Link href="/jobs" className="min-w-0 flex-1 truncate font-medium hover:underline" title={j.title}>
                {j.title}
              </Link>
              <span className="min-w-0 truncate text-muted" title={j.question?.text}>
                {j.status === "waiting" ? j.question?.text : t("Stopped by a restart; resume it from Jobs.")}
              </span>
              <JobStatusBadge status={j.status} />
            </li>
          ))}
          {troubled.map((c) => (
            <li key={c.id} className="flex items-center gap-3 px-4 py-2">
              <CameraProblem camera={c} t={t} />
            </li>
          ))}
          {deliveries.map((ev) => (
            <li key={ev.id} className="flex items-center gap-3 px-4 py-2">
              <FailedDelivery event={ev} t={t} />
            </li>
          ))}
        </ul>
      )}
    </Card>
  );
}

function CameraProblem({ camera, t }: { camera: Camera; t: Translate }) {
  const state = camera.status.state;
  const pending = camera.status.pending_restart;
  const icon =
    state === "error" ? <XCircle className="text-error" /> : state === "degraded" ? <AlertTriangle className="text-warn" /> : <RotateCw className="text-warn" />;
  const text =
    state === "error" || state === "degraded"
      ? camera.status.reason || t(capitalize(state))
      : t("Saved {what} changes apply after a restart", { what: pending.join(t(" and ")) });
  return (
    <>
      <span className="shrink-0 [&_svg]:size-4">{icon}</span>
      <Link href={`/cameras/${camera.id}`} className="shrink-0 font-medium hover:underline">
        {camera.name}
      </Link>
      <span className="min-w-0 flex-1 truncate text-muted" title={text}>
        {text}
      </span>
      <StateBadge state={state} reason={camera.status.reason} />
    </>
  );
}

function FailedDelivery({ event, t }: { event: EventItem; t: Translate }) {
  const last = event.deliveries.filter((d) => d.status === "failed").at(-1);
  return (
    <>
      <XCircle className="size-4 shrink-0 text-error" />
      <Link href={`/cameras/${event.camera_id}/events`} className="shrink-0 font-medium hover:underline">
        {event.camera_name}
      </Link>
      <span className="min-w-0 flex-1 truncate text-muted" title={last?.error}>
        {t("{type} to {target} failed", { type: event.type, target: last?.target_name || "—" })}
        {last?.error ? ` · ${last.error}` : ""}
      </span>
      <Mono className="text-muted">{formatTime(event.at)}</Mono>
    </>
  );
}

function LatestEvents() {
  const t = useT();
  const { data } = useEvents();
  const events = (data?.items ?? []).slice(0, 8);
  return (
    <Card>
      <CardHeader
        title={t("Latest events")}
        actions={
          <Link href="/events" className="text-xs text-muted hover:text-text">
            {t("All events")}
          </Link>
        }
      />
      {events.length === 0 ? (
        <Empty title={t("No events yet")} />
      ) : (
        <Table>
          <THead>
            <tr>
              <TH>{t("Time")}</TH>
              <TH>{t("Camera")}</TH>
              <TH>{t("Type")}</TH>
              <TH>{t("Delivery")}</TH>
            </tr>
          </THead>
          <TBody>
            {events.map((ev) => (
              <TR key={ev.id}>
                <TD>
                  <Mono>{formatTime(ev.at)}</Mono>
                </TD>
                <TD>
                  <Link href={`/cameras/${ev.camera_id}/events`} className="hover:underline">
                    {ev.camera_name}
                  </Link>
                </TD>
                <TD>
                  <Mono>{ev.type}</Mono>
                </TD>
                <TD>
                  <DeliveryBadge status={ev.delivery_status} />
                </TD>
              </TR>
            ))}
          </TBody>
        </Table>
      )}
    </Card>
  );
}

/** Where the panel and the API answer (D63), and how automation gets in. */
function PanelAccess() {
  const t = useT();
  const { data: node } = useNode();
  const { data: tokens } = useTokens();
  if (!node) return null;
  const active = (tokens ?? []).filter((tk) => !tk.expired).length;
  return (
    <Card>
      <CardHeader
        title={t("Panel and API access")}
        description={
          node.panel.all_interfaces
            ? t("Listening on every interface ({listen}); login required.", { listen: node.panel.listen })
            : t("Listening on one management address ({listen}); login required.", { listen: node.panel.listen })
        }
      />
      <div className="flex flex-col gap-3 p-4 text-[13px]">
        {node.panel.urls.length > 0 && (
          <ul className="flex flex-col gap-1">
            {node.panel.urls.map((u) => (
              <li key={u} className="flex items-center justify-between gap-2">
                <Mono>{u}</Mono>
                <CopyButton text={u} />
              </li>
            ))}
          </ul>
        )}
        <div className="flex items-center justify-between gap-2 border-t border-border/70 pt-3">
          <span className="flex items-center gap-2 text-muted [&_svg]:size-4">
            <KeyRound />
            {t("Automation uses API tokens with the Bearer header.")}
          </span>
          <Link href="/settings" className="shrink-0">
            <Badge tone={active > 0 ? "info" : "muted"}>{plural(t, active, "1 active token", "{n} active tokens")}</Badge>
          </Link>
        </div>
      </div>
    </Card>
  );
}
