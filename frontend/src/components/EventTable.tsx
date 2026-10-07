import { ChevronDown, ChevronRight } from "lucide-react";
import { Fragment, useState } from "react";
import type { EventItem } from "@/api/client";
import { Badge, DeliveryBadge, Mono } from "@/components/badges";
import { Button } from "@/components/ui/button";
import { Table, TBody, TD, TH, THead, TR } from "@/components/ui/table";
import { type Translate, useT } from "@/lib/i18n";
import { formatTime } from "@/lib/utils";

/** Events with their delivery; a row opens to show every attempt and the
 * payload. triggerNames names the stored triggers behind random events. */
export function EventTable({
  events,
  showCamera = true,
  triggerNames = {},
}: {
  events: EventItem[];
  showCamera?: boolean;
  triggerNames?: Record<string, string>;
}) {
  const t = useT();
  const [open, setOpen] = useState<string | null>(null);
  const columns = showCamera ? 11 : 10;
  return (
    <Table>
      <THead>
        <tr>
          <TH className="w-8" />
          <TH>{t("Time")}</TH>
          {showCamera && <TH>{t("Camera")}</TH>}
          <TH>{t("Type")}</TH>
          <TH>{t("Rule")}</TH>
          <TH>{t("Direction")}</TH>
          <TH>{t("Source")}</TH>
          <TH>{t("Delivery")}</TH>
          <TH className="text-right">{t("Latency")}</TH>
          <TH className="text-right">HTTP</TH>
          <TH>{t("Error")}</TH>
        </tr>
      </THead>
      <TBody>
        {events.map((ev) => {
          const last = ev.deliveries.at(-1);
          const expanded = open === ev.id;
          return (
            <Fragment key={ev.id}>
              <TR>
                <TD>
                  <Button variant="ghost" size="icon" onClick={() => setOpen(expanded ? null : ev.id)} aria-label={t("Details")} aria-expanded={expanded}>
                    {expanded ? <ChevronDown /> : <ChevronRight />}
                  </Button>
                </TD>
                <TD>
                  <Mono>{formatTime(ev.at)}</Mono>
                </TD>
                {showCamera && <TD>{ev.camera_name}</TD>}
                <TD>
                  <Mono>{ev.type}</Mono>
                </TD>
                <TD className="max-w-40 truncate" title={ruleName(ev)}>
                  {ruleName(ev) || "—"}
                </TD>
                <TD>
                  <Mono>{direction(ev) || "—"}</Mono>
                </TD>
                <TD className="max-w-44 truncate">{source(ev, triggerNames, t)}</TD>
                <TD>
                  <DeliveryBadge status={ev.delivery_status} />
                </TD>
                <TD className="text-right">
                  <Mono>{ev.latency_ms != null ? `${ev.latency_ms} ms` : "—"}</Mono>
                </TD>
                <TD className="text-right">
                  <Mono>{last?.http_status ?? "—"}</Mono>
                </TD>
                <TD className="max-w-80 truncate text-error" title={last?.error}>
                  {last?.error}
                </TD>
              </TR>
              {expanded && (
                <tr className="border-b border-border/70 bg-bg/40">
                  <td colSpan={columns} className="px-4 py-4">
                    <EventDetails event={ev} t={t} />
                  </td>
                </tr>
              )}
            </Fragment>
          );
        })}
      </TBody>
    </Table>
  );
}

function direction(ev: EventItem): string {
  const d = ev.data["direction"];
  return typeof d === "string" ? d : "";
}

function ruleName(ev: EventItem): string {
  const r = ev.data["rule"] as { name?: unknown } | undefined;
  return typeof r?.name === "string" ? r.name : "";
}

/** What produced the event: a manual trigger, or a random one by name. */
function source(ev: EventItem, names: Record<string, string>, t: Translate): string {
  const kind = ev.data["trigger"];
  const label = kind === "random" ? t("Random") : kind === "manual" ? t("Manual") : typeof kind === "string" ? kind : "—";
  const name = ev.trigger_id ? names[ev.trigger_id] : "";
  return name ? `${label} · ${name}` : label;
}

function EventDetails({ event, t }: { event: EventItem; t: Translate }) {
  return (
    <div className="grid grid-cols-2 gap-6">
      <section>
        <h3 className="mb-2 text-xs font-medium tracking-wide text-muted uppercase">{t("Deliveries")}</h3>
        {event.deliveries.length === 0 ? (
          <p className="text-[13px] text-muted">
            {event.delivery_status === "pending" ? t("Delivery in progress…") : t("The camera has no target for this event.")}
          </p>
        ) : (
          <div className="flex flex-col gap-1.5">
            {event.deliveries.map((d) => (
              <div key={d.id} className="flex flex-wrap items-center gap-2 text-[13px]">
                <Badge tone={d.status === "ok" ? "ok" : d.status === "retry" ? "warn" : d.status === "skipped" ? "muted" : "error"}>{d.status}</Badge>
                <span>{d.target_name}</span>
                <Mono className="text-muted">
                  {d.http_status
                    ? t("attempt {n} · {time} · {ms} ms · HTTP {http}", { n: d.attempt, time: formatTime(d.at), ms: d.latency_ms, http: d.http_status })
                    : t("attempt {n} · {time} · {ms} ms", { n: d.attempt, time: formatTime(d.at), ms: d.latency_ms })}
                </Mono>
                {d.error && <span className={d.status === "skipped" ? "text-muted" : "text-error"}>{d.error}</span>}
              </div>
            ))}
          </div>
        )}
      </section>
      <section className="min-w-0">
        <h3 className="mb-2 text-xs font-medium tracking-wide text-muted uppercase">{t("Event")}</h3>
        <pre className="max-h-64 overflow-auto rounded-sm border border-border bg-bg p-3 font-mono text-[12px]">
          {JSON.stringify(event.data, null, 2)}
        </pre>
      </section>
    </div>
  );
}
