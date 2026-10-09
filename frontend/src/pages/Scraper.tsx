import { ChevronDown, ChevronRight, FileDown, Radar, ScanSearch, Trash2, Wifi } from "lucide-react";
import { type FormEvent, useState } from "react";
import { ApiError, type DeviceInput, type Found, errorMessage } from "@/api/client";
import { useCaptures, useCreateDevice, useDeleteDevice, useDevices, useDiscover, useProbeDevice, usePrograms, useStartCapture } from "@/api/queries";
import { Badge, Mono } from "@/components/badges";
import { CopyButton } from "@/components/CopyButton";
import { toast } from "@/components/toast";
import { Button } from "@/components/ui/button";
import { Card, Empty, Notice, PageHeader } from "@/components/ui/card";
import { Dialog } from "@/components/ui/dialog";
import { Checkbox, Field, Input, Select } from "@/components/ui/form";
import { Table, TBody, TD, TH, THead, TR } from "@/components/ui/table";
import { useT } from "@/lib/i18n";

/** Register a camera the user owns or is authorized to capture, look at it read-only, and compile a draft profile (D44, RN-17). */
export function ScraperPage() {
  const t = useT();
  const { data: devices, isLoading, error } = useDevices();
  const [adding, setAdding] = useState<Partial<DeviceInput> | false>(false);
  const [discovering, setDiscovering] = useState(false);
  const probe = useProbeDevice();
  const remove = useDeleteDevice();
  const [open, setOpen] = useState<string | null>(null);
  return (
    <>
      <PageHeader
        title={t("Scraper")}
        description={t(
          "Observe a camera you own or are authorized to capture and compile what it answers into a draft profile. Every probe is read-only and rate-limited.",
        )}
        actions={
          <>
            <Button onClick={() => setDiscovering(true)}>
              <Wifi /> {t("Discover")}
            </Button>
            <Button variant="primary" onClick={() => setAdding({})}>
              <Radar /> {t("Register a device")}
            </Button>
          </>
        }
      />
      {error && <Notice tone="error">{errorMessage(error)}</Notice>}
      <Notice tone="info">
        {t(
          "The scraper only sends read-only requests, from a closed whitelist, and never writes to a device. Register only equipment you own or are allowed to capture.",
        )}
      </Notice>
      <Card className="mt-4">
        {isLoading ? (
          <Empty title={t("Loading…")} />
        ) : !devices?.length ? (
          <Empty icon={<ScanSearch />} title={t("No devices registered")}>
            {t("Register a camera by its address to look at it and build a draft profile from what it answers.")}
          </Empty>
        ) : (
          <Table>
            <THead>
              <tr>
                <TH>{t("Device")}</TH>
                <TH>{t("Address")}</TH>
                <TH>{t("Detected")}</TH>
                <TH>{t("Authorized")}</TH>
                <TH className="text-right">{t("Actions")}</TH>
              </tr>
            </THead>
            <TBody>
              {devices.flatMap((d) => [
                <TR key={d.id}>
                  <TD>
                    <span className="flex items-center gap-2">
                      <span className="font-medium">{d.name}</span>
                      {d.kind === "simulated" && (
                        <Badge tone="info" title={t("Its address is one of this node's cameras: a round-trip capture of a profile you made.")}>
                          {t("Simulated")}
                        </Badge>
                      )}
                    </span>
                    {d.username && <div className="text-xs text-muted">{t("as {user}", { user: d.username })}</div>}
                  </TD>
                  <TD>
                    <Mono>{d.host}</Mono>
                  </TD>
                  <TD>
                    {d.detected ? (
                      <span className="flex flex-col gap-0.5 text-xs">
                        <span className="flex flex-wrap gap-1">
                          {d.detected.services.map((s) => (
                            <Badge key={s.port} tone="muted" title={[s.server, s.note].filter(Boolean).join(" · ")}>
                              {s.port}/{s.proto}
                              {s.auth && s.auth !== "none" ? ` · ${s.auth}` : ""}
                            </Badge>
                          ))}
                        </span>
                        {d.detected.vendor && <span className="text-muted">{d.detected.vendor}</span>}
                      </span>
                    ) : (
                      <span className="text-muted">{t("not probed yet")}</span>
                    )}
                  </TD>
                  <TD>
                    {d.authorized ? (
                      <Badge tone="ok">{t("Yes")}</Badge>
                    ) : (
                      <Badge tone="warn" title={t("No probe runs until you confirm you may capture this device.")}>
                        {t("No")}
                      </Badge>
                    )}
                  </TD>
                  <TD className="text-right">
                    <span className="flex justify-end gap-1">
                      <Button size="sm" variant="ghost" onClick={() => setOpen(open === d.id ? null : d.id)}>
                        {open === d.id ? <ChevronDown /> : <ChevronRight />} {t("Captures")}
                      </Button>
                      <Button
                        size="sm"
                        disabled={!d.authorized || probe.isPending}
                        onClick={() =>
                          probe.mutate(d.id, {
                            onSuccess: (r) =>
                              toast(
                                r.detected?.reachable
                                  ? t("{name}: {n} ports answered", { name: d.name, n: r.detected.open_ports.length })
                                  : t("{name} did not answer", { name: d.name }),
                                r.detected?.reachable ? "ok" : "info",
                              ),
                            onError: (err) => toast(errorMessage(err), "error"),
                          })
                        }
                      >
                        <ScanSearch /> {t("Probe")}
                      </Button>
                      <Button
                        size="sm"
                        variant="ghost"
                        onClick={() => {
                          if (!confirm(t("Remove {name} and its captures?", { name: d.name }))) return;
                          remove.mutate(d.id, {
                            onSuccess: () => toast(t("{name} removed", { name: d.name }), "ok"),
                            onError: (err) => toast(errorMessage(err), "error"),
                          });
                        }}
                      >
                        <Trash2 /> {t("Remove")}
                      </Button>
                    </span>
                  </TD>
                </TR>,
                open === d.id ? (
                  <tr key={`${d.id}-cap`}>
                    <td colSpan={5} className="bg-surface-2/40 px-4 py-3">
                      <DeviceCaptures device={d} />
                    </td>
                  </tr>
                ) : null,
              ])}
            </TBody>
          </Table>
        )}
      </Card>
      {adding && <RegisterDialog initial={adding} onClose={() => setAdding(false)} />}
      {discovering && <DiscoverDialog onClose={() => setDiscovering(false)} onRegister={(d) => { setDiscovering(false); setAdding(d); }} />}
    </>
  );
}

