import { FlaskConical, Pencil, Plus, Send, Trash2 } from "lucide-react";
import { type FormEvent, useState } from "react";
import { ApiError, errorMessage, type Target, type TargetInput } from "@/api/client";
import { useCreateTarget, useDeleteTarget, useTargets, useTestTarget, useUpdateTarget } from "@/api/queries";
import { Badge, Mono } from "@/components/badges";
import { toast } from "@/components/toast";
import { Button } from "@/components/ui/button";
import { Card, Empty, Notice, PageHeader } from "@/components/ui/card";
import { Dialog } from "@/components/ui/dialog";
import { Checkbox, Field, Input, Select, Textarea } from "@/components/ui/form";
import { Table, TBody, TD, TH, THead, TR } from "@/components/ui/table";
import { type Translate, useT } from "@/lib/i18n";

type TargetType = NonNullable<TargetInput["type"]>;

/** What each type of target is, as the dialog explains it. */
const kinds: Record<TargetType, { label: string; placeholder: string; hint: string }> = {
  http: { label: "HTTP", placeholder: "http://192.168.1.10:8000/events", hint: "A request per event with the payload of the profile, as an HTTP notification." },
  mqtt: { label: "MQTT", placeholder: "mqtt://192.168.1.10:1883", hint: "A broker: the camera connects as it starts and publishes each event to the topic of its profile." },
  ftp: {
    label: "FTP",
    placeholder: "ftp://192.168.1.10/cameras",
    hint: "The snapshot of each event, in passive mode. The path is relative to the login directory; %2F starts it at the root.",
  },
  sftp: { label: "SFTP", placeholder: "sftp://192.168.1.10/srv/cameras", hint: "The snapshot of each event over SSH. The path is absolute; /~/ starts it at the home directory." },
  smtp: { label: "E-mail", placeholder: "smtp://192.168.1.10:25", hint: "A mail per event with the snapshot attached, at most one per interval of the profile." },
};

const order: TargetType[] = ["http", "mqtt", "ftp", "sftp", "smtp"];

export function TargetsPage() {
  const { data: targets, isLoading, error } = useTargets();
  const [editing, setEditing] = useState<Target | "new" | null>(null);
  const t = useT();

  return (
    <>
      <PageHeader
        title={t("Targets")}
        description={t("Receivers of the camera events: a VMS or any HTTP endpoint, an MQTT broker, FTP and SFTP servers, a mail server. Link them to cameras when creating them.")}
        actions={
          <Button variant="primary" onClick={() => setEditing("new")}>
            <Plus /> {t("New target")}
          </Button>
        }
      />
      {error && <Notice tone="error">{errorMessage(error)}</Notice>}
      <Card>
        {isLoading ? (
          <Empty title={t("Loading targets…")} />
        ) : !targets?.length ? (
          <Empty icon={<Send />} title={t("No targets")}>
            {t("Add where the cameras should send their events.")}
          </Empty>
        ) : (
          <Table>
            <THead>
              <tr>
                <TH>{t("Name")}</TH>
                <TH>{t("Type")}</TH>
                <TH>{t("Destination")}</TH>
                <TH>{t("Auth")}</TH>
                <TH>{t("Enabled")}</TH>
                <TH className="text-right">{t("Cameras")}</TH>
                <TH className="text-right">{t("Actions")}</TH>
              </tr>
            </THead>
            <TBody>
              {targets.map((tg) => (
                <TargetRow key={tg.id} target={tg} onEdit={() => setEditing(tg)} />
              ))}
            </TBody>
          </Table>
        )}
      </Card>
      {editing && <TargetDialog target={editing === "new" ? undefined : editing} onClose={() => setEditing(null)} />}
    </>
  );
}

/** The credentials a target uses, in a word. */
function authLabel(target: Target, t: Translate): string {
  switch (target.type) {
    case "http":
      if (!target.username) return t("none");
      return target.auth === "digest" ? t("Digest ({user})", { user: target.username }) : t("Basic ({user})", { user: target.username });
    case "sftp":
      return target.host_key ? t("{user}, pinned key", { user: target.username || "—" }) : target.username || t("none");
    default:
      return target.username || t("none");
  }
}

function destination(target: Target): string {
  switch (target.type) {
    case "http":
      return `${target.method} ${target.url}`;
    case "smtp":
      return `${target.url} → ${target.to.join(", ")}`;
    case "mqtt":
      return target.topic ? `${target.url} · ${target.topic}` : target.url;
    default:
      return target.url;
  }
}

