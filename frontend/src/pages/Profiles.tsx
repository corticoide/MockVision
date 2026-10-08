import { Archive, BadgeCheck, Boxes, CheckCircle2, CircleSlash, Clock, Library, ShieldAlert, ShieldCheck, ShieldQuestion, Upload, XCircle } from "lucide-react";
import { useRef, useState } from "react";
import { ApiError, errorMessage, type ImportReport, type Profile } from "@/api/client";
import { useImportPackage, useProfiles } from "@/api/queries";
import { Badge, LevelBadge, Mono } from "@/components/badges";
import { toast } from "@/components/toast";
import { Button } from "@/components/ui/button";
import { Card, CardHeader, Empty, Notice, PageHeader } from "@/components/ui/card";
import { Table, TBody, TD, TH, THead, TR } from "@/components/ui/table";
import { useT } from "@/lib/i18n";
import { Link, navigate } from "@/lib/router";
import { cn, formatTime } from "@/lib/utils";

/** The panel page of a profile version. */
export function profileHref(id: string, version: string) {
  return `/profiles/${id}/${encodeURIComponent(version)}`;
}

/** Imports a package picked in a file input: a profile or a plugin. The
 * report of the last import stays until dismissed. */
export function useImportFlow() {
  const importer = useImportPackage();
  const input = useRef<HTMLInputElement>(null);
  const [report, setReport] = useState<{ report: ImportReport; ok: boolean; message: string } | null>(null);
  const t = useT();

  const onFile = (file: File | undefined) => {
    if (!file) return;
    setReport(null);
    importer.mutate(file, {
      onSuccess: (res) => {
        if ("job" in res) {
          // Still queued or running after a minute: it goes on as a job.
          toast(t("The import of {name} goes on in the background; follow it in Jobs.", { name: file.name }), "info");
          navigate("/jobs");
          return;
        }
        if (res.plugin) {
          const id = `${res.plugin.engine} ${res.plugin.version}`;
          setReport({ report: res.report, ok: true, message: res.created ? t("Imported plugin {id}", { id }) : t("{id} was already installed", { id }) });
          toast(t("Plugin {name} installed: review its permissions and enable it in Plugins.", { name: res.plugin.engine }), "ok");
          return;
        }
        if (!res.profile) return;
        const id = `${res.profile.profile_id}@${res.profile.version}`;
        setReport({
          report: res.report,
          ok: true,
          message: res.created ? t("Imported {id}", { id }) : t("{id} was already installed", { id }),
        });
        toast(t("Profile {name} ready", { name: res.profile.name }), "ok");
      },
      onError: (err) => {
        const rep = err instanceof ApiError ? err.problem.report : undefined;
        if (rep) setReport({ report: rep, ok: false, message: errorMessage(err) });
        else toast(errorMessage(err), "error");
      },
    });
    if (input.current) input.current.value = "";
  };
  const picker = (
    <input ref={input} type="file" accept=".yaml,.yml,.mvpkg" className="hidden" onChange={(e) => onFile(e.target.files?.[0])} />
  );
  return { picker, pick: () => input.current?.click(), pending: importer.isPending, report, dismiss: () => setReport(null) };
}

export function ProfilesPage() {
  const { data: profiles, isLoading, error } = useProfiles();
  const flow = useImportFlow();
  const t = useT();

  return (
    <>
      <PageHeader
        title={t("Profiles")}
        description={t("Camera models: what each one serves and how. The official catalog comes with MockVision; a profile imported by hand starts as a draft.")}
        actions={
          <>
            {flow.picker}
            <Button variant="primary" onClick={flow.pick} disabled={flow.pending}>
              <Upload /> {flow.pending ? t("Validating…") : t("Import profile")}
            </Button>
          </>
        }
      />
      {error && <Notice tone="error">{errorMessage(error)}</Notice>}
      {flow.report && <ReportCard {...flow.report} onClose={flow.dismiss} />}
      <Card>
        {isLoading ? (
          <Empty title={t("Loading profiles…")} />
        ) : !profiles?.length ? (
          <Empty icon={<Boxes />} title={t("No profiles installed")}>
            {t("Import a profile.yaml or a .mvpkg package, for example")} <span className="font-mono">profiles/milesight-demo.yaml</span>.
          </Empty>
        ) : (
          <Table>
            <THead>
              <tr>
                <TH>{t("Name")}</TH>
                <TH>{t("Profile")}</TH>
                <TH>{t("Firmware")}</TH>
                <TH>{t("Level")}</TH>
                <TH>{t("Signature")}</TH>
                <TH className="text-right">{t("Cameras")}</TH>
                <TH>{t("Imported")}</TH>
              </tr>
            </THead>
            <TBody>
              {profiles.map((p) => (
                <TR key={p.id} className={cn(p.archived && "opacity-60")}>
                  <TD>
                    <div className="flex items-center gap-2">
                      <Link href={profileHref(p.profile_id, p.version)} className="font-medium hover:underline">
                        {p.name}
                      </Link>
                      <SourceBadge source={p.source} />
                      {p.archived && (
                        <Badge tone="muted" icon={<Archive />}>
                          {t("Archived")}
                        </Badge>
                      )}
                    </div>
                    {p.extends && <div className="text-xs text-muted">{t("extends {parent}", { parent: p.extends })}</div>}
                  </TD>
                  <TD>
                    <Mono>
                      {p.profile_id}@{p.version}
                    </Mono>
                  </TD>
                  <TD className="text-muted">{p.firmware.join(", ") || "—"}</TD>
                  <TD>
                    <LevelBadge level={p.level} />
                  </TD>
                  <TD>
                    <SignatureBadge profile={p} />
                  </TD>
                  <TD className="text-right">
                    <Mono>{p.camera_count}</Mono>
                  </TD>
                  <TD>
                    <Mono>{formatTime(p.created_at)}</Mono>
                  </TD>
                </TR>
              ))}
            </TBody>
          </Table>
        )}
      </Card>
    </>
  );
}

