import { Camera as CameraIcon, CopyPlus, Play, Plus, RotateCw, Square, Trash2, Zap } from "lucide-react";
import { useState } from "react";
import { type Camera, errorMessage } from "@/api/client";
import { useCameraAction, useCameras, useDeleteCamera, useNodeMetrics, useTrigger } from "@/api/queries";
import { Badge, LevelBadge, Mono, StateBadge } from "@/components/badges";
import { toast } from "@/components/toast";
import { Button } from "@/components/ui/button";
import { Card, Empty, Notice, PageHeader } from "@/components/ui/card";
import { Table, TBody, TD, TH, THead, TR } from "@/components/ui/table";
import { Link } from "@/lib/router";
import { formatBytes, formatPercent } from "@/lib/utils";
import { CloneDialog } from "./camera/dialogs";
import { NewCameraDialog } from "./NewCameraDialog";

export function CamerasPage() {
  const { data: cameras, isLoading, error } = useCameras();
  const { data: metrics } = useNodeMetrics();
  const [creating, setCreating] = useState(false);
  const [cloning, setCloning] = useState<Camera | null>(null);

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
                return (
                  <TR key={c.id}>
                    <TD>
                      <Link href={`/cameras/${c.id}`} className="font-medium hover:underline">
                        {c.name}
                      </Link>
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
                        {c.status.pending_restart.length > 0 && (
                          <Badge tone="warn" icon={<RotateCw />} title={`Saved ${c.status.pending_restart.join(" and ")} changes apply after a restart`}>
                            restart pending
                          </Badge>
                        )}
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
                      <RowActions camera={c} onClone={() => setCloning(c)} />
                    </TD>
                  </TR>
                );
              })}
            </TBody>
          </Table>
        )}
      </Card>
      <NewCameraDialog open={creating} onClose={() => setCreating(false)} />
      {cloning && <CloneDialog camera={cloning} open onClose={() => setCloning(null)} />}
    </>
  );
}

function RowActions({ camera, onClone }: { camera: Camera; onClone: () => void }) {
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
        <Button size="icon" variant="ghost" disabled={busy} onClick={() => run("start")} title="Start" aria-label={`Start ${camera.name}`}>
          <Play />
        </Button>
      ) : (
        <Button size="icon" variant="ghost" disabled={busy || !running} onClick={() => run("stop")} title="Stop" aria-label={`Stop ${camera.name}`}>
          <Square />
        </Button>
      )}
      <Button size="icon" variant="ghost" onClick={onClone} title="Clone" aria-label={`Clone ${camera.name}`}>
        <CopyPlus />
      </Button>
      <Button
        size="icon"
        variant="ghost"
        title="Delete"
        aria-label={`Delete ${camera.name}`}
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
