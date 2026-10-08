import { Plug, Power, PowerOff, ShieldAlert, Upload } from "lucide-react";
import { useState } from "react";
import { errorMessage, type Plugin } from "@/api/client";
import { usePlugins, useSettings, useUpdatePlugin, useUpdateSettings } from "@/api/queries";
import { Badge, Mono } from "@/components/badges";
import { toast } from "@/components/toast";
import { Button } from "@/components/ui/button";
import { Card, Empty, Notice, PageHeader } from "@/components/ui/card";
import { Dialog } from "@/components/ui/dialog";
import { Checkbox } from "@/components/ui/form";
import { Table, TBody, TD, TH, THead, TR } from "@/components/ui/table";
import { useT } from "@/lib/i18n";
import { formatTime } from "@/lib/utils";
import { ReportCard, SignatureBadge, useImportFlow } from "./Profiles";

/** What each permission lets a plugin do (D86). */
const permissionText: Record<Plugin["permissions"][number], string> = {
  "net.listen": "Receive connections on the ports of its cameras",
  "net.connect": "Open connections to other hosts, to deliver events",
  "accounts.read": "Read the users and passwords of its cameras",
  "state.read": "Read the parameters of its cameras",
  "state.write": "Change the parameters of its cameras",
  "events.emit": "Raise events",
  "events.deliver": "Deliver events to the cameras' targets",
  "media.read": "Read the streams and snapshots of its cameras",
  sd: "Read the recordings on the SD cards of its cameras",
};

export function PluginsPage() {
  const t = useT();
  const { data: plugins, isLoading, error } = usePlugins();
  const flow = useImportFlow();
  const [approving, setApproving] = useState<Plugin | null>(null);
  const update = useUpdatePlugin();
  const disable = (p: Plugin) =>
    update.mutate(
      { id: p.id, enabled: false },
      {
        onSuccess: () => toast(t("{name} is disabled; running cameras keep it until they restart.", { name: p.engine }), "ok"),
        onError: (err) => toast(errorMessage(err), "error"),
      },
    );
  return (
    <>
      <PageHeader
        title={t("Plugins")}
        description={t(
          "Engines from outside MockVision. Each runs as its own confined process in the cameras that use it, with only the permissions you approve when you enable it.",
        )}
        actions={
          <>
            {flow.picker}
            <Button variant="primary" onClick={flow.pick} disabled={flow.pending}>
              <Upload /> {flow.pending ? t("Validating…") : t("Import plugin")}
            </Button>
          </>
        }
      />
      {error && <Notice tone="error">{errorMessage(error)}</Notice>}
      {flow.report && <ReportCard {...flow.report} onClose={flow.dismiss} />}
      <UnsignedCard />
      <Card>
        {isLoading ? (
          <Empty title={t("Loading…")} />
        ) : !plugins?.length ? (
          <Empty icon={<Plug />} title={t("No plugins installed")}>
            {t("Import a plugin package (.mvpkg). The SDK and an example plugin are in")} <span className="font-mono">sdk/</span> {t("and")}{" "}
            <span className="font-mono">examples/plugins/hello</span>.
          </Empty>
        ) : (
          <Table>
            <THead>
              <tr>
                <TH>{t("Engine")}</TH>
                <TH>{t("Package")}</TH>
                <TH>{t("Signature")}</TH>
                <TH>{t("Permissions")}</TH>
                <TH>{t("Ports")}</TH>
                <TH>{t("Status")}</TH>
                <TH className="text-right">{t("Actions")}</TH>
              </tr>
            </THead>
            <TBody>
              {plugins.map((p) => (
                <TR key={p.id}>
                  <TD>
                    <span className="font-medium">{p.engine}</span> <Mono className="text-muted">{p.version}</Mono>
                    <div className="text-xs text-muted">{t("Installed {when}", { when: formatTime(p.installed_at) })}</div>
                  </TD>
                  <TD>
                    <Mono>{p.package}</Mono>
                  </TD>
                  <TD>
                    <SignatureBadge profile={p} />
                  </TD>
                  <TD>
                    <span className="flex flex-wrap gap-1">
                      {p.permissions.length === 0 && <span className="text-muted">{t("None")}</span>}
                      {p.permissions.map((perm) => (
                        <Badge key={perm} tone="muted" title={t(permissionText[perm])}>
                          {perm}
                        </Badge>
                      ))}
                    </span>
                  </TD>
                  <TD>
                    <Mono>{p.sockets.map((s) => `${s.default_port}/${s.network}`).join(", ") || "—"}</Mono>
                  </TD>
                  <TD>
                    {p.enabled ? (
                      <Badge tone="ok" title={p.approved_by ? t("Approved by {who}", { who: p.approved_by }) : undefined}>
                        {t("Enabled")}
                      </Badge>
                    ) : (
                      <Badge tone="muted">{t("Disabled")}</Badge>
                    )}
                  </TD>
                  <TD className="text-right">
                    {p.enabled ? (
                      <Button size="sm" variant="ghost" disabled={update.isPending} onClick={() => disable(p)}>
                        <PowerOff /> {t("Disable")}
                      </Button>
                    ) : (
                      <Button size="sm" disabled={update.isPending} onClick={() => setApproving(p)}>
                        <Power /> {t("Enable")}
                      </Button>
                    )}
                  </TD>
                </TR>
              ))}
            </TBody>
          </Table>
        )}
      </Card>
      {approving && <ApproveDialog plugin={approving} onClose={() => setApproving(null)} />}
    </>
  );
}

