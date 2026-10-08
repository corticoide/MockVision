import { Download } from "lucide-react";
import { type ReactNode, useState } from "react";
import { type Camera, type CameraMetrics, errorMessage } from "@/api/client";
import { type MetricsRange, type StatsWindow, useCameraClients, useCameraGaps, useCameraLogs, useCameraMetrics, useCameraRequests } from "@/api/queries";
import { Badge, Mono } from "@/components/badges";
import { Sparkline } from "@/components/Sparkline";
import { Button } from "@/components/ui/button";
import { Card, CardHeader, Empty, Notice } from "@/components/ui/card";
import { Select } from "@/components/ui/form";
import { Table, TBody, TD, TH, THead, TR } from "@/components/ui/table";
import { plural, type Translate, useT } from "@/lib/i18n";
import { cn, formatBitRate, formatBytes, formatMS, formatNumber, formatPercent, formatTime } from "@/lib/utils";

const ranges: { id: MetricsRange; label: string }[] = [
  { id: "10m", label: "Last 10 minutes" },
  { id: "1h", label: "Last hour" },
  { id: "24h", label: "Last 24 hours" },
  { id: "7d", label: "Last 7 days" },
];

const windows: { id: StatsWindow; label: string }[] = [
  { id: "1h", label: "Last hour" },
  { id: "24h", label: "Last 24 hours" },
  { id: "7d", label: "Last 7 days" },
];

/** What the camera served, to whom and how it went: the diagnosis of the equipment under test (D79, D92). */
export function DiagnosticsTab({ camera }: { camera: Camera }) {
  const t = useT();
  const [range, setRange] = useState<MetricsRange>("10m");
  const [win, setWin] = useState<StatsWindow>("1h");
  return (
    <div className="flex flex-col gap-4">
      <MetricsCard id={camera.id} range={range} setRange={setRange} />
      <div className="flex items-center justify-end gap-2 text-[13px] text-muted">
        {t("Clients and requests of the")}
        <Select value={win} onChange={(e) => setWin(e.target.value as StatsWindow)} className="w-44" aria-label={t("Window")}>
          {windows.map((w) => (
            <option key={w.id} value={w.id}>
              {t(w.label)}
            </option>
          ))}
        </Select>
      </div>
      <ClientsCard id={camera.id} win={win} />
      <RequestsCard id={camera.id} win={win} />
      <GapsCard id={camera.id} />
      <LogCard id={camera.id} />
    </div>
  );
}

/** Rates per second between consecutive samples of a counter; a restart starts again from zero. */
function rates(samples: CameraMetrics[], value: (s: CameraMetrics) => number): number[] {
  const out: number[] = [];
  for (let i = 1; i < samples.length; i++) {
    const dt = (new Date(samples[i].at).getTime() - new Date(samples[i - 1].at).getTime()) / 1000;
    const dv = value(samples[i]) - value(samples[i - 1]);
    out.push(dt > 0 && dv >= 0 ? dv / dt : 0);
  }
  return out;
}

