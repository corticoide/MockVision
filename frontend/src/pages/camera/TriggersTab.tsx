import { ChevronDown, ChevronRight, Plus, Trash2, Zap } from "lucide-react";
import { type FormEvent, useState } from "react";
import { ApiError, type Camera, errorMessage, type ProfileDetail, type Trigger, type TriggerInput } from "@/api/client";
import { useFireTrigger, useProfile, useSetCameraTriggers, useTrigger } from "@/api/queries";
import { toast } from "@/components/toast";
import { Button } from "@/components/ui/button";
import { Card, CardHeader, Empty, Notice } from "@/components/ui/card";
import { Checkbox, Field, Input, Select, Textarea } from "@/components/ui/form";
import { useDraft } from "@/lib/draft";
import { type Translate, useT } from "@/lib/i18n";
import { directionLabel, eventLabel, isReport, quickEvent, ruleEvents, ruleTypeFor } from "@/lib/vca";
import { isRunning, SaveBar } from "./parts";

/** Event types the camera can emit: the profile's, with a transport. */
function emittable(profile: ProfileDetail) {
  return profile.event_specs.filter((e) => e.transports.length > 0);
}

export function TriggersTab({ camera }: { camera: Camera }) {
  const t = useT();
  const { data: profile, error } = useProfile({ id: camera.profile.id, version: camera.profile.version });
  if (error) return <Notice tone="error">{errorMessage(error)}</Notice>;
  if (!profile) return <Card className="p-4 text-muted">{t("Loading…")}</Card>;
  if (emittable(profile).length === 0) {
    return (
      <Card>
        <Empty title={t("This camera emits no events")}>{t("Its profile defines no event with a transport.")}</Empty>
      </Card>
    );
  }
  return (
    <div className="flex flex-col gap-4">
      <Card>
        <CardHeader
          title={t("Fire an event now")}
          description={t("A manual trigger: the camera sends the event to its targets at once. What you leave empty, the camera makes up.")}
        />
        <div className="p-4">
          <ManualEventForm camera={camera} profile={profile} />
        </div>
      </Card>
      <RandomTriggers camera={camera} profile={profile} />
    </div>
  );
}

