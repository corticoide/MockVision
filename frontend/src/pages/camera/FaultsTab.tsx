import { Bug, CircleOff, Power } from "lucide-react";
import { type FormEvent, useEffect, useState } from "react";
import { ApiError, type Camera, errorMessage, type Fault, type FaultInput, type FaultKind } from "@/api/client";
import { useCameraFaults, useEndFault, useInjectFault, useRebootCamera } from "@/api/queries";
import { Badge, Mono } from "@/components/badges";
import { toast } from "@/components/toast";
import { Button } from "@/components/ui/button";
import { Card, Empty, Notice } from "@/components/ui/card";
import { Field, Input, Select } from "@/components/ui/form";
import { Table, TBody, TD, TH, THead, TR } from "@/components/ui/table";
import { type Translate, useT } from "@/lib/i18n";
import { formatTime } from "@/lib/utils";
import { isRunning, SectionTitle } from "./parts";

/** What each kind of fault does, as the form explains it. */
export const faultKinds: Record<FaultKind, { label: string; hint: string }> = {
  service_down: { label: "Service down", hint: "The protocol resets its connections and refuses new ones, as a service that crashed." },
  latency: { label: "Latency", hint: "Everything the protocol reads waits this long: every answer comes late." },
  error_status: { label: "Error status", hint: "Every request gets this status; 401 challenges, as a device that refuses the credentials." },
  clock_skew: { label: "Clock skew", hint: "The camera's clock moves: its events, answers and templates carry the moved time." },
  network_down: {
    label: "Network down",
    hint: "The camera answers nobody, not even ARP, and reaches nobody; it raises network_lost, delivered once it is back if its retries last.",
  },
  ip_conflict: { label: "IP conflict", hint: "The camera sees another device with its address: it raises ip_conflict and keeps answering." },
};

type FaultStatus = NonNullable<FaultInput["status"]>;
const statuses: FaultStatus[] = [401, 403, 404, 500, 503];

const kindOrder: FaultKind[] = ["service_down", "latency", "error_status", "clock_skew", "network_down", "ip_conflict"];

/** How long a fault lasts; 0 lasts until ended (RN-14). */
const durations = [
  { s: 30, label: "30 seconds" },
  { s: 60, label: "1 minute" },
  { s: 300, label: "5 minutes" },
  { s: 900, label: "15 minutes" },
  { s: 3600, label: "1 hour" },
  { s: 0, label: "Until ended" },
];

/** A fault's parameters in a few words. */
export function faultDetail(f: Fault, t: Translate): string {
  switch (f.kind) {
    case "service_down":
      return f.instance ?? "";
    case "latency":
      return t("{instance} +{ms} ms", { instance: f.instance ?? "", ms: f.latency_ms ?? 0 });
    case "error_status":
      return t("{instance} answers {status}", { instance: f.instance ?? "", status: f.status ?? 0 });
    case "clock_skew":
      return t("{s} s", { s: (f.skew_s ?? 0) > 0 ? `+${f.skew_s}` : String(f.skew_s ?? 0) });
    default:
      return "";
  }
}

/** Seconds as m:ss or h:mm:ss. */
function clock(seconds: number): string {
  const s = Math.max(0, Math.round(seconds));
  const h = Math.floor(s / 3600);
  const m = Math.floor((s % 3600) / 60);
  const pad = (n: number) => String(n).padStart(2, "0");
  return h > 0 ? `${h}:${pad(m)}:${pad(s % 60)}` : `${m}:${pad(s % 60)}`;
}

function useNow(ms: number): number {
  const [now, setNow] = useState(() => Date.now());
  useEffect(() => {
    const timer = setInterval(() => setNow(Date.now()), ms);
    return () => clearInterval(timer);
  }, [ms]);
  return now;
}