function MetricsCard({ id, range, setRange }: { id: string; range: MetricsRange; setRange: (r: MetricsRange) => void }) {
  const t = useT();
  const { data: samples, error } = useCameraMetrics(id, range);
  const list = samples ?? [];
  const last = list.length ? list[list.length - 1] : undefined;
  const reqRates = rates(list, (s) => s.requests);
  const outRates = rates(list, (s) => s.bytes_out);
  const inRates = rates(list, (s) => s.bytes_in);
  const rangeLabel = t(ranges.find((r) => r.id === range)!.label);
  return (
    <Card>
      <CardHeader
        title={t("Metrics")}
        description={t("Every second for 10 minutes, every 10 seconds for a day and every minute for a week.")}
        actions={
          <Select value={range} onChange={(e) => setRange(e.target.value as MetricsRange)} className="w-44" aria-label={t("Range")}>
            {ranges.map((r) => (
              <option key={r.id} value={r.id}>
                {t(r.label)}
              </option>
            ))}
          </Select>
        }
      />
      {error && <Notice tone="error">{errorMessage(error)}</Notice>}
      {list.length === 0 ? (
        <Empty title={t("No samples in this range")}>{t("A camera reports its metrics while it runs.")}</Empty>
      ) : (
        <div className="grid grid-cols-4 gap-4 p-4">
          <Metric label={t("CPU")} value={formatPercent(last?.cpu_percent)}>
            <Sparkline label={t("CPU, {range}", { range: rangeLabel })} series={[{ values: list.map((s) => s.cpu_percent), className: "text-info" }]} />
          </Metric>
          <Metric label={t("Memory")} value={formatBytes(last?.rss_bytes)}>
            <Sparkline label={t("Memory, {range}", { range: rangeLabel })} series={[{ values: list.map((s) => s.rss_bytes), className: "text-info" }]} />
          </Metric>
          <Metric label={t("Clients")} value={last?.clients ?? "—"}>
            <Sparkline label={t("Clients, {range}", { range: rangeLabel })} series={[{ values: list.map((s) => s.clients), className: "text-ok" }]} />
          </Metric>
          <Metric
            label={t("Requests and traffic")}
            value={t("{n}/s", { n: formatNumber(reqRates.at(-1) ?? 0, 1) })}
            detail={t("{out} sent, {in} received", { out: formatBitRate(outRates.at(-1)), in: formatBitRate(inRates.at(-1)) })}
          >
            <Sparkline
              label={t("Requests and traffic, {range}", { range: rangeLabel })}
              series={[
                { values: reqRates, className: "text-info" },
                { values: outRates.map((v) => (v * Math.max(1, ...reqRates)) / Math.max(1, ...outRates)), className: "text-ok" },
              ]}
            />
          </Metric>
        </div>
      )}
    </Card>
  );
}

function Metric({ label, value, detail, children }: { label: string; value: ReactNode; detail?: ReactNode; children: ReactNode }) {
  return (
    <div className="flex min-w-0 flex-col gap-1.5">
      <span className="text-[11px] font-medium tracking-wide text-muted uppercase">{label}</span>
      <span className="font-mono text-lg leading-none">{value}</span>
      {children}
      <span className="min-h-4 truncate text-xs text-muted">{detail}</span>
    </div>
  );
}

/** How often a client asks a route: every 1.02 s. */
function everyText(ms: number, t: Translate) {
  return ms > 0 ? t("every {interval}", { interval: formatMS(ms) }) : "—";
}

/** A route as the panel shows it: the profile's id, or an RTSP method. */
function RouteName({ route }: { route: string }) {
  const t = useT();
  if (route === "auth" || route === "rtsp:auth") return <span className="text-muted">{t("authentication challenge")}</span>;
  if (route.endsWith("auth-failed")) return <span className="text-error">{t("credentials refused")}</span>;
  if (route === "unknown") return <span className="text-info">{t("unknown request")}</span>;
  if (route === "fault") return <span className="text-warn">{t("answered by a fault")}</span>;
  return <Mono>{route}</Mono>;
}

