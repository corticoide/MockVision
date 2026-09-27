import { Camera as CameraIcon, CopyPlus, Play, Plus, RotateCw, Search, Square, Tag, Trash2, X, Zap } from "lucide-react";
import { type ReactNode, useMemo, useState } from "react";
import { type BulkAction, type BulkResult, type Camera, type CameraState, errorMessage } from "@/api/client";
import { useBulkCameras, useCameraAction, useCameras, useDeleteCamera, useNodeMetrics, useTrigger } from "@/api/queries";
import { Badge, LevelBadge, Mono, StateBadge } from "@/components/badges";
import { toast } from "@/components/toast";
import { Button } from "@/components/ui/button";
import { Card, Empty, Notice, PageHeader } from "@/components/ui/card";
import { Input, Select } from "@/components/ui/form";
import { Table, TBody, TD, TH, THead, TR } from "@/components/ui/table";
import { plural, type Translate, useT } from "@/lib/i18n";
import { Link, setSearch, useSearch } from "@/lib/router";
import { formatBytes, formatPercent } from "@/lib/utils";
import { CloneDialog } from "./camera/dialogs";
import { NewCameraDialog } from "./NewCameraDialog";

const states: CameraState[] = ["running", "degraded", "provisioning", "starting", "restarting", "stopping", "error", "stopped"];

interface Filter {
  q: string;
  state: string;
  profile: string;
  tag: string;
}

/** The same filter the node applies to GET /cameras. */
function matches(f: Filter, c: Camera): boolean {
  if (f.state && c.status.state !== f.state) return false;
  if (f.profile && c.profile.id !== f.profile && `${c.profile.id}@${c.profile.version}` !== f.profile) return false;
  if (f.tag && !c.tags.some((t) => t.toLowerCase() === f.tag.toLowerCase())) return false;
  const q = f.q.trim().toLowerCase();
  if (q) return [c.name, c.network.ip, c.network.mac, c.serial, ...c.tags].some((s) => s.toLowerCase().includes(q));
  return true;
}