export function FaultsTab({ camera }: { camera: Camera }) {
  const { data: faults, isLoading, error } = useCameraFaults(camera.id);
  const t = useT();
  const now = useNow(1000);
  const active = faults?.filter((f) => f.active) ?? [];
  const ended = faults?.filter((f) => !f.active) ?? [];

  return (
    <div className="flex flex-col gap-4">
      <Card className="p-4">
        <SectionTitle>{t("Inject a fault")}</SectionTitle>
        <p className="mb-3 text-[13px] text-muted">
          {t("Test how a client handles a camera that fails. Every fault ends on its own or by hand; while one is on, the camera shows as degraded.")}
        </p>
        <InjectForm camera={camera} />
      </Card>
      <Card>
        {error && <Notice tone="error">{errorMessage(error)}</Notice>}
        {isLoading ? (
          <Empty title={t("Loading faults…")} />
        ) : !faults?.length ? (
          <Empty icon={<Bug />} title={t("No faults")}>
            {t("The camera works as its profile describes.")}
          </Empty>
        ) : (
          <Table>
            <THead>
              <tr>
                <TH>{t("Fault")}</TH>
                <TH>{t("Detail")}</TH>
                <TH>{t("Started")}</TH>
                <TH>{t("Ends")}</TH>
                <TH>{t("By")}</TH>
                <TH className="text-right">{t("Actions")}</TH>
              </tr>
            </THead>
            <TBody>
              {[...active, ...ended].map((f) => (
                <FaultRow key={f.id} camera={camera} fault={f} now={now} />
              ))}
            </TBody>
          </Table>
        )}
      </Card>
      <RebootCard camera={camera} />
    </div>
  );
}

function FaultRow({ camera, fault: f, now }: { camera: Camera; fault: Fault; now: number }) {
  const end = useEndFault();
  const t = useT();
  let ends: string;
  if (f.active) {
    ends = f.expires_at ? t("in {time}", { time: clock((Date.parse(f.expires_at) - now) / 1000) }) : t("when ended");
  } else {
    const by = f.ended_by === "expired" ? t("its time was up") : f.ended_by === "replaced" ? t("replaced") : t("ended by {user}", { user: f.ended_by ?? "" });
    ends = `${formatTime(f.ended_at)} · ${by}`;
  }
  return (
    <TR className={f.active ? "" : "text-muted"}>
      <TD>
        <span className="inline-flex items-center gap-2">
          <Badge tone={f.active ? "warn" : "muted"}>{f.active ? t("Active") : t("Ended")}</Badge>
          {t(faultKinds[f.kind].label)}
        </span>
      </TD>
      <TD>
        <Mono>{faultDetail(f, t)}</Mono>
      </TD>
      <TD>
        <Mono>{formatTime(f.started_at)}</Mono>
      </TD>
      <TD>{f.active ? <Mono>{ends}</Mono> : ends}</TD>
      <TD className="text-muted">{f.created_by}</TD>
      <TD className="text-right">
        {f.active && (
          <Button
            size="sm"
            disabled={end.isPending}
            onClick={() => end.mutate({ id: camera.id, fault: f.id }, { onError: (err) => toast(errorMessage(err), "error") })}
          >
            <CircleOff /> {t("End")}
          </Button>
        )}
      </TD>
    </TR>
  );
}