function RegisterDialog({ initial, onClose }: { initial: Partial<DeviceInput>; onClose: () => void }) {
  const t = useT();
  const create = useCreateDevice();
  const [form, setForm] = useState<DeviceInput>({ name: initial.name ?? "", host: initial.host ?? "", ports: [], username: "", password: "", authorized: false });
  const [ports, setPorts] = useState((initial.ports ?? []).join(", "));
  const [errors, setErrors] = useState<Record<string, string>>({});
  const set = <K extends keyof DeviceInput>(k: K, v: DeviceInput[K]) => setForm({ ...form, [k]: v });
  const submit = (e: FormEvent) => {
    e.preventDefault();
    setErrors({});
    const parsed = ports
      .split(/[\s,]+/)
      .filter(Boolean)
      .map(Number);
    if (parsed.some((n) => !Number.isInteger(n) || n < 1 || n > 65535)) {
      setErrors({ ports: t("Ports must be numbers between 1 and 65535.") });
      return;
    }
    create.mutate(
      { ...form, ports: parsed, password: form.password || undefined },
      {
        onSuccess: (d) => {
          toast(t("{name} registered", { name: d.name }), "ok");
          onClose();
        },
        onError: (err) => {
          if (err instanceof ApiError) setErrors(err.fieldErrors());
          else toast(errorMessage(err), "error");
        },
      },
    );
  };
  return (
    <Dialog
      open
      onClose={onClose}
      title={t("Register a device")}
      description={t("Its address and, if its API needs them, the credentials. They are stored encrypted and never leave the node in a profile.")}
      footer={
        <>
          <Button onClick={onClose}>{t("Cancel")}</Button>
          <Button type="submit" form="register-device" variant="primary" disabled={create.isPending || !form.authorized}>
            {create.isPending ? t("Saving…") : t("Register")}
          </Button>
        </>
      }
    >
      <form id="register-device" onSubmit={submit} className="flex flex-col gap-3">
        <Field label={t("Name")} error={errors["name"]}>
          <Input value={form.name} onChange={(e) => set("name", e.target.value)} placeholder={t("Lab camera")} autoFocus />
        </Field>
        <Field label={t("Address")} error={errors["host"]} hint={t("IP address or host name on the LAN")}>
          <Input value={form.host} onChange={(e) => set("host", e.target.value)} placeholder="192.168.1.64" className="font-mono" />
        </Field>
        <Field label={t("Ports")} error={errors["ports"]} hint={t("Optional; common camera ports are probed when empty")}>
          <Input value={ports} onChange={(e) => setPorts(e.target.value)} placeholder="80, 554" className="font-mono" />
        </Field>
        <div className="grid grid-cols-2 gap-3">
          <Field label={t("Username")}>
            <Input value={form.username} onChange={(e) => set("username", e.target.value)} placeholder="admin" />
          </Field>
          <Field label={t("Password")}>
            <Input type="password" value={form.password ?? ""} onChange={(e) => set("password", e.target.value)} />
          </Field>
        </div>
        <Checkbox
          checked={form.authorized}
          onCheckedChange={(v) => set("authorized", v)}
          label={t("I own this device or am authorized to capture it (required before any probe).")}
        />
      </form>
    </Dialog>
  );
}

