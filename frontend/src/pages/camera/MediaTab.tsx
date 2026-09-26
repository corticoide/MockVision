import { type FormEvent, useState } from "react";
import { ApiError, type Camera, type CameraStream, errorMessage, type ProfileDetail } from "@/api/client";
import { useAssets, useProfile, useUpdateStream } from "@/api/queries";
import { Badge, Mono } from "@/components/badges";
import { CopyButton } from "@/components/CopyButton";
import { toast } from "@/components/toast";
import { Card } from "@/components/ui/card";
import { Field, Input, Select } from "@/components/ui/form";
import { Tabs } from "@/components/ui/tabs";
import { useDraft } from "@/lib/draft";
import { useT } from "@/lib/i18n";
import { codecLabel, streamLabel, streamPurpose, streamSummary } from "@/lib/media";
import { Link, setSearch, useSearch } from "@/lib/router";
import { formatBytes } from "@/lib/utils";
import { Info, SaveBar, SectionTitle, SnapshotPreview } from "./parts";

const renditionTone = { ready: "ok", pending: "info", failed: "error", missing: "muted" } as const;

type ProfileStream = ProfileDetail["streams"][number];

const savedOf = (s: CameraStream) => ({
  asset: s.asset_id,
  codec: s.codec as string,
  resolution: s.resolution,
  fps: String(s.fps),
  bitrate: String(s.bitrate),
  gop: String(s.gop),
});

export function MediaTab({ camera }: { camera: Camera }) {
  const t = useT();
  const { data: profile } = useProfile({ id: camera.profile.id, version: camera.profile.version });
  const search = useSearch();
  if (camera.streams.length === 0) return <Card className="p-4 text-muted">{t("The camera has no stream.")}</Card>;
  const stream = camera.streams.find((s) => s.name === search.get("stream")) ?? camera.streams[0];

  return (
    <div className="grid grid-cols-[minmax(0,1fr)_340px] items-start gap-4">
      <Card className="p-4">
        <p className="mb-2 text-[13px] text-muted">
          {t(
            "A camera encodes the same picture several times, one stream per use. Recorders and viewers pick the stream they need by its RTSP address.",
          )}
        </p>
        <Tabs
          label={t("Streams")}
          items={camera.streams.map((s) => ({ id: s.name, label: `${streamLabel(s.name, t)} · ${codecLabel(s.codec)} ${s.resolution}` }))}
          value={stream.name}
          onChange={(id) => setSearch({ stream: id === "main" ? undefined : id })}
        />
        <div role="tabpanel" id={`panel-${stream.name}`} aria-labelledby={`tab-${stream.name}`} className="pt-4">
          <StreamForm key={stream.name} camera={camera} stream={stream} spec={profile?.streams.find((s) => s.name === stream.name)} profile={profile} />
        </div>
      </Card>
      <Card className="p-4">
        <SectionTitle>{t("Snapshot of the {stream}", { stream: streamLabel(stream.name, t).toLowerCase() })}</SectionTitle>
        <SnapshotPreview camera={camera} stream={stream.name} />
        <p className="mt-2 text-xs text-muted">{t("The same picture every client gets from this stream, at its resolution.")}</p>
      </Card>
    </div>
  );
}

