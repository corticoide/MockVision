import { Activity, ChevronDown, ChevronRight } from "lucide-react";
import { Fragment, useState } from "react";
import { type EventItem, errorMessage } from "@/api/client";
import { useCameras, useEvents } from "@/api/queries";
import { Badge, DeliveryBadge, Mono } from "@/components/badges";
import { Button } from "@/components/ui/button";
import { Card, Empty, Notice, PageHeader } from "@/components/ui/card";
import { Select } from "@/components/ui/form";
import { Table, TBody, TD, TH, THead, TR } from "@/components/ui/table";
import { formatTime } from "@/lib/utils";

export function EventsPage() {
  const [cameraId, setCameraId] = useState("");
  const { data: cameras } = useCameras();
  const { data, isLoading, error } = useEvents(cameraId || undefined);
  const [open, setOpen] = useState<string | null>(null);
  const events = data?.items ?? [];

  return (
    <>
      <PageHeader
        title="Events"
        description="Events sent by the cameras and the delivery to their targets. New events appear live."
        actions={
          <Select value={cameraId} onChange={(e) => setCameraId(e.target.value)} className="w-56">
            <option value="">All cameras</option>
            {cameras?.map((c) => (
              <option key={c.id} value={c.id}>
                {c.name}
              </option>
            ))}
          </Select>
        }
      />
      {error && <Notice tone="error">{errorMessage(error)}</Notice>}
      <Card>
        {isLoading ? (
          <Empty title="Loading events…" />
        ) : events.length === 0 ? (
          <Empty icon={<Activity />} title="No events yet">
            Start a camera linked to a target and press “Line crossing” in Cameras.
          </Empty>
        ) : (
          <Table>
            <THead>
              <tr>
                <TH className="w-8" />
                <TH>Time</TH>
                <TH>Camera</TH>
                <TH>Type</TH>
                <TH>Direction</TH>
                <TH>Delivery</TH>
                <TH className="text-right">Latency</TH>
                <TH className="text-right">HTTP</TH>
                <TH>Error</TH>
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
                        <Button variant="ghost" size="icon" onClick={() => setOpen(expanded ? null : ev.id)} aria-label="Details">
                          {expanded ? <ChevronDown /> : <ChevronRight />}
                        </Button>
                      </TD>
                      <TD>
                        <Mono>{formatTime(ev.at)}</Mono>
                      </TD>
                      <TD>{ev.camera_name}</TD>
                      <TD>
                        <Mono>{ev.type}</Mono>
                      </TD>
                      <TD>
                        <Mono>{direction(ev) || "—"}</Mono>
                      </TD>
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
                        <td colSpan={9} className="px-4 py-4">
                          <EventDetails event={ev} />
                        </td>
                      </tr>
                    )}
                  </Fragment>
                );
              })}
            </TBody>
          </Table>
        )}
      </Card>
      {data?.next_cursor && <p className="mt-2 text-xs text-muted">Showing the latest {events.length} events.</p>}
    </>
  );
}

function direction(ev: EventItem): string {
  const d = ev.data["direction"];
  return typeof d === "string" ? d : "";
}

function EventDetails({ event }: { event: EventItem }) {
  return (
    <div className="grid grid-cols-2 gap-6">
      <section>
        <h3 className="mb-2 text-xs font-medium tracking-wide text-muted uppercase">Deliveries</h3>
        {event.deliveries.length === 0 ? (
          <p className="text-[13px] text-muted">
            {event.delivery_status === "pending" ? "Delivery in progress…" : "The camera has no target for this event."}
          </p>
        ) : (
          <div className="flex flex-col gap-1.5">
            {event.deliveries.map((d) => (
              <div key={d.id} className="flex flex-wrap items-center gap-2 text-[13px]">
                <Badge tone={d.status === "ok" ? "ok" : d.status === "retry" ? "warn" : "error"}>{d.status}</Badge>
                <span>{d.target_name}</span>
                <Mono className="text-muted">
                  attempt {d.attempt} · {formatTime(d.at)} · {d.latency_ms} ms{d.http_status ? ` · HTTP ${d.http_status}` : ""}
                </Mono>
                {d.error && <span className="text-error">{d.error}</span>}
              </div>
            ))}
          </div>
        )}
      </section>
      <section className="min-w-0">
        <h3 className="mb-2 text-xs font-medium tracking-wide text-muted uppercase">Event</h3>
        <pre className="max-h-64 overflow-auto rounded-sm border border-border bg-bg p-3 font-mono text-[12px]">
          {JSON.stringify(event.data, null, 2)}
        </pre>
      </section>
    </div>
  );
}