function DiscoverDialog({ onClose, onRegister }: { onClose: () => void; onRegister: (d: Partial<DeviceInput>) => void }) {
  const t = useT();
  const discover = useDiscover();
  const [cidr, setCidr] = useState("");
  const [multicast, setMulticast] = useState(true);
  const [found, setFound] = useState<Found[] | null>(null);
  const [error, setError] = useState("");
  const run = (e: FormEvent) => {
    e.preventDefault();
    setError("");
    setFound(null);
    discover.mutate(
      { cidr: cidr.trim() || undefined, multicast },
      {
        onSuccess: (items) => setFound(items),
        onError: (err) => setError(errorMessage(err)),
      },
    );
  };
  return (
    <Dialog
      open
      onClose={onClose}
      title={t("Discover cameras")}
      description={t("A read-only connect sweep of a private subnet and best-effort multicast queries. It never leaves the LAN (RN-17).")}
      footer={<Button onClick={onClose}>{t("Close")}</Button>}
    >
      <form onSubmit={run} className="flex items-end gap-2">
        <Field label={t("Subnet")} hint={t("Private ranges only, /22 or smaller")} className="flex-1">
          <Input value={cidr} onChange={(e) => setCidr(e.target.value)} placeholder="192.168.1.0/24" className="font-mono" />
        </Field>
        <Checkbox checked={multicast} onCheckedChange={setMulticast} label={t("Multicast")} className="mb-2" />
        <Button type="submit" variant="primary" disabled={discover.isPending} className="mb-2">
          {discover.isPending ? t("Scanning…") : t("Scan")}
        </Button>
      </form>
      {error && (
        <Notice tone="error">
          <span className="mt-2 block">{error}</span>
        </Notice>
      )}
      {found && (
        <div className="mt-3">
          {found.length === 0 ? (
            <Empty title={t("Nothing answered")}>{t("No camera answered on the subnet or by multicast.")}</Empty>
          ) : (
            <Table>
              <THead>
                <tr>
                  <TH>{t("Address")}</TH>
                  <TH>{t("Found by")}</TH>
                  <TH>{t("Ports")}</TH>
                  <TH className="text-right" />
                </tr>
              </THead>
              <TBody>
                {found.map((f) => (
                  <TR key={f.host}>
                    <TD>
                      <Mono>{f.host}</Mono>
                      {f.vendor && <div className="text-xs text-muted">{f.vendor}</div>}
                    </TD>
                    <TD className="text-xs text-muted">{f.via}</TD>
                    <TD>
                      <Mono className="text-xs">{(f.open_ports ?? []).join(", ") || "—"}</Mono>
                    </TD>
                    <TD className="text-right">
                      <Button size="sm" onClick={() => onRegister({ name: f.vendor ? `${f.vendor} ${f.host}` : f.host, host: f.host, ports: f.open_ports })}>
                        {t("Register")}
                      </Button>
                    </TD>
                  </TR>
                ))}
              </TBody>
            </Table>
          )}
        </div>
      )}
    </Dialog>
  );
}

