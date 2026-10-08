import { ShieldCheck, ShieldOff } from "lucide-react";
import { type FormEvent, useState } from "react";
import { ApiError, type Camera, errorMessage } from "@/api/client";
import { useNode, useProfile, useUpdateCamera } from "@/api/queries";
import { Badge, Mono } from "@/components/badges";
import { toast } from "@/components/toast";
import { Button } from "@/components/ui/button";
import { Card, Notice } from "@/components/ui/card";
import { NetworkModeFields } from "@/components/NetworkModeFields";
import { Checkbox, Field, Input, Select } from "@/components/ui/form";
import { useDraft } from "@/lib/draft";
import { type Translate, useT } from "@/lib/i18n";
import { defaultParent, isWireless } from "@/lib/network";
import { Info, isRunning, SaveBar, SectionTitle } from "./parts";

const splitList = (s: string) =>
  s
    .split(/[\s,]+/)
    .map((t) => t.trim())
    .filter(Boolean);

const savedOf = (camera: Camera) => {
  const n = camera.network;
  return {
    mode: n.mode as string,
    ipMode: n.ip_mode as string,
    force: n.force,
    ip: n.ip,
    netmask: n.netmask,
    gateway: n.gateway,
    parent: n.parent,
    mac: n.mac,
    defaultMac: false,
    dns: n.dns.join(", "),
  };
};

/** Where the address a camera holds came from. */
export function addressSource(source: string | undefined, t: Translate) {
  switch (source) {
    case "dhcp":
      return t("leased by DHCP");
    case "factory":
      return t("factory address: no DHCP server answered");
    case "static":
      return t("static");
  }
  return "";
}

