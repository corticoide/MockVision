import { Archive, ArchiveRestore, ArrowLeft, Copy, Download, GitCompare } from "lucide-react";
import { type FormEvent, type ReactNode, useState } from "react";
import { ApiError, errorMessage, type ProfileChange, type SelfTestResult } from "@/api/client";
import { exportURL, useDuplicateProfile, useProfile, useProfileAction, useProfileDiff, useProfiles } from "@/api/queries";
import { Badge, LevelBadge, Mono } from "@/components/badges";
import { toast } from "@/components/toast";
import { Button } from "@/components/ui/button";
import { Card, CardHeader, Empty, Notice, PageHeader } from "@/components/ui/card";
import { Dialog } from "@/components/ui/dialog";
import { Field, Input } from "@/components/ui/form";
import { Table, TBody, TD, TH, THead, TR } from "@/components/ui/table";
import { useT } from "@/lib/i18n";
import { Link, navigate } from "@/lib/router";
import { formatTime } from "@/lib/utils";
import { profileHref, ReportBody, SignatureBadge, SourceBadge } from "./Profiles";

export function ProfilePage({ id, version }: { id: string; version: string }) {
  const t = useT();
  const { data: p, error } = useProfile({ id, version });
  const { data: all } = useProfiles();
  const action = useProfileAction();
  const [duplicating, setDuplicating] = useState(false);
  const [compare, setCompare] = useState<string | null>(null);

  if (error) return <Notice tone="error">{errorMessage(error)}</Notice>;
  if (!p) return <Empty title={t("Loading profile…")} />;
  const versions = (all ?? []).filter((v) => v.profile_id === id && v.version !== version);
  const rep = p.report;

  return (
    <>
      <PageHeader
        title={p.name}
        description={
          <span className="flex items-center gap-2">
            <Link href="/profiles" className="inline-flex items-center gap-1 hover:underline">
              <ArrowLeft className="size-3.5" /> {t("Profiles")}
            </Link>
            <Mono>
              {p.profile_id}@{p.version}
            </Mono>
          </span>
        }
        actions={
          <>
            <a href={exportURL(id, version)} download className="inline-flex">
              <Button>
                <Download /> {t("Export")}
              </Button>
            </a>
            <Button onClick={() => setDuplicating(true)}>
              <Copy /> {t("Duplicate")}
            </Button>
            <Button
              disabled={action.isPending}
              onClick={() =>
                action.mutate(
                  { id, version, action: p.archived ? "unarchive" : "archive" },
                  { onError: (err) => toast(errorMessage(err), "error") },
                )
              }
            >
              {p.archived ? <ArchiveRestore /> : <Archive />} {p.archived ? t("Unarchive") : t("Archive")}
            </Button>
          </>
        }
      />
      {p.source === "catalog" && <Notice tone="info">{t("Shipped with MockVision, read only: duplicate it to change it.")}</Notice>}
      <div className="mt-4 grid grid-cols-[minmax(0,1fr)_minmax(0,1fr)] items-start gap-4">
        <Card>
          <CardHeader title={t("Package")} />
          <dl className="grid grid-cols-[150px_minmax(0,1fr)] gap-x-4 gap-y-2 p-4 text-[13px]">
            <Row label={t("Level")}>
              <LevelBadge level={p.level} />
            </Row>
            <Row label={t("Signature")}>
              <span className="flex items-center gap-2">
                <SignatureBadge profile={p} />
                {rep?.key_id && <Mono className="text-muted">{t("key {id}", { id: rep.key_id })}</Mono>}
              </span>
            </Row>
            <Row label={t("Source")}>
              {p.source === "upload" ? t("Imported") : <SourceBadge source={p.source} />}
            </Row>
            {p.extends && (
              <Row label={t("Extends")}>
                <Link href={profileHref(p.extends.split("@")[0], p.extends.split("@")[1])} className="font-mono hover:underline">
                  {p.extends}
                </Link>
              </Row>
            )}
            <Row label={t("Model")}>
              {p.vendor} {p.model}
            </Row>
            <Row label={t("Firmware")}>{p.firmware.join(", ") || "—"}</Row>
            <Row label={t("Cameras")}>{all?.find((v) => v.id === p.id)?.camera_count ?? 0}</Row>
            <Row label={t("Imported")}>
              <Mono>{formatTime(p.created_at)}</Mono>
            </Row>
          </dl>
        </Card>
        <Card>
          <CardHeader title={t("Versions")} description={t("Cameras stay on their version until you move them, from their page.")} />
          {versions.length === 0 ? (
            <p className="p-4 text-[13px] text-muted">{t("This is the only version installed.")}</p>
          ) : (
            <Table>
              <TBody>
                {versions.map((v) => (
                  <TR key={v.id}>
                    <TD>
                      <Link href={profileHref(v.profile_id, v.version)} className="font-mono hover:underline">
                        {v.version}
                      </Link>
                    </TD>
                    <TD>
                      <LevelBadge level={v.level} />
                    </TD>
                    <TD>
                      <Mono className="text-muted">{formatTime(v.created_at)}</Mono>
                    </TD>
                    <TD className="text-right">
                      <Button size="sm" variant="ghost" onClick={() => setCompare(compare === v.version ? null : v.version)}>
                        <GitCompare /> {compare === v.version ? t("Hide") : t("Compare")}
                      </Button>
                    </TD>
                  </TR>
                ))}
              </TBody>
            </Table>
          )}
        </Card>
        {compare && (
          <div className="col-span-2">
            <DiffCard id={id} from={version} to={compare} />
          </div>
        )}
        {rep && (
          <div className="col-span-2">
            <Card>
              <CardHeader title={t("Import report")} description={<Mono>sha256 {rep.sha256}</Mono>} />
              <ReportBody report={rep} />
            </Card>
          </div>
        )}
        {rep && (
          <div className="col-span-2">
            <SelfTestCard results={rep.self_test?.results} verified={rep.verified} />
          </div>
        )}
      </div>
      <DuplicateDialog open={duplicating} onClose={() => setDuplicating(false)} id={id} version={version} name={p.name} />
    </>
  );
}

