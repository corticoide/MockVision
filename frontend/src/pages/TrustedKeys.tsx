import { BadgeCheck, FileKey, Plus, Trash2 } from "lucide-react";
import { type FormEvent, useState } from "react";
import { ApiError, errorMessage } from "@/api/client";
import { useAddTrustedKey, useDeleteTrustedKey, useTrustedKeys } from "@/api/queries";
import { Badge, Mono } from "@/components/badges";
import { CopyButton } from "@/components/CopyButton";
import { toast } from "@/components/toast";
import { Button } from "@/components/ui/button";
import { Card, CardHeader, Empty, Notice } from "@/components/ui/card";
import { Dialog } from "@/components/ui/dialog";
import { Field, Input, Textarea } from "@/components/ui/form";
import { Table, TBody, TD, TH, THead, TR } from "@/components/ui/table";
import { useT } from "@/lib/i18n";
import { formatTime } from "@/lib/utils";

/** The keys whose package signatures the node trusts (D83). */
export function TrustedKeysCard() {
  const t = useT();
  const { data: keys, isLoading, error } = useTrustedKeys();
  const remove = useDeleteTrustedKey();
  const [adding, setAdding] = useState(false);
  return (
    <Card id="trusted-keys">
      <CardHeader
        title={t("Package signatures")}
        description={t(
          "Packages signed with these minisign keys install as signed; the official catalog keys come with MockVision. Unsigned profiles install with a warning.",
        )}
        actions={
          <Button size="sm" variant="primary" onClick={() => setAdding(true)}>
            <Plus /> {t("Trust a key")}
          </Button>
        }
      />
      {error && <Notice tone="error">{errorMessage(error)}</Notice>}
      {isLoading ? (
        <Empty title={t("Loading…")} />
      ) : !keys?.length ? (
        <Empty icon={<FileKey />} title={t("No signing keys")}>
          {t("This build carries no official catalog key. Add the public key of whoever signs your packages, such as your company's.")}
        </Empty>
      ) : (
        <Table>
          <THead>
            <tr>
              <TH>{t("Name")}</TH>
              <TH>{t("Key ID")}</TH>
              <TH>{t("Public key")}</TH>
              <TH>{t("Added")}</TH>
              <TH className="text-right">{t("Actions")}</TH>
            </tr>
          </THead>
          <TBody>
            {keys.map((k) => (
              <TR key={k.key_id}>
                <TD>
                  <span className="flex items-center gap-2">
                    {k.name}
                    {k.builtin && (
                      <Badge tone="ok" icon={<BadgeCheck />}>
                        {t("Official")}
                      </Badge>
                    )}
                  </span>
                </TD>
                <TD>
                  <Mono>{k.key_id}</Mono>
                </TD>
                <TD>
                  <span className="flex items-center gap-1">
                    <Mono className="max-w-[260px] truncate">{k.public_key}</Mono>
                    <CopyButton text={k.public_key} />
                  </span>
                </TD>
                <TD>
                  <Mono>{k.added_at ? formatTime(k.added_at) : "—"}</Mono>
                </TD>
                <TD className="text-right">
                  {!k.builtin && k.id && (
                    <Button
                      size="sm"
                      variant="ghost"
                      disabled={remove.isPending}
                      onClick={() => {
                        if (!confirm(t("Stop trusting {name}? Packages imported before keep their status.", { name: k.name }))) return;
                        remove.mutate(k.id!, {
                          onSuccess: () => toast(t("{name} is no longer trusted", { name: k.name }), "ok"),
                          onError: (err) => toast(errorMessage(err), "error"),
                        });
                      }}
                    >
                      <Trash2 /> {t("Remove")}
                    </Button>
                  )}
                </TD>
              </TR>
            ))}
          </TBody>
        </Table>
      )}
      <AddKeyDialog open={adding} onClose={() => setAdding(false)} />
    </Card>
  );
}

function AddKeyDialog({ open, onClose }: { open: boolean; onClose: () => void }) {
  const t = useT();
  const add = useAddTrustedKey();
  const [name, setName] = useState("");
  const [key, setKey] = useState("");
  const [errors, setErrors] = useState<Record<string, string>>({});
  const submit = (e: FormEvent) => {
    e.preventDefault();
    setErrors({});
    add.mutate(
      { name: name.trim(), public_key: key },
      {
        onSuccess: (k) => {
          toast(t("Packages signed by {name} are trusted now", { name: k.name }), "ok");
          setName("");
          setKey("");
          onClose();
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
      title={t("Trust a signing key")}
      description={t("Paste a minisign public key: the .pub file that mockvision pkg keygen or minisign -G writes, or its key line.")}
      footer={
        <>
          <Button onClick={onClose}>{t("Cancel")}</Button>
          <Button type="submit" form="trust-key" variant="primary" disabled={add.isPending || !name.trim() || !key.trim()}>
            {add.isPending ? t("Saving…") : t("Trust")}
          </Button>
        </>
      }
    >
      <form id="trust-key" onSubmit={submit} className="flex flex-col gap-3">
        <Field label={t("Name")} error={errors["name"]}>
          <Input value={name} onChange={(e) => setName(e.target.value)} placeholder={t("Acme packages")} autoFocus />
        </Field>
        <Field label={t("Public key")} error={errors["public_key"]}>
          <Textarea value={key} onChange={(e) => setKey(e.target.value)} rows={3} className="font-mono" placeholder="RWQ…" />
        </Field>
      </form>
    </Dialog>
  );
}
