import { Minus, Pencil, RotateCcw, Spline, Trash2, Zap } from "lucide-react";
import { type FormEvent, useState } from "react";
import { type Analytics, ApiError, type Camera, errorMessage, type Point, type ProfileDetail } from "@/api/client";
import { useCameraAnalytics, useProfile, useResetAnalytics, useSetCameraRules, useTrigger } from "@/api/queries";
import { Badge } from "@/components/badges";
import { toast } from "@/components/toast";
import { Button } from "@/components/ui/button";
import { Card, Empty, Notice } from "@/components/ui/card";
import { Checkbox, Field, Input, Select } from "@/components/ui/form";
import { useDraft } from "@/lib/draft";
import { type Translate, useT } from "@/lib/i18n";
import { cn, formatTime } from "@/lib/utils";
import { crosses, directionLabel, eventLabel, ruleChoices, ruleColors, ruleEvents } from "@/lib/vca";
import { isRunning, SaveBar, SectionTitle } from "./parts";
import { type CanvasRule, type Drawing, RuleCanvas } from "./RuleCanvas";

interface RuleDraft extends CanvasRule {
  id: string;
  events: string[];
  object_classes: string[];
}

const savedOf = (camera: Camera): RuleDraft[] =>
  camera.rules.map((r) => ({
    key: r.id,
    id: r.id,
    name: r.name,
    type: r.type,
    points: r.points,
    direction: r.direction,
    events: r.events,
    object_classes: r.object_classes,
    enabled: r.enabled,
  }));

/** The next free "Line n" or "Region n". */
function nextName(rules: RuleDraft[], type: "line" | "region", t: Translate) {
  const names = new Set(rules.map((r) => r.name.toLowerCase()));
  for (let n = 1; ; n++) {
    const name = type === "line" ? t("Line {n}", { n }) : t("Region {n}", { n });
    if (!names.has(name.toLowerCase())) return name;
  }
}

export function RulesTab({ camera }: { camera: Camera }) {
  const t = useT();
  const { data: profile, error } = useProfile({ id: camera.profile.id, version: camera.profile.version });
  if (error) return <Notice tone="error">{errorMessage(error)}</Notice>;
  if (!profile) return <Card className="p-4 text-muted">{t("Loading…")}</Card>;
  if (profile.vca.rules.length === 0) {
    return (
      <Card>
        <Empty title={t("This camera has no video analytics")}>
          {t("Its profile declares no rules: its events can still be fired from the Triggers tab.")}
        </Empty>
      </Card>
    );
  }
  return <RulesEditor camera={camera} profile={profile} />;
}

