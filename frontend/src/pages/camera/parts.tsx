import { type ReactNode, useEffect, useState } from "react";
import type { Camera } from "@/api/client";
import { Badge, Mono } from "@/components/badges";
import { CopyButton } from "@/components/CopyButton";
import { Button } from "@/components/ui/button";
import { useT } from "@/lib/i18n";

export function isRunning(camera: Camera) {
  return camera.status.state === "running" || camera.status.state === "degraded";
}

export function SectionTitle({ children }: { children: ReactNode }) {
  return <h3 className="mb-2 text-xs font-medium tracking-wide text-muted uppercase">{children}</h3>;
}

export function Info({ label, value }: { label: string; value: ReactNode }) {
  return (
    <div className="min-w-0">
      <div className="text-xs text-muted">{label}</div>
      <div className="truncate">{value}</div>
    </div>
  );
}

/** The main stream's snapshot, refreshed every 5 s while the camera runs. */
export function SnapshotPreview({ camera }: { camera: Camera }) {
  const t = useT();
  const running = isRunning(camera);
  const [tick, setTick] = useState(0);
  useEffect(() => {
    if (!running) return;
    const timer = setInterval(() => setTick((n) => n + 1), 5000);
    return () => clearInterval(timer);
  }, [running]);
  const stream = camera.streams[0];
  // A new rendition means a new picture: reload it at once.
  const version = `${stream?.rendition_id ?? ""}-${tick}`;
  return stream?.rendition_status === "ready" ? (
    <img
      src={`/api/v1/cameras/${camera.id}/snapshot?v=${version}`}
      alt={t("Snapshot of {name}", { name: camera.name })}
      className="aspect-video w-full rounded-sm border border-border bg-black object-contain"
    />
  ) : (
    <div className="flex aspect-video w-full items-center justify-center rounded-sm border border-border text-xs text-muted">
      {stream?.rendition_status === "failed" ? t("Encoding failed: {err}", { err: stream.rendition_error ?? "" }) : t("Encoding the stream…")}
    </div>
  );
}

export function EndpointList({ camera }: { camera: Camera }) {
  const t = useT();
  if (camera.endpoints.length === 0) return <p className="text-[13px] text-muted">{t("No protocol is enabled.")}</p>;
  return (
    <div className="flex flex-col gap-1.5">
      {camera.endpoints.map((e) => (
        <div key={e.instance} className="flex items-center gap-2">
          <Badge tone="info">{e.protocol.toUpperCase()}</Badge>
          <Mono className="select-all">{e.url}</Mono>
          <CopyButton text={e.url} label={t("Copy URL")} />
        </div>
      ))}
    </div>
  );
}

/** Save and discard buttons of a form, with an optional note. */
export function SaveBar({
  dirty,
  saving,
  onDiscard,
  note,
  form,
}: {
  dirty: boolean;
  saving: boolean;
  onDiscard: () => void;
  note?: ReactNode;
  form?: string;
}) {
  const t = useT();
  return (
    <div className="mt-4 flex items-center justify-end gap-3 border-t border-border pt-3">
      {note && <span className="mr-auto text-xs text-muted">{note}</span>}
      <Button onClick={onDiscard} disabled={!dirty || saving}>
        {t("Discard")}
      </Button>
      <Button type="submit" form={form} variant="primary" disabled={!dirty || saving}>
        {saving ? t("Saving…") : t("Save")}
      </Button>
    </div>
  );
}
