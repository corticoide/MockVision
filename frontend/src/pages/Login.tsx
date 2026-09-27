import { type FormEvent, useState } from "react";
import { errorMessage } from "@/api/client";
import { useLogin } from "@/api/queries";
import { LanguageSelect } from "@/components/LanguageSelect";
import { Logo } from "@/components/Layout";
import { Button } from "@/components/ui/button";
import { Card, Notice } from "@/components/ui/card";
import { Field, Input } from "@/components/ui/form";
import { useT } from "@/lib/i18n";

/** Login, or the first-run wizard that creates the administrator. */
export function LoginPage({ mode }: { mode: "login" | "setup" }) {
  const [username, setUsername] = useState("admin");
  const [password, setPassword] = useState("");
  const [confirm, setConfirm] = useState("");
  const [setupCode, setSetupCode] = useState("");
  const [localError, setLocalError] = useState("");
  const login = useLogin(mode);
  const t = useT();

  const submit = (e: FormEvent) => {
    e.preventDefault();
    setLocalError("");
    if (mode === "setup" && password !== confirm) {
      setLocalError(t("The passwords do not match."));
      return;
    }
    login.mutate(mode === "setup" ? { username, password, setup_code: setupCode } : { username, password });
  };

  return (
    <div className="flex min-h-full items-center justify-center p-6">
      <Card className="w-full max-w-sm p-6">
        <div className="mb-5 flex flex-col gap-3">
          <Logo />
          <div>
            <h1 className="text-base font-semibold">{mode === "setup" ? t("Create the administrator") : t("Sign in")}</h1>
            <p className="mt-1 text-[13px] text-muted">
              {mode === "setup"
                ? t("This node has no users yet. The first account manages everything; there are no default credentials.")
                : t("Sign in to manage the simulated cameras of this node.")}
            </p>
          </div>
        </div>
        <form onSubmit={submit} className="flex flex-col gap-3">
          {mode === "setup" && (
            <Field
              label={t("Setup code")}
              hint={t("Printed in the node's log when it starts, and saved in the setup-code file of its data directory.")}
            >
              <Input
                value={setupCode}
                onChange={(e) => setSetupCode(e.target.value)}
                autoComplete="one-time-code"
                spellCheck={false}
                className="font-mono"
                required
                autoFocus
              />
            </Field>
          )}
          <Field label={t("Username")}>
            <Input value={username} onChange={(e) => setUsername(e.target.value)} autoComplete="username" required />
          </Field>
          <Field label={t("Password")} hint={mode === "setup" ? t("At least 10 characters.") : undefined}>
            <Input
              type="password"
              value={password}
              onChange={(e) => setPassword(e.target.value)}
              autoComplete={mode === "setup" ? "new-password" : "current-password"}
              required
              autoFocus={mode === "login"}
            />
          </Field>
          {mode === "setup" && (
            <Field label={t("Repeat password")}>
              <Input type="password" value={confirm} onChange={(e) => setConfirm(e.target.value)} autoComplete="new-password" required />
            </Field>
          )}
          {(localError || login.error) && <Notice tone="error">{localError || errorMessage(login.error)}</Notice>}
          <Button type="submit" variant="primary" disabled={login.isPending} className="mt-1 justify-center">
            {mode === "setup" ? t("Create and sign in") : t("Sign in")}
          </Button>
        </form>
        <div className="mt-4 flex justify-center border-t border-border pt-3">
          <LanguageSelect />
        </div>
      </Card>
    </div>
  );
}