function RulesEditor({ camera, profile }: { camera: Camera; profile: ProfileDetail }) {
  const t = useT();
  const save = useSetCameraRules(camera.id);
  const trigger = useTrigger();
  const form = useDraft(savedOf(camera));
  const rules = form.draft;
  const [selected, setSelected] = useState<number | null>(null);
  const [drawing, setDrawing] = useState<Drawing | null>(null);
  const [errors, setErrors] = useState<Record<string, string>>({});
  const [seq, setSeq] = useState(0);
  const running = isRunning(camera);

  const kinds = profile.vca.rules;
  // What each kind of rule can report comes from the profile: a line may
  // report more than crossings, and a profile may have none.
  const lineChoices = ruleChoices("line", camera.event_types);
  const regionChoices = ruleChoices("region", camera.event_types);
  const classes = profile.vca.object_classes;

  // What the camera counted from its events, and where objects were.
  const [showHeat, setShowHeat] = useState(false);
  const stats = useCameraAnalytics(camera.id, showHeat ? 64 : 0, showHeat ? 36 : 0, running);
  const resetStats = useResetAnalytics(camera.id);
  const counts = running ? stats.data : undefined;
  const full = rules.length >= 16;
  const current = selected != null ? rules[selected] : null;

  const update = (i: number, patch: Partial<RuleDraft>) => form.update((rs) => rs.map((r, j) => (j === i ? { ...r, ...patch } : r)));

  const drawn = (points: Point[]) => {
    const d = drawing;
    setDrawing(null);
    if (!d) return;
    if (d.index != null) {
      update(d.index, { points });
      return;
    }
    const rule: RuleDraft = {
      key: `new-${seq}`,
      id: "",
      name: nextName(rules, d.type, t),
      type: d.type,
      points,
      direction: d.type === "line" ? "both" : "",
      events: d.type === "line" && lineChoices.includes("line_crossing") ? ["line_crossing"] : (d.type === "line" ? lineChoices : regionChoices).slice(0, 1),
      object_classes: [],
      enabled: true,
    };
    setSeq(seq + 1);
    form.update((rs) => [...rs, rule]);
    setSelected(rules.length);
  };

  const remove = (i: number) => {
    form.update((rs) => rs.filter((_, j) => j !== i));
    setSelected(null);
    setDrawing(null);
  };

  const fire = (rule: RuleDraft, type: string) =>
    trigger.mutate(
      { id: camera.id, type, rule_id: rule.id },
      {
        onSuccess: () => toast(t("{event} on {rule} sent from {name}", { event: eventLabel(type, t), rule: rule.name, name: camera.name }), "ok"),
        onError: (err) => toast(errorMessage(err), "error"),
      },
    );

  const submit = (e: FormEvent) => {
    e.preventDefault();
    setErrors({});
    setDrawing(null);
    save.mutate(
      rules.map((r) => ({
        id: r.id || undefined,
        name: r.name.trim(),
        type: r.type,
        points: r.points,
        direction: r.type === "line" ? (r.direction as "A->B" | "B->A" | "both") : undefined,
        events: r.events,
        object_classes: r.object_classes,
        enabled: r.enabled,
      })),
      {
        onSuccess: (saved) => {
          form.resetTo(savedOf(saved));
          toast(t("Rules saved; the camera uses them at once"), "ok");
        },
        onError: (err) => {
          if (err instanceof ApiError) setErrors(err.fieldErrors());
          toast(errorMessage(err), "error");
        },
      },
    );
  };

  const hint = !drawing
    ? t("Select a rule to edit it: drag its corners or the whole shape; a focused corner moves with the arrow keys.")
    : drawing.type === "line"
      ? t("Click where the line starts and where it ends. Side A is on its left, walking from the first point to the second.")
      : t("Click each corner. Click the first one again, double-click or press Enter to close it; Backspace removes the last corner, Escape cancels.");
  const fieldError = (i: number, f: string) => errors[`rules[${i}].${f}`];
  const hasError = (i: number) => Object.keys(errors).some((k) => k.startsWith(`rules[${i}]`));

  return (
    <div className="grid grid-cols-[minmax(0,1fr)_360px] items-start gap-4">
      <Card className="p-4">
        <div className="mb-3 flex flex-wrap items-center gap-2">
          {kinds.includes("line") && (
            <Button size="sm" disabled={full || drawing != null || lineChoices.length === 0} onClick={() => setDrawing({ type: "line", index: null })}>
              <Minus /> {t("Draw a line")}
            </Button>
          )}
          {kinds.includes("region") && (
            <Button size="sm" disabled={full || drawing != null || regionChoices.length === 0} onClick={() => setDrawing({ type: "region", index: null })}>
              <Spline /> {t("Draw a region")}
            </Button>
          )}
          {drawing && (
            <Button size="sm" variant="ghost" onClick={() => setDrawing(null)}>
              {t("Cancel")}
            </Button>
          )}
          <span className="text-xs text-muted">{full ? t("A camera has at most 16 rules.") : null}</span>
          <span className="ml-auto flex items-center gap-3">
            <Checkbox label={t("Heat map")} checked={showHeat} disabled={!running} onChange={(e) => setShowHeat(e.target.checked)} />
            <Button
              size="sm"
              variant="ghost"
              disabled={!running || resetStats.isPending}
              title={t("Start the counts again from zero")}
              onClick={() =>
                resetStats.mutate(undefined, {
                  onSuccess: () => toast(t("Counts reset"), "ok"),
                  onError: (err) => toast(errorMessage(err), "error"),
                })
              }
            >
              <RotateCcw /> {t("Reset counts")}
            </Button>
          </span>
        </div>
        <RuleCanvas
          camera={camera}
          rules={rules}
          selected={selected}
          drawing={drawing}
          onSelect={setSelected}
          onChange={(i, points) => update(i, { points })}
          onDrawn={drawn}
          onCancel={() => setDrawing(null)}
          heat={showHeat ? counts?.heat : undefined}
        />
        <p className="mt-2 text-xs text-muted">{hint}</p>
        <p className="mt-1 text-xs text-muted">
          {t(
            "Rules say where events happen; triggers say when. The camera does not look at the picture: an event names the rule, the direction and an object placed on it.",
          )}
        </p>
        <p className="mt-1 text-xs text-muted">
          {counts
            ? t("The camera counts crossings, entries and exits, and where objects were, from its events since {time}.", { time: formatTime(counts.since) })
            : t("Start the camera to see what it counts: crossings, entries and exits, and a heat map.")}
        </p>
      </Card>

      <Card className="p-4">
        <form id="camera-rules" onSubmit={submit}>
          <SectionTitle>{t("Rules")}</SectionTitle>
          {rules.length === 0 ? (
            <p className="mb-2 text-[13px] text-muted">{t("No rules yet. Draw a line or a region on the picture.")}</p>
          ) : (
            <ul className="mb-3 flex flex-col gap-1.5">
              {rules.map((r, i) => (
                <li
                  key={r.key}
                  className={cn(
                    "rounded-sm border px-2 py-1.5",
                    i === selected ? "border-info/60 bg-surface-2" : "border-border",
                    hasError(i) && "border-error/60",
                  )}
                >
                  <div className="flex items-center gap-2">
                    <span className={cn("size-2.5 shrink-0 rounded-full bg-current", ruleColors[i % ruleColors.length])} aria-hidden />
                    <button
                      type="button"
                      className="min-w-0 flex-1 cursor-pointer truncate text-left text-[13px] font-medium"
                      onClick={() => {
                        setDrawing(null);
                        setSelected(i === selected ? null : i);
                      }}
                      aria-pressed={i === selected}
                    >
                      {r.name || t("Unnamed")}
                    </button>
                    <Badge tone="muted">{r.type === "line" ? t("Line") : t("Region")}</Badge>
                    <Checkbox
                      label={<span className="sr-only">{t("Enabled")}</span>}
                      checked={r.enabled}
                      onChange={(e) => update(i, { enabled: e.target.checked })}
                      title={t("Enabled")}
                    />
                    <Button size="icon" variant="ghost" title={t("Remove")} aria-label={t("Remove {name}", { name: r.name })} onClick={() => remove(i)}>
                      <Trash2 />
                    </Button>
                  </div>
                  {counts && r.id && <RuleCounts rule={r} counts={counts} t={t} />}
                  <div className="mt-1 flex flex-wrap gap-1 pl-4.5">
                    {ruleEvents(r).map((type) => (
                      <Button
                        key={type}
                        size="sm"
                        variant="ghost"
                        className="h-6 px-1.5 text-[11px]"
                        disabled={!running || !r.id || form.dirty || !r.enabled || trigger.isPending}
                        title={
                          !running
                            ? t("Start the camera to fire events")
                            : !r.id || form.dirty
                              ? t("Save the rules first")
                              : !r.enabled
                                ? t("The rule is disabled")
                                : t("Fire it now")
                        }
                        onClick={() => fire(r, type)}
                      >
                        <Zap /> {eventLabel(type, t)}
                      </Button>
                    ))}
                  </div>
                </li>
              ))}
            </ul>
          )}

          {current && selected != null && (
            <div className="flex flex-col gap-3 border-t border-border pt-3">
              <Field label={t("Name")} error={fieldError(selected, "name")}>
                <Input value={current.name} maxLength={32} onChange={(e) => update(selected, { name: e.target.value })} required />
              </Field>
              {current.type === "line" && (
                <Field
                  label={t("Direction")}
                  hint={t("Which crossings the line reports: from side A to side B, the other way, or both.")}
                  error={fieldError(selected, "direction")}
                >
                  <Select value={current.direction} onChange={(e) => update(selected, { direction: e.target.value })}>
                    {["both", "A->B", "B->A"].map((d) => (
                      <option key={d} value={d}>
                        {directionLabel(d, t)}
                      </option>
                    ))}
                  </Select>
                </Field>
              )}
              {(current.type === "region" || lineChoices.length > 1) && (
                <Field
                  label={t("Events")}
                  group
                  error={fieldError(selected, "events")}
                  hint={current.type === "line" ? t("What the line reports.") : t("What the region reports.")}
                >
                  {(current.type === "line" ? lineChoices : regionChoices).map((type, _, choices) => (
                    <Checkbox
                      key={type}
                      label={eventLabel(type, t)}
                      checked={current.events.includes(type)}
                      onChange={(e) =>
                        update(selected, {
                          events: e.target.checked ? choices.filter((x) => x === type || current.events.includes(x)) : current.events.filter((x) => x !== type),
                        })
                      }
                    />
                  ))}
                </Field>
              )}
              {classes.length > 0 && (
                <Field label={t("Objects")} group error={fieldError(selected, "object_classes")} hint={t("None checked: every object the camera detects.")}>
                  <div className="grid grid-cols-2 gap-1">
                    {classes.map((c) => (
                      <Checkbox
                        key={c}
                        label={c}
                        checked={current.object_classes.includes(c)}
                        onChange={(e) =>
                          update(selected, {
                            object_classes: e.target.checked ? classes.filter((x) => x === c || current.object_classes.includes(x)) : current.object_classes.filter((x) => x !== c),
                          })
                        }
                      />
                    ))}
                  </div>
                </Field>
              )}
              {fieldError(selected, "points") && <Notice tone="error">{fieldError(selected, "points")}</Notice>}
              {current.type === "region" && crosses(current.points) && <Notice tone="warn">{t("The sides of the region cross each other; move a corner.")}</Notice>}
              <div>
                <Button size="sm" onClick={() => setDrawing({ type: current.type, index: selected })} disabled={drawing != null}>
                  <Pencil /> {t("Draw it again")}
                </Button>
              </div>
            </div>
          )}
          {errors["rules"] && (
            <div className="mt-3">
              <Notice tone="error">{errors["rules"]}</Notice>
            </div>
          )}
          <SaveBar
            form="camera-rules"
            dirty={form.dirty}
            saving={save.isPending}
            onDiscard={() => {
              form.discard();
              setErrors({});
              setDrawing(null);
              setSelected(null);
            }}
            note={t("Changes apply at once, without restarting the camera.")}
          />
        </form>
      </Card>
    </div>
  );
}

/** RuleCounts shows what the camera counted on a rule: crossings of a line
 * each way, or the entries, exits and occupancy of a region. */
function RuleCounts({ rule, counts, t }: { rule: { id: string; type: "line" | "region" }; counts: Analytics; t: Translate }) {
  if (rule.type === "line") {
    const c = counts.lines.find((l) => l.rule_id === rule.id);
    if (!c) return null;
    return (
      <p className="mt-0.5 pl-4.5 font-mono text-[11px] text-muted">
        A → B {c.a_to_b} · B → A {c.b_to_a}
      </p>
    );
  }
  const c = counts.regions.find((r) => r.rule_id === rule.id);
  if (!c) return null;
  return (
    <p className="mt-0.5 pl-4.5 font-mono text-[11px] text-muted">
      {t("Inside {n}", { n: c.occupancy })} · {t("In {n}", { n: c.entries })} · {t("Out {n}", { n: c.exits })}
    </p>
  );
}