function TargetRow({ target, onEdit }: { target: Target; onEdit: () => void }) {
  const test = useTestTarget();
  const update = useUpdateTarget();
  const del = useDeleteTarget();
  const t = useT();

  const runTest = () =>
    test.mutate(target.id, {
      onSuccess: (r) => {
        const from = r.from === "camera" ? t("from camera {camera}", { camera: r.camera ?? "" }) : t("from the node");
        if (!r.ok) return toast(t("{name}: {error} ({ms} ms, {from})", { name: target.name, error: r.error ?? t("failed"), ms: r.latency_ms, from }), "error");
        return r.http_status
          ? toast(t("{name}: HTTP {status} in {ms} ms, {from}", { name: target.name, status: r.http_status, ms: r.latency_ms, from }), "ok")
          : toast(t("{name}: connected in {ms} ms, {from}", { name: target.name, ms: r.latency_ms, from }), "ok");
      },
      onError: (err) => toast(errorMessage(err), "error"),
    });

  return (
    <TR>
      <TD className="font-medium">{target.name}</TD>
      <TD>
        <Badge tone="info">{kinds[target.type].label}</Badge>
      </TD>
      <TD className="max-w-[360px] truncate" title={destination(target)}>
        <Mono>{destination(target)}</Mono>
      </TD>
      <TD className="text-muted">{authLabel(target, t)}</TD>
      <TD>
        <Checkbox
          label={target.enabled ? t("Yes") : t("No")}
          checked={target.enabled}
          disabled={update.isPending}
          onChange={(e) =>
            update.mutate({ id: target.id, body: { enabled: e.target.checked } }, { onError: (err) => toast(errorMessage(err), "error") })
          }
        />
      </TD>
      <TD className="text-right">
        <Mono>{target.camera_count}</Mono>
      </TD>
      <TD>
        <div className="flex items-center justify-end gap-1">
          <Button
            size="sm"
            variant="secondary"
            onClick={runTest}
            disabled={test.isPending}
            title={t("Test the target from a running camera that uses it, or else from the node; nothing is uploaded or mailed")}
          >
            <FlaskConical /> {test.isPending ? t("Testing…") : t("Test")}
          </Button>
          <Button size="icon" variant="ghost" title={t("Edit")} aria-label={t("Edit")} onClick={onEdit}>
            <Pencil />
          </Button>
          <Button
            size="icon"
            variant="ghost"
            title={target.camera_count > 0 ? t("In use by cameras") : t("Delete")}
            aria-label={t("Delete")}
            disabled={del.isPending || target.camera_count > 0}
            onClick={() => {
              if (confirm(t("Delete target {name}?", { name: target.name }))) del.mutate(target.id, { onError: (err) => toast(errorMessage(err), "error") });
            }}
          >
            <Trash2 />
          </Button>
        </div>
      </TD>
    </TR>
  );
}

/** Parses "Name: value" lines into a header map. */
function parseHeaders(text: string): Record<string, string> {
  const out: Record<string, string> = {};
  for (const line of text.split("\n")) {
    const i = line.indexOf(":");
    if (i <= 0) continue;
    out[line.slice(0, i).trim()] = line.slice(i + 1).trim();
  }
  return out;
}

function formatHeaders(h: Record<string, string> | undefined): string {
  return Object.entries(h ?? {})
    .map(([k, v]) => `${k}: ${v}`)
    .join("\n");
}

/** A number field of the delivery override: empty keeps the profile's. */
function optionalNumber(text: string, scale = 1): number | undefined {
  const s = text.trim();
  if (s === "") return undefined;
  const n = Number(s.replace(",", "."));
  return Number.isFinite(n) ? Math.round(n * scale) : undefined;
}

function seconds(ms: number | undefined): string {
  return ms === undefined ? "" : String(ms / 1000);
}

