import { ChevronDown, ChevronRight, CircleHelp, ListChecks, Play, Square, Wand2 } from "lucide-react";
import { Fragment, useEffect, useState } from "react";
import { errorMessage, type Job, type JobEvent } from "@/api/client";
import { useJobTopic } from "@/api/live";
import { type JobFilter, useCreateJob, useJob, useJobAction, useJobs } from "@/api/queries";
import { JobStatusBadge, Mono } from "@/components/badges";
import { ProgressBar } from "@/components/Sparkline";
import { toast } from "@/components/toast";
import { Button } from "@/components/ui/button";
import { Card, Empty, Notice, PageHeader } from "@/components/ui/card";
import { Select } from "@/components/ui/form";
import { Table, TBody, TD, TH, THead, TR } from "@/components/ui/table";
import { type Translate, useT } from "@/lib/i18n";
import { setSearch, useSearch } from "@/lib/router";
import { formatDuration, formatPercent, formatTime } from "@/lib/utils";

const types: Job["type"][] = ["rendition", "import", "renditions.prepare"];

export function typeLabel(type: string, t: Translate) {
  switch (type) {
    case "rendition":
      return t("Rendition");
    case "import":
      return t("Import");
    case "renditions.prepare":
      return t("Prepare renditions");
  }
  return type;
}

/** Options the node offers are shown in the panel's language by their ID. */
function optionLabel(id: string, label: string, t: Translate) {
  switch (id) {
    case "retry":
      return t("Retry");
    case "skip":
      return t("Skip it");
    case "stop":
      return t("Stop the job");
  }
  return label;
}

const open = (j: Job) => ["queued", "running", "waiting", "interrupted"].includes(j.status);

export function JobsPage() {
  const t = useT();
  const search = useSearch();
  const filter: JobFilter = {
    status: search.get("status") ?? "",
    type: (types as string[]).includes(search.get("type") ?? "") ? (search.get("type") as Job["type"]) : undefined,
  };
  const { data, isLoading, error } = useJobs(filter);
  const { data: waitingPage } = useJobs({ status: "waiting" });
  const create = useCreateJob();
  const jobs = data?.items ?? [];
  const waiting = waitingPage?.items ?? [];

  const prepare = () =>
    create.mutate(
      { type: "renditions.prepare" },
      {
        onSuccess: (j) => toast(t("{title}: queued", { title: j.title }), "ok"),
        onError: (err) => toast(errorMessage(err), "error"),
      },
    );

  return (
    <>
      <PageHeader
        title={t("Jobs")}
        description={t(
          "Encodings and imports run in the background with their progress. Closing the browser stops nothing; after a restart, interrupted jobs resume on request.",
        )}
        actions={
          <Button
            variant="primary"
            onClick={prepare}
            disabled={create.isPending}
            title={t("Encode now every rendition the cameras need, so starting them waits for nothing")}
          >
            <Wand2 /> {t("Prepare renditions")}
          </Button>
        }
      />
      {waiting.map((j) => (
        <QuestionCard key={j.id} job={j} />
      ))}
      <div className="mb-3 flex flex-wrap items-center gap-2">
        <Select value={filter.status ?? ""} onChange={(e) => setSearch({ status: e.target.value })} className="w-44" aria-label={t("State")}>
          <option value="">{t("Every job")}</option>
          <option value="active">{t("Active jobs")}</option>
          <option value="finished">{t("Finished jobs")}</option>
          <option value="failed">{t("Failed jobs")}</option>
          <option value="interrupted">{t("Interrupted jobs")}</option>
        </Select>
        <Select value={filter.type ?? ""} onChange={(e) => setSearch({ type: e.target.value })} className="w-48" aria-label={t("Type")}>
          <option value="">{t("Every type")}</option>
          {types.map((ty) => (
            <option key={ty} value={ty}>
              {typeLabel(ty, t)}
            </option>
          ))}
        </Select>
      </div>
      {error && <Notice tone="error">{errorMessage(error)}</Notice>}
      <Card>
        {isLoading ? (
          <Empty title={t("Loading jobs…")} />
        ) : jobs.length === 0 ? (
          <Empty icon={<ListChecks />} title={t("No jobs")}>
            {t("Jobs appear when a camera needs a stream encoded, when a profile is imported, or when you prepare renditions.")}
          </Empty>
        ) : (
          <JobTable jobs={jobs} />
        )}
      </Card>
    </>
  );
}

