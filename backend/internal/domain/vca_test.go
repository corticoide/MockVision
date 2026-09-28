package domain

import (
	"errors"
	"math"
	"strings"
	"testing"
	"time"
)

// demoCaps is what a profile with lines, regions and their events allows.
var demoCaps = VCACaps{
	RuleTypes:     []RuleType{RuleLine, RuleRegion},
	ObjectClasses: []string{"person", "car", "truck"},
	Events: map[string]time.Duration{
		"line_crossing": time.Second, "region_entrance": 0, "region_exit": 0, "loitering": 0, "lpr": 2 * time.Second,
	},
}

func line(name string) Rule {
	return Rule{ID: "L-" + name, Name: name, Type: RuleLine, Points: []Point{{0.1, 0.5}, {0.9, 0.5}}, Direction: CrossBoth, Enabled: true}
}

func region(name string, events ...string) Rule {
	return Rule{ID: "R-" + name, Name: name, Type: RuleRegion, Points: []Point{{0.2, 0.2}, {0.8, 0.2}, {0.8, 0.8}, {0.2, 0.8}},
		Events: events, Enabled: true}
}

// fieldErrors returns the fields a validation error names.
func fieldErrors(t *testing.T, err error) map[string]string {
	t.Helper()
	out := map[string]string{}
	if err == nil {
		return out
	}
	var ve *ValidationError
	if !errors.As(err, &ve) {
		t.Fatalf("want a validation error, got %v", err)
	}
	for _, f := range ve.Fields {
		out[f.Field] = f.Message
	}
	return out
}

func TestValidateRules(t *testing.T) {
	ok := []Rule{line("Gate"), region("Parking", "region_entrance", "loitering")}
	ok[0].ObjectClasses = []string{"car", "truck"}
	if err := ValidateRules(ok, demoCaps); err != nil {
		t.Fatalf("valid rules refused: %v", err)
	}

	bowtie := region("Bowtie", "region_exit")
	bowtie.Points = []Point{{0.2, 0.2}, {0.8, 0.8}, {0.8, 0.2}, {0.2, 0.8}}
	touching := region("Touching", "region_exit")
	touching.Points = []Point{{0.2, 0.2}, {0.5, 0.5}, {0.8, 0.2}, {0.8, 0.8}, {0.5, 0.5}, {0.2, 0.8}}
	tiny := region("Tiny", "region_exit")
	tiny.Points = []Point{{0.5, 0.5}, {0.51, 0.5}, {0.5, 0.51}}
	withLine := func(change func(*Rule)) Rule {
		r := line("L")
		change(&r)
		return r
	}
	withRegion := func(change func(*Rule)) Rule {
		r := region("R", "loitering")
		change(&r)
		return r
	}
	cases := []struct {
		name  string
		rule  Rule
		caps  *VCACaps
		field string
		msg   string
	}{
		{"unknown type", Rule{Name: "X", Type: "circle"}, nil, "rules[0].type", "must be line or region"},
		{"three-point line", withLine(func(r *Rule) { r.Points = append(r.Points, Point{0.5, 0.9}) }), nil, "rules[0].points", "two points"},
		{"short line", withLine(func(r *Rule) { r.Points = []Point{{0.5, 0.5}, {0.505, 0.5}} }), nil, "rules[0].points", "too short"},
		{"outside the picture", withLine(func(r *Rule) { r.Points[1].X = 1.2 }), nil, "rules[0].points", "between 0 and 1"},
		{"not a number", withLine(func(r *Rule) { r.Points[0].Y = math.NaN() }), nil, "rules[0].points", "between 0 and 1"},
		{"two-point region", withRegion(func(r *Rule) { r.Points = r.Points[:2] }), nil, "rules[0].points", "from 3 to 20"},
		{"crossing sides", bowtie, nil, "rules[0].points", "cross"},
		{"sides touching at a corner", touching, nil, "rules[0].points", "cross"},
		{"tiny region", tiny, nil, "rules[0].points", "too small"},
		{"direction", withLine(func(r *Rule) { r.Direction = "left" }), nil, "rules[0].direction", "A->B, B->A or both"},
		{"region with a direction", withRegion(func(r *Rule) { r.Direction = CrossAB }), nil, "rules[0].direction", "only lines"},
		{"region without events", region("R"), nil, "rules[0].events", "at least one"},
		{"not a region event", region("R", "lpr"), nil, "rules[0].events", "not a region event"},
		{"undelivered region event", region("R", "intrusion"), nil, "rules[0].events", "does not deliver intrusion"},
		{"line events", withLine(func(r *Rule) { r.Events = []string{"loitering"} }), nil, "rules[0].events", "line_crossing events only"},
		{"unknown class", withLine(func(r *Rule) { r.ObjectClasses = []string{"boat"} }), nil, "rules[0].object_classes", `does not detect "boat"`},
		{"duplicated class", withLine(func(r *Rule) { r.ObjectClasses = []string{"car", "car"} }), nil, "rules[0].object_classes", "duplicated"},
		{"name with spaces", line(" L"), nil, "rules[0].name", "spaces"},
		{"long name", line(strings.Repeat("n", 33)), nil, "rules[0].name", "at most 32"},
		{"profile without lines", line("L"), &VCACaps{RuleTypes: []RuleType{RuleRegion}, Events: demoCaps.Events}, "rules[0].type", "has no line rules"},
		{"profile without crossings", line("L"), &VCACaps{RuleTypes: []RuleType{RuleLine}, Events: map[string]time.Duration{"lpr": 0}}, "rules[0].type", "does not deliver line_crossing"},
	}
	for _, c := range cases {
		caps := demoCaps
		if c.caps != nil {
			caps = *c.caps
		}
		got := fieldErrors(t, ValidateRules([]Rule{c.rule}, caps))
		if !strings.Contains(got[c.field], c.msg) {
			t.Errorf("%s: want %q on %s, got %v", c.name, c.msg, c.field, got)
		}
	}

	// Classes are free when the profile lists none.
	free := line("L")
	free.ObjectClasses = []string{"boat"}
	if err := ValidateRules([]Rule{free}, VCACaps{RuleTypes: []RuleType{RuleLine}, Events: demoCaps.Events}); err != nil {
		t.Errorf("a class the profile does not list was refused: %v", err)
	}

	dup := []Rule{line("Gate"), line("gate")}
	dup[1].ID = "other"
	if got := fieldErrors(t, ValidateRules(dup, demoCaps)); !strings.Contains(got["rules[1].name"], "duplicated") {
		t.Errorf("names differing in case: %v", got)
	}
	many := make([]Rule, MaxRules+1)
	for i := range many {
		many[i] = line("L" + strings.Repeat("x", i))
		many[i].ID = ""
	}
	if got := fieldErrors(t, ValidateRules(many, demoCaps)); got["rules"] == "" {
		t.Errorf("more than %d rules accepted", MaxRules)
	}
}

