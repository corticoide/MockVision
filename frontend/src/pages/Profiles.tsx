import { Archive, ArchiveRestore, Boxes, CheckCircle2, CircleSlash, Upload, XCircle } from "lucide-react";
import { useRef, useState } from "react";
import { ApiError, errorMessage, type ImportReport } from "@/api/client";
import { useImportPackage, useProfileAction, useProfiles } from "@/api/queries";
import { Badge, LevelBadge, Mono } from "@/components/badges";
import { toast } from "@/components/toast";
import { Button } from "@/components/ui/button";
import { Card, CardHeader, Empty, Notice, PageHeader } from "@/components/ui/card";
import { Table, TBody, TD, TH, THead, TR } from "@/components/ui/table";
import { useT } from "@/lib/i18n";
import { cn, formatTime } from "@/lib/utils";

export function ProfilesPage() {
  const { data: profiles, isLoading, error } = useProfiles();
  const importer = useImportPackage();
  const action = useProfileAction();
  const input = useRef<HTMLInputElement>(null);
  const [report, setReport] = useState<{ report: ImportReport; ok: boolean; message: string } | null>(null);
  const t = useT();

  const onFile = (file: File | undefined) => {
    if (!file) return;
    setReport(null);
    importer.mutate(file, {
      onSuccess: (res) => {
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

  return (
    <>
      <PageHeader
        title={t("Profiles")}
        description={t("Camera models: what each one serves and how. A profile imported by hand starts as a draft.")}
        actions={
          <>
            <input
              ref={input}
              type="file"
              accept=".yaml,.yml,.mvpkg"
              className="hidden"
              onChange={(e) => onFile(e.target.files?.[0])}
            />
            <Button variant="primary" onClick={() => input.current?.click()} disabled={importer.isPending}>
              <Upload /> {importer.isPending ? t("Validating…") : t("Import profile")}
            </Button>
          </>
        }
      />
      {error && <Notice tone="error">{errorMessage(error)}</Notice>}
      {report && <ReportCard {...report} onClose={() => setReport(null)} />}
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
                <TH className="text-right">{t("Actions")}</TH>
              </tr>
            </THead>
            <TBody>
              {profiles.map((p) => (
                <TR key={p.id} className={cn(p.archived && "opacity-60")}>
                  <TD>
                    <div className="flex items-center gap-2">
                      <span className="font-medium">{p.name}</span>
                      {p.archived && (
                        <Badge tone="muted" icon={<Archive />}>
                          {t("Archived")}
                        </Badge>
                      )}
                    </div>
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
                    <Badge tone={p.signature_status === "valid" ? "ok" : "muted"}>{p.signature_status}</Badge>
                  </TD>
                  <TD className="text-right">
                    <Mono>{p.camera_count}</Mono>
                  </TD>
                  <TD>
                    <Mono>{formatTime(p.created_at)}</Mono>
                  </TD>
                  <TD className="text-right">
                    <Button
                      size="sm"
                      variant="ghost"
                      disabled={action.isPending}
                      onClick={() =>
                        action.mutate(
                          { id: p.profile_id, version: p.version, action: p.archived ? "unarchive" : "archive" },
                          { onError: (err) => toast(errorMessage(err), "error") },
                        )
                      }
                    >
                      {p.archived ? <ArchiveRestore /> : <Archive />} {p.archived ? t("Unarchive") : t("Archive")}
                    </Button>
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

function ReportCard({ report, ok, message, onClose }: { report: ImportReport; ok: boolean; message: string; onClose: () => void }) {
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
            {report.kind} · sha256 {report.sha256.slice(0, 16)}… · signature {report.signature}
            {report.level ? ` · level ${report.level}` : ""}
          </Mono>
        }
        actions={
          <Button size="sm" variant="ghost" onClick={onClose}>
            {t("Dismiss")}
          </Button>
        }
      />
      <div className="grid grid-cols-[220px_minmax(0,1fr)] gap-6 px-4 py-3 text-[13px]">
        <ol className="flex flex-col gap-1">
          {report.steps.map((s) => (
            <li key={s.name} className="flex items-center gap-2" title={s.note}>
              {s.status === "passed" ? (
                <CheckCircle2 className="size-3.5 text-ok" />
              ) : s.status === "failed" ? (
                <XCircle className="size-3.5 text-error" />
              ) : (
                <CircleSlash className="size-3.5 text-muted" />
              )}
              <span className={cn(s.status === "skipped" && "text-muted")}>{s.name}</span>
            </li>
          ))}
        </ol>
        <div className="flex min-w-0 flex-col gap-1">
          {report.problems.length === 0 ? (
            <span className="text-muted">{t("No problems found.")}</span>
          ) : (
            report.problems.map((p, i) => (
              <div key={i} className="flex gap-2">
                <Badge tone={p.severity === "error" ? "error" : "warn"}>{p.severity}</Badge>
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
    </Card>
  );
}
