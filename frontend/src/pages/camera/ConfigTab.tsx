import { type FormEvent, useState } from "react";
import { ApiError, type Camera, errorMessage, type Param } from "@/api/client";
import { useCameraConfig, usePatchConfig } from "@/api/queries";
import { Badge, Mono } from "@/components/badges";
import { toast } from "@/components/toast";
import { Card, Empty, Notice } from "@/components/ui/card";
import { Checkbox, Input, Select } from "@/components/ui/form";
import { Table, TBody, TD, TH, THead, TR } from "@/components/ui/table";
import { cn, formatTime } from "@/lib/utils";
import { SaveBar } from "./parts";

/** Raw editor values: strings for text inputs, booleans for checkboxes. */
type Draft = Record<string, string | boolean>;

const shown = (v: unknown) => (v === null || v === undefined ? "" : typeof v === "string" ? v : JSON.stringify(v));

function toDraft(p: Param): string | boolean {
  if (p.type === "bool") return Boolean(p.value);
  if (p.type === "enum") return String(p.values?.findIndex((o) => shown(o) === shown(p.value)) ?? -1);
  return shown(p.value);
}

function fromDraft(p: Param, d: string | boolean): unknown {
  switch (p.type) {
    case "bool":
      return Boolean(d);
    case "enum":
      return p.values?.[Number(d)];
    case "int":
      return Number.parseInt(String(d), 10);
    case "float":
      return Number.parseFloat(String(d));
    default:
      return String(d);
  }
}

function originLabel(origin: string) {
  if (origin === "profile") return "profile default";
  if (origin.startsWith("client:")) return `client ${origin.slice(7)}`;
  return origin;
}

export function ConfigTab({ camera }: { camera: Camera }) {
  const { data: params, isLoading, error } = useCameraConfig(camera.id);
  const patch = usePatchConfig(camera.id);
  const [draft, setDraft] = useState<Draft>({});
  const [errors, setErrors] = useState<Record<string, string>>({});

  if (error) return <Notice tone="error">{errorMessage(error)}</Notice>;
  if (isLoading || !params) return <Card><Empty title="Loading parameters…" /></Card>;
  if (params.length === 0) return <Card><Empty title="The profile declares no parameters" /></Card>;

  const changed = params.filter((p) => p.key in draft && draft[p.key] !== toDraft(p));

  const submit = (e: FormEvent) => {
    e.preventDefault();
    setErrors({});
    const values: Record<string, unknown> = {};
    for (const p of changed) values[p.key] = fromDraft(p, draft[p.key]);
    patch.mutate(values, {
      onSuccess: () => {
        setDraft({});
        toast(`${changed.length} parameter${changed.length === 1 ? "" : "s"} saved`, "ok");
      },
      onError: (err) => {
        if (err instanceof ApiError) setErrors(err.fieldErrors());
        toast(errorMessage(err), "error");
      },
    });
  };

  return (
    <Card>
      <form id="camera-config" onSubmit={submit}>
        <Table>
          <THead>
            <tr>
              <TH>Parameter</TH>
              <TH>Value</TH>
              <TH>Default</TH>
              <TH>Effect</TH>
              <TH>Last change</TH>
            </tr>
          </THead>
          <TBody>
            {params.map((p) => {
              const value = p.key in draft ? draft[p.key] : toDraft(p);
              const edited = p.key in draft && draft[p.key] !== toDraft(p);
              const set = (v: string | boolean) => setDraft((d) => ({ ...d, [p.key]: v }));
              return (
                <TR key={p.key} className={cn("h-11", edited && "bg-info/5")}>
                  <TD className="max-w-80 whitespace-normal">
                    <div className="font-mono text-[12px]">{p.key}</div>
                    {p.description && <div className="text-xs text-muted">{p.description}</div>}
                  </TD>
                  <TD>
                    <ParamEditor param={p} value={value} onChange={set} />
                    {errors[p.key] && <div className="text-xs text-error">{errors[p.key]}</div>}
                  </TD>
                  <TD>
                    <Mono className="text-muted">{shown(p.default) || "—"}</Mono>
                  </TD>
                  <TD>
                    {p.effective ? (
                      <Badge tone="info" title={`Bound to ${p.bind}`}>
                        effective
                      </Badge>
                    ) : (
                      <Badge tone="muted" title="Stored and returned; no effect on the simulation">
                        declarative
                      </Badge>
                    )}
                  </TD>
                  <TD className="text-xs text-muted">
                    {originLabel(p.origin)}
                    {p.origin !== "profile" && <> · {formatTime(p.updated_at)}</>}
                  </TD>
                </TR>
              );
            })}
          </TBody>
        </Table>
      </form>
      <div className="px-4 pb-4">
        <SaveBar
          form="camera-config"
          dirty={changed.length > 0}
          saving={patch.isPending}
          onDiscard={() => {
            setDraft({});
            setErrors({});
          }}
          note="Applied at once. The last change wins, from the panel or a client of the emulated API (RN-08)."
        />
      </div>
    </Card>
  );
}

function ParamEditor({ param: p, value, onChange }: { param: Param; value: string | boolean; onChange: (v: string | boolean) => void }) {
  const label = `Value of ${p.key}`;
  switch (p.type) {
    case "bool":
      return <Checkbox label={value ? "On" : "Off"} checked={Boolean(value)} onChange={(e) => onChange(e.target.checked)} aria-label={label} />;
    case "enum":
      return (
        <Select value={String(value)} onChange={(e) => onChange(e.target.value)} className="h-7 w-48" aria-label={label}>
          {p.values?.map((o, i) => (
            <option key={i} value={String(i)}>
              {shown(o)}
            </option>
          ))}
        </Select>
      );
    case "int":
    case "float":
      return (
        <Input
          type="number"
          step={p.type === "int" ? 1 : "any"}
          min={p.min}
          max={p.max}
          value={String(value)}
          onChange={(e) => onChange(e.target.value)}
          className="h-7 w-32 font-mono"
          aria-label={label}
        />
      );
    default:
      return <Input value={String(value)} onChange={(e) => onChange(e.target.value)} className="h-7 w-64" aria-label={label} />;
  }
}
