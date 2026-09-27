import { AlertTriangle, History, KeyRound, Plus, Trash2 } from "lucide-react";
import { type FormEvent, useState } from "react";
import { ApiError, type CreatedToken, errorMessage, type Token } from "@/api/client";
import { useCreateToken, useRevokeToken, useTokens } from "@/api/queries";
import { Badge, Mono } from "@/components/badges";
import { CopyButton } from "@/components/CopyButton";
import { toast } from "@/components/toast";
import { Button } from "@/components/ui/button";
import { Card, CardHeader, Empty, Notice } from "@/components/ui/card";
import { Dialog } from "@/components/ui/dialog";
import { Field, Input, Select } from "@/components/ui/form";
import { Table, TBody, TD, TH, THead, TR } from "@/components/ui/table";
import { useT } from "@/lib/i18n";
import { Link } from "@/lib/router";
import { formatTime } from "@/lib/utils";

/** API tokens for automation (D54): created, listed and revoked here only. */
export function TokensCard() {
  const t = useT();
  const { data: tokens, isLoading, error } = useTokens();
  const [creating, setCreating] = useState(false);
  return (
    <Card id="tokens">
      <CardHeader
        title={t("API tokens")}
        description={t("For CI and scripts: send the token as Authorization: Bearer. A read token can only query the node; tokens are managed from the panel only.")}
        actions={
          <Button size="sm" variant="primary" onClick={() => setCreating(true)}>
            <Plus /> {t("New token")}
          </Button>
        }
      />
      {error && <Notice tone="error">{errorMessage(error)}</Notice>}
      {isLoading ? (
        <Empty title={t("Loading…")} />
      ) : !tokens?.length ? (
        <Empty icon={<KeyRound />} title={t("No API tokens")}>
          {t("Create one to automate tests from CI: create cameras, fire events and read the results.")}
        </Empty>
      ) : (
        <Table>
          <THead>
            <tr>
              <TH>{t("Name")}</TH>
              <TH>{t("Token")}</TH>
              <TH>{t("Scope")}</TH>
              <TH>{t("Created on")}</TH>
              <TH>{t("Expires")}</TH>
              <TH>{t("Last used")}</TH>
              <TH className="text-right">{t("Actions")}</TH>
            </tr>
          </THead>
          <TBody>
            {tokens.map((tk) => (
              <TokenRow key={tk.id} token={tk} />
            ))}
          </TBody>
        </Table>
      )}
      <NewTokenDialog open={creating} onClose={() => setCreating(false)} />
    </Card>
  );
}

function TokenRow({ token }: { token: Token }) {
  const t = useT();
  const revoke = useRevokeToken();
  const write = token.scopes.includes("write");
  return (
    <TR>
      <TD className="font-medium">{token.name}</TD>
      <TD>
        <Mono className="text-muted">{token.prefix}…</Mono>
      </TD>
      <TD>
        <Badge tone={write ? "warn" : "info"}>{write ? t("read and write") : t("read only")}</Badge>
      </TD>
      <TD>
        <Mono>{formatTime(token.created_at)}</Mono>
      </TD>
      <TD>
        {token.expired ? (
          <Badge tone="error" icon={<AlertTriangle />}>
            {t("expired")}
          </Badge>
        ) : (
          <Mono>{token.expires_at ? formatTime(token.expires_at) : t("never")}</Mono>
        )}
      </TD>
      <TD>
        {token.last_used_at ? (
          <Mono>
            {formatTime(token.last_used_at)} <span className="text-muted">{token.last_used_ip}</span>
          </Mono>
        ) : (
          <span className="text-muted">{t("never")}</span>
        )}
      </TD>
      <TD className="text-right">
        <div className="flex items-center justify-end gap-1">
          <Link
            href={`/audit?token=${token.id}`}
            title={t("What this token changed")}
            aria-label={t("Activity of {name}", { name: token.name })}
            className="inline-flex size-7 items-center justify-center rounded-sm text-muted hover:bg-surface-2 hover:text-text [&_svg]:size-3.5"
          >
            <History />
          </Link>
          <Button
            size="icon"
            variant="ghost"
            title={t("Revoke")}
            aria-label={t("Revoke {name}", { name: token.name })}
            disabled={revoke.isPending}
            onClick={() => {
              if (!confirm(t("Revoke token {name}? Requests that use it will fail at once.", { name: token.name }))) return;
              revoke.mutate(token.id, {
                onSuccess: () => toast(t("Token {name} revoked", { name: token.name }), "ok"),
                onError: (err) => toast(errorMessage(err), "error"),
              });
            }}
          >
            <Trash2 />
          </Button>
        </div>
      </TD>
    </TR>
  );
}