/** The setting that lets an admin enable unsigned plugins (D84). */
function UnsignedCard() {
  const t = useT();
  const { data: settings } = useSettings();
  const update = useUpdateSettings();
  // What was asked shows at once; the server's answer replaces it.
  const [asked, setAsked] = useState<boolean | null>(null);
  if (!settings) return null;
  return (
    <Card className="mb-4">
      <div className="flex flex-col gap-1 p-4">
        <Checkbox
          checked={asked ?? settings.allow_unsigned_plugins ?? false}
          disabled={update.isPending}
          onChange={(e) => {
            setAsked(e.target.checked);
            update.mutate(
              { allow_unsigned_plugins: e.target.checked },
              {
                onSuccess: (s) => toast(s.allow_unsigned_plugins ? t("Unsigned plugins can be enabled") : t("Unsigned plugins are disabled"), "ok"),
                onError: (err) => toast(errorMessage(err), "error"),
                onSettled: () => setAsked(null),
              },
            );
          }}
          label={t("Allow unsigned plugins")}
        />
        <p className="text-xs text-muted">
          {t(
            "A plugin no trusted key signed can be enabled only while this is on; turning it off disables them. Use it for plugins you build yourself.",
          )}
        </p>
      </div>
    </Card>
  );
}

/** Enabling a plugin approves the permissions it asks for. */
function ApproveDialog({ plugin: p, onClose }: { plugin: Plugin; onClose: () => void }) {
  const t = useT();
  const update = useUpdatePlugin();
  const enable = () =>
    update.mutate(
      { id: p.id, enabled: true },
      {
        onSuccess: () => {
          toast(t("{name} is enabled: profiles can use engine {name}.", { name: p.engine }), "ok");
          onClose();
        },
        onError: (err) => toast(errorMessage(err), "error"),
      },
    );
  return (
    <Dialog
      open
      onClose={onClose}
      title={t("Enable {name} {version}", { name: p.engine, version: p.version })}
      description={t("Profiles can then use engine {name}, and the cameras that do run its program with these permissions:", { name: p.engine })}
      footer={
        <>
          <Button onClick={onClose}>{t("Cancel")}</Button>
          <Button variant="primary" onClick={enable} disabled={update.isPending}>
            {update.isPending ? t("Saving…") : t("Approve and enable")}
          </Button>
        </>
      }
    >
      <div className="flex flex-col gap-3 text-[13px]">
        {p.permissions.length === 0 ? (
          <p className="text-muted">{t("It asks for no permission: it cannot reach its cameras' ports, state or events.")}</p>
        ) : (
          <ul className="flex flex-col gap-1.5">
            {p.permissions.map((perm) => (
              <li key={perm} className="flex items-baseline gap-2">
                <Mono className="w-28 shrink-0">{perm}</Mono>
                <span>{t(permissionText[perm])}</span>
              </li>
            ))}
          </ul>
        )}
        {p.signature_status !== "official" && p.signature_status !== "trusted" && (
          <Notice tone="warn">
            <span className="flex items-center gap-2">
              <ShieldAlert className="size-4" />
              {t("Nothing proves who made this plugin or that it is unchanged. Enable it only if you trust where it came from.")}
            </span>
          </Notice>
        )}
        <p className="text-xs text-muted">
          {t("Whatever it is granted, it runs confined: it reads only its own files, cannot connect anywhere without net.connect and dies with its camera.")}
        </p>
      </div>
    </Dialog>
  );
}