/** A decision a job waits for, with its options. */
function QuestionCard({ job }: { job: Job }) {
  const t = useT();
  const action = useJobAction();
  const q = job.question;
  if (!q) return null;
  return (
    <div className="mb-3">
      <Notice tone="warn">
        <div className="flex flex-wrap items-center gap-3">
          <CircleHelp className="size-4 shrink-0 text-warn" />
          <div className="min-w-0 flex-1">
            <div className="font-medium">{job.title}</div>
            <div className="text-muted">{q.text}</div>
            <div className="text-xs text-muted">
              {t("Without an answer at {time} the job takes “{option}”.", {
                time: formatTime(q.expires_at),
                option: optionLabel(q.default, q.options.find((o) => o.id === q.default)?.label ?? q.default, t),
              })}
            </div>
          </div>
          <div className="flex gap-1.5">
            {q.options.map((o) => (
              <Button
                key={o.id}
                size="sm"
                variant={o.id === q.default ? "primary" : "secondary"}
                disabled={action.isPending}
                onClick={() =>
                  action.mutate({ id: job.id, action: "answer", answer: o.id }, { onError: (err) => toast(errorMessage(err), "error") })
                }
              >
                {optionLabel(o.id, o.label, t)}
              </Button>
            ))}
          </div>
        </div>
      </Notice>
    </div>
  );
}

function JobTable({ jobs }: { jobs: Job[] }) {
  const t = useT();
  const [expanded, setExpanded] = useState<string | null>(null);
  return (
    <Table>
      <THead>
        <tr>
          <TH className="w-8" />
          <TH>{t("State")}</TH>
          <TH>{t("Job")}</TH>
          <TH className="w-48">{t("Progress")}</TH>
          <TH>{t("By")}</TH>
          <TH>{t("Started")}</TH>
          <TH className="text-right">{t("Took")}</TH>
          <TH className="text-right">{t("Actions")}</TH>
        </tr>
      </THead>
      <TBody>
        {jobs.map((j) => {
          const isOpen = expanded === j.id;
          return (
            <Fragment key={j.id}>
              <TR>
                <TD>
                  <Button variant="ghost" size="icon" onClick={() => setExpanded(isOpen ? null : j.id)} aria-label={t("Details")} aria-expanded={isOpen}>
                    {isOpen ? <ChevronDown /> : <ChevronRight />}
                  </Button>
                </TD>
                <TD>
                  <JobStatusBadge status={j.status} />
                </TD>
                <TD className="max-w-96">
                  <div className="truncate font-medium" title={j.title}>
                    {j.title}
                  </div>
                  <div className="truncate text-xs text-muted" title={j.error || j.step}>
                    {typeLabel(j.type, t)}
                    {j.status === "failed" && j.error ? ` · ${j.error}` : j.step && open(j) ? ` · ${j.step}` : ""}
                  </div>
                </TD>
                <TD>
                  <div className="flex items-center gap-2">
                    <ProgressBar
                      value={j.progress}
                      label={t("Progress of {title}", { title: j.title })}
                      className={j.status === "failed" ? "text-error" : j.status === "completed" ? "text-ok" : "text-info"}
                    />
                    <Mono className="w-10 shrink-0 text-right text-muted">{formatPercent(j.progress * 100, 0)}</Mono>
                  </div>
                </TD>
                <TD className="text-muted">{j.created_by}</TD>
                <TD>
                  <Mono>{formatTime(j.started_at ?? j.created_at)}</Mono>
                </TD>
                <TD className="text-right">
                  <Took job={j} />
                </TD>
                <TD className="text-right">
                  <JobActions job={j} />
                </TD>
              </TR>
              {isOpen && (
                <tr className="border-b border-border/70 bg-bg/40">
                  <td colSpan={8} className="px-4 py-3">
                    <JobHistory id={j.id} />
                  </td>
                </tr>
              )}
            </Fragment>
          );
        })}
      </TBody>
    </Table>
  );
}