function ClientsCard({ id, win }: { id: string; win: StatsWindow }) {
  const t = useT();
  const { data: clients, error, isLoading } = useCameraClients(id, win);
  return (
    <Card>
      <CardHeader
        title={t("Clients")}
        description={t("Who uses the camera, what it asks and how often, how long it stays connected and what fails for it.")}
      />
      {error && <Notice tone="error">{errorMessage(error)}</Notice>}
      {isLoading ? (
        <Empty title={t("Loading…")} />
      ) : !clients?.length ? (
        <Empty title={t("No clients in this window")}>{t("Connect a player, a recorder or a script to the camera; it shows here within 10 seconds.")}</Empty>
      ) : (
        <Table>
          <THead>
            <tr>
              <TH>{t("Client")}</TH>
              <TH>{t("What it asks")}</TH>
              <TH className="text-right">{t("Connections")}</TH>
              <TH>{t("Session")}</TH>
              <TH className="text-right">{t("Requests")}</TH>
              <TH className="text-right">{t("Failures")}</TH>
              <TH>{t("Last seen")}</TH>
            </tr>
          </THead>
          <TBody>
            {clients.map((c) => (
              <TR key={c.ip}>
                <TD>
                  <span className="flex items-center gap-2">
                    <Mono>{c.ip}</Mono>
                    {c.connected && <Badge tone="ok">{t("Connected")}</Badge>}
                  </span>
                  <div className="text-xs text-muted">{c.protocols.join(", ").toUpperCase()}</div>
                </TD>
                <TD>
                  <div className="flex flex-col gap-0.5 text-xs">
                    {c.routes.slice(0, 4).map((r) => (
                      <span key={r.route} className="flex items-baseline gap-1.5">
                        <RouteName route={r.route} />
                        <span className="text-muted">
                          ×{r.count} · {everyText(r.interval_ms, t)}
                        </span>
                      </span>
                    ))}
                    {c.routes.length > 4 && <span className="text-muted">{t("and {n} more", { n: c.routes.length - 4 })}</span>}
                  </div>
                </TD>
                <TD className="text-right">
                  <Mono>{c.connections}</Mono>
                </TD>
                <TD>
                  <Mono className="text-xs">
                    {c.mean_session_ms ? t("{mean} mean, {max} max", { mean: formatMS(c.mean_session_ms), max: formatMS(c.max_session_ms) }) : "—"}
                  </Mono>
                </TD>
                <TD className="text-right">
                  <Mono>{c.requests}</Mono>
                </TD>
                <TD className="text-right">
                  <span className="flex flex-col items-end gap-0.5 text-xs">
                    {c.errors > 0 && <span className="text-warn">{plural(t, c.errors, "1 error", "{n} errors")}</span>}
                    {c.auth_failures > 0 && <span className="text-error">{plural(t, c.auth_failures, "1 refused password", "{n} refused passwords")}</span>}
                    {c.gaps > 0 && <span className="text-info">{plural(t, c.gaps, "1 unknown request", "{n} unknown requests")}</span>}
                    {c.errors + c.auth_failures + c.gaps === 0 && <span className="text-muted">—</span>}
                  </span>
                </TD>
                <TD>
                  <Mono>{formatTime(c.last_seen)}</Mono>
                </TD>
              </TR>
            ))}
          </TBody>
        </Table>
      )}
    </Card>
  );
}

function RequestsCard({ id, win }: { id: string; win: StatsWindow }) {
  const t = useT();
  const { data, error, isLoading } = useCameraRequests(id, win);
  const routes = data?.routes ?? [];
  const minutes = data?.minutes ?? [];
  const exportHref = (format: "csv" | "json") => `/api/v1/cameras/${encodeURIComponent(id)}/requests?window=${win}&format=${format}`;
  return (
    <Card>
      <CardHeader
        title={t("Requests")}
        description={t("By route and client, a minute at a time; latencies are the camera's own.")}
        actions={
          <>
            <Button asChild size="sm" variant="ghost">
              <a href={exportHref("csv")} download>
                <Download /> CSV
              </a>
            </Button>
            <Button asChild size="sm" variant="ghost">
              <a href={exportHref("json")} download>
                <Download /> JSON
              </a>
            </Button>
          </>
        }
      />
      {error && <Notice tone="error">{errorMessage(error)}</Notice>}
      {minutes.length > 1 && (
        <div className="px-4 pt-3">
          <Sparkline
            label={t("Requests a minute")}
            className="h-10"
            series={[
              { values: minutes.map((m) => m.count), className: "text-info" },
              { values: minutes.map((m) => m.errors + m.auth_failures), className: "text-error" },
            ]}
          />
        </div>
      )}
      {isLoading ? (
        <Empty title={t("Loading…")} />
      ) : routes.length === 0 ? (
        <Empty title={t("No requests in this window")} />
      ) : (
        <Table>
          <THead>
            <tr>
              <TH>{t("Route")}</TH>
              <TH>{t("Client")}</TH>
              <TH className="text-right">{t("Count")}</TH>
              <TH>{t("How often")}</TH>
              <TH className="text-right">p50</TH>
              <TH className="text-right">p95</TH>
              <TH className="text-right">{t("Errors")}</TH>
              <TH>{t("Last")}</TH>
            </tr>
          </THead>
          <TBody>
            {routes.slice(0, 200).map((r) => (
              <TR key={`${r.route} ${r.client_ip}`}>
                <TD>
                  <RouteName route={r.route} />
                </TD>
                <TD>
                  <Mono>{r.client_ip}</Mono>
                </TD>
                <TD className="text-right">
                  <Mono>{r.count}</Mono>
                </TD>
                <TD className="text-xs text-muted">{everyText(r.interval_ms, t)}</TD>
                <TD className="text-right">
                  <Mono>{formatMS(r.p50_ms)}</Mono>
                </TD>
                <TD className="text-right">
                  <Mono>{formatMS(r.p95_ms)}</Mono>
                </TD>
                <TD className={cn("text-right", r.errors + r.auth_failures > 0 && "text-error")}>
                  <Mono>{r.errors + r.auth_failures}</Mono>
                </TD>
                <TD>
                  <Mono>{formatTime(r.last_at)}</Mono>
                </TD>
              </TR>
            ))}
          </TBody>
        </Table>
      )}
    </Card>
  );
}