func TestValidateTriggers(t *testing.T) {
	rules := []Rule{line("Gate"), region("Parking", "region_entrance")}
	random := func(name, event, rule string) Trigger {
		return Trigger{Name: name, Type: TriggerRandom, EventType: event, RuleID: rule, MinSeconds: 5, MaxSeconds: 30, Enabled: true}
	}
	ok := []Trigger{
		random("Traffic", "line_crossing", "L-Gate"),
		random("Anywhere", "region_entrance", ""),
		func() Trigger {
			t := random("Plates", "lpr", "")
			t.Plates, t.PlateMasks = []string{"AB123CD"}, []string{`AA999AA`, `\A99`}
			t.Speed = &SpeedRange{Min: 20, Max: 90, Limit: 60, Unit: "km/h"}
			return t
		}(),
	}
	if err := ValidateTriggers(ok, rules, demoCaps); err != nil {
		t.Fatalf("valid triggers refused: %v", err)
	}

	lpr := func(change func(*Trigger)) Trigger {
		t := random("T", "lpr", "")
		change(&t)
		return t
	}
	cases := []struct {
		name    string
		trigger Trigger
		field   string
		msg     string
	}{
		{"manual is not stored", func() Trigger { t := random("T", "line_crossing", ""); t.Type = TriggerManual; return t }(), "triggers[0].type", "must be random"},
		{"unknown event", random("T", "explosion", ""), "triggers[0].event_type", "not a canonical"},
		{"undelivered event", random("T", "tamper", ""), "triggers[0].event_type", "does not deliver tamper"},
		{"missing rule", random("T", "line_crossing", "nope"), "triggers[0].rule_id", "not a rule of the camera"},
		{"rule of another kind", random("T", "line_crossing", "R-Parking"), "triggers[0].rule_id", "does not report line_crossing"},
		{"region event the rule lacks", random("T", "region_exit", "R-Parking"), "triggers[0].rule_id", "does not report region_exit"},
		{"rule for lpr", random("T", "lpr", "L-Gate"), "triggers[0].rule_id", "do not come from a rule"},
		{"zero seconds", lpr(func(t *Trigger) { t.MinSeconds = 0 }), "triggers[0].min_seconds", "between 1 and"},
		{"max below min", lpr(func(t *Trigger) { t.MaxSeconds = 2 }), "triggers[0].max_seconds", "less than the minimum"},
		{"faster than the profile", lpr(func(t *Trigger) { t.MinSeconds = 1 }), "triggers[0].min_seconds", "at least 2 s"},
		{"a day at most", lpr(func(t *Trigger) { t.MaxSeconds = MaxTriggerSeconds + 1 }), "triggers[0].max_seconds", "between 1 and"},
		{"long plate", lpr(func(t *Trigger) { t.Plates = []string{strings.Repeat("A", 17)} }), "triggers[0].plates", "1 to 16"},
		{"plate with spaces", lpr(func(t *Trigger) { t.Plates = []string{"AB 12 "} }), "triggers[0].plates", "spaces"},
		{"fixed mask", lpr(func(t *Trigger) { t.PlateMasks = []string{"BCD"} }), "triggers[0].plate_masks", "no variable positions"},
		{"long mask", lpr(func(t *Trigger) { t.PlateMasks = []string{strings.Repeat("9", 17)} }), "triggers[0].plate_masks", "longer than 16"},
		{"speed range", lpr(func(t *Trigger) { t.Speed = &SpeedRange{Min: 90, Max: 20, Unit: "km/h"} }), "triggers[0].speed", "minimum first"},
		{"speed unit", lpr(func(t *Trigger) { t.Speed = &SpeedRange{Min: 1, Max: 2, Unit: "knots"} }), "triggers[0].speed.unit", "km/h or mph"},
		{"speed limit", lpr(func(t *Trigger) { t.Speed = &SpeedRange{Min: 1, Max: 2, Limit: -1, Unit: "mph"} }), "triggers[0].speed.limit", "between 0"},
	}
	for _, c := range cases {
		got := fieldErrors(t, ValidateTriggers([]Trigger{c.trigger}, rules, demoCaps))
		if !strings.Contains(got[c.field], c.msg) {
			t.Errorf("%s: want %q on %s, got %v", c.name, c.msg, c.field, got)
		}
	}
}