function StreamForm({
  camera,
  stream,
  spec,
  profile,
}: {
  camera: Camera;
  stream: CameraStream;
  spec?: ProfileStream;
  profile?: ProfileDetail;
}) {
  const t = useT();
  const { data: assets } = useAssets();
  const update = useUpdateStream(camera.id);
  const bound = (field: string) => profile?.params.some((p) => p.bind === `media.${stream.name}.${field}`) ?? false;
  const form = useDraft(savedOf(stream));
  const initial = savedOf(stream);
  const d = form.draft;
  const [errors, setErrors] = useState<Record<string, string>>({});
  const mjpeg = d.codec === "mjpeg";

  const submit = (e: FormEvent) => {
    e.preventDefault();
    setErrors({});
    const changed = (k: keyof typeof d) => d[k] !== initial[k];
    update.mutate(
      {
        stream: stream.name,
        asset_id: changed("asset") ? d.asset : undefined,
        codec: changed("codec") ? (d.codec as CameraStream["codec"]) : undefined,
        resolution: changed("resolution") ? d.resolution : undefined,
        fps: changed("fps") ? Number(d.fps) : undefined,
        bitrate: changed("bitrate") ? Number(d.bitrate) : undefined,
        gop: changed("gop") && !mjpeg ? Number(d.gop) : undefined,
      },
      {
        onSuccess: (saved) => {
          const s = saved.streams.find((x) => x.name === stream.name);
          if (s) form.resetTo(savedOf(s));
          toast(t("Stream saved; it is encoded again and the camera switches to it"), "ok");
        },
        onError: (err) => {
          if (err instanceof ApiError) setErrors(err.fieldErrors());
          toast(errorMessage(err), "error");
        },
      },
    );
  };

  const fixed = t("Fixed by the profile.");
  const current = assets?.find((a) => a.id === stream.asset_id);
  const seconds = Number(d.gop) / Math.max(1, Number(d.fps));

  return (
    <>
      <div className="mb-3 flex flex-wrap items-center gap-2">
        <SectionTitle>{streamLabel(stream.name, t)}</SectionTitle>
        <Badge tone={renditionTone[stream.rendition_status]}>{t(stream.rendition_status)}</Badge>
        <span className="mb-2 text-xs text-muted">{streamPurpose(stream.name, t)}</span>
      </div>
      {stream.url ? (
        <div className="mb-4 flex items-center gap-2 text-[13px]">
          <Badge tone="info">RTSP</Badge>
          <Mono className="select-all">{stream.url}</Mono>
          <CopyButton text={stream.url} label={t("Copy URL")} />
        </div>
      ) : (
        <p className="mb-4 text-xs text-muted">{t("No RTSP protocol serves this stream; its snapshot is still available.")}</p>
      )}
      <form id={`camera-media-${stream.name}`} onSubmit={submit} className="grid grid-cols-3 gap-x-4 gap-y-3">
        <Field label={t("Image")} error={errors["asset_id"]} hint={<Link href="/assets" className="underline">{t("Manage images")}</Link>}>
          <Select value={d.asset} onChange={(e) => form.set({ asset: e.target.value })}>
            {assets?.map((a) => (
              <option key={a.id} value={a.id}>
                {a.builtin ? t("Test pattern") : a.filename} ({a.width}×{a.height})
              </option>
            ))}
          </Select>
        </Field>
        <Field
          label={t("Codec")}
          error={errors["codec"]}
          hint={
            bound("codec")
              ? t("H.264 plays everywhere; H.265 needs about half the bitrate but not every client plays it; MJPEG sends each frame as a JPEG.")
              : fixed
          }
        >
          <Select value={d.codec} onChange={(e) => form.set({ codec: e.target.value })} disabled={!bound("codec")}>
            {(spec?.codecs ?? [stream.codec]).map((c) => (
              <option key={c} value={c}>
                {codecLabel(c)}
                {c === spec?.default.codec ? t(" (default)") : ""}
              </option>
            ))}
          </Select>
        </Field>
        <Field
          label={t("Resolution")}
          error={errors["resolution"]}
          hint={bound("resolution") ? t("Also changes the profile parameter bound to it.") : fixed}
        >
          <Select value={d.resolution} onChange={(e) => form.set({ resolution: e.target.value })} disabled={!bound("resolution")}>
            {(spec?.resolutions ?? [stream.resolution]).map((r) => (
              <option key={r} value={r}>
                {r}
                {r === spec?.default.resolution ? t(" (default)") : ""}
              </option>
            ))}
          </Select>
        </Field>
        <Field
          label={t("Frame rate (fps)")}
          error={errors["fps"]}
          hint={bound("fps") ? t("Between {min} and {max}.", { min: spec?.fps_min || 1, max: spec?.fps_max || 60 }) : fixed}
        >
          <Input
            type="number"
            min={spec?.fps_min || 1}
            max={spec?.fps_max || 60}
            value={d.fps}
            onChange={(e) => form.set({ fps: e.target.value })}
            disabled={!bound("fps")}
            className="font-mono"
          />
        </Field>
        <Field
          label={t("Bitrate (kbit/s)")}
          error={errors["bitrate"]}
          hint={
            bound("bitrate")
              ? t("Bandwidth the stream uses. Between {min} and {max}.", { min: spec?.bitrate_min || 16, max: spec?.bitrate_max || 100000 })
              : fixed
          }
        >
          <Input
            type="number"
            min={spec?.bitrate_min || 16}
            max={spec?.bitrate_max || 100000}
            value={d.bitrate}
            onChange={(e) => form.set({ bitrate: e.target.value })}
            disabled={!bound("bitrate")}
            className="font-mono"
          />
        </Field>
        <Field
          label={t("GOP (frames)")}
          error={errors["gop"]}
          hint={
            mjpeg
              ? t("Every MJPEG frame is a whole picture: there is no GOP.")
              : bound("gop")
                ? t("Frames from one keyframe to the next ({seconds} s). A client that connects shows the picture from a keyframe.", {
                    seconds: Number.isFinite(seconds) ? seconds.toFixed(1) : "—",
                  })
                : fixed
          }
        >
          <Input
            type="number"
            min={1}
            max={600}
            value={mjpeg ? "1" : d.gop}
            onChange={(e) => form.set({ gop: e.target.value })}
            disabled={mjpeg || !bound("gop")}
            className="font-mono"
          />
        </Field>
      </form>
      <div className="mt-4 grid grid-cols-3 gap-x-6 gap-y-2 text-[13px]">
        <Info label={t("Now")} value={streamSummary(stream)} />
        <Info label={t("Bitrate")} value={<Mono>{stream.bitrate} kbit/s</Mono>} />
        <Info label={t("Image")} value={current ? `${current.builtin ? t("Test pattern") : current.filename} · ${formatBytes(current.size)}` : "—"} />
      </div>
      {stream.rendition_error && <p className="mt-2 text-xs text-error">{stream.rendition_error}</p>}
      <SaveBar
        form={`camera-media-${stream.name}`}
        dirty={form.dirty}
        saving={update.isPending}
        onDiscard={() => {
          form.discard();
          setErrors({});
        }}
        note={t("Applied without restarting: the stream is encoded once and the camera switches to it; its viewers reconnect (RN-09).")}
      />
    </>
  );
}