const lifetimes = [7, 30, 90, 365, 0];

function NewTokenDialog({ open, onClose }: { open: boolean; onClose: () => void }) {
  const t = useT();
  const create = useCreateToken();
  const [name, setName] = useState("");
  const [scope, setScope] = useState<"read" | "write">("write");
  const [days, setDays] = useState(90);
  const [errors, setErrors] = useState<Record<string, string>>({});
  const [created, setCreated] = useState<CreatedToken | null>(null);

  const close = () => {
    setName("");
    setScope("write");
    setDays(90);
    setErrors({});
    setCreated(null);
    create.reset();
    onClose();
  };

  const submit = (e: FormEvent) => {
    e.preventDefault();
    setErrors({});
    create.mutate(
      { name: name.trim(), scopes: [scope], expires_in_days: days > 0 ? days : null },
      {
        onSuccess: setCreated,
        onError: (err) => {
          if (err instanceof ApiError) setErrors(err.fieldErrors());
          else toast(errorMessage(err), "error");
        },
      },
    );
  };

  if (created) {
    const example = `curl -H "Authorization: Bearer ${created.secret}" ${location.origin}/api/v1/cameras`;
    return (
      <Dialog open={open} onClose={close} title={t("Token {name} created", { name: created.token.name })} footer={<Button variant="primary" onClick={close}>{t("Done")}</Button>}>
        <div className="flex flex-col gap-3">
          <Notice tone="warn">{t("Copy it now: the node keeps only its hash and will not show it again.")}</Notice>
          <div className="flex items-center gap-2">
            <Input readOnly value={created.secret} className="font-mono" aria-label={t("Token")} onFocus={(e) => e.target.select()} />
            <CopyButton text={created.secret} />
          </div>
          <Field label={t("Example")}>
            <pre className="overflow-x-auto rounded-sm border border-border bg-bg p-2 font-mono text-[12px]">{example}</pre>
          </Field>
        </div>
      </Dialog>
    );
  }

  return (
    <Dialog
      open={open}
      onClose={close}
      title={t("New API token")}
      description={t("The token acts as you, from wherever it is used; every change it makes is audited with its name.")}
      footer={
        <>
          <Button onClick={close}>{t("Cancel")}</Button>
          <Button type="submit" form="new-token" variant="primary" disabled={create.isPending || !name.trim()}>
            {create.isPending ? t("Creating…") : t("Create token")}
          </Button>
        </>
      }
    >
      <form id="new-token" onSubmit={submit} className="grid grid-cols-2 gap-x-4 gap-y-3">
        <Field label={t("Name")} error={errors["name"]} className="col-span-2" hint={t("Where it is used, such as the CI job.")}>
          <Input value={name} onChange={(e) => setName(e.target.value)} maxLength={64} autoFocus placeholder="ci-nightly" />
        </Field>
        <Field label={t("Scope")} error={errors["scopes"]}>
          <Select value={scope} onChange={(e) => setScope(e.target.value as "read" | "write")}>
            <option value="write">{t("Read and write")}</option>
            <option value="read">{t("Read only")}</option>
          </Select>
        </Field>
        <Field label={t("Expires")} error={errors["expires_in_days"]}>
          <Select value={days} onChange={(e) => setDays(Number(e.target.value))}>
            {lifetimes.map((d) => (
              <option key={d} value={d}>
                {d === 0 ? t("Never") : t("In {n} days", { n: d })}
              </option>
            ))}
          </Select>
        </Field>
      </form>
    </Dialog>
  );
}