function GapsCard({ id }: { id: string }) {
  const t = useT();
  const { data: gaps, error } = useCameraGaps(id);
  return (
    <Card>
      <CardHeader
        title={t("Unknown requests")}
        description={t("Requests the profile does not know: the camera answers them as the real one would, and they show what the profile lacks.")}
      />
      {error && <Notice tone="error">{errorMessage(error)}</Notice>}
      {!gaps?.length ? (
        <Empty title={t("No unknown requests")}>{t("Every request so far matched a route of the profile.")}</Empty>
      ) : (
        <Table>
          <THead>
            <tr>
              <TH>{t("Request")}</TH>
              <TH>{t("Client")}</TH>
              <TH className="text-right">{t("Count")}</TH>
              <TH>{t("First")}</TH>
              <TH>{t("Last")}</TH>
            </tr>
          </THead>
          <TBody>
            {gaps.map((g) => (
              <TR key={`${g.protocol} ${g.summary} ${g.client_ip}`}>
                <TD>
                  <span className="flex items-center gap-2">
                    <Badge tone="muted">{g.protocol.toUpperCase()}</Badge>
                    <Mono className="break-all">{g.summary}</Mono>
                  </span>
                </TD>
                <TD>
                  <Mono>{g.client_ip}</Mono>
                </TD>
                <TD className="text-right">
                  <Mono>{g.count}</Mono>
                </TD>
                <TD>
                  <Mono>{formatTime(g.first_at)}</Mono>
                </TD>
                <TD>
                  <Mono>{formatTime(g.last_at)}</Mono>
                </TD>
              </TR>
            ))}
          </TBody>
        </Table>
      )}
    </Card>
  );
}

const levelTone = { DEBUG: "muted", INFO: "info", WARN: "warn", ERROR: "error" } as const;

function LogCard({ id }: { id: string }) {
  const t = useT();
  const [limit, setLimit] = useState(100);
  const { data: logs, error } = useCameraLogs(id, limit);
  return (
    <Card>
      <CardHeader title={t("Log")} description={t("What the camera logged, and what the service did with it; kept for 7 days.")} />
      {error && <Notice tone="error">{errorMessage(error)}</Notice>}
      {!logs?.length ? (
        <Empty title={t("Nothing logged yet")} />
      ) : (
        <div className="flex flex-col divide-y divide-border">
          {logs.map((l) => (
            <div key={l.id} className="flex items-baseline gap-3 px-4 py-1.5 text-[13px]">
              <Mono className="w-36 shrink-0 text-muted">{formatTime(l.at)}</Mono>
              <Badge tone={levelTone[l.level]}>{l.level}</Badge>
              <span className="w-16 shrink-0 text-xs text-muted">{l.source === "camera" ? t("camera") : t("service")}</span>
              <span className="min-w-0 break-words">{l.msg}</span>
              {Object.keys(l.attrs).length > 0 && (
                <span className="min-w-0 truncate" title={JSON.stringify(l.attrs)}>
                  <Mono className="text-xs text-muted">
                    {Object.entries(l.attrs)
                      .map(([k, v]) => `${k}=${typeof v === "string" ? v : JSON.stringify(v)}`)
                      .join(" ")}
                  </Mono>
                </span>
              )}
            </div>
          ))}
          {logs.length >= limit && limit < 500 && (
            <div className="px-4 py-2">
              <Button size="sm" variant="ghost" onClick={() => setLimit(500)}>
                {t("Show more")}
              </Button>
            </div>
          )}
        </div>
      )}
    </Card>
  );
}
