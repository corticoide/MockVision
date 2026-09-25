import { type FormEvent, useEffect, useState } from "react";
import { ApiError, type Camera, errorMessage } from "@/api/client";
import { useCloneCamera, useNode, useProfile, useResetCamera } from "@/api/queries";
import { Mono } from "@/components/badges";
import { toast } from "@/components/toast";
import { Button } from "@/components/ui/button";
import { Notice } from "@/components/ui/card";
import { Dialog } from "@/components/ui/dialog";
import { Checkbox, Field, Input } from "@/components/ui/form";
import { navigate } from "@/lib/router";
import { cn } from "@/lib/utils";
import { isRunning } from "./parts";

/** Copies a camera with a new name and address. */
export function CloneDialog({ camera, open, onClose }: { camera: Camera; open: boolean; onClose: () => void }) {
  const { data: node } = useNode();
  const clone = useCloneCamera(camera.id);
  const local = node?.runtime === "local";
  const [name, setName] = useState("");
  const [ip, setIp] = useState("");
  const [start, setStart] = useState(false);
  const [errors, setErrors] = useState<Record<string, string>>({});

  useEffect(() => {
    if (!open) return;
    setName(`${camera.name} copy`);
    setIp("");
    setErrors({});
    clone.reset();
    // eslint-disable-next-line react-hooks/exhaustive-deps
  }, [open]);

  const submit = (e: FormEvent) => {
    e.preventDefault();
    setErrors({});
    clone.mutate(
      {
        name: name.trim(),
        network: local ? undefined : { ip: ip.trim(), netmask: camera.network.netmask, gateway: camera.network.gateway, parent: camera.network.parent },
        start,
      },
      {
        onSuccess: (copy) => {
          toast(`Camera ${copy.name} created from ${camera.name}`, "ok");
          onClose();
          navigate(`/cameras/${copy.id}`);
        },
        onError: (err) => {
          if (err instanceof ApiError) setErrors(err.fieldErrors());
        },
      },
    );
  };

  return (
    <Dialog
      open={open}
      onClose={onClose}
      title={`Clone ${camera.name}`}
      description="Same profile, parameters, accounts, protocols, picture and targets; its own ID, serial and MAC."
      footer={
        <>
          <Button onClick={onClose}>Cancel</Button>
          <Button variant="primary" type="submit" form="clone-camera" disabled={clone.isPending || !name.trim() || (!local && !ip.trim())}>
            {clone.isPending ? "Cloning…" : "Clone camera"}
          </Button>
        </>
      }
    >
      <form id="clone-camera" onSubmit={submit} className="grid grid-cols-2 gap-x-4 gap-y-3">
        <Field label="Name" error={errors["name"]} className="col-span-2">
          <Input value={name} onChange={(e) => setName(e.target.value)} required autoFocus />
        </Field>
        {!local && (
          <Field label="IP address" error={errors["network.ip"]} hint={`Same netmask and gateway as ${camera.name}.`} className="col-span-2">
            <Input value={ip} onChange={(e) => setIp(e.target.value)} placeholder="192.168.1.51" className="font-mono" required />
          </Field>
        )}
        <div className="col-span-2">
          <Checkbox label="Start it now" checked={start} onChange={(e) => setStart(e.target.checked)} />
        </div>
        {clone.error && !Object.keys(errors).length && (
          <div className="col-span-2">
            <Notice tone="error">{errorMessage(clone.error)}</Notice>
          </div>
        )}
      </form>
    </Dialog>
  );
}

/** The two restore levels of RN-10. */
export function ResetDialog({ camera, open, onClose }: { camera: Camera; open: boolean; onClose: () => void }) {
  const { data: profile } = useProfile({ id: camera.profile.id, version: camera.profile.version });
  const { data: node } = useNode();
  const reset = useResetCamera(camera.id);
  const [scope, setScope] = useState<"settings" | "full">("settings");
  const local = node?.runtime === "local";

  useEffect(() => {
    if (open) {
      setScope("settings");
      reset.reset();
    }
    // eslint-disable-next-line react-hooks/exhaustive-deps
  }, [open]);

  const factoryIP = profile?.factory_ip;
  const options = [
    {
      id: "settings" as const,
      title: "Restore settings",
      text: "Parameters, accounts and protocols go back to the profile's defaults. The network identity stays.",
    },
    {
      id: "full" as const,
      title: "Factory reset",
      text: local
        ? "Everything above, plus the MAC derived from the camera ID."
        : `Everything above, plus the profile's factory address${factoryIP ? ` ${factoryIP}` : ""} and the default MAC.`,
    },
  ];

  return (
    <Dialog
      open={open}
      onClose={onClose}
      title={`Restore ${camera.name}`}
      description="Like the reset button of the real device. The picture is kept."
      footer={
        <>
          <Button onClick={onClose}>Cancel</Button>
          <Button
            variant="danger"
            disabled={reset.isPending}
            onClick={() =>
              reset.mutate(scope, {
                onSuccess: () => {
                  toast(isRunning(camera) ? `${camera.name} restored; it reboots` : `${camera.name} restored`, "ok");
                  onClose();
                },
              })
            }
          >
            {reset.isPending ? "Restoring…" : scope === "full" ? "Factory reset" : "Restore settings"}
          </Button>
        </>
      }
    >
      <div role="radiogroup" aria-label="Restore level" className="flex flex-col gap-2">
        {options.map((o) => (
          <label
            key={o.id}
            className={cn(
              "flex cursor-pointer gap-3 rounded-sm border border-border p-3 hover:bg-surface-2",
              scope === o.id && "border-brand bg-surface-2",
            )}
          >
            <input type="radio" name="scope" className="mt-0.5 accent-brand" checked={scope === o.id} onChange={() => setScope(o.id)} />
            <span>
              <span className="block text-[13px] font-medium">{o.title}</span>
              <span className="block text-xs text-muted">{o.text}</span>
            </span>
          </label>
        ))}
      </div>
      {isRunning(camera) && (
        <p className="mt-3 text-xs text-muted">
          The camera is running: it reboots to apply the reset, as the real one does.
        </p>
      )}
      {scope === "full" && !local && !factoryIP && (
        <div className="mt-3">
          <Notice tone="warn">The profile declares no factory address, so a factory reset is refused.</Notice>
        </div>
      )}
      {reset.error && (
        <div className="mt-3">
          <Notice tone="error">{errorMessage(reset.error)}</Notice>
        </div>
      )}
      {scope === "full" && factoryIP && !local && (
        <p className="mt-3 text-xs text-muted">
          New address: <Mono>{factoryIP}</Mono>. Another camera cannot be using it.
        </p>
      )}
    </Dialog>
  );
}
