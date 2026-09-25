import { Plus, Trash2 } from "lucide-react";
import { type FormEvent, useState } from "react";
import { ApiError, type Camera, errorMessage } from "@/api/client";
import { useSetCameraUsers } from "@/api/queries";
import { toast } from "@/components/toast";
import { Button } from "@/components/ui/button";
import { Card, Notice } from "@/components/ui/card";
import { Input, Select } from "@/components/ui/form";
import { Table, TBody, TD, TH, THead, TR } from "@/components/ui/table";
import { useDraft } from "@/lib/draft";
import { useT } from "@/lib/i18n";
import { SaveBar } from "./parts";

type Role = "admin" | "operator" | "viewer";

interface Row {
  username: string;
  role: Role;
  password: string;
  existing: boolean;
}

const savedOf = (camera: Camera): Row[] =>
  camera.users.map((u) => ({ username: u.username, role: u.role as Role, password: "", existing: true }));

export function UsersTab({ camera }: { camera: Camera }) {
  const save = useSetCameraUsers(camera.id);
  const t = useT();
  const form = useDraft(savedOf(camera));
  const rows = form.draft;
  const [errors, setErrors] = useState<Record<string, string>>({});

  const set = (i: number, patch: Partial<Row>) => form.update((rs) => rs.map((r, j) => (j === i ? { ...r, ...patch } : r)));

  const submit = (e: FormEvent) => {
    e.preventDefault();
    setErrors({});
    save.mutate(
      rows.map((r) => ({ username: r.username.trim(), role: r.role, password: r.password || undefined })),
      {
        onSuccess: (saved) => {
          form.resetTo(savedOf(saved));
          toast(t("Accounts saved; the camera uses them at once"), "ok");
        },
        onError: (err) => {
          if (err instanceof ApiError) setErrors(err.fieldErrors());
          toast(errorMessage(err), "error");
        },
      },
    );
  };

  return (
    <Card>
      <form id="camera-users" onSubmit={submit}>
        <Table>
          <THead>
            <tr>
              <TH>{t("Username")}</TH>
              <TH>{t("Role")}</TH>
              <TH>{t("Password")}</TH>
              <TH className="w-10" />
            </tr>
          </THead>
          <TBody>
            {rows.map((r, i) => {
              const userError = errors[`users[${i}].username`];
              const passError = errors[`users[${i}].password`];
              const roleError = errors[`users[${i}].role`];
              return (
                <TR key={r.existing ? r.username : `new-${i}`} className="h-11">
                  <TD>
                    {r.existing ? (
                      <span className="font-mono text-[12px]">{r.username}</span>
                    ) : (
                      <Input
                        value={r.username}
                        onChange={(e) => set(i, { username: e.target.value })}
                        placeholder="operator"
                        aria-label={t("Username")}
                        className="h-7 w-48 font-mono"
                        autoComplete="off"
                        required
                      />
                    )}
                    {userError && <div className="text-xs text-error">{userError}</div>}
                  </TD>
                  <TD>
                    <Select value={r.role} onChange={(e) => set(i, { role: e.target.value as Role })} className="h-7 w-36" aria-label={t("Role of {name}", { name: r.username || t("the new account") })}>
                      <option value="admin">admin</option>
                      <option value="operator">operator</option>
                      <option value="viewer">viewer</option>
                    </Select>
                    {roleError && <div className="text-xs text-error">{roleError}</div>}
                  </TD>
                  <TD>
                    <Input
                      type="password"
                      value={r.password}
                      onChange={(e) => set(i, { password: e.target.value })}
                      placeholder={r.existing ? t("unchanged") : t("required")}
                      aria-label={t("Password of {name}", { name: r.username || t("the new account") })}
                      className="h-7 w-56"
                      autoComplete="new-password"
                    />
                    {passError && <div className="text-xs text-error">{passError}</div>}
                  </TD>
                  <TD>
                    <Button
                      size="icon"
                      variant="ghost"
                      title={t("Remove")}
                      aria-label={t("Remove {name}", { name: r.username || t("the new account") })}
                      onClick={() => form.update((rs) => rs.filter((_, j) => j !== i))}
                    >
                      <Trash2 />
                    </Button>
                  </TD>
                </TR>
              );
            })}
          </TBody>
        </Table>
      </form>
      <div className="px-4 pb-4">
        <div className="mt-3 flex items-center gap-3">
          <Button
            size="sm"
            onClick={() => form.update((rs) => [...rs, { username: "", role: "operator", password: "", existing: false }])}
          >
            <Plus /> {t("Add account")}
          </Button>
          <span className="text-xs text-muted">{t("Cameras accept weak passwords on purpose: they imitate real devices.")}</span>
        </div>
        {errors["users"] && (
          <div className="mt-3">
            <Notice tone="error">{errors["users"]}</Notice>
          </div>
        )}
        <SaveBar
          form="camera-users"
          dirty={form.dirty}
          saving={save.isPending}
          onDiscard={() => {
            form.discard();
            setErrors({});
          }}
          note={t("At least one admin (RN-11). Changes apply at once; clients re-authenticate with the new passwords.")}
        />
      </div>
    </Card>
  );
}
