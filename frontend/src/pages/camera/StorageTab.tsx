import { Download, Eraser, HardDrive, Save } from "lucide-react";
import { type FormEvent, useState } from "react";
import { ApiError, type Camera, errorMessage, type Recording, type Storage, type StorageStatus } from "@/api/client";
import { recordingURL, useFormatStorage, useRecordings, useStorage, useUpdateStorage } from "@/api/queries";
import { Badge, Mono } from "@/components/badges";
import { ProgressBar } from "@/components/Sparkline";
import { toast } from "@/components/toast";
import { Button } from "@/components/ui/button";
import { Card, Empty, Notice } from "@/components/ui/card";
import { Checkbox, Field, Input, Select } from "@/components/ui/form";
import { Table, TBody, TD, TH, THead, TR } from "@/components/ui/table";
import { type Translate, useT } from "@/lib/i18n";
import { formatBytes, formatTime } from "@/lib/utils";
import { SectionTitle } from "./parts";

type Kind = Storage["kind"];

/** Card sizes as shops sell them, in MiB. */
const cardSizes = [64, 256, 1024, 4096, 8192, 16384, 32768, 65536, 131072, 262144, 524288, 1048576];

function sizeLabel(mb: number): string {
  return mb >= 1024 ? `${mb / 1024} GB` : `${mb} MB`;
}

const states: Record<string, { label: string; tone: "ok" | "warn" | "error" | "muted" }> = {
  present: { label: "Working", tone: "ok" },
  full: { label: "Full", tone: "warn" },
  absent: { label: "Missing", tone: "error" },
  error: { label: "Error", tone: "error" },
  read_only: { label: "Read only", tone: "warn" },
};

export function StorageTab({ camera }: { camera: Camera }) {
  const { data: storage, isLoading, error } = useStorage(camera.id);
  const t = useT();
  if (error) return <Notice tone="error">{errorMessage(error)}</Notice>;
  if (isLoading || !storage) return <Empty title={t("Loading storage…")} />;
  return (
    <div className="flex flex-col gap-4">
      <Card className="p-4">
        <SectionTitle>{t("Where it records")}</SectionTitle>
        <p className="mb-3 text-[13px] text-muted">
          {t(
            "Events record their snapshot and a clip, as the profile says, on a simulated SD card (a directory of the node with a quota) or on a NAS share the camera writes itself. Clients search and download them through the camera's API and play them back over RTSP.",
          )}
        </p>
        <StorageForm key={`${storage.kind}-${storage.size_mb}-${storage.nas_url}`} camera={camera} storage={storage} />
      </Card>
      {storage.kind !== "none" && <StateCard camera={camera} storage={storage} />}
      <RecordsCard storage={storage} />
      {storage.kind !== "none" && <RecordingsCard camera={camera} />}
    </div>
  );
}