/** Where a package came from: the official catalog (read only) or a copy. */
export function SourceBadge({ source }: { source: Profile["source"] }) {
  const t = useT();
  if (source === "catalog")
    return (
      <Badge tone="info" icon={<Library />} title={t("Shipped with MockVision, read only: duplicate it to change it.")}>
        {t("Catalog")}
      </Badge>
    );
  if (source === "duplicate") return <Badge tone="muted">{t("Copy")}</Badge>;
  return null;
}

/** Who signed a package, as the node judged it when it was imported (D83, D84). */
export function SignatureBadge({ profile: p }: { profile: Pick<Profile, "signature_status" | "signer"> }) {
  const t = useT();
  switch (p.signature_status) {
    case "official":
      return (
        <Badge tone="ok" icon={<BadgeCheck />} title={p.signer}>
          {t("Official")}
        </Badge>
      );
    case "trusted":
      return (
        <Badge tone="ok" icon={<ShieldCheck />} title={p.signer}>
          {t("Signed by {name}", { name: p.signer ?? "" })}
        </Badge>
      );
    case "invalid":
      return (
        <Badge tone="error" icon={<ShieldAlert />}>
          {t("Invalid signature")}
        </Badge>
      );
    default:
      return (
        <Badge tone="muted" icon={<ShieldQuestion />} title={t("Nothing proves who made the package or that it is unchanged.")}>
          {t("Unsigned")}
        </Badge>
      );
  }
}

export function ReportCard({ report, ok, message, onClose }: { report: ImportReport; ok: boolean; message: string; onClose: () => void }) {
  const t = useT();
  return (
    <Card className={cn("mb-4", ok ? "border-ok/40" : "border-error/40")}>
      <CardHeader
        title={
          <span className="flex items-center gap-2">
            {ok ? <CheckCircle2 className="size-4 text-ok" /> : <XCircle className="size-4 text-error" />}
            {message}
          </span>
        }
        description={
          <Mono>
            {report.kind} · sha256 {report.sha256.slice(0, 16)}… · {t("signature")} {report.signature}
            {report.level ? ` · ${t("level")} ${report.level}` : ""}
          </Mono>
        }
        actions={
          <Button size="sm" variant="ghost" onClick={onClose}>
            {t("Dismiss")}
          </Button>
        }
      />
      <ReportBody report={report} />
    </Card>
  );
}

/** The steps of an import and what they found. */
export function ReportBody({ report }: { report: ImportReport }) {
  const t = useT();
  return (
    <div className="grid grid-cols-[240px_minmax(0,1fr)] gap-6 px-4 py-3 text-[13px]">
      <ol className="flex flex-col gap-1">
        {report.steps.map((s) => (
          <li key={s.name} className="flex items-center gap-2" title={s.note}>
            {s.status === "passed" ? (
              <CheckCircle2 className="size-3.5 text-ok" />
            ) : s.status === "failed" ? (
              <XCircle className="size-3.5 text-error" />
            ) : s.status === "pending" ? (
              <Clock className="size-3.5 text-info" />
            ) : (
              <CircleSlash className="size-3.5 text-muted" />
            )}
            <span className={cn(s.status === "skipped" && "text-muted")}>{t(stepLabels[s.name] ?? s.name)}</span>
          </li>
        ))}
      </ol>
      <div className="flex min-w-0 flex-col gap-1">
        {report.problems.length === 0 ? (
          <span className="text-muted">{t("No problems found.")}</span>
        ) : (
          report.problems.map((p, i) => (
            <div key={i} className="flex gap-2">
              <Badge tone={p.severity === "error" ? "error" : "warn"}>{p.severity === "error" ? t("error") : t("warning")}</Badge>
              {p.file && (
                <Mono className="text-muted">
                  {p.file}
                  {p.line ? `:${p.line}` : ""}
                </Mono>
              )}
              <span className="min-w-0">{p.message}</span>
            </div>
          ))
        )}
      </div>
    </div>
  );
}

const stepLabels: Record<string, string> = {
  integrity: "Integrity",
  signature: "Signature",
  compatibility: "Compatibility",
  yaml: "YAML",
  schema: "Schema",
  lint: "Lint",
  inheritance: "Inheritance",
  templates: "Templates",
  selftest: "Self-test",
  plugin: "Plugin",
  describe: "Program run",
};