/** A manual trigger: an event of a type, on a rule, with the data given. */
export function ManualEventForm({ camera, profile, onSent }: { camera: Camera; profile: ProfileDetail; onSent?: () => void }) {
  const t = useT();
  const trigger = useTrigger();
  const types = emittable(profile).map((e) => e.type);
  const quick = quickEvent(camera)?.type;
  const [type, setType] = useState(quick && types.includes(quick) ? quick : types[0]);
  const [ruleId, setRuleId] = useState("");
  const [direction, setDirection] = useState("");
  const [objectClass, setObjectClass] = useState("");
  const [plate, setPlate] = useState("");
  const [speed, setSpeed] = useState("");
  const running = isRunning(camera);

  const kind = ruleTypeFor(type, camera.event_types);
  const report = isReport(type, camera.event_types);
  const candidates = camera.rules.filter((r) => ruleEvents(r).includes(type));
  // Events of lines and regions happen on one: without any, none can.
  const noRule = kind !== "" && !candidates.some((r) => r.enabled);
  const rule = candidates.find((r) => r.id === ruleId);
  const directions = kind !== "line" ? [] : rule && rule.direction !== "both" ? [rule.direction] : ["A->B", "B->A"];
  const classes = rule?.object_classes.length ? rule.object_classes : profile.vca.object_classes;

  const choose = (next: string) => {
    setType(next);
    setRuleId("");
    setDirection("");
  };

  const submit = (e: FormEvent) => {
    e.preventDefault();
    trigger.mutate(
      {
        id: camera.id,
        type,
        rule_id: ruleId || undefined,
        direction: (direction || undefined) as "A->B" | "B->A" | undefined,
        object: objectClass ? { class: objectClass } : undefined,
        plate: plate.trim() ? { text: plate.trim() } : undefined,
        speed: speed ? { value: Number(speed), unit: "km/h" } : undefined,
      },
      {
        onSuccess: () => {
          toast(t("{event} sent from {name}", { event: eventLabel(type, t), name: camera.name }), "ok");
          onSent?.();
        },
        onError: (err) => toast(errorMessage(err), "error"),
      },
    );
  };

  return (
    <form onSubmit={submit} className="flex flex-col gap-3">
      <div className="grid grid-cols-[repeat(auto-fill,minmax(210px,1fr))] gap-3">
        <Field label={t("Event")}>
          <Select value={type} onChange={(e) => choose(e.target.value)}>
            {types.map((ty) => (
              <option key={ty} value={ty}>
                {eventLabel(ty, t)}
              </option>
            ))}
          </Select>
        </Field>
        {kind && (
          <Field label={t("Rule")} error={noRule ? t("No enabled rule reports it: draw one in the Rules tab.") : undefined}>
            <Select value={ruleId} onChange={(e) => setRuleId(e.target.value)} disabled={noRule}>
              <option value="">{t("The first enabled one")}</option>
              {candidates.map((r) => (
                <option key={r.id} value={r.id} disabled={!r.enabled}>
                  {r.enabled ? r.name : t("{name} (disabled)", { name: r.name })}
                </option>
              ))}
            </Select>
          </Field>
        )}
        {kind === "line" && (
          <Field label={t("Direction")}>
            <Select value={direction} onChange={(e) => setDirection(e.target.value)}>
              <option value="">{t("As the rule reports")}</option>
              {directions.map((d) => (
                <option key={d} value={d}>
                  {directionLabel(d, t)}
                </option>
              ))}
            </Select>
          </Field>
        )}
        {classes.length > 0 && !report && (
          <Field label={t("Object")}>
            <Select value={objectClass} onChange={(e) => setObjectClass(e.target.value)}>
              <option value="">{t("Any")}</option>
              {classes.map((c) => (
                <option key={c} value={c}>
                  {c}
                </option>
              ))}
            </Select>
          </Field>
        )}
        {type === "lpr" && (
          <Field label={t("Plate")} hint={t("Empty: a made-up one.")}>
            <Input value={plate} maxLength={16} onChange={(e) => setPlate(e.target.value)} placeholder="AB123CD" className="font-mono" />
          </Field>
        )}
        {type === "speed" && (
          <Field label={t("Speed (km/h)")} hint={t("Empty: a made-up one.")}>
            <Input type="number" min={0} max={500} value={speed} onChange={(e) => setSpeed(e.target.value)} />
          </Field>
        )}
      </div>
      <div className="flex items-center gap-3">
        <Button type="submit" variant="primary" size="sm" disabled={!running || noRule || trigger.isPending}>
          <Zap /> {t("Fire")}
        </Button>
        {!running && <span className="text-xs text-muted">{t("Start the camera to fire events.")}</span>}
        {running && report && <span className="text-xs text-muted">{t("A report carries what the camera counted so far.")}</span>}
      </div>
    </form>
  );
}

interface TriggerDraft {
  key: string;
  id: string;
  name: string;
  event_type: string;
  rule_id: string;
  min_seconds: string;
  max_seconds: string;
  plates: string;
  plate_masks: string;
  speed: { min: string; max: string; limit: string; unit: string } | null;
  enabled: boolean;
}

const draftOf = (tr: Trigger): TriggerDraft => ({
  key: tr.id,
  id: tr.id,
  name: tr.name,
  event_type: tr.event_type,
  rule_id: tr.rule_id,
  min_seconds: String(tr.min_seconds),
  max_seconds: String(tr.max_seconds),
  plates: tr.plates.join("\n"),
  plate_masks: tr.plate_masks.join(", "),
  speed: tr.speed ? { min: String(tr.speed.min), max: String(tr.speed.max), limit: String(tr.speed.limit), unit: tr.speed.unit } : null,
  enabled: tr.enabled,
});

