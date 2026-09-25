import { type FormEvent, useEffect, useMemo, useState } from "react";
import { ApiError, type CreateCamera, errorMessage } from "@/api/client";
import { useAssets, useCreateCamera, useNode, useProfile, useProfiles, useTargets } from "@/api/queries";
import { toast } from "@/components/toast";
import { Button } from "@/components/ui/button";
import { Notice } from "@/components/ui/card";
import { Dialog } from "@/components/ui/dialog";
import { Checkbox, Field, Input, Select } from "@/components/ui/form";
import { navigate } from "@/lib/router";

export function NewCameraDialog({ open, onClose }: { open: boolean; onClose: () => void }) {
  const { data: node } = useNode();
  const { data: profiles } = useProfiles();
  const { data: assets } = useAssets();
  const { data: targets } = useTargets();
  const create = useCreateCamera();

  const available = useMemo(() => (profiles ?? []).filter((p) => !p.archived), [profiles]);
  const [profileKey, setProfileKey] = useState("");
  const ref = useMemo(() => {
    if (!profileKey) return null;
    const [id, version] = profileKey.split("@");
    return { id, version };
  }, [profileKey]);
  const { data: profile } = useProfile(ref);
  const stream = profile?.streams.find((s) => s.name === "main");

  const [name, setName] = useState("");
  const [ip, setIp] = useState("");
  const [netmask, setNetmask] = useState("255.255.255.0");
  const [gateway, setGateway] = useState("");
  const [resolution, setResolution] = useState("");
  const [assetId, setAssetId] = useState("");
  const [username, setUsername] = useState("");
  const [password, setPassword] = useState("");
  const [targetIds, setTargetIds] = useState<string[]>([]);
  const [autostart, setAutostart] = useState(true);
  const [start, setStart] = useState(true);
  const [fieldErrors, setFieldErrors] = useState<Record<string, string>>({});

  useEffect(() => {
    if (!profileKey && available.length > 0) setProfileKey(`${available[0].profile_id}@${available[0].version}`);
  }, [available, profileKey]);
  useEffect(() => {
    if (stream) setResolution(stream.default.resolution);
    if (profile && !username) setUsername(profile.factory_users[0]?.username ?? "admin");
  }, [stream, profile, username]);
  useEffect(() => {
    if (open) {
      setFieldErrors({});
      create.reset();
    }
    // eslint-disable-next-line react-hooks/exhaustive-deps
  }, [open]);

  const local = node?.runtime === "local";

  const submit = (e: FormEvent) => {
    e.preventDefault();
    if (!ref) return;
    const body: CreateCamera = {
      name: name.trim(),
      profile_id: ref.id,
      profile_version: ref.version,
      network: local ? undefined : { ip: ip.trim(), netmask: netmask.trim() || undefined, gateway: gateway.trim() || undefined },
      stream: { resolution: resolution || undefined, asset_id: assetId || undefined },
      users: password ? [{ username: username.trim() || "admin", password, role: "admin" }] : undefined,
      target_ids: targetIds,
      autostart,
      start,
    };
    create.mutate(body, {
      onSuccess: (cam) => {
        toast(`Camera ${cam.name} created${start ? "; starting" : ""}`, "ok");
        setName("");
        setIp("");
        setPassword("");
        onClose();
      },
      onError: (err) => {
        if (err instanceof ApiError) setFieldErrors(err.fieldErrors());
      },
    });
  };

  const noProfiles = profiles && available.length === 0;

  return (
    <Dialog
      open={open}
      onClose={onClose}
      title="New camera"
      description="The camera appears on the LAN with its own IP and MAC and behaves as its profile describes."
      footer={
        <>
          <Button onClick={onClose}>Cancel</Button>
          <Button variant="primary" type="submit" form="new-camera" disabled={create.isPending || !ref || !name.trim()}>
            {create.isPending ? "Creating…" : "Create camera"}
          </Button>
        </>
      }
    >
      {noProfiles ? (
        <Notice tone="warn">
          No profiles installed yet.{" "}
          <button className="cursor-pointer underline" onClick={() => navigate("/profiles")}>
            Import a profile
          </button>{" "}
          first, for example <span className="font-mono">profiles/milesight-demo.yaml</span>.
        </Notice>
      ) : (
        <form id="new-camera" onSubmit={submit} className="grid grid-cols-2 gap-x-4 gap-y-3">
          <Field label="Name" error={fieldErrors["name"]} className="col-span-2">
            <Input value={name} onChange={(e) => setName(e.target.value)} placeholder="Gate 1" required autoFocus />
          </Field>
          <Field label="Profile" className="col-span-2">
            <Select value={profileKey} onChange={(e) => setProfileKey(e.target.value)}>
              {available.map((p) => (
                <option key={p.id} value={`${p.profile_id}@${p.version}`}>
                  {p.name} — {p.profile_id}@{p.version} ({p.level})
                </option>
              ))}
            </Select>
          </Field>

          {local ? (
            <div className="col-span-2">
              <Notice tone="info">Local mode: the camera answers on 127.0.0.1 with its own ports; no IP or MAC on the LAN.</Notice>
            </div>
          ) : (
            <>
              <Field label="IP address" error={fieldErrors["network.ip"]} hint={profile?.factory_ip ? `Factory IP: ${profile.factory_ip}` : undefined}>
                <Input value={ip} onChange={(e) => setIp(e.target.value)} placeholder="192.168.1.50" required className="font-mono" />
              </Field>
              <Field label="Netmask" error={fieldErrors["network.netmask"]}>
                <Input value={netmask} onChange={(e) => setNetmask(e.target.value)} className="font-mono" />
              </Field>
              <Field label="Gateway" error={fieldErrors["network.gateway"]} hint="Empty: the node's gateway when it is in the subnet.">
                <Input value={gateway} onChange={(e) => setGateway(e.target.value)} className="font-mono" />
              </Field>
              <div />
            </>
          )}

          <Field label="Resolution" error={fieldErrors["stream.resolution"]}>
            <Select value={resolution} onChange={(e) => setResolution(e.target.value)}>
              {stream?.resolutions.map((r) => (
                <option key={r} value={r}>
                  {r}
                  {r === stream.default.resolution ? " (default)" : ""}
                </option>
              ))}
            </Select>
          </Field>
          <Field label="Image" error={fieldErrors["stream.asset_id"]} hint="The stream loops this picture, encoded once.">
            <Select value={assetId} onChange={(e) => setAssetId(e.target.value)}>
              <option value="">Test pattern</option>
              {assets
                ?.filter((a) => !a.builtin)
                .map((a) => (
                  <option key={a.id} value={a.id}>
                    {a.filename} ({a.width}×{a.height})
                  </option>
                ))}
            </Select>
          </Field>

          <Field label="Camera user" error={fieldErrors["users[0].username"]}>
            <Input value={username} onChange={(e) => setUsername(e.target.value)} autoComplete="off" />
          </Field>
          <Field label="Password" error={fieldErrors["users[0].password"]} hint="Empty: the profile's factory account.">
            <Input type="password" value={password} onChange={(e) => setPassword(e.target.value)} autoComplete="new-password" />
          </Field>

          <Field label="Event targets" group className="col-span-2" hint={targets?.length ? undefined : "No targets yet; add them in Targets."}>
            <div className="flex flex-wrap gap-x-4 gap-y-1 py-1">
              {targets?.map((t) => (
                <Checkbox
                  key={t.id}
                  label={`${t.name} (${t.url})`}
                  checked={targetIds.includes(t.id)}
                  onChange={(e) => setTargetIds((ids) => (e.target.checked ? [...ids, t.id] : ids.filter((x) => x !== t.id)))}
                />
              ))}
            </div>
          </Field>

          <div className="col-span-2 flex gap-6">
            <Checkbox label="Start with the node (autostart)" checked={autostart} onChange={(e) => setAutostart(e.target.checked)} />
            <Checkbox label="Start now" checked={start} onChange={(e) => setStart(e.target.checked)} />
          </div>

          {create.error && (
            <div className="col-span-2">
              <Notice tone="error">{errorMessage(create.error)}</Notice>
            </div>
          )}
        </form>
      )}
    </Dialog>
  );
}
