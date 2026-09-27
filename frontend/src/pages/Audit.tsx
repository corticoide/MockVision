import { Bot, ChevronDown, ChevronRight, Cpu, KeyRound, Monitor, ScrollText } from "lucide-react";
import { Fragment, useState } from "react";
import { type AuditEntry, errorMessage } from "@/api/client";
import { type AuditFilter, useAudit, useCameras, useTokens } from "@/api/queries";
import { Badge, Mono } from "@/components/badges";
import { Button } from "@/components/ui/button";
import { Card, Empty, Notice, PageHeader } from "@/components/ui/card";
import { Select } from "@/components/ui/form";
import { Table, TBody, TD, TH, THead, TR } from "@/components/ui/table";
import { type Translate, useT } from "@/lib/i18n";
import { Link, setSearch, useSearch } from "@/lib/router";
import { formatTime } from "@/lib/utils";

const origins = ["panel", "api", "camera", "system"] as const;
type Origin = (typeof origins)[number];

// Action families offered in the filter; the node matches them as prefixes.
const actions = ["camera", "event", "target", "asset", "profile", "package", "settings", "token", "auth"];

export function AuditPage() {
  const t = useT();
  const search = useSearch();
  const origin = search.get("origin") ?? "";
  const cameraId = search.get("camera") ?? "";
  const action = search.get("action") ?? "";
  const tokenId = search.get("token") ?? "";
  const filter: AuditFilter = {
    origin: (origins as readonly string[]).includes(origin) ? (origin as Origin) : undefined,
    entity_type: cameraId ? "camera" : undefined,
    entity_id: cameraId || undefined,
    action: action || undefined,
    token_id: tokenId || undefined,
  };
  const { data, isLoading, error, fetchNextPage, hasNextPage, isFetchingNextPage } = useAudit(filter);
  const { data: cameras } = useCameras();
  const { data: tokens } = useTokens();
  const entries = data?.pages.flatMap((p) => p.items) ?? [];
  const filtered = !!(origin || cameraId || action || tokenId);

  return (
    <>
      <PageHeader
        title={t("Audit")}
        description={t("Who changed what and from where: the panel, the API with a token, a client of a camera's emulated API or the node itself. Kept 90 days.")}
      />
      <div className="mb-3 flex flex-wrap items-center gap-2">
        <Select value={origin} onChange={(e) => setSearch({ origin: e.target.value })} className="w-44" aria-label={t("Origin")}>
          <option value="">{t("Every origin")}</option>
          {origins.map((o) => (
            <option key={o} value={o}>
              {originLabel(o, t)}
            </option>
          ))}
        </Select>
        <Select value={cameraId} onChange={(e) => setSearch({ camera: e.target.value })} className="w-56" aria-label={t("Camera")}>
          <option value="">{t("Every camera and entity")}</option>
          {cameras?.map((c) => (
            <option key={c.id} value={c.id}>
              {c.name}
            </option>
          ))}
          {cameraId && !cameras?.some((c) => c.id === cameraId) && <option value={cameraId}>{t("Deleted camera {id}", { id: cameraId })}</option>}
        </Select>
        <Select value={action} onChange={(e) => setSearch({ action: e.target.value })} className="w-44" aria-label={t("Action")}>
          <option value="">{t("Every action")}</option>
          {actions.map((a) => (
            <option key={a} value={a}>
              {a}.*
            </option>
          ))}
        </Select>
        {tokenId && (
          <Badge tone="info" icon={<KeyRound />}>
            {t("Token {name}", { name: tokens?.find((tk) => tk.id === tokenId)?.name ?? tokenId })}
          </Badge>
        )}
        {filtered && (
          <Button size="sm" variant="ghost" onClick={() => setSearch({ origin: "", camera: "", action: "", token: "" })}>
            {t("Clear filters")}
          </Button>
        )}
      </div>
      {error && <Notice tone="error">{errorMessage(error)}</Notice>}
      <Card>
        {isLoading ? (
          <Empty title={t("Loading the audit log…")} />
        ) : entries.length === 0 ? (
          <Empty icon={<ScrollText />} title={filtered ? t("No entries match the filters") : t("No entries yet")} />
        ) : (
          <AuditTable entries={entries} />
        )}
      </Card>
      {hasNextPage && (
        <div className="mt-3 flex justify-center">
          <Button size="sm" onClick={() => fetchNextPage()} disabled={isFetchingNextPage}>
            {isFetchingNextPage ? t("Loading…") : t("Load older entries")}
          </Button>
        </div>
      )}
    </>
  );
}