function StorageForm({ camera, storage }: { camera: Camera; storage: Storage }) {
  const update = useUpdateStorage();
  const t = useT();
  const [kind, setKind] = useState<Kind>(storage.kind);
  const [size, setSize] = useState(storage.kind === "sd" ? storage.size_mb : Math.min(1024, storage.max_sd_mb));
  const [overwrite, setOverwrite] = useState(storage.overwrite);
  const [url, setURL] = useState(storage.nas_url);
  const [username, setUsername] = useState(storage.nas_username);
  const [password, setPassword] = useState("");
  const [errors, setErrors] = useState<Record<string, string>>({});
  const sizes = [...new Set([...cardSizes.filter((s) => s <= storage.max_sd_mb), ...(storage.kind === "sd" ? [storage.size_mb] : [])])].sort((a, b) => a - b);

  const submit = (e: FormEvent) => {
    e.preventDefault();
    setErrors({});
    if (storage.kind === "sd" && kind !== "sd" && !confirm(t("Take the card out? Its recordings are wiped."))) return;
    update.mutate(
      {
        id: camera.id,
        body: {
          kind,
          size_mb: kind === "sd" ? size : undefined,
          overwrite,
          nas_url: kind === "nas" ? url : undefined,
          nas_username: kind === "nas" ? username : undefined,
          nas_password: kind === "nas" && password !== "" ? password : undefined,
        },
      },
      {
        onSuccess: () => {
          setPassword("");
          toast(t("Storage saved"), "ok");
        },
        onError: (err) => {
          if (err instanceof ApiError) setErrors(err.fieldErrors());
          toast(errorMessage(err), "error");
        },
      },
    );
  };

  return (
    <form onSubmit={submit} className="grid grid-cols-4 items-end gap-x-4 gap-y-3">
      <Field label={t("Storage")} error={errors["kind"]}>
        <Select value={kind} onChange={(e) => setKind(e.target.value as Kind)}>
          <option value="none">{t("None")}</option>
          <option value="sd" disabled={storage.max_sd_mb === 0}>
            {t("SD card")}
          </option>
          <option value="nas" disabled={storage.nas_protocols.length === 0}>
            {t("NAS share")}
          </option>
        </Select>
      </Field>
      {kind === "sd" && (
        <>
          <Field
            label={t("Card size")}
            error={errors["size_mb"]}
            hint={t("Up to {max}, as the model takes.", {
              max: sizeLabel(storage.max_sd_mb),
            })}
          >
            <Select value={size} onChange={(e) => setSize(Number(e.target.value))}>
              {sizes.map((s) => (
                <option key={s} value={s}>
                  {sizeLabel(s)}
                </option>
              ))}
            </Select>
          </Field>
          <div className="col-span-2 pb-2">
            <Checkbox label={t("Overwrite the oldest recordings when full")} checked={overwrite} onChange={(e) => setOverwrite(e.target.checked)} />
          </div>
        </>
      )}
      {kind === "nas" && (
        <>
          <Field
            label={t("Share URL")}
            error={errors["nas_url"]}
            className="col-span-3"
            hint={t("Over {protocols}: nfs://host/export or smb://host/share/folder.", { protocols: storage.nas_protocols.join(", ") })}
          >
            <Input value={url} onChange={(e) => setURL(e.target.value)} placeholder="smb://nas.local/cameras" spellCheck={false} />
          </Field>
          <Field label={t("Username")} error={errors["nas_username"]} hint={t("SMB only; DOMAIN\\user for a domain account.")}>
            <Input value={username} onChange={(e) => setUsername(e.target.value)} autoComplete="off" />
          </Field>
          <Field label={t("Password")} error={errors["nas_password"]}>
            <Input
              type="password"
              value={password}
              onChange={(e) => setPassword(e.target.value)}
              autoComplete="new-password"
              placeholder={storage.has_nas_password ? t("Unchanged") : ""}
            />
          </Field>
          <div />
        </>
      )}
      {kind === "none" && <div className="col-span-2" />}
      <div className={kind === "sd" ? "col-span-4 flex justify-end" : "flex justify-end"}>
        <Button variant="primary" type="submit" disabled={update.isPending}>
          <Save /> {update.isPending ? t("Saving…") : t("Save")}
        </Button>
      </div>
    </form>
  );
}

function stateBadge(status: StorageStatus, t: Translate) {
  const s = states[status.state] ?? {
    label: status.state,
    tone: "muted" as const,
  };
  return <Badge tone={s.tone}>{t(s.label)}</Badge>;
}

function StateCard({ camera, storage }: { camera: Camera; storage: Storage }) {
  const format = useFormatStorage();
  const t = useT();
  const st = storage.status;
  const sd = storage.kind === "sd";
  const share = !sd && st.state === "error";
  const doFormat = () => {
    if (
      !confirm(
        t("Format the SD card of {name}? Every recording on it is wiped.", {
          name: camera.name,
        }),
      )
    )
      return;
    format.mutate(camera.id, {
      onSuccess: () => toast(t("SD card formatted"), "ok"),
      onError: (err) => toast(errorMessage(err), "error"),
    });
  };
  return (
    <Card className="p-4">
      <SectionTitle>{sd ? t("SD card") : t("NAS share")}</SectionTitle>
      <div className="flex items-center gap-6">
        <div className="flex items-center gap-2">
          <HardDrive className="size-4 text-muted" />
          {stateBadge(st, t)}
        </div>
        {sd ? (
          <div className="flex min-w-0 flex-1 items-center gap-3">
            <ProgressBar
              value={st.capacity_bytes > 0 ? st.used_bytes / st.capacity_bytes : 0}
              label={t("SD card usage")}
              className={st.state === "full" ? "text-warn" : "text-info"}
            />
            <Mono className="shrink-0 text-muted">
              {t("{used} of {total}", {
                used: formatBytes(st.used_bytes),
                total: formatBytes(st.capacity_bytes),
              })}
            </Mono>
          </div>
        ) : (
          <Mono className="min-w-0 flex-1 truncate text-muted">{storage.nas_url}</Mono>
        )}
        <span className="shrink-0 text-[13px] text-muted">{t("{n} files", { n: st.files })}</span>
        {sd && (
          <Button size="sm" onClick={doFormat} disabled={format.isPending}>
            <Eraser /> {t("Format")}
          </Button>
        )}
      </div>
      {sd && (
        <p className="mt-2 text-xs text-muted">
          {st.overwrite ? t("When full, the oldest recordings make room for the new ones.") : t("When full, the card stops recording and raises storage_full.")}
        </p>
      )}
      {share && storage.nas_error && (
        <div className="mt-3">
          <Notice tone="error">
            {t("The camera cannot reach its share:")} <span className="font-mono text-xs">{storage.nas_error}</span>
          </Notice>
        </div>
      )}
    </Card>
  );
}