const list = (s: string) =>
  s
    .split(/[\n,]/)
    .map((x) => x.trim())
    .filter(Boolean);

function nextName(triggers: TriggerDraft[], t: Translate) {
  const names = new Set(triggers.map((x) => x.name.toLowerCase()));
  for (let n = 1; ; n++) {
    const name = t("Random {n}", { n });
    if (!names.has(name.toLowerCase())) return name;
  }
}

function RandomTriggers({ camera, profile }: { camera: Camera; profile: ProfileDetail }) {
  const t = useT();
  const save = useSetCameraTriggers(camera.id);
  const fire = useFireTrigger();
  const form = useDraft(camera.triggers.map(draftOf));
  const triggers = form.draft;
  const [errors, setErrors] = useState<Record<string, string>>({});
  const [open, setOpen] = useState<Set<string>>(new Set());
  const [seq, setSeq] = useState(0);
  const running = isRunning(camera);
  const specs = emittable(profile);

  const update = (i: number, patch: Partial<TriggerDraft>) => form.update((ts) => ts.map((x, j) => (j === i ? { ...x, ...patch } : x)));
  const toggle = (key: string) =>
    setOpen((s) => {
      const next = new Set(s);
      if (next.has(key)) next.delete(key);
      else next.add(key);
      return next;
    });

  const add = () => {
    const type = quickEvent(camera)?.type;
    const spec = specs.find((s) => s.type === type) ?? specs[0];
    const min = Math.max(5, Math.ceil(spec.min_interval_ms / 1000));
    form.update((ts) => [
      ...ts,
      {
        key: `new-${seq}`,
        id: "",
        name: nextName(ts, t),
        event_type: spec.type,
        rule_id: "",
        min_seconds: String(min),
        max_seconds: String(min * 6),
        plates: "",
        plate_masks: "",
        speed: null,
        enabled: true,
      },
    ]);
    setSeq(seq + 1);
  };

  const submit = (e: FormEvent) => {
    e.preventDefault();
    setErrors({});
    const body: TriggerInput[] = triggers.map((x) => ({
      id: x.id || undefined,
      name: x.name.trim(),
      type: "random",
      event_type: x.event_type,
      rule_id: x.rule_id,
      min_seconds: Number(x.min_seconds),
      max_seconds: Number(x.max_seconds),
      plates: list(x.plates),
      plate_masks: list(x.plate_masks),
      speed: x.speed
        ? { min: Number(x.speed.min), max: Number(x.speed.max), limit: Number(x.speed.limit || 0), unit: x.speed.unit as "km/h" | "mph" }
        : null,
      enabled: x.enabled,
    }));
    save.mutate(body, {
      onSuccess: (saved) => {
        form.resetTo(saved.triggers.map(draftOf));
        toast(t("Triggers saved; the camera uses them at once"), "ok");
      },
      onError: (err) => {
        if (err instanceof ApiError) setErrors(err.fieldErrors());
        toast(errorMessage(err), "error");
      },
    });
  };

  const fireOnce = (x: TriggerDraft) =>
    fire.mutate(
      { id: camera.id, trigger: x.id },
      {
        onSuccess: () => toast(t("{trigger} fired once from {name}", { trigger: x.name, name: camera.name }), "ok"),
        onError: (err) => toast(errorMessage(err), "error"),
      },
    );

  return (
    <Card>
      <CardHeader
        title={t("Random triggers")}
        description={t("While the camera runs, each enabled trigger emits its event at a random moment within its wait, again and again: steady traffic to test against.")}
        actions={
          <Button size="sm" onClick={add} disabled={triggers.length >= 16}>
            <Plus /> {t("Add trigger")}
          </Button>
        }
      />
      <form id="camera-triggers" onSubmit={submit} className="p-4">
        {triggers.length === 0 && <p className="text-[13px] text-muted">{t("No random triggers yet.")}</p>}
        <div className="flex flex-col gap-3">
          {triggers.map((x, i) => {
            const e = (f: string) => errors[`triggers[${i}].${f}`];
            const kind = ruleTypeFor(x.event_type, camera.event_types);
            const report = isReport(x.event_type, camera.event_types);
            const candidates = camera.rules.filter((r) => ruleEvents(r).includes(x.event_type));
            const rule = candidates.find((r) => r.id === x.rule_id);
            const spec = specs.find((s) => s.type === x.event_type);
            const expanded = open.has(x.key);
            const warning =
              kind && x.rule_id && rule && !rule.enabled
                ? t("Its rule is disabled: the trigger raises nothing.")
                : kind && !x.rule_id && !candidates.some((r) => r.enabled)
                  ? t("No enabled rule reports these events: the trigger raises nothing until one does.")
                  : "";
            return (
              <div key={x.key} className="rounded-sm border border-border p-3">
                <div className="grid grid-cols-[minmax(0,1.1fr)_minmax(0,1fr)_minmax(0,1fr)_auto] items-start gap-3">
                  <Field label={t("Name")} error={e("name")}>
                    <Input value={x.name} maxLength={32} onChange={(ev) => update(i, { name: ev.target.value })} required />
                  </Field>
                  <Field label={t("Event")} error={e("event_type")}>
                    <Select value={x.event_type} onChange={(ev) => update(i, { event_type: ev.target.value, rule_id: "" })}>
                      {specs.map((s) => (
                        <option key={s.type} value={s.type}>
                          {eventLabel(s.type, t)}
                        </option>
                      ))}
                    </Select>
                  </Field>
                  <Field label={t("Rule")} error={e("rule_id")}>
                    <Select value={x.rule_id} onChange={(ev) => update(i, { rule_id: ev.target.value })} disabled={!kind}>
                      <option value="">{kind ? t("Any enabled rule") : report ? t("None: a report") : t("None: not a rule event")}</option>
                      {candidates.map((r) => (
                        <option key={r.id} value={r.id}>
                          {r.enabled ? r.name : t("{name} (disabled)", { name: r.name })}
                        </option>
                      ))}
                    </Select>
                  </Field>
                  <Field
                    label={t("Every (seconds)")}
                    error={e("min_seconds") || e("max_seconds")}
                    hint={spec && spec.min_interval_ms > 0 ? t("At least {s} s for this event.", { s: Math.ceil(spec.min_interval_ms / 1000) }) : undefined}
                  >
                    <div className="flex items-center gap-1.5">
                      <Input
                        type="number"
                        min={1}
                        max={86400}
                        value={x.min_seconds}
                        onChange={(ev) => update(i, { min_seconds: ev.target.value })}
                        className="w-20"
                        aria-label={t("Shortest wait")}
                      />
                      <span className="text-muted">–</span>
                      <Input
                        type="number"
                        min={1}
                        max={86400}
                        value={x.max_seconds}
                        onChange={(ev) => update(i, { max_seconds: ev.target.value })}
                        className="w-20"
                        aria-label={t("Longest wait")}
                      />
                    </div>
                  </Field>
                </div>
                {warning && <p className="mt-2 text-xs text-warn">{warning}</p>}
                {report && (
                  <p className="mt-2 text-xs text-muted">
                    {t("A report carries what the camera counted; the same shortest and longest wait sends one at a fixed interval.")}
                  </p>
                )}
                <div className="mt-2 flex flex-wrap items-center gap-3">
                  <Checkbox label={t("Enabled")} checked={x.enabled} onCheckedChange={(checked) => update(i, { enabled: checked })} />
                  {!report && (
                    <Button size="sm" variant="ghost" onClick={() => toggle(x.key)} aria-expanded={expanded}>
                      {expanded ? <ChevronDown /> : <ChevronRight />} {t("Event data")}
                    </Button>
                  )}
                  <span className="ml-auto" />
                  <Button
                    size="sm"
                    variant="ghost"
                    disabled={!running || !x.id || form.dirty || fire.isPending}
                    title={!running ? t("Start the camera to fire events") : !x.id || form.dirty ? t("Save the triggers first") : t("Fire it now")}
                    onClick={() => fireOnce(x)}
                  >
                    <Zap /> {t("Fire once")}
                  </Button>
                  <Button
                    size="icon"
                    variant="ghost"
                    title={t("Remove")}
                    aria-label={t("Remove {name}", { name: x.name })}
                    onClick={() => form.update((ts) => ts.filter((_, j) => j !== i))}
                  >
                    <Trash2 />
                  </Button>
                </div>
                {expanded && !report && (
                  <div className="mt-3 grid grid-cols-[minmax(0,1fr)_minmax(0,1fr)_minmax(0,1.2fr)] items-start gap-3 border-t border-border pt-3">
                    <Field label={t("Plates")} error={e("plates")} hint={t("One per line; each event picks one.")}>
                      <Textarea
                        value={x.plates}
                        onChange={(ev) => update(i, { plates: ev.target.value })}
                        rows={3}
                        className="font-mono"
                        placeholder={"AB123CD\nAC456DE"}
                      />
                    </Field>
                    <Field label={t("Plate formats")} error={e("plate_masks")} hint={t("9 a digit, A a letter, X a hex digit; the rest as typed. AA999AA makes AB123CD.")}>
                      <Input
                        value={x.plate_masks}
                        onChange={(ev) => update(i, { plate_masks: ev.target.value })}
                        placeholder="AA999AA, AAA999"
                        className="font-mono"
                      />
                    </Field>
                    <Field label={t("Speed")} group error={e("speed") || e("speed.limit") || e("speed.unit")}>
                      <Checkbox
                        label={t("Give each event a speed")}
                        checked={x.speed != null}
                        onCheckedChange={(checked) => update(i, { speed: checked ? { min: "20", max: "120", limit: "60", unit: "km/h" } : null })}
                      />
                      {x.speed && (
                        <div className="flex flex-wrap items-center gap-1.5">
                          <Input
                            type="number"
                            value={x.speed.min}
                            onChange={(ev) => update(i, { speed: { ...x.speed!, min: ev.target.value } })}
                            className="w-18"
                            aria-label={t("Lowest speed")}
                          />
                          <span className="text-muted">–</span>
                          <Input
                            type="number"
                            value={x.speed.max}
                            onChange={(ev) => update(i, { speed: { ...x.speed!, max: ev.target.value } })}
                            className="w-18"
                            aria-label={t("Highest speed")}
                          />
                          <Select value={x.speed.unit} onChange={(ev) => update(i, { speed: { ...x.speed!, unit: ev.target.value } })} className="w-22" aria-label={t("Unit")}>
                            <option value="km/h">km/h</option>
                            <option value="mph">mph</option>
                          </Select>
                          <span className="text-xs text-muted">{t("limit")}</span>
                          <Input
                            type="number"
                            value={x.speed.limit}
                            onChange={(ev) => update(i, { speed: { ...x.speed!, limit: ev.target.value } })}
                            className="w-18"
                            aria-label={t("Speed limit")}
                          />
                        </div>
                      )}
                    </Field>
                  </div>
                )}
              </div>
            );
          })}
        </div>
        {errors["triggers"] && (
          <div className="mt-3">
            <Notice tone="error">{errors["triggers"]}</Notice>
          </div>
        )}
        <SaveBar
          form="camera-triggers"
          dirty={form.dirty}
          saving={save.isPending}
          onDiscard={() => {
            form.discard();
            setErrors({});
          }}
          note={t("Changes apply at once. A trigger's events take its plates, formats and speed; lpr events always carry a plate.")}
        />
      </form>
    </Card>
  );
}
