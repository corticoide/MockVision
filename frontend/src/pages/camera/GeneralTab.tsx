import { type FormEvent, useState } from "react";
import { ApiError, type Camera, errorMessage } from "@/api/client";
import { useNodeMetrics, useTargets, useUpdateCamera } from "@/api/queries";
import { Mono } from "@/components/badges";
import { toast } from "@/components/toast";
import { Card } from "@/components/ui/card";
import { Checkbox, Field, Input } from "@/components/ui/form";
import { useDraft } from "@/lib/draft";
import { useT } from "@/lib/i18n";
import { Link } from "@/lib/router";
import { formatBytes, formatPercent, formatTime, sinceText } from "@/lib/utils";
import { EndpointList, Info, isRunning, SaveBar, SectionTitle, SnapshotPreview } from "./parts";

const splitTags = (s: string) =>
  s
    .split(",")
    .map((t) => t.trim())
    .filter(Boolean);

const savedOf = (camera: Camera) => ({
  name: camera.name,
  tags: camera.tags.join(", "),
  autostart: camera.autostart,
  targetIds: camera.targets.map((t) => t.id).sort(),
});

export function GeneralTab({ camera }: { camera: Camera }) {
  const { data: targets } = useTargets();
  const update = useUpdateCamera();
  const t = useT();
  const form = useDraft(savedOf(camera));
  const { name, tags, autostart, targetIds } = form.draft;
  const [errors, setErrors] = useState<Record<string, string>>({});

  const submit = (e: FormEvent) => {
    e.preventDefault();
    setErrors({});
    update.mutate(
      { id: camera.id, body: { name: name.trim(), tags: splitTags(tags), autostart, target_ids: targetIds } },
      {
        onSuccess: (saved) => {
          form.resetTo(savedOf(saved));
          toast(t("Camera saved"), "ok");
        },
        onError: (err) => {
          if (err instanceof ApiError) setErrors(err.fieldErrors());
          toast(errorMessage(err), "error");
        },
      },
    );
  };

  return (
    <div className="grid grid-cols-[minmax(0,1fr)_340px] items-start gap-4">
      <div className="flex flex-col gap-4">
        <Card className="p-4">
          <form id="camera-general" onSubmit={submit} className="grid grid-cols-2 gap-x-4 gap-y-3">
            <Field label={t("Name")} error={errors["name"]}>
              <Input value={name} onChange={(e) => form.set({ name: e.target.value })} required />
            </Field>
            <Field label={t("Tags")} hint={t("Comma separated, for filtering.")} error={errors["tags"]}>
              <Input value={tags} onChange={(e) => form.set({ tags: e.target.value })} placeholder="gate, north" />
            </Field>
            <div className="col-span-2">
              <Checkbox
                label={t("Start with the node (autostart)")}
                checked={autostart}
                onChange={(e) => form.set({ autostart: e.target.checked })}
              />
            </div>
            <Field
              label={t("Event targets")}
              group
              className="col-span-2"
              error={errors["target_ids"]}
              hint={
                targets?.length ? (
                  t("Changes reach a running camera at once.")
                ) : (
                  <>
                    {t("No targets yet; add them in")} <Link href="/targets" className="underline">{t("Targets")}</Link>.
                  </>
                )
              }
            >
              <div className="flex flex-wrap gap-x-4 gap-y-1 py-1">
                {targets?.map((target) => (
                  <Checkbox
                    key={target.id}
                    label={
                      <>
                        {target.name} <Mono className="text-muted">{target.url}</Mono>
                        {!target.enabled && <span className="text-warn">{t(" (disabled)")}</span>}
                      </>
                    }
                    checked={targetIds.includes(target.id)}
                    onChange={(e) =>
                      form.set({ targetIds: (e.target.checked ? [...targetIds, target.id] : targetIds.filter((x) => x !== target.id)).sort() })
                    }
                  />
                ))}
              </div>
            </Field>
          </form>
          <SaveBar
            form="camera-general"
            dirty={form.dirty}
            saving={update.isPending}
            onDiscard={() => {
              form.discard();
              setErrors({});
            }}
          />
        </Card>
        <Card className="p-4">
          <SectionTitle>{t("Endpoints")}</SectionTitle>
          <EndpointList camera={camera} />
          <p className="mt-2 text-xs text-muted">
            {t("Accounts")}: {camera.users.map((u) => `${u.username} (${u.role})`).join(", ")} · {t("Digest authentication")}
          </p>
        </Card>
        <StatusCard camera={camera} />
      </div>
      <Card className="p-4">
        <SectionTitle>{t("Snapshot")}</SectionTitle>
        <SnapshotPreview camera={camera} />
      </Card>
    </div>
  );
}

function StatusCard({ camera }: { camera: Camera }) {
  const { data: metrics } = useNodeMetrics();
  const m = metrics?.cameras[camera.id];
  const running = isRunning(camera);
  const stream = camera.streams[0];
  return (
    <Card className="p-4">
      <SectionTitle>Status</SectionTitle>
      <div className="grid grid-cols-3 gap-x-6 gap-y-3 text-[13px]">
        <Info label="Serial" value={<Mono>{camera.serial}</Mono>} />
        <Info label="Profile" value={<Mono>{`${camera.profile.id}@${camera.profile.version}`}</Mono>} />
        <Info label="Stream" value={stream ? `${stream.codec.toUpperCase()} ${stream.resolution} @ ${stream.fps} fps` : "—"} />
        <Info label="Up for" value={running ? sinceText(camera.status.started_at) : "—"} />
        <Info label="Last heartbeat" value={formatTime(camera.status.last_heartbeat)} />
        <Info label="PID / namespace" value={camera.status.pid ? <Mono>{[camera.status.pid, camera.status.netns].filter(Boolean).join(" · ")}</Mono> : "—"} />
        <Info label="CPU" value={<Mono>{running && m ? formatPercent(m.cpu_percent) : "—"}</Mono>} />
        <Info label="RAM" value={<Mono>{running && m ? formatBytes(m.rss_bytes) : "—"}</Mono>} />
        <Info label="Clients" value={<Mono>{running && m ? m.clients : "—"}</Mono>} />
        <Info label="Created" value={formatTime(camera.created_at)} />
        <Info label="Updated" value={formatTime(camera.updated_at)} />
        <Info label="Retries" value={camera.status.retries || "—"} />
      </div>
    </Card>
  );
}