/** How long a job took, counting live while it runs or waits for an answer. */
function Took({ job }: { job: Job }) {
  const live = job.status === "running" || job.status === "waiting";
  const [, setTick] = useState(0);
  useEffect(() => {
    if (!live) return;
    const timer = setInterval(() => setTick((n) => n + 1), 1000);
    return () => clearInterval(timer);
  }, [live]);
  const done = job.finished_at && !open(job);
  return <Mono>{job.started_at && (live || done) ? formatDuration(job.started_at, done ? job.finished_at : null) : "—"}</Mono>;
}

function JobActions({ job }: { job: Job }) {
  const t = useT();
  const action = useJobAction();
  const run = (a: "cancel" | "resume") =>
    action.mutate({ id: job.id, action: a }, { onError: (err) => toast(errorMessage(err), "error") });
  return (
    <div className="flex items-center justify-end gap-1">
      {job.status === "interrupted" && (
        <Button size="sm" onClick={() => run("resume")} disabled={action.isPending} title={t("Continue from its last checkpoint")}>
          <Play /> {t("Resume")}
        </Button>
      )}
      {open(job) && (
        <Button
          size="icon"
          variant="ghost"
          onClick={() => run("cancel")}
          disabled={action.isPending}
          title={t("Cancel")}
          aria-label={t("Cancel {title}", { title: job.title })}
        >
          <Square />
        </Button>
      )}
    </div>
  );
}

/** A job's history, growing live, and its result. */
function JobHistory({ id }: { id: string }) {
  const t = useT();
  const { data, isLoading } = useJob(id);
  useJobTopic(id);
  if (isLoading || !data) return <p className="text-[13px] text-muted">{t("Loading…")}</p>;
  const result = JSON.stringify(data.result, null, 2);
  return (
    <div className="grid grid-cols-2 gap-6">
      <section>
        <h3 className="mb-2 text-xs font-medium tracking-wide text-muted uppercase">{t("History")}</h3>
        <ol className="flex max-h-64 flex-col gap-1 overflow-auto text-[13px]">
          {data.events.map((e) => (
            <li key={e.seq} className="flex gap-3">
              <Mono className="shrink-0 text-muted">{formatTime(e.at)}</Mono>
              <span className="min-w-0">{describeEvent(e, t)}</span>
            </li>
          ))}
        </ol>
      </section>
      <section className="min-w-0">
        <h3 className="mb-2 text-xs font-medium tracking-wide text-muted uppercase">{t("Result")}</h3>
        {data.error && <p className="mb-2 text-[13px] text-error">{data.error}</p>}
        {result === "{}" ? (
          <p className="text-[13px] text-muted">{t("Not yet.")}</p>
        ) : (
          <pre className="max-h-64 overflow-auto rounded-sm border border-border bg-bg p-3 font-mono text-[12px]">{result}</pre>
        )}
      </section>
    </div>
  );
}

function describeEvent(e: JobEvent, t: Translate): string {
  const d = e.data as Record<string, unknown>;
  const str = (k: string) => (typeof d[k] === "string" ? (d[k] as string) : "");
  switch (e.kind) {
    case "status": {
      const status = t(statusLabel(str("status")));
      const why = str("error") || str("reason");
      return why ? `${status}: ${why}` : status;
    }
    case "step":
      return t("Step: {step}", { step: str("step") });
    case "log":
      return str("message");
    case "question":
      return t("Asked: {text}", { text: str("text") });
    case "answer":
      return str("by") === "timeout"
        ? t("No answer in time; took “{answer}”", { answer: optionLabel(str("answer"), str("answer"), t) })
        : t("Answered “{answer}”", { answer: optionLabel(str("answer"), str("answer"), t) });
  }
  return e.kind;
}

function statusLabel(status: string) {
  const labels: Record<string, string> = {
    queued: "Queued",
    running: "In progress",
    waiting: "Waiting for you",
    completed: "Completed",
    failed: "Failed",
    canceled: "Canceled",
    interrupted: "Interrupted",
  };
  return labels[status] ?? status;
}