function RecordsCard({ storage }: { storage: Storage }) {
  const t = useT();
  return (
    <Card className="p-4">
      <SectionTitle>{t("What events record")}</SectionTitle>
      {storage.records.length === 0 ? (
        <p className="text-[13px] text-muted">{t("The profile records no event.")}</p>
      ) : (
        <div className="flex flex-wrap gap-2">
          {storage.records.map((r) => (
            <Badge key={r.event} tone="muted">
              <Mono>{r.event}</Mono>
              {" · "}
              {[
                r.snapshot ? t("snapshot") : "",
                r.clip_s > 0
                  ? t("{s} s clip of {stream}", {
                      s: r.clip_s,
                      stream: r.stream,
                    })
                  : "",
              ]
                .filter(Boolean)
                .join(t(" and "))}
            </Badge>
          ))}
        </div>
      )}
      {storage.kind === "none" && storage.records.length > 0 && (
        <p className="mt-2 text-xs text-muted">{t("Give the camera an SD card or a NAS share for them to record.")}</p>
      )}
    </Card>
  );
}

function clipLength(r: Recording): string {
  const s = Math.round((Date.parse(r.end) - Date.parse(r.start)) / 1000);
  return s > 0 ? `${s} s` : "";
}

function RecordingsCard({ camera }: { camera: Camera }) {
  const { data: recordings, isLoading, error } = useRecordings(camera.id);
  const t = useT();
  return (
    <Card>
      {error && <Notice tone="error">{errorMessage(error)}</Notice>}
      {isLoading ? (
        <Empty title={t("Loading recordings…")} />
      ) : !recordings?.length ? (
        <Empty icon={<HardDrive />} title={t("No recordings")}>
          {t("Events that record leave their files here while the camera runs.")}
        </Empty>
      ) : (
        <Table>
          <THead>
            <tr>
              <TH>{t("Recorded")}</TH>
              <TH>{t("Event")}</TH>
              <TH>{t("Kind")}</TH>
              <TH>{t("Length")}</TH>
              <TH>{t("Size")}</TH>
              <TH>{t("File")}</TH>
              <TH className="text-right">{t("Actions")}</TH>
            </tr>
          </THead>
          <TBody>
            {recordings.map((r) => (
              <TR key={r.id}>
                <TD>
                  <Mono>{formatTime(r.start)}</Mono>
                </TD>
                <TD>
                  <Mono>{r.event_type}</Mono>
                </TD>
                <TD>
                  <Badge tone={r.kind === "clip" ? "info" : "muted"}>{r.kind === "clip" ? t("Clip") : t("Snapshot")}</Badge>
                </TD>
                <TD>
                  <Mono>{clipLength(r)}</Mono>
                </TD>
                <TD>
                  <Mono>{formatBytes(r.size)}</Mono>
                </TD>
                <TD className="max-w-80">
                  <Mono className="block truncate text-muted">{r.name}</Mono>
                </TD>
                <TD className="text-right">
                  <a
                    href={recordingURL(camera.id, r.id)}
                    download
                    className="inline-flex items-center gap-1 text-[13px] text-muted hover:text-text [&_svg]:size-3.5"
                  >
                    <Download /> {t("Download")}
                  </a>
                </TD>
              </TR>
            ))}
          </TBody>
        </Table>
      )}
    </Card>
  );
}