function InjectForm({ camera }: { camera: Camera }) {
  const inject = useInjectFault();
  const t = useT();
  const servers = camera.protocols.filter((p) => p.role === "server" && p.enabled);
  const [kind, setKind] = useState<FaultKind>("service_down");
  const [instance, setInstance] = useState(servers[0]?.instance ?? "");
  const [status, setStatus] = useState<FaultStatus>(500);
  const [latency, setLatency] = useState("1000");
  const [skew, setSkew] = useState("3600");
  const [duration, setDuration] = useState(300);
  const [errors, setErrors] = useState<Record<string, string>>({});

  const needsInstance = kind === "service_down" || kind === "latency" || kind === "error_status";
  const choices = kind === "error_status" ? servers.filter((p) => p.engine === "http-api" || p.engine === "rtsp") : servers;
  const chosen = choices.some((p) => p.instance === instance) ? instance : (choices[0]?.instance ?? "");

  const submit = (e: FormEvent) => {
    e.preventDefault();
    setErrors({});
    inject.mutate(
      {
        id: camera.id,
        body: {
          kind,
          instance: needsInstance ? chosen : undefined,
          status: kind === "error_status" ? status : undefined,
          latency_ms: kind === "latency" ? Number(latency) : undefined,
          skew_s: kind === "clock_skew" ? Number(skew) : undefined,
          duration_s: duration,
        },
      },
      {
        onSuccess: () => toast(t("Fault {kind} on", { kind: t(faultKinds[kind].label) }), "ok"),
        onError: (err) => {
          if (err instanceof ApiError) setErrors(err.fieldErrors());
          toast(errorMessage(err), "error");
        },
      },
    );
  };

  return (
    <form onSubmit={submit} className="grid grid-cols-4 items-end gap-x-4 gap-y-3">
      <Field label={t("Fault")} error={errors["kind"]}>
        <Select value={kind} onChange={(e) => setKind(e.target.value as FaultKind)}>
          {kindOrder.map((k) => (
            <option key={k} value={k}>
              {t(faultKinds[k].label)}
            </option>
          ))}
        </Select>
      </Field>
      {needsInstance ? (
        <Field label={t("Protocol")} error={errors["instance"]}>
          <Select value={chosen} onChange={(e) => setInstance(e.target.value)} disabled={choices.length === 0}>
            {choices.map((p) => (
              <option key={p.instance} value={p.instance}>
                {p.instance} ({p.engine})
              </option>
            ))}
          </Select>
        </Field>
      ) : (
        <div />
      )}
      {kind === "error_status" ? (
        <Field label={t("Status")} error={errors["status"]}>
          <Select value={status} onChange={(e) => setStatus(Number(e.target.value) as FaultStatus)}>
            {statuses.map((s) => (
              <option key={s} value={s}>
                {s}
              </option>
            ))}
          </Select>
        </Field>
      ) : kind === "latency" ? (
        <Field label={t("Delay (ms)")} error={errors["latency_ms"]}>
          <Input value={latency} onChange={(e) => setLatency(e.target.value)} inputMode="numeric" />
        </Field>
      ) : kind === "clock_skew" ? (
        <Field label={t("Skew (s)")} error={errors["skew_s"]} hint={t("Negative: the clock is behind.")}>
          <Input value={skew} onChange={(e) => setSkew(e.target.value)} inputMode="numeric" />
        </Field>
      ) : (
        <div />
      )}
      <Field label={t("Duration")} error={errors["duration_s"]}>
        <Select value={duration} onChange={(e) => setDuration(Number(e.target.value))}>
          {durations.map((d) => (
            <option key={d.s} value={d.s}>
              {t(d.label)}
            </option>
          ))}
        </Select>
      </Field>
      <p className="col-span-3 text-xs text-muted">{t(faultKinds[kind].hint)}</p>
      <div className="flex justify-end">
        <Button variant="primary" type="submit" disabled={inject.isPending || (needsInstance && !chosen)}>
          <Bug /> {inject.isPending ? t("Injecting…") : t("Inject")}
        </Button>
      </div>
    </form>
  );
}

function RebootCard({ camera }: { camera: Camera }) {
  const reboot = useRebootCamera();
  const t = useT();
  const [seconds, setSeconds] = useState("");
  const running = isRunning(camera);
  const go = () => {
    const s = seconds.trim() === "" ? undefined : Number(seconds);
    if (!confirm(t("Reboot {name}? It leaves the network until it boots again.", { name: camera.name }))) return;
    reboot.mutate({ id: camera.id, seconds: s }, { onError: (err) => toast(errorMessage(err), "error") });
  };
  return (
    <Card className="p-4">
      <SectionTitle>{t("Reboot")}</SectionTitle>
      <div className="flex items-end gap-4">
        <p className="flex-1 text-[13px] text-muted">
          {t("As the real device: the camera leaves the network and comes back after its boot time, with the faults still on. Empty: the profile's boot time.")}
        </p>
        <Field label={t("Boot time (s)")} className="w-40">
          <Input value={seconds} onChange={(e) => setSeconds(e.target.value)} inputMode="numeric" placeholder={t("profile's")} />
        </Field>
        <Button onClick={go} disabled={!running || reboot.isPending}>
          <Power /> {t("Reboot")}
        </Button>
      </div>
    </Card>
  );
}
