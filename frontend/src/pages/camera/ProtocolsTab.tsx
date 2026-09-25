import { type FormEvent, useState } from "react";
import { ApiError, type Camera, errorMessage } from "@/api/client";
import { useSetCameraProtocols } from "@/api/queries";
import { Badge, Mono } from "@/components/badges";
import { toast } from "@/components/toast";
import { Card, Notice } from "@/components/ui/card";
import { Checkbox, Input } from "@/components/ui/form";
import { Table, TBody, TD, TH, THead, TR } from "@/components/ui/table";
import { useDraft } from "@/lib/draft";
import { isRunning, SaveBar } from "./parts";

interface Row {
  instance: string;
  enabled: boolean;
  port: string;
}

const savedOf = (camera: Camera): Row[] =>
  camera.protocols.map((p) => ({ instance: p.instance, enabled: p.enabled, port: String(p.port) }));

export function ProtocolsTab({ camera }: { camera: Camera }) {
  const save = useSetCameraProtocols(camera.id);
  const form = useDraft(savedOf(camera));
  const rows = form.draft;
  const [errors, setErrors] = useState<Record<string, string>>({});

  const set = (i: number, patch: Partial<Row>) => form.update((rs) => rs.map((r, j) => (j === i ? { ...r, ...patch } : r)));

  const submit = (e: FormEvent) => {
    e.preventDefault();
    setErrors({});
    save.mutate(
      rows.map((r, i) => ({
        instance: r.instance,
        enabled: r.enabled,
        port: camera.protocols[i].role === "server" ? Number(r.port) : undefined,
      })),
      {
        onSuccess: (saved) => {
          form.resetTo(savedOf(saved));
          toast(isRunning(camera) ? "Protocols saved; they apply when the camera restarts" : "Protocols saved", "ok");
        },
        onError: (err) => {
          if (err instanceof ApiError) setErrors(err.fieldErrors());
          toast(errorMessage(err), "error");
        },
      },
    );
  };

  return (
    <Card>
      <form id="camera-protocols" onSubmit={submit}>
        <Table>
          <THead>
            <tr>
              <TH>Protocol</TH>
              <TH>Engine</TH>
              <TH>Role</TH>
              <TH>Enabled</TH>
              <TH>Port</TH>
              <TH>Profile default</TH>
            </tr>
          </THead>
          <TBody>
            {camera.protocols.map((p, i) => {
              const row = rows[i];
              const error = errors[`protocols[${i}].port`] ?? errors[`protocols[${i}].instance`];
              return (
                <TR key={p.instance}>
                  <TD className="font-medium">{p.instance}</TD>
                  <TD>
                    <Mono>{p.engine}</Mono>
                  </TD>
                  <TD>
                    <Badge tone={p.role === "server" ? "info" : "muted"}>{p.role === "server" ? "serves clients" : "sends out"}</Badge>
                  </TD>
                  <TD>
                    <Checkbox
                      label={row.enabled ? "Yes" : "No"}
                      checked={row.enabled}
                      onChange={(e) => set(i, { enabled: e.target.checked })}
                    />
                  </TD>
                  <TD>
                    {p.role === "server" ? (
                      <div className="flex items-center gap-2">
                        <Input
                          type="number"
                          min={1}
                          max={65535}
                          value={row.port}
                          onChange={(e) => set(i, { port: e.target.value })}
                          className="h-7 w-24 font-mono"
                          aria-label={`Port of ${p.instance}`}
                          disabled={!row.enabled}
                        />
                        {error && <span className="text-xs text-error">{error}</span>}
                      </div>
                    ) : (
                      <span className="text-muted">—</span>
                    )}
                  </TD>
                  <TD>
                    <Mono className="text-muted">{p.role === "server" ? p.default_port : "—"}</Mono>
                  </TD>
                </TR>
              );
            })}
          </TBody>
        </Table>
      </form>
      <div className="px-4 pb-4">
        {errors["protocols"] && (
          <div className="mt-3">
            <Notice tone="error">{errors["protocols"]}</Notice>
          </div>
        )}
        <SaveBar
          form="camera-protocols"
          dirty={form.dirty}
          saving={save.isPending}
          onDiscard={() => {
            form.discard();
            setErrors({});
          }}
          note="The protocols come from the profile (RN-04). Changes apply when the camera restarts."
        />
      </div>
    </Card>
  );
}
