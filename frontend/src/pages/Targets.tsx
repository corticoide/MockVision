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

export function TargetsPage() {
  const { data: targets, isLoading, error } = useTargets();
  const [creating, setCreating] = useState(false);

  return (
    <>
      <PageHeader
        title="Targets"
        description="Receivers of the camera events: a VMS, an NVR or any HTTP endpoint. Link them to cameras when creating them."
        actions={
          <Button variant="primary" onClick={() => setCreating(true)}>
            <Plus /> New target
          </Button>
        }
      />
      {error && <Notice tone="error">{errorMessage(error)}</Notice>}
      <Card>
        {isLoading ? (
          <Empty title="Loading targets…" />
        ) : !targets?.length ? (
          <Empty icon={<Send />} title="No targets">
            Add the URL where the cameras should send their events.
          </Empty>
        ) : (
          <Table>
            <THead>
              <tr>
                <TH>Name</TH>
                <TH>Request</TH>
                <TH>Auth</TH>
                <TH>Enabled</TH>
                <TH className="text-right">Cameras</TH>
                <TH className="text-right">Actions</TH>
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

function TargetRow({ target: t }: { target: Target }) {
  const test = useTestTarget();
  const update = useUpdateTarget();
  const del = useDeleteTarget();

  const runTest = () =>
    test.mutate(t.id, {
      onSuccess: (r) =>
        r.ok
          ? toast(`${t.name}: HTTP ${r.http_status} in ${r.latency_ms} ms`, "ok")
          : toast(`${t.name}: ${r.error ?? "failed"} (${r.latency_ms} ms)`, "error"),
      onError: (err) => toast(errorMessage(err), "error"),
    });

  return (
    <TR>
      <TD className="font-medium">{t.name}</TD>
      <TD>
        <Mono>
          {t.method} {t.url}
        </Mono>
      </TD>
      <TD className="text-muted">{t.username ? `Basic (${t.username})` : "none"}</TD>
      <TD>
        <Checkbox
          label={t.enabled ? "Yes" : "No"}
          checked={t.enabled}
          disabled={update.isPending}
          onChange={(e) =>
            update.mutate({ id: t.id, body: { enabled: e.target.checked } }, { onError: (err) => toast(errorMessage(err), "error") })
          }
        />
      </TD>
      <TD className="text-right">
        <Mono>{t.camera_count}</Mono>
      </TD>
      <TD>
        <div className="flex items-center justify-end gap-1">
          <Button size="sm" variant="secondary" onClick={runTest} disabled={test.isPending} title="Send a test request from the node">
            <FlaskConical /> {test.isPending ? "Testing…" : "Test"}
          </Button>
          <Button
            size="icon"
            variant="ghost"
            title={t.camera_count > 0 ? "In use by cameras" : "Delete"}
            aria-label="Delete"
            disabled={del.isPending || t.camera_count > 0}
            onClick={() => {
              if (confirm(`Delete target ${t.name}?`)) del.mutate(t.id, { onError: (err) => toast(errorMessage(err), "error") });
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
        onSuccess: (t) => {
          toast(`Target ${t.name} created`, "ok");
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
      title="New target"
      description="Cameras deliver their events here with the payload of their profile."
      footer={
        <>
          <Button onClick={onClose}>Cancel</Button>
          <Button variant="primary" type="submit" form="new-target" disabled={create.isPending || !name.trim() || !url.trim()}>
            {create.isPending ? "Creating…" : "Create target"}
          </Button>
        </>
      }
    >
      <form id="new-target" onSubmit={submit} className="grid grid-cols-2 gap-x-4 gap-y-3">
        <Field label="Name" error={fieldErrors["name"]} className="col-span-2">
          <Input value={name} onChange={(e) => setName(e.target.value)} placeholder="VMS lab" required autoFocus />
        </Field>
        <div className="col-span-2 grid grid-cols-[110px_minmax(0,1fr)] gap-x-4">
          <Field label="Method" error={fieldErrors["method"]}>
            <Select value={method} onChange={(e) => setMethod(e.target.value as typeof method)}>
              <option>POST</option>
              <option>PUT</option>
              <option>GET</option>
            </Select>
          </Field>
          <Field label="URL" error={fieldErrors["url"]}>
            <Input
              value={url}
              onChange={(e) => setUrl(e.target.value)}
              placeholder="http://192.168.1.10:8000/events"
              required
              className="font-mono"
            />
          </Field>
        </div>
        <Field label="Username">
          <Input value={username} onChange={(e) => setUsername(e.target.value)} autoComplete="off" />
        </Field>
        <Field label="Password" hint="Stored encrypted; Basic authentication.">
          <Input type="password" value={password} onChange={(e) => setPassword(e.target.value)} autoComplete="new-password" />
        </Field>
        <Field label="Headers" error={fieldErrors["headers"]} hint="One per line: Name: value" className="col-span-2">
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