function Row({ label, children }: { label: string; children: ReactNode }) {
  return (
    <>
      <dt className="text-muted">{label}</dt>
      <dd className="min-w-0">{children}</dd>
    </>
  );
}

const sectionLabels: Record<string, string> = {
  state: "Parameter",
  streams: "Stream",
  engines: "Engine",
  routes: "Route",
  events: "Event",
  identity: "Identity",
  vca: "Analytics",
  storage: "Storage",
};

/** What changes from one version to another (D05). */
export function DiffCard({ id, from, to }: { id: string; from: string; to: string }) {
  const t = useT();
  const { data, error, isLoading } = useProfileDiff(id, from, to);
  return (
    <Card>
      <CardHeader title={t("From {from} to {to}", { from, to })} />
      {error ? (
        <Notice tone="error">{errorMessage(error)}</Notice>
      ) : isLoading || !data ? (
        <p className="p-4 text-[13px] text-muted">{t("Comparing…")}</p>
      ) : (
        <ChangeList changes={data.changes} />
      )}
    </Card>
  );
}

export function ChangeList({ changes }: { changes: ProfileChange[] }) {
  const t = useT();
  if (changes.length === 0) return <p className="p-4 text-[13px] text-muted">{t("The two versions are the same.")}</p>;
  return (
    <Table>
      <THead>
        <tr>
          <TH>{t("What")}</TH>
          <TH>{t("Name")}</TH>
          <TH>{t("Change")}</TH>
          <TH>{t("Details")}</TH>
        </tr>
      </THead>
      <TBody>
        {changes.map((c) => (
          <TR key={c.section + c.key}>
            <TD className="text-muted">{t(sectionLabels[c.section] ?? c.section)}</TD>
            <TD>
              <Mono>{c.key}</Mono>
            </TD>
            <TD>
              <Badge tone={c.kind === "added" ? "ok" : c.kind === "removed" ? "error" : "warn"}>
                {c.kind === "added" ? t("added") : c.kind === "removed" ? t("removed") : t("changed")}
              </Badge>
            </TD>
            <TD className="text-xs">
              {(c.details ?? []).map((d) => (
                <div key={d} className="font-mono break-all">
                  {d}
                </div>
              ))}
            </TD>
          </TR>
        ))}
      </TBody>
    </Table>
  );
}

