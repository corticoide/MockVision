import { type FormEvent, useState } from "react";
import { ApiError, type Camera, errorMessage } from "@/api/client";
import { useNode, useUpdateCamera } from "@/api/queries";
import { toast } from "@/components/toast";
import { Button } from "@/components/ui/button";
import { Card, Notice } from "@/components/ui/card";
import { Field, Input, Select } from "@/components/ui/form";
import { useDraft } from "@/lib/draft";
import { isRunning, SaveBar } from "./parts";

const splitList = (s: string) =>
  s
    .split(/[\s,]+/)
    .map((t) => t.trim())
    .filter(Boolean);

const savedOf = (camera: Camera) => {
  const n = camera.network;
  return { ip: n.ip, netmask: n.netmask, gateway: n.gateway, parent: n.parent, mac: n.mac, defaultMac: false, dns: n.dns.join(", ") };
};

export function NetworkTab({ camera }: { camera: Camera }) {
  const { data: node } = useNode();
  const update = useUpdateCamera();
  const local = node?.runtime === "local";
  const form = useDraft(savedOf(camera));
  const { ip, netmask, gateway, parent, mac, defaultMac, dns } = form.draft;
  const [errors, setErrors] = useState<Record<string, string>>({});

  const submit = (e: FormEvent) => {
    e.preventDefault();
    setErrors({});
    update.mutate(
      {
        id: camera.id,
        body: {
          network: {
            ip: ip.trim(),
            netmask: netmask.trim(),
            gateway: gateway.trim(),
            parent,
            mac: defaultMac ? undefined : mac.trim(),
            default_mac: defaultMac || undefined,
            dns: splitList(dns),
          },
        },
      },
      {
        onSuccess: (saved) => {
          form.resetTo(savedOf(saved));
          toast(isRunning(camera) ? "Network saved; it applies when the camera restarts" : "Network saved", "ok");
        },
        onError: (err) => {
          if (err instanceof ApiError) setErrors(err.fieldErrors());
          toast(errorMessage(err), "error");
        },
      },
    );
  };

  const interfaces = (node?.interfaces ?? []).filter((i) => !i.loopback);

  return (
    <Card className="p-4">
      {local && (
        <div className="mb-4">
          <Notice tone="info">
            Local mode: the camera answers on 127.0.0.1 with its own ports, so only the MAC and the DNS servers apply here.
          </Notice>
        </div>
      )}
      <form id="camera-network" onSubmit={submit} className="grid grid-cols-3 gap-x-4 gap-y-3">
        <Field label="IP address" error={errors["network.ip"]}>
          <Input value={ip} onChange={(e) => form.set({ ip: e.target.value })} className="font-mono" placeholder="192.168.1.50" required={!local} disabled={local} />
        </Field>
        <Field label="Netmask" error={errors["network.netmask"]} hint="Empty: the parent interface's.">
          <Input value={netmask} onChange={(e) => form.set({ netmask: e.target.value })} className="font-mono" disabled={local} />
        </Field>
        <Field label="Gateway" error={errors["network.gateway"]} hint="Empty: the node's, when it is in the subnet.">
          <Input value={gateway} onChange={(e) => form.set({ gateway: e.target.value })} className="font-mono" disabled={local} />
        </Field>
        <Field label="MAC address" error={errors["network.mac"]} hint={defaultMac ? "Derived again from the camera ID on save." : "Locally administered and stable."}>
          <div className="flex gap-2">
            <Input
              value={defaultMac ? "" : mac}
              placeholder={defaultMac ? "default" : undefined}
              onChange={(e) => form.set({ mac: e.target.value, defaultMac: false })}
              className="font-mono"
            />
            <Button size="md" onClick={() => form.set({ defaultMac: true })} disabled={defaultMac} title="Go back to the MAC derived from the camera ID">
              Default
            </Button>
          </div>
        </Field>
        <Field label="DNS servers" error={errors["network.dns"]} hint="Up to 3; empty: the node's.">
          <Input value={dns} onChange={(e) => form.set({ dns: e.target.value })} className="font-mono" placeholder="1.1.1.1, 8.8.8.8" />
        </Field>
        <Field label="Parent interface" error={errors["network.parent"]} hint="Where the camera's macvlan attaches.">
          <Select value={parent} onChange={(e) => form.set({ parent: e.target.value })} disabled={local}>
            <option value="">Default ({node?.parent_interface || node?.default_interface || "none"})</option>
            {interfaces.map((i) => (
              <option key={i.name} value={i.name}>
                {i.name} — {i.addrs?.join(", ") || "no address"}
              </option>
            ))}
          </Select>
        </Field>
      </form>
      <SaveBar
        form="camera-network"
        dirty={form.dirty}
        saving={update.isPending}
        onDiscard={() => {
          form.discard();
          setErrors({});
        }}
        note="Network changes apply when the camera restarts (RN-09). The new address is probed before it is used."
      />
    </Card>
  );
}
