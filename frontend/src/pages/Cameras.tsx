import { Camera as CameraIcon, ChevronDown, ChevronRight, Play, Plus, Square, Trash2, Zap } from "lucide-react";
import { Fragment, type ReactNode, useEffect, useState } from "react";
import { type Camera, errorMessage } from "@/api/client";
import { useCameraAction, useCameras, useDeleteCamera, useNodeMetrics, useTrigger } from "@/api/queries";
import { Badge, LevelBadge, Mono, StateBadge } from "@/components/badges";
import { CopyButton } from "@/components/CopyButton";
import { toast } from "@/components/toast";
import { Button } from "@/components/ui/button";
import { Card, Empty, Notice, PageHeader } from "@/components/ui/card";
import { Table, TBody, TD, TH, THead, TR } from "@/components/ui/table";
import { formatBytes, formatPercent, formatTime, sinceText } from "@/lib/utils";
import { NewCameraDialog } from "./NewCameraDialog";

export function CamerasPage() {
  const { data: cameras, isLoading, error } = useCameras();
  const { data: metrics } = useNodeMetrics();
  const [creating, setCreating] = useState(false);
  const [open, setOpen] = useState<string | null>(null);

  return (
    <>
      <PageHeader
        title="Cameras"
        description="Simulated IP cameras on this node. Each one answers on the LAN as its profile describes."
        actions={
          <Button variant="primary" onClick={() => setCreating(true)}>
            <Plus /> New camera
          </Button>
        }
      />
      {error && <Notice tone="error">{errorMessage(error)}</Notice>}
      <Card>
        {isLoading ? (
          <Empty title="Loading cameras…" />
        ) : !cameras?.length ? (
          <Empty icon={<CameraIcon />} title="No cameras yet">
            Import a profile, then create a camera with a free IP address of your LAN.
          </Empty>
        ) : (
          <Table>
            <THead>
              <tr>
                <TH className="w-8" />
                <TH>Name</TH>
                <TH>Profile</TH>
                <TH>Address</TH>
                <TH>State</TH>
                <TH className="text-right">CPU</TH>
                <TH className="text-right">RAM</TH>
                <TH className="text-right">Clients</TH>
                <TH className="text-right">Actions</TH>
              </tr>
            </THead>
            <TBody>
              {cameras.map((c) => {
                const m = metrics?.cameras[c.id];
                const expanded = open === c.id;
                return (
                  <Fragment key={c.id}>
                    <TR>
                      <TD>
                        <Button variant="ghost" size="icon" onClick={() => setOpen(expanded ? null : c.id)} aria-label="Details">
                          {expanded ? <ChevronDown /> : <ChevronRight />}
                        </Button>
                      </TD>
                      <TD>
                        <div className="font-medium">{c.name}</div>
                      </TD>
                      <TD>
                        <div className="flex items-center gap-2">
                          <span className="text-muted">
                            {c.profile.vendor} {c.profile.model}
                          </span>
                          <LevelBadge level={c.profile.level} />
                        </div>
                      </TD>
                      <TD>
                        <Mono>{c.network.ip || "127.0.0.1"}</Mono>
                        <Mono className="ml-2 text-muted">{c.network.mac}</Mono>
                      </TD>
                      <TD>
                        <div className="flex items-center gap-2">
                          <StateBadge state={c.status.state} reason={c.status.reason} />
                          {c.status.state === "error" && (
                            <span className="max-w-72 truncate text-xs text-error" title={c.status.reason}>
                              {c.status.reason}
                            </span>
                          )}
                        </div>
                      </TD>
                      <TD className="text-right">
                        <Mono>{m ? formatPercent(m.cpu_percent) : "—"}</Mono>
                      </TD>
                      <TD className="text-right">
                        <Mono>{m ? formatBytes(m.rss_bytes) : "—"}</Mono>
                      </TD>
                      <TD className="text-right">
                        <Mono>{m ? m.clients : "—"}</Mono>
                      </TD>
                      <TD className="text-right">
                        <RowActions camera={c} />
                      </TD>
                    </TR>
                    {expanded && (
                      <tr className="border-b border-border/70 bg-bg/40">
                        <td colSpan={9} className="px-4 py-4">
                          <CameraDetails camera={c} />
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
      <NewCameraDialog open={creating} onClose={() => setCreating(false)} />
    </>
  );
}

function RowActions({ camera }: { camera: Camera }) {
  const action = useCameraAction();
  const trigger = useTrigger();
  const del = useDeleteCamera();
  const state = camera.status.state;
  const running = state === "running" || state === "degraded";
  const busy = ["provisioning", "starting", "stopping", "restarting"].includes(state) || action.isPending;
  const canStart = state === "stopped" || state === "error";

  const run = (a: "start" | "stop") =>
    action.mutate(
      { id: camera.id, action: a },
      { onError: (err) => toast(`${camera.name}: ${errorMessage(err)}`, "error") },
    );

  return (
    <div className="flex items-center justify-end gap-1">
      <Button
        size="sm"
        variant="secondary"
        disabled={!running || trigger.isPending}
        title="Send a line-crossing event to the camera's targets"
        onClick={() =>
          trigger.mutate(
            { id: camera.id, type: "line_crossing" },
            {
              onSuccess: () => toast(`Line crossing sent from ${camera.name}`, "ok"),
              onError: (err) => toast(errorMessage(err), "error"),
            },
          )
        }
      >
        <Zap /> Line crossing
      </Button>
      {canStart ? (
        <Button size="icon" variant="ghost" disabled={busy} onClick={() => run("start")} title="Start" aria-label="Start">
          <Play />
        </Button>
      ) : (
        <Button size="icon" variant="ghost" disabled={busy || !running} onClick={() => run("stop")} title="Stop" aria-label="Stop">
          <Square />
        </Button>
      )}
      <Button
        size="icon"
        variant="ghost"
        title="Delete"
        aria-label="Delete"
        disabled={del.isPending}
        onClick={() => {
          if (confirm(`Delete camera ${camera.name} and its events?`)) {
            del.mutate(camera.id, { onError: (err) => toast(errorMessage(err), "error") });
          }
        }}
      >
        <Trash2 />
      </Button>
    </div>
  );
}

function CameraDetails({ camera }: { camera: Camera }) {
  const running = camera.status.state === "running" || camera.status.state === "degraded";
  const [tick, setTick] = useState(0);
  useEffect(() => {
    if (!running) return;
    const t = setInterval(() => setTick((n) => n + 1), 5000);
    return () => clearInterval(t);
  }, [running]);
  const stream = camera.streams[0];

  return (
    <div className="grid grid-cols-[minmax(0,1fr)_320px] gap-6">
      <div className="flex flex-col gap-4">
        <section>
          <h3 className="mb-2 text-xs font-medium tracking-wide text-muted uppercase">Endpoints</h3>
          <div className="flex flex-col gap-1.5">
            {camera.endpoints.map((e) => (
              <div key={e.instance} className="flex items-center gap-2">
                <Badge tone="info">{e.protocol.toUpperCase()}</Badge>
                <Mono className="select-all">{e.url}</Mono>
                <CopyButton text={e.url} label="Copy URL" />
              </div>
            ))}
            <p className="text-xs text-muted">
              Accounts: {camera.users.map((u) => `${u.username} (${u.role})`).join(", ")} · Digest authentication
            </p>
          </div>
        </section>
        <section className="grid grid-cols-3 gap-x-6 gap-y-2 text-[13px]">
          <Info label="Serial" value={<Mono>{camera.serial}</Mono>} />
          <Info label="MAC" value={<Mono>{camera.network.mac}</Mono>} />
          <Info label="Parent / namespace" value={<Mono>{[camera.network.parent || "—", camera.status.netns].filter(Boolean).join(" → ")}</Mono>} />
          <Info label="Stream" value={stream ? `${stream.codec.toUpperCase()} ${stream.resolution} @ ${stream.fps} fps` : "—"} />
          <Info label="Rendition" value={stream ? stream.rendition_status : "—"} />
          <Info label="Up for" value={running ? sinceText(camera.status.started_at) : "—"} />
          <Info label="Last heartbeat" value={formatTime(camera.status.last_heartbeat)} />
          <Info label="PID" value={camera.status.pid ? <Mono>{camera.status.pid}</Mono> : "—"} />
          <Info label="Targets" value={camera.targets.length ? camera.targets.map((t) => t.name).join(", ") : "none"} />
        </section>
        {camera.status.reason && (
          <Notice tone={camera.status.state === "error" ? "error" : "info"}>{camera.status.reason}</Notice>
        )}
      </div>
      <div>
        <h3 className="mb-2 text-xs font-medium tracking-wide text-muted uppercase">Snapshot</h3>
        {stream?.rendition_status === "ready" ? (
          <img
            src={`/api/v1/cameras/${camera.id}/snapshot?t=${tick}`}
            alt={`Snapshot of ${camera.name}`}
            className="aspect-video w-full rounded-sm border border-border bg-black object-contain"
          />
        ) : (
          <div className="flex aspect-video w-full items-center justify-center rounded-sm border border-border text-xs text-muted">
            {stream?.rendition_status === "failed" ? "Encoding failed" : "Encoding the stream…"}
          </div>
        )}
      </div>
    </div>
  );
}

function Info({ label, value }: { label: string; value: ReactNode }) {
  return (
    <div className="min-w-0">
      <div className="text-xs text-muted">{label}</div>
      <div className="truncate">{value}</div>
    </div>
  );
}