/** The replay of the package's fixtures, and what it verified (D88). */
function SelfTestCard({ results, verified }: { results?: SelfTestResult[]; verified?: Record<string, string> }) {
  const t = useT();
  const entries = Object.entries(verified ?? {}).sort();
  const count = (s: string) => entries.filter(([, v]) => v === s).length;
  return (
    <Card>
      <CardHeader
        title={t("Self-test")}
        description={
          results
            ? t("Recordings of the real device, replayed against a camera of this profile on a network that reaches nothing.")
            : t("The package has no recordings of a real device: its routes and events are declared, not verified.")
        }
        actions={
          entries.length > 0 && (
            <span className="flex gap-1.5">
              <Badge tone="ok">{t("{n} verified", { n: count("verified") })}</Badge>
              {count("failed") > 0 && <Badge tone="error">{t("{n} failed", { n: count("failed") })}</Badge>}
              <Badge tone="muted">{t("{n} declared", { n: count("declared") })}</Badge>
            </span>
          )
        }
      />
      {results && results.length > 0 && (
        <Table>
          <THead>
            <tr>
              <TH>{t("Recording")}</TH>
              <TH>{t("Covers")}</TH>
              <TH>{t("Result")}</TH>
              <TH>{t("Details")}</TH>
            </tr>
          </THead>
          <TBody>
            {results.map((r) => (
              <TR key={r.id}>
                <TD>
                  <Mono>{r.id}</Mono>
                </TD>
                <TD>
                  <Mono className="text-muted">{r.covers}</Mono>
                </TD>
                <TD>
                  <Badge tone={r.status === "passed" ? "ok" : r.status === "failed" ? "error" : "muted"}>
                    {r.status === "passed" ? t("Matches") : r.status === "failed" ? t("Differs") : t("Not checked")}
                  </Badge>
                </TD>
                <TD className="text-xs break-all">{r.detail}</TD>
              </TR>
            ))}
          </TBody>
        </Table>
      )}
      {entries.length > 0 && (
        <div className="flex flex-wrap gap-1.5 border-t border-border p-4">
          {entries.map(([k, v]) => (
            <Badge key={k} tone={v === "verified" ? "ok" : v === "failed" ? "error" : "muted"} title={v}>
              {k.replace(/^route:/, t("route") + " ").replace(/^event:/, t("event") + " ")}
            </Badge>
          ))}
        </div>
      )}
    </Card>
  );
}

function DuplicateDialog({ open, onClose, id, version, name }: { open: boolean; onClose: () => void; id: string; version: string; name: string }) {
  const t = useT();
  const dup = useDuplicateProfile();
  const [profileID, setProfileID] = useState("");
  const [copyVersion, setCopyVersion] = useState("0.1.0");
  const [copyName, setCopyName] = useState("");
  const [errors, setErrors] = useState<Record<string, string>>({});
  const submit = (e: FormEvent) => {
    e.preventDefault();
    setErrors({});
    dup.mutate(
      { id, version, body: { profile_id: profileID.trim(), version: copyVersion.trim() || undefined, name: copyName.trim() || undefined } },
      {
        onSuccess: (res) => {
          onClose();
          if (!res.profile) return;
          toast(t("Profile {name} ready", { name: res.profile.name }), "ok");
          navigate(profileHref(res.profile.profile_id, res.profile.version));
        },
        onError: (err) => {
          if (err instanceof ApiError) setErrors(err.fieldErrors());
          toast(errorMessage(err), "error");
        },
      },
    );
  };
  return (
    <Dialog
      open={open}
      onClose={onClose}
      title={t("Duplicate {id}@{version}", { id, version })}
      description={t("The copy is a profile of your own, unsigned: export it, change it and import its next version.")}
      footer={
        <>
          <Button onClick={onClose}>{t("Cancel")}</Button>
          <Button type="submit" form="duplicate-profile" variant="primary" disabled={dup.isPending || !profileID.trim()}>
            {dup.isPending ? t("Duplicating…") : t("Duplicate")}
          </Button>
        </>
      }
    >
      <form id="duplicate-profile" onSubmit={submit} className="grid grid-cols-2 gap-x-4 gap-y-3">
        <Field label={t("Profile ID")} error={errors["profile_id"]} hint={t("vendor/model, in lower case")} className="col-span-2">
          <Input value={profileID} onChange={(e) => setProfileID(e.target.value)} placeholder="acme/my-camera" className="font-mono" autoFocus />
        </Field>
        <Field label={t("Version")} error={errors["version"]}>
          <Input value={copyVersion} onChange={(e) => setCopyVersion(e.target.value)} className="font-mono" />
        </Field>
        <Field label={t("Name")} error={errors["name"]}>
          <Input value={copyName} onChange={(e) => setCopyName(e.target.value)} placeholder={t("{name} (copy)", { name })} />
        </Field>
      </form>
    </Dialog>
  );
}