export function CamerasPage() {
  const { data: cameras, isLoading, error } = useCameras();
  const { data: metrics } = useNodeMetrics();
  const search = useSearch();
  const [creating, setCreating] = useState(false);
  const [cloning, setCloning] = useState<Camera | null>(null);
  const [selected, setSelected] = useState<Set<string>>(new Set());
  const bulk = useBulkCameras();
  const [failures, setFailures] = useState<Failure[]>([]);
  const t = useT();

  const filter: Filter = { q: search.get("q") ?? "", state: search.get("state") ?? "", profile: search.get("profile") ?? "", tag: search.get("tag") ?? "" };
  const filtered = !!(filter.q || filter.state || filter.profile || filter.tag);
  const all = cameras ?? [];
  const visible = all.filter((c) => matches(filter, c));
  // Bulk actions reach only the selected cameras the filters show.
  const chosen = visible.filter((c) => selected.has(c.id));
  const allChosen = visible.length > 0 && chosen.length === visible.length;

  const toggle = (id: string) =>
    setSelected((prev) => {
      const next = new Set(prev);
      if (next.has(id)) next.delete(id);
      else next.add(id);
      return next;
    });
  const toggleAll = () => setSelected(allChosen ? new Set() : new Set(visible.map((c) => c.id)));

  // The bulk action lives here, not in its bar: cameras may leave the
  // filtered view while it runs (a stop under "Running"), which would
  // unmount the bar before the results arrive.
  const runBulk = (action: BulkAction["action"]) => {
    const targets = chosen;
    if (action === "delete" && !confirm(plural(t, targets.length, "Delete 1 camera and its events?", "Delete {n} cameras and their events?"))) return;
    const names = new Map(targets.map((c) => [c.id, c.name]));
    setFailures([]);
    bulk.mutate(
      { action, ids: targets.map((c) => c.id) },
      {
        onSuccess: (res) => {
          const failed = res.results.filter((r) => !r.ok);
          setFailures(failed.map((r) => ({ name: names.get(r.id) ?? r.id, message: problemText(r) })));
          toast(summary(res, t), failed.length ? "error" : "ok");
          // Keep the ones that failed selected, to retry or look at them.
          if (action === "delete" || action === "clone") setSelected(new Set(failed.map((r) => r.id)));
        },
        onError: (err) => toast(errorMessage(err), "error"),
      },
    );
  };

  return (
    <>
      <PageHeader
        title={t("Cameras")}
        description={t("Simulated IP cameras on this node. Each one answers on the LAN as its profile describes.")}
        actions={
          <Button variant="primary" onClick={() => setCreating(true)}>
            <Plus /> {t("New camera")}
          </Button>
        }
      />
      {error && <Notice tone="error">{errorMessage(error)}</Notice>}
      {all.length > 0 && <Filters cameras={all} filter={filter} filtered={filtered} />}
      <Card>
        {(chosen.length > 0 || bulk.isPending || failures.length > 0) && (
          <BulkBar
            count={chosen.length}
            pending={bulk.isPending}
            failures={failures}
            onRun={runBulk}
            onClear={() => {
              setSelected(new Set());
              setFailures([]);
            }}
          />
        )}
        {isLoading ? (
          <Empty title={t("Loading cameras…")} />
        ) : all.length === 0 ? (
          <Empty icon={<CameraIcon />} title={t("No cameras yet")}>
            {t("Import a profile, then create a camera with a free IP address of your LAN.")}
          </Empty>
        ) : visible.length === 0 ? (
          <Empty icon={<Search />} title={t("No camera matches the filters")}>
            <Button size="sm" variant="ghost" onClick={() => setSearch({ q: "", state: "", profile: "", tag: "" })}>
              {t("Clear filters")}
            </Button>
          </Empty>
        ) : (
          <Table>
            <THead>
              <tr>
                <TH className="w-8">
                  <input
                    type="checkbox"
                    className="size-3.5 accent-brand"
                    checked={allChosen}
                    ref={(el) => {
                      if (el) el.indeterminate = chosen.length > 0 && !allChosen;
                    }}
                    onChange={toggleAll}
                    aria-label={t("Select every camera shown")}
                  />
                </TH>
                <TH>{t("Name")}</TH>
                <TH>{t("Profile")}</TH>
                <TH>{t("Address")}</TH>
                <TH>{t("State")}</TH>
                <TH className="text-right">CPU</TH>
                <TH className="text-right">RAM</TH>
                <TH className="text-right">{t("Clients")}</TH>
                <TH className="text-right">{t("Actions")}</TH>
              </tr>
            </THead>
            <TBody>
              {visible.map((c) => {
                const m = metrics?.cameras[c.id];
                return (
                  <TR key={c.id} className={selected.has(c.id) ? "bg-surface-2/40" : undefined}>
                    <TD>
                      <input
                        type="checkbox"
                        className="size-3.5 accent-brand"
                        checked={selected.has(c.id)}
                        onChange={() => toggle(c.id)}
                        aria-label={t("Select {name}", { name: c.name })}
                      />
                    </TD>
                    <TD>
                      <div className="flex items-center gap-2">
                        <Link href={`/cameras/${c.id}`} className="font-medium hover:underline">
                          {c.name}
                        </Link>
                        {c.tags.map((tag) => (
                          <button key={tag} type="button" className="cursor-pointer" onClick={() => setSearch({ tag })} title={t("Show cameras tagged {tag}", { tag })}>
                            <Badge icon={<Tag />}>{tag}</Badge>
                          </button>
                        ))}
                      </div>
                    </TD>
                    <TD>
                      <div className="flex items-center gap-2">
                        <span className="text-muted">
                          {c.profile.vendor} {c.profile.model}
                        </span>
                        <LevelBadge level={c.profile.level} />
                      </div>
                    </TD>
                    <TD>
                      <Mono>{c.network.ip || "127.0.0.1"}</Mono>
                      <Mono className="ml-2 text-muted">{c.network.mac}</Mono>
                    </TD>
                    <TD>
                      <div className="flex items-center gap-2">
                        <StateBadge state={c.status.state} reason={c.status.reason} />
                        {c.status.pending_restart.length > 0 && (
                          <Badge tone="warn" icon={<RotateCw />} title={t("Saved {what} changes apply after a restart", { what: c.status.pending_restart.join(" and ") })}>
                            {t("restart pending")}
                          </Badge>
                        )}
                        {c.status.state === "error" && (
                          <span className="max-w-72 truncate text-xs text-error" title={c.status.reason}>
                            {c.status.reason}
                          </span>
                        )}
                      </div>
                    </TD>
                    <TD className="text-right">
                      <Mono>{m ? formatPercent(m.cpu_percent) : "—"}</Mono>
                    </TD>
                    <TD className="text-right">
                      <Mono>{m ? formatBytes(m.rss_bytes) : "—"}</Mono>
                    </TD>
                    <TD className="text-right">
                      <Mono>{m ? m.clients : "—"}</Mono>
                    </TD>
                    <TD className="text-right">
                      <RowActions camera={c} onClone={() => setCloning(c)} />
                    </TD>
                  </TR>
                );
              })}
            </TBody>
          </Table>
        )}
      </Card>
      {filtered && visible.length > 0 && (
        <p className="mt-2 text-xs text-muted">{t("Showing {n} of {total} cameras.", { n: visible.length, total: all.length })}</p>
      )}
      <NewCameraDialog open={creating} onClose={() => setCreating(false)} />
      {cloning && <CloneDialog camera={cloning} open onClose={() => setCloning(null)} />}
    </>
  );
}

