import { type FormEvent, type ReactNode, useEffect, useState } from "react";
import { ApiError, errorMessage, type Settings } from "@/api/client";
import { useNode, useNodeMetrics, useSettings, useUpdateSettings } from "@/api/queries";
import { Badge, Mono } from "@/components/badges";
import { toast } from "@/components/toast";
import { Button } from "@/components/ui/button";
import { Card, CardHeader, Notice, PageHeader } from "@/components/ui/card";
import { Field, Input, Select } from "@/components/ui/form";
import { useT } from "@/lib/i18n";
import { formatBytes, formatTime, sinceText } from "@/lib/utils";
import { TokensCard } from "./Tokens";

export function SettingsPage() {
  const t = useT();
  return (
    <>
      <PageHeader title={t("Settings")} description={t("Limits of this node, the network the cameras join and the API tokens for automation.")} />
      <div className="grid grid-cols-[minmax(0,1fr)_minmax(0,1fr)] items-start gap-4">
        <LimitsCard />
        <NodeCard />
        <div className="col-span-2">
          <TokensCard />
        </div>
      </div>
    </>
  );
}

function LimitsCard() {
  const { data: settings, error } = useSettings();
  const { data: node } = useNode();
  const update = useUpdateSettings();
  const t = useT();
  const [form, setForm] = useState<Settings | null>(null);
  const [fieldErrors, setFieldErrors] = useState<Record<string, string>>({});

  useEffect(() => {
    if (settings) setForm(settings);
  }, [settings]);

  if (error) return <Notice tone="error">{errorMessage(error)}</Notice>;
  if (!form) return <Card className="p-4 text-muted">{t("Loading…")}</Card>;

  const set = <K extends keyof Settings>(k: K, v: Settings[K]) => setForm({ ...form, [k]: v });
  const num = (k: "max_cameras" | "max_ram_percent" | "max_cpu_percent" | "events_retention_days" | "max_jobs" | "job_step_timeout_seconds") => ({
    type: "number",
    value: String(form[k]),
    onChange: (e: { target: { value: string } }) => set(k, Number(e.target.value)),
    className: "font-mono",
  });
  const interfaces = (node?.interfaces ?? []).filter((i) => !i.loopback);

  const submit = (e: FormEvent) => {
    e.preventDefault();
    setFieldErrors({});
    update.mutate(form, {
      onSuccess: () => toast(t("Settings saved"), "ok"),
      onError: (err) => {
        if (err instanceof ApiError) setFieldErrors(err.fieldErrors());
        toast(errorMessage(err), "error");
      },
    });
  };

  return (
    <Card>
      <CardHeader title={t("Limits")} description={t("Creating or starting a camera beyond them is rejected with the reason.")} />
      <form onSubmit={submit} className="grid grid-cols-2 gap-x-4 gap-y-3 p-4">
        <Field label={t("Maximum cameras")} error={fieldErrors["max_cameras"]}>
          <Input {...num("max_cameras")} min={1} max={1000} />
        </Field>
        <Field label={t("Event retention (days)")} error={fieldErrors["events_retention_days"]}>
          <Input {...num("events_retention_days")} min={1} max={365} />
        </Field>
        <Field label={t("Maximum RAM use (%)")} error={fieldErrors["max_ram_percent"]} hint={t("Of the node's memory, counting what the new camera needs.")}>
          <Input {...num("max_ram_percent")} min={10} max={99} />
        </Field>
        <Field label={t("Maximum sustained CPU (%)")} error={fieldErrors["max_cpu_percent"]} hint={t("One-minute average of the node.")}>
          <Input {...num("max_cpu_percent")} min={10} max={100} />
        </Field>
        <Field label={t("Jobs at once")} error={fieldErrors["max_jobs"]} hint={t("Encodings and imports beyond it wait in the queue.")}>
          <Input {...num("max_jobs")} min={1} max={16} />
        </Field>
        <Field label={t("Job step timeout (s)")} error={fieldErrors["job_step_timeout_seconds"]} hint={t("A step that takes longer fails its job.")}>
          <Input {...num("job_step_timeout_seconds")} min={10} max={3600} />
        </Field>
        <Field
          label={t("Parent interface")}
          error={fieldErrors["parent_interface"]}
          className="col-span-2"
          hint={node?.runtime === "local" ? t("Not used in local mode.") : t("New cameras attach to this interface with macvlan. Empty: the default route's interface.")}
        >
          <Select value={form.parent_interface} onChange={(e) => set("parent_interface", e.target.value)}>
            <option value="">{t("Default ({iface})", { iface: node?.default_interface || t("none") })}</option>
            {interfaces.map((i) => (
              <option key={i.name} value={i.name}>
                {i.name} — {i.addrs?.join(", ") || t("no address")}
                {i.up ? "" : t(" (down)")}
              </option>
            ))}
          </Select>
        </Field>
        <div className="col-span-2 flex justify-end gap-2">
          <Button onClick={() => settings && setForm(settings)} disabled={update.isPending}>
            {t("Reset")}
          </Button>
          <Button type="submit" variant="primary" disabled={update.isPending}>
            {update.isPending ? t("Saving…") : t("Save")}
          </Button>
        </div>
      </form>
    </Card>
  );
}

function NodeCard() {
  const { data: node } = useNode();
  const { data: m } = useNodeMetrics();
  const t = useT();
  if (!node) return null;
  const counts = m?.camera_counts ?? node.cameras;
  return (
    <Card>
      <CardHeader
        title={t("Node")}
        actions={node.runtime === "local" ? <Badge tone="warn">{t("local mode")}</Badge> : <Badge tone="ok">{t("network namespaces")}</Badge>}
      />
      <dl className="grid grid-cols-[160px_minmax(0,1fr)] gap-x-4 gap-y-2 p-4 text-[13px]">
        <Row label={t("Hostname")}>{node.hostname}</Row>
        <Row label={t("Version")}>
          <Mono>{node.version}</Mono>
        </Row>
        <Row label={t("Up for")}>{sinceText(node.started_at)}</Row>
        <Row label={t("CPUs")}>{node.cpu_count}</Row>
        <Row label={t("Memory")}>{formatBytes(node.mem_total)}</Row>
        <Row label={t("Default route")}>
          <Mono>
            {node.default_interface || "—"}
            {node.default_gateway ? t(" via {gw}", { gw: node.default_gateway }) : ""}
          </Mono>
        </Row>
        <Row label={t("Cameras")}>
          {t("{running} running, {error} in error, {total} total", { running: counts.running, error: counts.error, total: counts.total })}
        </Row>
        {m && (
          <>
            <Row label={t("Database writes")}>
              <Mono>
                {t("{tx} transactions · {st} statements", { tx: m.db.write_transactions, st: m.db.write_statements })}
              </Mono>
            </Row>
            <Row label={t("Metrics at")}>
              <Mono>{formatTime(m.at)}</Mono>
            </Row>
          </>
        )}
      </dl>
    </Card>
  );
}

function Row({ label, children }: { label: string; children: ReactNode }) {
  return (
    <>
      <dt className="text-muted">{label}</dt>
      <dd className="min-w-0 truncate">{children}</dd>
    </>
  );
}