function DeviceCaptures({ device }: { device: { id: string; authorized: boolean; receiver_url: string } }) {
  const t = useT();
  const { data: programs } = usePrograms(device.id);
  const { data: captures } = useCaptures(device.id, true);
  const start = useStartCapture();
  const [program, setProgram] = useState("");
  const chosen = program || programs?.[0]?.program_id || "";
  return (
    <div className="flex flex-col gap-3">
      <div className="flex items-end gap-2">
        <Field label={t("Capture program")} className="w-72">
          <Select value={chosen} onChange={(e) => setProgram(e.target.value)} disabled={!programs?.length}>
            {programs?.map((p) => (
              <option key={`${p.program_id}@${p.version}`} value={p.program_id}>
                {p.name} · {p.program_id}
              </option>
            ))}
          </Select>
        </Field>
        <Button
          variant="primary"
          size="sm"
          disabled={!device.authorized || !chosen || start.isPending}
          className="mb-0.5"
          onClick={() =>
            start.mutate(
              { id: device.id, program: chosen },
              {
                onSuccess: () => toast(t("Capture started; it runs read-only in the background."), "ok"),
                onError: (err) => toast(errorMessage(err), "error"),
              },
            )
          }
        >
          <ScanSearch /> {t("Capture")}
        </Button>
      </div>
      <div className="flex items-center gap-2 text-xs text-muted">
        <span>{t("Event receiver URL (point the camera's alarm here):")}</span>
        <Mono className="select-all">{device.receiver_url}</Mono>
        <CopyButton text={device.receiver_url} />
      </div>
      {!captures?.length ? (
        <p className="text-[13px] text-muted">{t("No captures yet.")}</p>
      ) : (
        <Table>
          <THead>
            <tr>
              <TH>{t("Program")}</TH>
              <TH>{t("Status")}</TH>
              <TH>{t("Recorded")}</TH>
              <TH />
            </tr>
          </THead>
          <TBody>
            {captures.map((c) => (
              <TR key={c.id}>
                <TD>
                  <Mono>{c.program}</Mono>
                </TD>
                <TD>
                  <Badge tone={c.status === "done" ? "ok" : c.status === "failed" ? "error" : "info"}>{t(c.status)}</Badge>
                </TD>
                <TD className="text-muted">
                  {c.result ? t("{ok} of {n} steps", { ok: c.result.ok, n: c.result.steps }) : "—"}
                </TD>
                <TD className="text-right">
                  {c.status === "done" && !c.draft_profile_id && <CompileButton capture={c} />}
                  {c.draft_profile_id && (
                    <Badge tone="ok" icon={<FileDown />}>
                      {t("Draft profile")}
                    </Badge>
                  )}
                </TD>
              </TR>
            ))}
          </TBody>
        </Table>
      )}
    </div>
  );
}

function CompileButton({ capture }: { capture: { id: string } }) {
  const t = useT();
  // Feature 20 wires compilation; the button appears once a capture is done.
  void capture;
  return (
    <Button size="sm" variant="ghost" disabled title={t("Compile to a draft profile (coming next)")}>
      <FileDown /> {t("Compile")}
    </Button>
  );
}
