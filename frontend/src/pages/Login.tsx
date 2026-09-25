import { type FormEvent, useState } from "react";
import { errorMessage } from "@/api/client";
import { useLogin } from "@/api/queries";
import { Logo } from "@/components/Layout";
import { Button } from "@/components/ui/button";
import { Card, Notice } from "@/components/ui/card";
import { Field, Input } from "@/components/ui/form";

/** Login, or the first-run wizard that creates the administrator. */
export function LoginPage({ mode }: { mode: "login" | "setup" }) {
  const [username, setUsername] = useState("admin");
  const [password, setPassword] = useState("");
  const [confirm, setConfirm] = useState("");
  const [localError, setLocalError] = useState("");
  const login = useLogin(mode);

  const submit = (e: FormEvent) => {
    e.preventDefault();
    setLocalError("");
    if (mode === "setup" && password !== confirm) {
      setLocalError("The passwords do not match.");
      return;
    }
    login.mutate({ username, password });
  };

  return (
    <div className="flex min-h-full items-center justify-center p-6">
      <Card className="w-full max-w-sm p-6">
        <div className="mb-5 flex flex-col gap-3">
          <Logo />
          <div>
            <h1 className="text-base font-semibold">{mode === "setup" ? "Create the administrator" : "Sign in"}</h1>
            <p className="mt-1 text-[13px] text-muted">
              {mode === "setup"
                ? "This node has no users yet. The first account manages everything; there are no default credentials."
                : "Sign in to manage the simulated cameras of this node."}
            </p>
          </div>
        </div>
        <form onSubmit={submit} className="flex flex-col gap-3">
          <Field label="Username">
            <Input value={username} onChange={(e) => setUsername(e.target.value)} autoComplete="username" required />
          </Field>
          <Field label="Password" hint={mode === "setup" ? "At least 10 characters." : undefined}>
            <Input
              type="password"
              value={password}
              onChange={(e) => setPassword(e.target.value)}
              autoComplete={mode === "setup" ? "new-password" : "current-password"}
              required
              autoFocus
            />
          </Field>
          {mode === "setup" && (
            <Field label="Repeat password">
              <Input type="password" value={confirm} onChange={(e) => setConfirm(e.target.value)} autoComplete="new-password" required />
            </Field>
          )}
          {(localError || login.error) && <Notice tone="error">{localError || errorMessage(login.error)}</Notice>}
          <Button type="submit" variant="primary" disabled={login.isPending} className="mt-1 justify-center">
            {mode === "setup" ? "Create and sign in" : "Sign in"}
          </Button>
        </form>
      </Card>
    </div>
  );
}