function TargetDialog({ target, onClose }: { target?: Target; onClose: () => void }) {
  const create = useCreateTarget();
  const update = useUpdateTarget();
  const t = useT();
  const editing = target !== undefined;
  const [type, setType] = useState<TargetType>(target?.type ?? "http");
  const [name, setName] = useState(target?.name ?? "");
  const [url, setUrl] = useState(target?.url ?? "");
  const [method, setMethod] = useState(target?.method || "POST");
  const [auth, setAuth] = useState<"basic" | "digest">(target?.auth ?? "basic");
  const [username, setUsername] = useState(target?.username ?? "");
  const [password, setPassword] = useState("");
  const [headers, setHeaders] = useState(formatHeaders(target?.headers));
  const [topic, setTopic] = useState(target?.topic ?? "");
  const [clientID, setClientID] = useState(target?.client_id ?? "");
  const [hostKey, setHostKey] = useState(target?.host_key ?? "");
  const [tls, setTLS] = useState<"none" | "starttls" | "tls">(target?.tls ?? "none");
  const [from, setFrom] = useState(target?.from ?? "");
  const [to, setTo] = useState((target?.to ?? []).join(", "));
  const [insecure, setInsecure] = useState(target?.insecure ?? false);
  const [timeout, setTimeoutS] = useState(seconds(target?.delivery?.timeout_ms));
  const [retries, setRetries] = useState(target?.delivery?.retries === undefined ? "" : String(target.delivery.retries));
  const [backoff, setBackoff] = useState(seconds(target?.delivery?.backoff_ms));
  const [fieldErrors, setFieldErrors] = useState<Record<string, string>>({});
  const pending = create.isPending || update.isPending;
  const error = create.error ?? update.error;

  const submit = (e: FormEvent) => {
    e.preventDefault();
    setFieldErrors({});
    const body: TargetInput = {
      name: name.trim(),
      url: url.trim(),
      username: username.trim(),
      delivery: { timeout_ms: optionalNumber(timeout, 1000), retries: optionalNumber(retries), backoff_ms: optionalNumber(backoff, 1000) },
    };
    if (!editing) body.type = type;
    if (password) body.password = password;
    switch (type) {
      case "http":
        Object.assign(body, { method, auth, headers: parseHeaders(headers) });
        break;
      case "mqtt":
        Object.assign(body, { topic: topic.trim(), client_id: clientID.trim(), insecure });
        break;
      case "sftp":
        body.host_key = hostKey.trim();
        break;
      case "smtp":
        Object.assign(body, { tls, from: from.trim(), to: to.split(/[,;\s]+/).filter(Boolean), insecure });
        break;
    }
    const done = {
      onSuccess: (saved: Target) => {
        toast(editing ? t("Target {name} saved", { name: saved.name }) : t("Target {name} created", { name: saved.name }), "ok");
        onClose();
      },
      onError: (err: Error) => {
        if (err instanceof ApiError) setFieldErrors(err.fieldErrors());
      },
    };
    if (editing) update.mutate({ id: target.id, body }, done);
    else create.mutate(body, done);
  };

  const kind = kinds[type];
  return (
    <Dialog
      open
      onClose={onClose}
      title={editing ? t("Edit target {name}", { name: target.name }) : t("New target")}
      description={t("Cameras deliver their events here with the payload of their profile.")}
      footer={
        <>
          <Button onClick={onClose}>{t("Cancel")}</Button>
          <Button variant="primary" type="submit" form="target-form" disabled={pending || !name.trim() || !url.trim()}>
            {pending ? t("Saving…") : editing ? t("Save") : t("Create target")}
          </Button>
        </>
      }
    >
      <form id="target-form" onSubmit={submit} className="grid grid-cols-2 gap-x-4 gap-y-3">
        <Field label={t("Name")} error={fieldErrors["name"]}>
          <Input value={name} onChange={(e) => setName(e.target.value)} placeholder="VMS lab" required autoFocus />
        </Field>
        <Field label={t("Type")} error={fieldErrors["type"]} hint={editing ? t("The type of a target does not change.") : undefined}>
          <Select value={type} disabled={editing} onChange={(e) => setType(e.target.value as TargetType)}>
            {order.map((k) => (
              <option key={k} value={k}>
                {t(kinds[k].label)}
              </option>
            ))}
          </Select>
        </Field>
        <p className="col-span-2 -mt-1 text-xs text-muted">{t(kind.hint)}</p>
        {type === "http" ? (
          <div className="col-span-2 grid grid-cols-[110px_minmax(0,1fr)] gap-x-4">
            <Field label={t("Method")} error={fieldErrors["method"]}>
              <Select value={method} onChange={(e) => setMethod(e.target.value)}>
                <option>POST</option>
                <option>PUT</option>
                <option>GET</option>
              </Select>
            </Field>
            <Field label={t("URL")} error={fieldErrors["url"]}>
              <Input value={url} onChange={(e) => setUrl(e.target.value)} placeholder={kind.placeholder} required className="font-mono" />
            </Field>
          </div>
        ) : (
          <Field label={t("URL")} error={fieldErrors["url"]} className="col-span-2">
            <Input value={url} onChange={(e) => setUrl(e.target.value)} placeholder={kind.placeholder} required className="font-mono" />
          </Field>
        )}
        <Field label={t("Username")} error={fieldErrors["username"]}>
          <Input value={username} onChange={(e) => setUsername(e.target.value)} autoComplete="off" />
        </Field>
        <Field
          label={t("Password")}
          hint={editing && target.has_password ? t("Stored encrypted; leave it empty to keep it.") : t("Stored encrypted.")}
        >
          <Input type="password" value={password} onChange={(e) => setPassword(e.target.value)} autoComplete="new-password" />
        </Field>
        {type === "http" && (
          <>
            <Field label={t("Authentication")} error={fieldErrors["auth"]} hint={t("Digest answers the target's challenge, as cameras do.")}>
              <Select value={auth} onChange={(e) => setAuth(e.target.value as typeof auth)}>
                <option value="basic">Basic</option>
                <option value="digest">Digest</option>
              </Select>
            </Field>
            <div />
            <Field label={t("Headers")} error={fieldErrors["headers"]} hint={t("One per line: Name: value")} className="col-span-2">
              <Textarea value={headers} onChange={(e) => setHeaders(e.target.value)} className="font-mono" rows={3} />
            </Field>
          </>
        )}
        {type === "mqtt" && (
          <>
            <Field label={t("Topic")} error={fieldErrors["topic"]} hint={t("Optional: replaces the profile's; a template such as cams/{{ .Camera.Serial }}.")}>
              <Input value={topic} onChange={(e) => setTopic(e.target.value)} className="font-mono" />
            </Field>
            <Field label={t("Client ID")} error={fieldErrors["client_id"]} hint={t("Optional template; each camera needs its own, the serial by default.")}>
              <Input value={clientID} onChange={(e) => setClientID(e.target.value)} className="font-mono" />
            </Field>
          </>
        )}
        {type === "sftp" && (
          <Field
            label={t("Host key")}
            error={fieldErrors["host_key"]}
            hint={t("Optional SHA256 fingerprint the server's key must have; empty accepts any, as most cameras do.")}
            className="col-span-2"
          >
            <Input value={hostKey} onChange={(e) => setHostKey(e.target.value)} placeholder="SHA256:…" className="font-mono" />
          </Field>
        )}
        {type === "smtp" && (
          <>
            <Field label={t("Security")} error={fieldErrors["tls"]}>
              <Select value={tls} onChange={(e) => setTLS(e.target.value as typeof tls)}>
                <option value="none">{t("None")}</option>
                <option value="starttls">STARTTLS</option>
                <option value="tls">{t("TLS (port 465)")}</option>
              </Select>
            </Field>
            <Field label={t("Sender")} error={fieldErrors["from"]}>
              <Input value={from} onChange={(e) => setFrom(e.target.value)} placeholder="cameras@example.com" type="email" />
            </Field>
            <Field label={t("Recipients")} error={fieldErrors["to"]} hint={t("Up to 5, separated by commas.")} className="col-span-2">
              <Input value={to} onChange={(e) => setTo(e.target.value)} placeholder="ops@example.com, guard@example.com" />
            </Field>
          </>
        )}
        {(type === "mqtt" || type === "smtp") && (
          <Checkbox
            className="col-span-2"
            label={t("Accept a TLS certificate that does not verify, as a test server's self-signed one")}
            checked={insecure}
            onChange={(e) => setInsecure(e.target.checked)}
          />
        )}
        <details className="col-span-2" open={Boolean(timeout || retries || backoff)}>
          <summary className="cursor-pointer text-xs font-medium text-muted">{t("Delivery (empty: as the profile says)")}</summary>
          <div className="mt-2 grid grid-cols-3 gap-x-4">
            <Field label={t("Timeout (s)")} error={fieldErrors["delivery.timeout_ms"]}>
              <Input value={timeout} onChange={(e) => setTimeoutS(e.target.value)} inputMode="decimal" />
            </Field>
            <Field label={t("Retries")} error={fieldErrors["delivery.retries"]}>
              <Input value={retries} onChange={(e) => setRetries(e.target.value)} inputMode="numeric" />
            </Field>
            <Field label={t("Pause between attempts (s)")} error={fieldErrors["delivery.backoff_ms"]}>
              <Input value={backoff} onChange={(e) => setBackoff(e.target.value)} inputMode="decimal" />
            </Field>
          </div>
        </details>
        {error && (
          <div className="col-span-2">
            <Notice tone="error">{errorMessage(error)}</Notice>
          </div>
        )}
      </form>
    </Dialog>
  );
}