function originLabel(o: string, t: Translate) {
  switch (o) {
    case "panel":
      return t("Panel");
    case "api":
      return t("API (token)");
    case "camera":
      return t("Camera (emulated API)");
    case "system":
      return t("Node");
  }
  return o;
}

function OriginBadge({ entry, t }: { entry: AuditEntry; t: Translate }) {
  switch (entry.origin) {
    case "api":
      return (
        <Badge tone="info" icon={<KeyRound />} title={entry.token ? t("Token {name}", { name: entry.token.name }) : undefined}>
          {entry.token ? `API · ${entry.token.name}` : "API"}
        </Badge>
      );
    case "camera":
      return (
        <Badge tone="warn" icon={<Bot />}>
          {t("Emulated API")}
        </Badge>
      );
    case "system":
      return (
        <Badge tone="muted" icon={<Cpu />}>
          {t("Node")}
        </Badge>
      );
  }
  return (
    <Badge tone="muted" icon={<Monitor />}>
      {t("Panel")}
    </Badge>
  );
}

/** Who made the change: a panel user, or for the emulated API the client. */
function who(entry: AuditEntry, t: Translate) {
  switch (entry.actor.type) {
    case "camera":
      return entry.origin_ip ? t("client {ip}", { ip: entry.origin_ip }) : t("a client");
    case "system":
      return entry.actor.id || t("node");
  }
  return entry.actor.name || entry.actor.id || "—";
}

function AuditTable({ entries }: { entries: AuditEntry[] }) {
  const t = useT();
  const [open, setOpen] = useState<string | null>(null);
  return (
    <Table>
      <THead>
        <tr>
          <TH className="w-8" />
          <TH>{t("Time")}</TH>
          <TH>{t("Who")}</TH>
          <TH>{t("Origin")}</TH>
          <TH>{t("From")}</TH>
          <TH>{t("Action")}</TH>
          <TH>{t("Entity")}</TH>
        </tr>
      </THead>
      <TBody>
        {entries.map((e) => {
          const expanded = open === e.id;
          return (
            <Fragment key={e.id}>
              <TR>
                <TD>
                  <Button variant="ghost" size="icon" onClick={() => setOpen(expanded ? null : e.id)} aria-label={t("Details")} aria-expanded={expanded}>
                    {expanded ? <ChevronDown /> : <ChevronRight />}
                  </Button>
                </TD>
                <TD>
                  <Mono>{formatTime(e.at)}</Mono>
                </TD>
                <TD>{who(e, t)}</TD>
                <TD>
                  <OriginBadge entry={e} t={t} />
                </TD>
                <TD>
                  <Mono className="text-muted">{e.origin_ip || "—"}</Mono>
                </TD>
                <TD>
                  <Mono className={e.action.endsWith("_failed") ? "text-error" : undefined}>{e.action}</Mono>
                </TD>
                <TD className="max-w-72 truncate">
                  <Entity entry={e} />
                </TD>
              </TR>
              {expanded && (
                <tr className="border-b border-border/70 bg-bg/40">
                  <td colSpan={7} className="px-4 py-3">
                    <pre className="max-h-64 overflow-auto rounded-sm border border-border bg-bg p-3 font-mono text-[12px]">
                      {JSON.stringify(e.diff, null, 2)}
                    </pre>
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

function Entity({ entry }: { entry: AuditEntry }) {
  const { type, id, name } = entry.entity;
  const label = name || id || "—";
  if (type === "camera" && id && !entry.action.endsWith(".delete")) {
    return (
      <Link href={`/cameras/${id}`} className="hover:underline" title={id}>
        {label}
      </Link>
    );
  }
  return (
    <span title={id}>
      <span className="text-muted">{type}</span> {label}
    </span>
  );
}
