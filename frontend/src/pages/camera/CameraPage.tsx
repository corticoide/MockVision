import { ArrowLeft, CopyPlus, Play, RotateCcw, RotateCw, Square, Trash2, Zap } from "lucide-react";
import { useState } from "react";
import { type Camera, errorMessage } from "@/api/client";
import { useCameraTopic } from "@/api/live";
import { useCamera, useCameraAction, useDeleteCamera, useEvents, useTrigger } from "@/api/queries";
import { LevelBadge, Mono, StateBadge } from "@/components/badges";
import { EventTable } from "@/components/EventTable";
import { toast } from "@/components/toast";
import { Button } from "@/components/ui/button";
import { Card, Empty, Notice } from "@/components/ui/card";
import { TabPanel, Tabs } from "@/components/ui/tabs";
import { useT } from "@/lib/i18n";
import { Link, navigate } from "@/lib/router";
import { ConfigTab } from "./ConfigTab";
import { CloneDialog, ResetDialog } from "./dialogs";
import { GeneralTab } from "./GeneralTab";
import { MediaTab } from "./MediaTab";
import { NetworkTab } from "./NetworkTab";
import { isRunning } from "./parts";
import { ProtocolsTab } from "./ProtocolsTab";
import { UsersTab } from "./UsersTab";

const sections = [
  { id: "general", label: "General" },
  { id: "network", label: "Network" },
  { id: "protocols", label: "Protocols" },
  { id: "media", label: "Media" },
  { id: "users", label: "Users" },
  { id: "config", label: "Configuration" },
  { id: "events", label: "Events" },
];

export function CameraPage({ id, tab }: { id: string; tab?: string }) {
  const { data: camera, isLoading, error } = useCamera(id);
  const t = useT();
  useCameraTopic(id);

  if (error) return <Notice tone="error">{errorMessage(error)}</Notice>;
  if (isLoading) return <Empty title={t("Loading camera…")} />;
  if (!camera) {
    return (
      <Card>
        <Empty title={t("Camera not found")}>
          {t("It may have been deleted.")} <Link href="/cameras" className="underline">{t("Back to the cameras")}</Link>.
        </Empty>
      </Card>
    );
  }

  const current = sections.some((s) => s.id === tab) ? tab! : "general";
  const pending = camera.status.pending_restart;
  const go = (s: string) => navigate(`/cameras/${id}${s === "general" ? "" : `/${s}`}`);

  return (
    <>
      <Header camera={camera} />
      {pending.length > 0 && <PendingRestart camera={camera} />}
      {camera.status.state === "error" && camera.status.reason && (
        <div className="mb-3">
          <Notice tone="error">{camera.status.reason}</Notice>
        </div>
      )}
      <Tabs
        label={t("Camera sections")}
        items={sections.map((s) => ({ id: s.id, label: t(s.label), badge: (pending as string[]).includes(s.id) }))}
        value={current}
        onChange={go}
      />
      <TabPanel key={camera.id} id={current}>
        {current === "general" && <GeneralTab camera={camera} />}
        {current === "network" && <NetworkTab camera={camera} />}
        {current === "protocols" && <ProtocolsTab camera={camera} />}
        {current === "media" && <MediaTab camera={camera} />}
        {current === "users" && <UsersTab camera={camera} />}
        {current === "config" && <ConfigTab camera={camera} />}
        {current === "events" && <EventsTab camera={camera} />}
      </TabPanel>
    </>
  );
}