export function NetworkTab({ camera }: { camera: Camera }) {
  const { data: node } = useNode();
  const { data: profile } = useProfile({ id: camera.profile.id, version: camera.profile.version });
  const update = useUpdateCamera();
  const t = useT();
  const local = node?.runtime === "local";
  const form = useDraft(savedOf(camera));
  const d = form.draft;
  const [errors, setErrors] = useState<Record<string, string>>({});
  const dhcp = d.ipMode === "dhcp";
  const ipvlan = d.mode === "ipvlan";

  const submit = (e: FormEvent) => {
    e.preventDefault();
    setErrors({});
    update.mutate(
      {
        id: camera.id,
        body: {
          network: {
            mode: d.mode as "macvlan" | "ipvlan",
            ip_mode: d.ipMode as "static" | "dhcp",
            force: d.force,
            ip: dhcp ? "" : d.ip.trim(),
            netmask: dhcp ? "" : d.netmask.trim(),
            gateway: dhcp ? "" : d.gateway.trim(),
            parent: d.parent,
            mac: d.defaultMac ? undefined : d.mac.trim(),
            default_mac: d.defaultMac || undefined,
            dns: splitList(d.dns),
          },
        },
      },
      {
        onSuccess: (saved) => {
          form.resetTo(savedOf(saved));
          toast(isRunning(camera) ? t("Network saved; it applies when the camera restarts") : t("Network saved"), "ok");
        },
        onError: (err) => {
          if (err instanceof ApiError) setErrors(err.fieldErrors());
          toast(errorMessage(err), "error");
        },
      },
    );
  };

  const interfaces = (node?.interfaces ?? []).filter((i) => !i.loopback);
  const parentName = d.parent || defaultParent(node);
  const parentWireless = isWireless(node, parentName);
  const st = camera.status;
  const savedIpvlan = camera.network.mode === "ipvlan";

  return (
    <div className="flex flex-col gap-4">
      {!local && (
        <Card className="p-4">
          <SectionTitle>{t("On the LAN now")}</SectionTitle>
          <div className="grid grid-cols-4 gap-x-6 gap-y-2 text-[13px]">
            <Info
              label={t("Address")}
              value={
                st.ip ? (
                  <span className="flex flex-col items-start gap-1">
                    <Mono>{st.ip}</Mono>
                    {st.ip_source && <Badge tone={st.ip_source === "factory" ? "warn" : "muted"}>{addressSource(st.ip_source, t)}</Badge>}
                  </span>
                ) : (
                  "—"
                )
              }
            />
            <Info label={t("MAC it answers with")} value={<Mono>{st.mac || (savedIpvlan ? t("the node's") : camera.network.mac)}</Mono>} />
            <Info label={t("Mode")} value={savedIpvlan ? "ipvlan" : "macvlan"} />
            <Info
              label={t("Outbound firewall")}
              value={
                isRunning(camera) ? (
                  st.firewall ? (
                    <span className="flex items-center gap-1.5 text-ok">
                      <ShieldCheck className="size-3.5" /> {t("only toward the event targets")}
                    </span>
                  ) : (
                    <span className="flex items-center gap-1.5 text-warn">
                      <ShieldOff className="size-3.5" /> {t("not available on this node")}
                    </span>
                  )
                ) : (
                  "—"
                )
              }
            />
          </div>
        </Card>
      )}
      <Card className="p-4">
        {local && (
          <div className="mb-4">
            <Notice tone="info">
              {t("Local mode: the camera answers on 127.0.0.1 with its own ports, so only the MAC and the DNS servers apply here.")}
            </Notice>
          </div>
        )}
        <form id="camera-network" onSubmit={submit} className="grid grid-cols-3 gap-x-4 gap-y-3">
          <NetworkModeFields
            value={{ mode: d.mode, ipMode: d.ipMode }}
            onChange={(next) => form.set(next)}
            factoryIP={profile?.factory_ip}
            errors={errors}
            disabled={local}
          />
          <Field label={t("Parent interface")} error={errors["network.parent"]} hint={t("The node's network card the camera attaches to.")}>
            <Select value={d.parent} onChange={(e) => form.set({ parent: e.target.value })} disabled={local}>
              <option value="">{t("Default ({iface})", { iface: defaultParent(node) || t("none") })}</option>
              {interfaces.map((i) => (
                <option key={i.name} value={i.name}>
                  {i.name}
                  {i.wireless ? " (Wi-Fi)" : ""} — {i.addrs?.join(", ") || t("no address")}
                </option>
              ))}
            </Select>
          </Field>
          {parentWireless && !ipvlan && !local && (
            <div className="col-span-3">
              <Notice tone="warn">{t("{iface} is a Wi-Fi interface: access points refuse the extra MACs of macvlan cameras. Choose ipvlan.", { iface: parentName })}</Notice>
            </div>
          )}
          {!dhcp && (
            <>
              <Field label={t("IP address")} error={errors["network.ip"]}>
                <Input value={d.ip} onChange={(e) => form.set({ ip: e.target.value })} className="font-mono" placeholder="192.168.1.50" required={!local} disabled={local} />
              </Field>
              <Field label={t("Netmask")} error={errors["network.netmask"]} hint={t("Empty: the parent interface's.")}>
                <Input value={d.netmask} onChange={(e) => form.set({ netmask: e.target.value })} className="font-mono" disabled={local} />
              </Field>
              <Field label={t("Gateway")} error={errors["network.gateway"]} hint={t("Empty: the node's, when it is in the subnet.")}>
                <Input value={d.gateway} onChange={(e) => form.set({ gateway: e.target.value })} className="font-mono" disabled={local} />
              </Field>
            </>
          )}
          <Field
            label={t("MAC address")}
            error={errors["network.mac"]}
            hint={ipvlan ? t("Kept for macvlan; with ipvlan the camera answers with the node's MAC.") : d.defaultMac ? t("Derived again from the camera ID on save.") : t("Locally administered and stable.")}
          >
            <div className="flex gap-2">
              <Input
                value={d.defaultMac ? "" : d.mac}
                placeholder={d.defaultMac ? t("default") : undefined}
                onChange={(e) => form.set({ mac: e.target.value, defaultMac: false })}
                className="font-mono"
              />
              <Button size="md" onClick={() => form.set({ defaultMac: true })} disabled={d.defaultMac} title={t("Go back to the MAC derived from the camera ID")}>
                {t("Default")}
              </Button>
            </div>
          </Field>
          <Field
            label={t("DNS servers")}
            error={errors["network.dns"]}
            hint={dhcp ? t("Up to 3; empty: the lease's, else the node's.") : t("Up to 3; empty: the node's.")}
          >
            <Input value={d.dns} onChange={(e) => form.set({ dns: e.target.value })} className="font-mono" placeholder="1.1.1.1, 8.8.8.8" />
          </Field>
          <div className="col-span-3">
            <Checkbox
              checked={d.force}
              onCheckedChange={(checked) => form.set({ force: checked })}
              disabled={local}
              label={t("Start even if another device answers on this IP or MAC")}
            />
            {d.force && (
              <p className="mt-1 text-xs text-warn">
                {t("Only to test how clients handle a conflict: two devices with one address break each other's traffic.")}
              </p>
            )}
          </div>
        </form>
        <SaveBar
          form="camera-network"
          dirty={form.dirty}
          saving={update.isPending}
          onDiscard={() => {
            form.discard();
            setErrors({});
          }}
          note={t("Network changes apply when the camera restarts. Its IP and MAC are probed on the LAN before use.")}
        />
      </Card>
    </div>
  );
}