/** Search, state, profile and tag filters, kept in the URL. */
function Filters({ cameras, filter, filtered }: { cameras: Camera[]; filter: Filter; filtered: boolean }) {
  const t = useT();
  const counts = useMemo(() => {
    const n: Record<string, number> = {};
    for (const c of cameras) n[c.status.state] = (n[c.status.state] ?? 0) + 1;
    return n;
  }, [cameras]);
  const profiles = useMemo(() => {
    const m = new Map<string, string>();
    for (const c of cameras) m.set(c.profile.id, `${c.profile.vendor} ${c.profile.model}`);
    return [...m.entries()].sort((a, b) => a[1].localeCompare(b[1]));
  }, [cameras]);
  const tags = useMemo(() => [...new Set(cameras.flatMap((c) => c.tags))].sort((a, b) => a.localeCompare(b)), [cameras]);
  return (
    <div className="mb-3 flex flex-wrap items-center gap-2">
      <div className="relative">
        <Search className="pointer-events-none absolute top-2 left-2 size-4 text-muted" />
        <Input
          value={filter.q}
          onChange={(e) => setSearch({ q: e.target.value })}
          placeholder={t("Name, IP, MAC, serial or tag")}
          className="w-64 pl-8"
          aria-label={t("Search cameras")}
        />
      </div>
      <Select value={filter.state} onChange={(e) => setSearch({ state: e.target.value })} className="w-44" aria-label={t("State")}>
        <option value="">{t("Every state")}</option>
        {states
          .filter((s) => counts[s] || s === filter.state)
          .map((s) => (
            <option key={s} value={s}>
              {t(s.charAt(0).toUpperCase() + s.slice(1))} ({counts[s] ?? 0})
            </option>
          ))}
      </Select>
      <Select value={filter.profile} onChange={(e) => setSearch({ profile: e.target.value })} className="w-48" aria-label={t("Profile")}>
        <option value="">{t("Every profile")}</option>
        {profiles.map(([id, label]) => (
          <option key={id} value={id}>
            {label}
          </option>
        ))}
      </Select>
      {(tags.length > 0 || filter.tag) && (
        <Select value={filter.tag} onChange={(e) => setSearch({ tag: e.target.value })} className="w-44" aria-label={t("Tag")}>
          <option value="">{t("Every tag")}</option>
          {[...new Set([...tags, ...(filter.tag ? [filter.tag] : [])])].map((tag) => (
            <option key={tag} value={tag}>
              {tag}
            </option>
          ))}
        </Select>
      )}
      {filtered && (
        <Button size="sm" variant="ghost" onClick={() => setSearch({ q: "", state: "", profile: "", tag: "" })}>
          <X /> {t("Clear filters")}
        </Button>
      )}
    </div>
  );
}

interface Failure {
  name: string;
  message: string;
}