function Header({ camera }: { camera: Camera }) {
  const action = useCameraAction();
  const trigger = useTrigger();
  const del = useDeleteCamera();
  const t = useT();
  const [cloning, setCloning] = useState(false);
  const [resetting, setResetting] = useState(false);
  const state = camera.status.state;
  const running = isRunning(camera);
  const busy = ["provisioning", "starting", "stopping", "restarting"].includes(state) || action.isPending;
  const canStart = state === "stopped" || state === "error";

  const run = (a: "start" | "stop" | "restart") =>
    action.mutate({ id: camera.id, action: a }, { onError: (err) => toast(errorMessage(err), "error") });

  return (
    <div className="mb-4">
      <Link href="/cameras" className="mb-2 inline-flex items-center gap-1 text-xs text-muted hover:text-text [&_svg]:size-3.5">
        <ArrowLeft /> {t("Cameras")}
      </Link>
      <div className="flex items-start justify-between gap-4">
        <div className="min-w-0">
          <div className="flex items-center gap-3">
            <h1 className="truncate text-lg font-semibold tracking-tight">{camera.name}</h1>
            <StateBadge state={state} reason={camera.status.reason} />
          </div>
          <div className="mt-1 flex flex-wrap items-center gap-x-3 gap-y-1 text-[13px] text-muted">
            <span>
              {camera.profile.vendor} {camera.profile.model}
            </span>
            <LevelBadge level={camera.profile.level} />
            <Mono>{camera.network.ip || "127.0.0.1"}</Mono>
            <Mono>{camera.network.mac}</Mono>
          </div>
        </div>
        <div className="flex shrink-0 flex-wrap items-center justify-end gap-1.5">
          <Button
            size="sm"
            variant="secondary"
            disabled={!running || trigger.isPending}
            title={t("Send a line-crossing event to the camera's targets")}
            onClick={() =>
              trigger.mutate(
                { id: camera.id, type: "line_crossing" },
                {
                  onSuccess: () => toast(t("Line crossing sent from {name}", { name: camera.name }), "ok"),
                  onError: (err) => toast(errorMessage(err), "error"),
                },
              )
            }
          >
            <Zap /> {t("Line crossing")}
          </Button>
          {canStart ? (
            <Button size="sm" variant="primary" disabled={busy} onClick={() => run("start")}>
              <Play /> {t("Start")}
            </Button>
          ) : (
            <>
              <Button size="sm" disabled={busy || !running} onClick={() => run("restart")}>
                <RotateCw /> {t("Restart")}
              </Button>
              <Button size="sm" disabled={busy || !running} onClick={() => run("stop")}>
                <Square /> {t("Stop")}
              </Button>
            </>
          )}
          <Button size="sm" onClick={() => setCloning(true)}>
            <CopyPlus /> {t("Clone")}
          </Button>
          <Button size="sm" onClick={() => setResetting(true)}>
            <RotateCcw /> {t("Restore")}
          </Button>
          <Button
            size="sm"
            variant="danger"
            disabled={del.isPending}
            onClick={() => {
              if (!confirm(t("Delete camera {name} and its events?", { name: camera.name }))) return;
              del.mutate(camera.id, {
                onSuccess: () => {
                  toast(t("Camera {name} deleted", { name: camera.name }), "ok");
                  navigate("/cameras");
                },
                onError: (err) => toast(errorMessage(err), "error"),
              });
            }}
          >
            <Trash2 /> {t("Delete")}
          </Button>
        </div>
      </div>
      <CloneDialog camera={camera} open={cloning} onClose={() => setCloning(false)} />
      <ResetDialog camera={camera} open={resetting} onClose={() => setResetting(false)} />
    </div>
  );
}

function PendingRestart({ camera }: { camera: Camera }) {
  const action = useCameraAction();
  const t = useT();
  const what = camera.status.pending_restart.map((p) => (p === "network" ? t("network") : t("protocols"))).join(t(" and "));
  return (
    <div className="mb-3">
      <Notice tone="warn">
        <div className="flex items-center justify-between gap-4">
          <span>
            {t("Saved changes to the {what} apply when the camera restarts (RN-09); it keeps running with the previous ones.", { what })}
          </span>
          <Button
            size="sm"
            disabled={action.isPending}
            onClick={() =>
              action.mutate(
                { id: camera.id, action: "restart" },
                {
                  onSuccess: () => toast(t("{name} is restarting", { name: camera.name }), "ok"),
                  onError: (err) => toast(errorMessage(err), "error"),
                },
              )
            }
          >
            <RotateCw /> {t("Restart now")}
          </Button>
        </div>
      </Notice>
    </div>
  );
}

function EventsTab({ camera }: { camera: Camera }) {
  const { data, isLoading, error } = useEvents(camera.id);
  const t = useT();
  const events = data?.items ?? [];
  if (error) return <Notice tone="error">{errorMessage(error)}</Notice>;
  return (
    <Card>
      {isLoading ? (
        <Empty title={t("Loading events…")} />
      ) : events.length === 0 ? (
        <Empty title={t("No events from this camera yet")}>{t('Press "Line crossing" while the camera runs.')}</Empty>
      ) : (
        <EventTable events={events} showCamera={false} />
      )}
    </Card>
  );
}
