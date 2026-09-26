import { FlaskConical, Plus, Send, Trash2 } from "lucide-react";
import { type FormEvent, useState } from "react";
import { ApiError, errorMessage, type Target } from "@/api/client";
import { useCreateTarget, useDeleteTarget, useTargets, useTestTarget, useUpdateTarget } from "@/api/queries";
import { Mono } from "@/components/badges";
import { toast } from "@/components/toast";
import { Button } from "@/components/ui/button";
import { Card, Empty, Notice, PageHeader } from "@/components/ui/card";
import { Dialog } from "@/components/ui/dialog";
import { Checkbox, Field, Input, Select, Textarea } from "@/components/ui/form";
import { Table, TBody, TD, TH, THead, TR } from "@/components/ui/table";
import { useT } from "@/lib/i18n";

export function TargetsPage() {
  const { data: targets, isLoading, error } = useTargets();
  const [creating, setCreating] = useState(false);
  const t = useT();

  return (
    <>
      <PageHeader
        title={t("Targets")}
        description={t("Receivers of the camera events: a VMS, an NVR or any HTTP endpoint. Link them to cameras when creating them.")}
        actions={
          <Button variant="primary" onClick={() => setCreating(true)}>
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
            {t("Add the URL where the cameras should send their events.")}
          </Empty>
        ) : (
          <Table>
            <THead>
              <tr>
                <TH>{t("Name")}</TH>
                <TH>{t("Request")}</TH>
                <TH>{t("Auth")}</TH>
                <TH>{t("Enabled")}</TH>
                <TH className="text-right">{t("Cameras")}</TH>
                <TH className="text-right">{t("Actions")}</TH>
              </tr>
            </THead>
            <TBody>
              {targets.map((t) => (
                <TargetRow key={t.id} target={t} />
              ))}
            </TBody>
          </Table>
        )}
      </Card>
      <NewTargetDialog open={creating} onClose={() => setCreating(false)} />
    </>
  );
}

function TargetRow({ target }: { target: Target }) {
  const test = useTestTarget();
  const update = useUpdateTarget();
  const del = useDeleteTarget();
  const t = useT();

  const runTest = () =>
    test.mutate(target.id, {
      onSuccess: (r) => {
        const from = r.from === "camera" ? t("from camera {camera}", { camera: r.camera ?? "" }) : t("from the node");
        return r.ok
          ? toast(t("{name}: HTTP {status} in {ms} ms, {from}", { name: target.name, status: r.http_status ?? 0, ms: r.latency_ms, from }), "ok")
          : toast(t("{name}: {error} ({ms} ms, {from})", { name: target.name, error: r.error ?? t("failed"), ms: r.latency_ms, from }), "error");
      },
      onError: (err) => toast(errorMessage(err), "error"),
    });

  return (
    <TR>
      <TD className="font-medium">{target.name}</TD>
      <TD>
        <Mono>
          {target.method} {target.url}
        </Mono>
      </TD>
      <TD className="text-muted">{target.username ? t("Basic ({user})", { user: target.username }) : t("none")}</TD>
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
          <Button size="sm" variant="secondary" onClick={runTest} disabled={test.isPending} title={t("Send a test request from a running camera that uses the target, or else from the node")}>
            <FlaskConical /> {test.isPending ? t("Testing…") : t("Test")}
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
function parseHeaders(text: string): Record<string, string> | undefined {
  const out: Record<string, string> = {};
  for (const line of text.split("\n")) {
    const i = line.indexOf(":");
    if (i <= 0) continue;
    out[line.slice(0, i).trim()] = line.slice(i + 1).trim();
  }
  return Object.keys(out).length ? out : undefined;
}

function NewTargetDialog({ open, onClose }: { open: boolean; onClose: () => void }) {
  const create = useCreateTarget();
  const t = useT();
  const [name, setName] = useState("");
  const [url, setUrl] = useState("");
  const [method, setMethod] = useState<"POST" | "PUT" | "GET">("POST");
  const [username, setUsername] = useState("");
  const [password, setPassword] = useState("");
  const [headers, setHeaders] = useState("");
  const [fieldErrors, setFieldErrors] = useState<Record<string, string>>({});

  const submit = (e: FormEvent) => {
    e.preventDefault();
    setFieldErrors({});
    create.mutate(
      {
        name: name.trim(),
        type: "http",
        url: url.trim(),
        method,
        username: username.trim() || undefined,
        password: password || undefined,
        headers: parseHeaders(headers),
      },
      {
        onSuccess: (created) => {
          toast(t("Target {name} created", { name: created.name }), "ok");
          setName("");
          setUrl("");
          setUsername("");
          setPassword("");
          setHeaders("");
          onClose();
        },
        onError: (err) => {
          if (err instanceof ApiError) setFieldErrors(err.fieldErrors());
        },
      },
    );
  };

  return (
    <Dialog
      open={open}
      onClose={onClose}
      title={t("New target")}
      description={t("Cameras deliver their events here with the payload of their profile.")}
      footer={
        <>
          <Button onClick={onClose}>{t("Cancel")}</Button>
          <Button variant="primary" type="submit" form="new-target" disabled={create.isPending || !name.trim() || !url.trim()}>
            {create.isPending ? t("Creating…") : t("Create target")}
          </Button>
        </>
      }
    >
      <form id="new-target" onSubmit={submit} className="grid grid-cols-2 gap-x-4 gap-y-3">
        <Field label={t("Name")} error={fieldErrors["name"]} className="col-span-2">
          <Input value={name} onChange={(e) => setName(e.target.value)} placeholder="VMS lab" required autoFocus />
        </Field>
        <div className="col-span-2 grid grid-cols-[110px_minmax(0,1fr)] gap-x-4">
          <Field label={t("Method")} error={fieldErrors["method"]}>
            <Select value={method} onChange={(e) => setMethod(e.target.value as typeof method)}>
              <option>POST</option>
              <option>PUT</option>
              <option>GET</option>
            </Select>
          </Field>
          <Field label={t("URL")} error={fieldErrors["url"]}>
            <Input
              value={url}
              onChange={(e) => setUrl(e.target.value)}
              placeholder="http://192.168.1.10:8000/events"
              required
              className="font-mono"
            />
          </Field>
        </div>
        <Field label={t("Username")}>
          <Input value={username} onChange={(e) => setUsername(e.target.value)} autoComplete="off" />
        </Field>
        <Field label={t("Password")} hint={t("Stored encrypted; Basic authentication.")}>
          <Input type="password" value={password} onChange={(e) => setPassword(e.target.value)} autoComplete="new-password" />
        </Field>
        <Field label={t("Headers")} error={fieldErrors["headers"]} hint={t("One per line: Name: value")} className="col-span-2">
          <Textarea value={headers} onChange={(e) => setHeaders(e.target.value)} className="font-mono" rows={3} />
        </Field>
        {create.error && (
          <div className="col-span-2">
            <Notice tone="error">{errorMessage(create.error)}</Notice>
          </div>
        )}
      </form>
    </Dialog>
  );
}
