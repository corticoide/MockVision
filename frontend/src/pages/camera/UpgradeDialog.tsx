import { useEffect, useState } from "react";
import { type Camera, errorMessage, type UpgradeItem, type UpgradePlan } from "@/api/client";
import { useProfiles, useUpgradeProfile } from "@/api/queries";
import { Badge, LevelBadge, Mono } from "@/components/badges";
import { toast } from "@/components/toast";
import { Button } from "@/components/ui/button";
import { Notice } from "@/components/ui/card";
import { Dialog } from "@/components/ui/dialog";
import { Field, Select } from "@/components/ui/form";
import { Table, TBody, TD, TH, THead, TR } from "@/components/ui/table";
import { useT } from "@/lib/i18n";
import { ChangeList } from "@/pages/ProfilePage";

/** The other installed, unarchived versions of a camera's profile. */
export function useOtherVersions(camera: Camera) {
  const { data } = useProfiles();
  return (data ?? []).filter((p) => p.profile_id === camera.profile.id && p.version !== camera.profile.version && !p.archived);
}

/** Moves a camera to another version of its profile, showing first what that does (D05). */
export function UpgradeDialog({ camera, open, onClose }: { camera: Camera; open: boolean; onClose: () => void }) {
  const t = useT();
  const versions = useOtherVersions(camera);
  const [version, setVersion] = useState("");
  const [plan, setPlan] = useState<UpgradePlan | null>(null);
  const planner = useUpgradeProfile(camera.id);
  const applier = useUpgradeProfile(camera.id);

  useEffect(() => {
    if (open && !version && versions.length > 0) setVersion(versions[0].version);
  }, [open, version, versions]);

  useEffect(() => {
    setPlan(null);
    if (!open || !version) return;
    planner.mutate(
      { version, dry_run: true },
      { onSuccess: (res) => setPlan(res.plan), onError: (err) => toast(errorMessage(err), "error") },
    );
    // eslint-disable-next-line react-hooks/exhaustive-deps
  }, [open, version]);

  const apply = () =>
    applier.mutate(
      { version },
      {
        onSuccess: () => {
          toast(t("{name} moved to {version}", { name: camera.name, version }), "ok");
          onClose();
        },
        onError: (err) => toast(errorMessage(err), "error"),
      },
    );

  return (
    <Dialog
      open={open}
      onClose={onClose}
      className="w-[min(860px,calc(100vw-32px))]"
      title={t("Change the profile version of {name}", { name: camera.name })}
      description={t("Values someone set stay when the new version accepts them; the rest follow its defaults.")}
      footer={
        <>
          <Button onClick={onClose}>{t("Cancel")}</Button>
          <Button variant="primary" onClick={apply} disabled={!plan || applier.isPending}>
            {applier.isPending ? t("Applying…") : plan?.restart ? t("Apply and restart") : t("Apply")}
          </Button>
        </>
      }
    >
      <div className="flex flex-col gap-4 text-[13px]">
        <Field label={t("Version")} hint={t("Now {version}", { version: camera.profile.version })}>
          <Select value={version} onChange={(e) => setVersion(e.target.value)}>
            {versions.map((v) => (
              <option key={v.id} value={v.version}>
                {v.version} — {v.level}
              </option>
            ))}
          </Select>
        </Field>
        {versions.find((v) => v.version === version) && (
          <div className="flex items-center gap-2">
            <Mono>
              {camera.profile.id}@{version}
            </Mono>
            <LevelBadge level={versions.find((v) => v.version === version)!.level} />
          </div>
        )}
        {planner.isPending && <p className="text-muted">{t("Working out what changes…")}</p>}
        {plan && (
          <>
            {plan.restart && <Notice tone="warn">{t("The camera is running: it restarts to take the new version.")}</Notice>}
            {[plan.params, plan.protocols, plan.streams, plan.rules, plan.triggers].every((items) => items.every((i) => i.action === "kept")) && (
              <p className="text-muted">{t("Only the version changes: its parameters, protocols, streams and analytics stay as they are.")}</p>
            )}
            <PlanTable title={t("Parameters")} items={plan.params} />
            <PlanTable title={t("Protocols")} items={plan.protocols} />
            <PlanTable title={t("Streams")} items={plan.streams} />
            <PlanTable title={t("Rules")} items={plan.rules} />
            <PlanTable title={t("Triggers")} items={plan.triggers} />
            <details>
              <summary className="cursor-pointer text-muted">{t("Every difference between the versions ({n})", { n: plan.changes.length })}</summary>
              <ChangeList changes={plan.changes} />
            </details>
          </>
        )}
      </div>
    </Dialog>
  );
}

const actionTone: Record<string, "ok" | "warn" | "error" | "info" | "muted"> = {
  kept: "muted",
  default: "info",
  reset: "warn",
  added: "ok",
  dropped: "error",
};

function PlanTable({ title, items }: { title: string; items: UpgradeItem[] }) {
  const t = useT();
  const labels: Record<string, string> = {
    kept: t("kept"),
    default: t("new default"),
    reset: t("back to default"),
    added: t("added"),
    dropped: t("removed"),
  };
  // What stays as it is says nothing.
  const shown = items.filter((i) => i.action !== "kept");
  if (shown.length === 0) return null;
  return (
    <div>
      <h3 className="mb-1 text-xs font-semibold text-muted uppercase">{title}</h3>
      <Table>
        <THead>
          <tr>
            <TH>{t("Name")}</TH>
            <TH>{t("Change")}</TH>
            <TH>{t("Value")}</TH>
          </tr>
        </THead>
        <TBody>
          {shown.map((i) => (
            <TR key={i.key}>
              <TD>
                <Mono>{i.key}</Mono>
              </TD>
              <TD>
                <Badge tone={actionTone[i.action] ?? "muted"} title={i.reason}>
                  {labels[i.action] ?? i.action}
                </Badge>
              </TD>
              <TD className="text-xs">
                {i.value !== undefined && i.value !== 0 && <Mono>{JSON.stringify(i.value)}</Mono>}
                {i.reason && <div className="text-muted">{i.reason}</div>}
              </TD>
            </TR>
          ))}
        </TBody>
      </Table>
    </div>
  );
}