func TestRuleEvents(t *testing.T) {
	if got := line("L").EventTypes(); len(got) != 1 || got[0] != "line_crossing" {
		t.Errorf("a line reports %v", got)
	}
	r := region("R", "region_exit", "intrusion")
	if !r.Reports("intrusion") || r.Reports("region_entrance") || r.Reports("line_crossing") {
		t.Errorf("region events: %v", r.EventTypes())
	}
	for typ, want := range map[string]RuleType{"line_crossing": RuleLine, "loitering": RuleRegion, "lpr": "", "custom:x": ""} {
		if got := RuleTypeFor(typ); got != want {
			t.Errorf("RuleTypeFor(%s) = %q, want %q", typ, got, want)
		}
	}
	for _, typ := range []string{"line_crossing", "region_exit"} {
		d, ok := DefaultRule(typ)
		if !ok || !d.Reports(typ) || ValidateRules([]Rule{d}, demoCaps) != nil {
			t.Errorf("default rule for %s: %+v", typ, d)
		}
	}
	if _, ok := DefaultRule("lpr"); ok {
		t.Error("lpr events have no default rule")
	}
}

func TestGeometry(t *testing.T) {
	// Walking right along a horizontal line, side A is above it.
	a, b := Point{0.1, 0.5}, Point{0.9, 0.5}
	if Side(a, b, Point{0.5, 0.4}) >= 0 || Side(a, b, Point{0.5, 0.6}) <= 0 || Side(a, b, Point{0.5, 0.5}) != 0 {
		t.Error("sides of a line")
	}
	sq := region("R", "loitering").Points
	if !Contains(sq, Point{0.5, 0.5}) || Contains(sq, Point{0.1, 0.5}) || Contains(sq, Point{0.9, 0.9}) {
		t.Error("point in a square")
	}
	if got := polygonArea(sq); math.Abs(got-0.36) > 1e-9 {
		t.Errorf("area %v, want 0.36", got)
	}
}