/** Actions on every selected camera, and the cameras where one failed. */
function BulkBar({
  count,
  pending,
  failures,
  onRun,
  onClear,
}: {
  count: number;
  pending: boolean;
  failures: Failure[];
  onRun: (action: BulkAction["action"]) => void;
  onClear: () => void;
}) {
  const t = useT();
  const button = (action: BulkAction["action"], icon: ReactNode, label: string, title?: string) => (
    <Button size="sm" variant={action === "delete" ? "danger" : "secondary"} disabled={pending || count === 0} onClick={() => onRun(action)} title={title}>
      {icon} {label}
    </Button>
  );
  return (
    <div className="border-b border-border bg-surface-2/40">
      <div className="flex flex-wrap items-center gap-2 px-3 py-2">
        <span className="mr-2 text-[13px] font-medium">{plural(t, count, "1 selected", "{n} selected")}</span>
        {button("start", <Play />, t("Start"))}
        {button("stop", <Square />, t("Stop"))}
        {button("restart", <RotateCw />, t("Restart"))}
        {button("clone", <CopyPlus />, t("Clone"), t("Each copy takes the next free name and address of its subnet"))}
        {button("delete", <Trash2 />, t("Delete"))}
        {pending && <span className="text-xs text-muted">{t("Working…")}</span>}
        <Button size="sm" variant="ghost" className="ml-auto" onClick={onClear} disabled={pending}>
          {t("Clear selection")}
        </Button>
      </div>
      {failures.length > 0 && (
        <div className="px-3 pb-2">
          <Notice tone="error">
            <ul className="flex flex-col gap-0.5">
              {failures.map((f) => (
                <li key={f.name}>
                  <span className="font-medium">{f.name}</span>: {f.message}
                </li>
              ))}
            </ul>
          </Notice>
        </div>
      )}
    </div>
  );
}

function problemText(r: BulkResult["results"][number]): string {
  const p = r.error;
  if (!p) return "";
  const fields = p.errors?.map((e) => (e.field ? `${e.field}: ${e.message}` : e.message));
  return fields?.length ? fields.join("; ") : p.detail || p.title;
}

function summary(res: BulkResult, t: Translate): string {
  const n = res.succeeded;
  const done = {
    start: plural(t, n, "1 started", "{n} started"),
    stop: plural(t, n, "1 stopped", "{n} stopped"),
    restart: plural(t, n, "1 restarted", "{n} restarted"),
    clone: plural(t, n, "1 copy created", "{n} copies created"),
    delete: plural(t, n, "1 deleted", "{n} deleted"),
  }[res.action as BulkAction["action"]] ?? String(n);
  return res.failed ? t("{done}, {failed} failed", { done, failed: res.failed }) : done;
}

function RowActions({ camera, onClone }: { camera: Camera; onClone: () => void }) {
  const action = useCameraAction();
  const trigger = useTrigger();
  const del = useDeleteCamera();
  const t = useT();
  const state = camera.status.state;
  const running = state === "running" || state === "degraded";
  const busy = ["provisioning", "starting", "stopping", "restarting"].includes(state) || action.isPending;
  const canStart = state === "stopped" || state === "error";

  const run = (a: "start" | "stop") =>
    action.mutate(
      { id: camera.id, action: a },
      { onError: (err) => toast(`${camera.name}: ${errorMessage(err)}`, "error") },
    );

  return (
    <div className="flex items-center justify-end gap-1">
      <Button
        size="icon"
        variant="ghost"
        disabled={!running || trigger.isPending}
        title={t("Send a line-crossing event to the camera's targets")}
        aria-label={t("Line crossing")}
        onClick={() =>
          trigger.mutate(
            { id: camera.id, type: "line_crossing" },
            {
              onSuccess: () => toast(t("Line crossing sent from {name}", { name: camera.name }), "ok"),
              onError: (err) => toast(errorMessage(err), "error"),
            },
          )
        }
      >
        <Zap />
      </Button>
      {canStart ? (
        <Button size="icon" variant="ghost" disabled={busy} onClick={() => run("start")} title={t("Start")} aria-label={t("Start {name}", { name: camera.name })}>
          <Play />
        </Button>
      ) : (
        <Button size="icon" variant="ghost" disabled={busy || !running} onClick={() => run("stop")} title={t("Stop")} aria-label={t("Stop {name}", { name: camera.name })}>
          <Square />
        </Button>
      )}
      <Button size="icon" variant="ghost" onClick={onClone} title={t("Clone")} aria-label={t("Clone {name}", { name: camera.name })}>
        <CopyPlus />
      </Button>
      <Button
        size="icon"
        variant="ghost"
        title={t("Delete")}
        aria-label={t("Delete {name}", { name: camera.name })}
        disabled={del.isPending}
        onClick={() => {
          if (confirm(t("Delete camera {name} and its events?", { name: camera.name }))) {
            del.mutate(camera.id, { onError: (err) => toast(errorMessage(err), "error") });
          }
        }}
      >
        <Trash2 />
      </Button>
    </div>
  );
}
