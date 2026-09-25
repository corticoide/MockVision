import { type FormEvent, useState } from "react";
import { ApiError, type Camera, errorMessage } from "@/api/client";
import { useAssets, useProfile, useUpdateStream } from "@/api/queries";
import { Badge, Mono } from "@/components/badges";
import { toast } from "@/components/toast";
import { Card } from "@/components/ui/card";
import { Field, Input, Select } from "@/components/ui/form";
import { useDraft } from "@/lib/draft";
import { Link } from "@/lib/router";
import { formatBytes } from "@/lib/utils";
import { Info, SaveBar, SectionTitle, SnapshotPreview } from "./parts";

const renditionTone = { ready: "ok", pending: "info", failed: "error", missing: "muted" } as const;

const savedOf = (camera: Camera) => {
  const stream = camera.streams[0];
  return { asset: stream?.asset_id ?? "", resolution: stream?.resolution ?? "", fps: String(stream?.fps ?? "") };
};

export function MediaTab({ camera }: { camera: Camera }) {
  const { data: profile } = useProfile({ id: camera.profile.id, version: camera.profile.version });
  const { data: assets } = useAssets();
  const update = useUpdateStream(camera.id);
  const stream = camera.streams[0];
  const spec = profile?.streams.find((s) => s.name === stream?.name);
  const bound = (field: string) => profile?.params.some((p) => p.bind === `media.${stream?.name}.${field}`) ?? false;

  const form = useDraft(savedOf(camera));
  const initial = savedOf(camera);
  const { asset, resolution, fps } = form.draft;
  const [errors, setErrors] = useState<Record<string, string>>({});

  if (!stream) return <Card className="p-4 text-muted">The camera has no stream.</Card>;

  const submit = (e: FormEvent) => {
    e.preventDefault();
    setErrors({});
    update.mutate(
      {
        stream: stream.name,
        asset_id: asset !== initial.asset ? asset : undefined,
        resolution: resolution !== initial.resolution ? resolution : undefined,
        fps: fps !== initial.fps ? Number(fps) : undefined,
      },
      {
        onSuccess: (saved) => {
          form.resetTo(savedOf(saved));
          toast("Stream saved; it is encoded again and the camera switches to it", "ok");
        },
        onError: (err) => {
          if (err instanceof ApiError) setErrors(err.fieldErrors());
          toast(errorMessage(err), "error");
        },
      },
    );
  };

  const current = assets?.find((a) => a.id === stream.asset_id);

  return (
    <div className="grid grid-cols-[minmax(0,1fr)_340px] items-start gap-4">
      <Card className="p-4">
        <div className="mb-3 flex items-center gap-2">
          <SectionTitle>Stream {stream.name}</SectionTitle>
          <Badge tone={renditionTone[stream.rendition_status]}>{stream.rendition_status}</Badge>
        </div>
        <form id="camera-media" onSubmit={submit} className="grid grid-cols-3 gap-x-4 gap-y-3">
          <Field label="Image" error={errors["asset_id"]} hint={<Link href="/assets" className="underline">Manage images</Link>}>
            <Select value={asset} onChange={(e) => form.set({ asset: e.target.value })}>
              {assets?.map((a) => (
                <option key={a.id} value={a.id}>
                  {a.builtin ? "Test pattern" : a.filename} ({a.width}×{a.height})
                </option>
              ))}
            </Select>
          </Field>
          <Field
            label="Resolution"
            error={errors["resolution"]}
            hint={bound("resolution") ? "Also changes the profile parameter bound to it." : "Fixed by the profile."}
          >
            <Select value={resolution} onChange={(e) => form.set({ resolution: e.target.value })} disabled={!bound("resolution")}>
              {(spec?.resolutions ?? [stream.resolution]).map((r) => (
                <option key={r} value={r}>
                  {r}
                  {r === spec?.default.resolution ? " (default)" : ""}
                </option>
              ))}
            </Select>
          </Field>
          <Field
            label="Frame rate (fps)"
            error={errors["fps"]}
            hint={bound("fps") ? `Between ${spec?.fps_min ?? 1} and ${spec?.fps_max ?? 30}.` : "Fixed by the profile."}
          >
            <Input
              type="number"
              min={spec?.fps_min || 1}
              max={spec?.fps_max || 60}
              value={fps}
              onChange={(e) => form.set({ fps: e.target.value })}
              disabled={!bound("fps")}
              className="font-mono"
            />
          </Field>
        </form>
        <div className="mt-4 grid grid-cols-4 gap-x-6 gap-y-2 text-[13px]">
          <Info label="Codec" value={stream.codec.toUpperCase()} />
          <Info label="Bitrate" value={<Mono>{stream.bitrate} kbit/s</Mono>} />
          <Info label="GOP" value={<Mono>{stream.gop} frames</Mono>} />
          <Info label="Image" value={current ? `${current.filename} · ${formatBytes(current.size)}` : "—"} />
        </div>
        {stream.rendition_error && <p className="mt-2 text-xs text-error">{stream.rendition_error}</p>}
        <SaveBar
          form="camera-media"
          dirty={form.dirty}
          saving={update.isPending}
          onDiscard={() => {
            form.discard();
            setErrors({});
          }}
          note="Applied without restarting: the picture is encoded once and the camera switches to it (RN-09)."
        />
      </Card>
      <Card className="p-4">
        <SectionTitle>Snapshot</SectionTitle>
        <SnapshotPreview camera={camera} />
      </Card>
    </div>
  );
}
