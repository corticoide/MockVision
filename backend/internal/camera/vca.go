package camera

import (
	"context"
	"errors"
	"fmt"
	"math"
	"math/rand/v2"
	"reflect"
	"slices"
	"sync"
	"time"

	"github.com/corticoide/mockvision/backend/internal/domain"
	"github.com/corticoide/mockvision/backend/internal/ipc"
	"github.com/corticoide/mockvision/sdk/engine"
)

// vcaRuntime is the camera's video analytics (D39, D40): it keeps the rules
// and stored triggers the service configured, builds the canonical events
// they raise and runs the random triggers while the camera runs. There is no
// detection on the picture: triggers say when an object passes, and the
// rules where.
type vcaRuntime struct {
	rt *Runtime
	// second is how long a trigger second lasts; tests shorten it.
	second time.Duration

	// stats counts what the events the camera emits show.
	stats *analytics

	mu       sync.Mutex
	caps     domain.VCACaps
	rules    []domain.Rule
	triggers map[string]domain.Trigger
	loops    map[string]*triggerLoop
	stopped  bool
}

// triggerLoop runs one random trigger.
type triggerLoop struct {
	def    domain.Trigger
	cancel context.CancelFunc
	done   chan struct{}
}

func newVCA(rt *Runtime) *vcaRuntime {
	return &vcaRuntime{rt: rt, second: time.Second, stats: newAnalytics(), triggers: map[string]domain.Trigger{}, loops: map[string]*triggerLoop{}}
}

// setCaps sets what the camera's profile lets its analytics do.
func (v *vcaRuntime) setCaps(caps domain.VCACaps) {
	v.mu.Lock()
	defer v.mu.Unlock()
	v.caps = caps
}

// currentRules returns the camera's rules.
func (v *vcaRuntime) currentRules() []domain.Rule {
	v.mu.Lock()
	defer v.mu.Unlock()
	return slices.Clone(v.rules)
}

// analytics returns what the camera counted, with the heat map at cols by
// rows cells (none for 0), after starting again from zero if asked.
func (v *vcaRuntime) analytics(q ipc.AnalyticsQuery) ipc.Analytics {
	if q.Reset {
		v.stats.reset()
	}
	return v.stats.snapshot(v.currentRules(), q.Cols, q.Rows)
}

// set replaces the rules and triggers. A random trigger that did not change
// keeps its loop, so editing a rule does not reset when its next event
// comes; rules are read at each event, so their changes apply at once.
func (v *vcaRuntime) set(cfg ipc.VCA) {
	v.mu.Lock()
	defer v.mu.Unlock()
	if v.stopped {
		return
	}
	v.rules = cfg.Rules
	v.triggers = make(map[string]domain.Trigger, len(cfg.Triggers))
	for _, t := range cfg.Triggers {
		v.triggers[t.ID] = t
	}
	for id, l := range v.loops {
		if t, ok := v.triggers[id]; !ok || !runs(t) || !reflect.DeepEqual(t, l.def) {
			// It may be firing: it sees the cancel once it takes the lock.
			l.cancel()
			delete(v.loops, id)
		}
	}
	for id, t := range v.triggers {
		if runs(t) && v.loops[id] == nil {
			ctx, cancel := context.WithCancel(context.Background())
			l := &triggerLoop{def: t, cancel: cancel, done: make(chan struct{})}
			v.loops[id] = l
			go v.loop(ctx, l)
		}
	}
}

func runs(t domain.Trigger) bool {
	return t.Enabled && t.Type == domain.TriggerRandom
}

// stop ends every loop and waits for them: nothing fires once the camera
// stops.
func (v *vcaRuntime) stop() {
	v.mu.Lock()
	v.stopped = true
	loops := v.loops
	v.loops = map[string]*triggerLoop{}
	v.mu.Unlock()
	for _, l := range loops {
		l.cancel()
		<-l.done
	}
}

// loop emits an event of a random trigger at a random moment between its
// minimum and maximum wait, again and again. An event the camera refuses,
// for instance within the profile's minimum interval, is skipped.
func (v *vcaRuntime) loop(ctx context.Context, l *triggerLoop) {
	defer close(l.done)
	timer := time.NewTimer(v.wait(l.def))
	defer timer.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-timer.C:
		}
		if _, err := v.fire(ctx, l.def.ID, domain.TriggerRandom); err != nil && ctx.Err() == nil {
			v.rt.log.Debug("random trigger skipped an event", "trigger", l.def.Name, "error", err)
		}
		timer.Reset(v.wait(l.def))
	}
}

// wait draws the time to the next event of a random trigger.
func (v *vcaRuntime) wait(t domain.Trigger) time.Duration {
	lo, hi := time.Duration(t.MinSeconds)*v.second, time.Duration(t.MaxSeconds)*v.second
	if hi <= lo {
		return lo
	}
	return lo + rand.N(hi-lo+1)
}

// fire emits an event of a stored trigger: at its moment (random) or when
// someone asks for one (manual), even if the trigger is disabled.
func (v *vcaRuntime) fire(ctx context.Context, id string, kind domain.TriggerType) (engine.Event, error) {
	v.mu.Lock()
	if err := ctx.Err(); err != nil {
		v.mu.Unlock()
		return engine.Event{}, err
	}
	t, ok := v.triggers[id]
	if !ok {
		v.mu.Unlock()
		return engine.Event{}, errors.New("the camera has no such trigger yet")
	}
	rule, err := v.ruleFor(t)
	v.mu.Unlock()
	if err != nil {
		return engine.Event{}, err
	}
	return v.emit(ctx, fireSpec{typ: t.EventType, kind: kind, triggerID: t.ID, rule: rule,
		plates: t.Plates, masks: t.PlateMasks, speeds: t.Speed})
}

// ruleFor picks the rule an event of a stored trigger happens on: its own,
// which must be enabled, or any enabled rule that reports the type. An
// event that comes from rules needs one: a real camera raises it from a
// line or region someone configured. The lock must be held.
func (v *vcaRuntime) ruleFor(t domain.Trigger) (*domain.Rule, error) {
	if v.caps.RuleTypeFor(t.EventType) == "" {
		return nil, nil
	}
	if t.RuleID != "" {
		for _, r := range v.rules {
			if r.ID != t.RuleID {
				continue
			}
			if !r.Enabled {
				return nil, fmt.Errorf("rule %s is disabled", r.Name)
			}
			if !r.Reports(t.EventType) {
				return nil, fmt.Errorf("rule %s does not report %s events", r.Name, t.EventType)
			}
			return &r, nil
		}
		return nil, errors.New("the rule of the trigger is gone")
	}
	var candidates []domain.Rule
	for _, r := range v.rules {
		if r.Enabled && r.Reports(t.EventType) {
			candidates = append(candidates, r)
		}
	}
	if len(candidates) == 0 {
		return nil, fmt.Errorf("no enabled rule reports %s events", t.EventType)
	}
	r := candidates[rand.IntN(len(candidates))]
	return &r, nil
}

// manual emits the event the panel or the API asked for, on the rule the
// service picked; the events that come from rules need one.
func (v *vcaRuntime) manual(ctx context.Context, tr *ipc.Trigger) (engine.Event, error) {
	v.mu.Lock()
	kind := v.caps.RuleTypeFor(tr.Type)
	v.mu.Unlock()
	if kind != "" && (tr.Rule == nil || tr.Rule.Type != kind) {
		return engine.Event{}, fmt.Errorf("%s events come from a %s rule", tr.Type, kind)
	}
	return v.emit(ctx, fireSpec{typ: tr.Type, kind: domain.TriggerManual, rule: tr.Rule, direction: tr.Direction,
		object: tr.Object, plate: tr.Plate, speed: tr.Speed, custom: tr.Custom})
}

// fireSpec is an event to emit: its type, what produced it, the rule it
// happens on, what the caller already knows of it and, for a stored
// trigger, the plates and speeds to generate the rest from.
type fireSpec struct {
	typ       string
	kind      domain.TriggerType
	triggerID string
	rule      *domain.Rule
	direction string
	object    *engine.Object
	plate     *engine.Plate
	speed     *engine.Speed
	custom    map[string]any
	plates    []string
	masks     []string
	speeds    *domain.SpeedRange
}

// emit builds the canonical event and hands it to the event bus. A profile
// parameter bound to events.<type>.enabled turns the type off, as the
// analytics switch of a real camera does.
func (v *vcaRuntime) emit(ctx context.Context, s fireSpec) (engine.Event, error) {
	if on, ok := v.rt.state.Canon("events." + s.typ + ".enabled"); ok {
		if enabled, isBool := on.(bool); isBool && !enabled {
			return engine.Event{}, fmt.Errorf("%s detection is turned off on the camera", s.typ)
		}
	}
	v.mu.Lock()
	report := v.caps.Reports[s.typ]
	v.mu.Unlock()
	e := engine.Event{Type: s.typ, Trigger: string(s.kind), Direction: s.direction, Custom: s.custom}
	if report {
		// A report carries what the camera counted, which its template reads.
		return v.rt.events.emit(ctx, e, s.triggerID)
	}
	if r := s.rule; r != nil {
		e.Rule = &engine.Rule{ID: r.ID, Name: r.Name, Type: string(r.Type)}
		if r.Type == domain.RuleLine && e.Direction == "" {
			e.Direction = crossing(r, s.kind == domain.TriggerRandom)
		}
	}
	if s.object != nil || s.rule != nil || s.typ == string(domain.EventLPR) || s.typ == string(domain.EventSpeed) {
		e.Object = v.object(s.object, s.typ, s.rule, e.Direction)
	}
	if s.plate != nil || len(s.plates)+len(s.masks) > 0 || s.typ == string(domain.EventLPR) {
		e.Plate = plate(s.plate, s.plates, s.masks)
	}
	if s.speed != nil || s.speeds != nil || s.typ == string(domain.EventSpeed) {
		e.Speed = speed(s.speed, s.speeds)
	}
	return v.rt.events.emit(ctx, e, s.triggerID)
}

// observe counts an event the camera is about to send; reports carry
// counts and add none.
func (v *vcaRuntime) observe(e engine.Event) {
	v.mu.Lock()
	report := v.caps.Reports[e.Type]
	v.mu.Unlock()
	if !report {
		v.stats.record(e)
	}
}

// crossing is the direction of a line crossing: the rule's, when it reports
// one way only; otherwise random for random triggers and A->B for the rest.
func crossing(r *domain.Rule, random bool) string {
	switch {
	case r.Direction == domain.CrossAB || r.Direction == domain.CrossBA:
		return r.Direction
	case random && rand.IntN(2) == 1:
		return domain.CrossBA
	}
	return domain.CrossAB
}

// defaultClasses are what a camera detects when neither the rule nor the
// profile says.
var defaultClasses = []string{"person", "car"}

// colored classes get a color: vehicles, not people.
var colored = map[string]bool{"car": true, "truck": true, "bus": true, "van": true, "vehicle": true, "motorcycle": true, "bicycle": true}

var colors = []string{"white", "black", "gray", "silver", "red", "blue", "green", "yellow", "brown"}

// object completes the detected object: a class the rule detects (the
// profile's, without one), a color for vehicles, a confidence and a box
// where the rule is. Plates and speeds belong to vehicles.
func (v *vcaRuntime) object(given *engine.Object, typ string, r *domain.Rule, direction string) *engine.Object {
	o := engine.Object{}
	if given != nil {
		o = *given
	}
	if o.Class == "" {
		classes := defaultClasses
		if r != nil && len(r.ObjectClasses) > 0 {
			classes = r.ObjectClasses
		} else if vca := v.rt.model.Doc.VCA; vca != nil && len(vca.ObjectClasses) > 0 {
			classes = vca.ObjectClasses
		}
		if typ == string(domain.EventLPR) || typ == string(domain.EventSpeed) {
			var vehicles []string
			for _, c := range classes {
				if colored[c] {
					vehicles = append(vehicles, c)
				}
			}
			classes = cmpOr(vehicles, []string{"car"})
		}
		o.Class = classes[rand.IntN(len(classes))]
	}
	if o.Color == "" && colored[o.Class] {
		o.Color = colors[rand.IntN(len(colors))]
	}
	if o.Confidence == 0 {
		o.Confidence = round(0.8+rand.Float64()*0.19, 2)
	}
	if o.Box == nil {
		b := boxFor(o.Class, r, direction)
		o.Box = &b
	}
	return &o
}

func cmpOr(a, b []string) []string {
	if len(a) > 0 {
		return a
	}
	return b
}

// boxFor places an object's box, in picture fractions, where its rule is:
// on a line, just past it on the side it crossed to; inside a region; in
// the lower middle of the picture without a rule.
func boxFor(class string, r *domain.Rule, direction string) engine.Box {
	w, h := sizeOf(class)
	c := domain.Point{X: 0.3 + rand.Float64()*0.4, Y: 0.45 + rand.Float64()*0.3}
	switch {
	case r != nil && r.Type == domain.RuleLine && len(r.Points) == 2:
		a, b := r.Points[0], r.Points[1]
		t := 0.2 + rand.Float64()*0.6
		c = domain.Point{X: a.X + (b.X-a.X)*t, Y: a.Y + (b.Y-a.Y)*t}
		// The normal pointing to side B; side A is the other way.
		dx, dy := b.X-a.X, b.Y-a.Y
		n := math.Hypot(dx, dy)
		nx, ny := -dy/n, dx/n
		if direction == domain.CrossBA {
			nx, ny = -nx, -ny
		}
		c.X += nx * w / 2
		c.Y += ny * h / 2
	case r != nil && r.Type == domain.RuleRegion && len(r.Points) >= 3:
		c = pointIn(r.Points)
	}
	x := min(max(c.X-w/2, 0), 1-w)
	y := min(max(c.Y-h/2, 0), 1-h)
	return engine.Box{X: round(x, 3), Y: round(y, 3), W: round(w, 3), H: round(h, 3)}
}

// sizeOf draws the size of an object's box, as a fraction of the picture.
func sizeOf(class string) (w, h float64) {
	between := func(lo, hi float64) float64 { return lo + rand.Float64()*(hi-lo) }
	switch class {
	case "person":
		return between(0.04, 0.08), between(0.12, 0.25)
	case "car", "van", "vehicle":
		return between(0.12, 0.22), between(0.08, 0.15)
	case "truck", "bus":
		return between(0.2, 0.35), between(0.15, 0.25)
	case "motorcycle", "bicycle":
		return between(0.05, 0.09), between(0.08, 0.14)
	}
	return between(0.08, 0.15), between(0.08, 0.15)
}

// pointIn draws a point inside a polygon: a random point of its bounding
// box that falls inside, or the average of its corners.
func pointIn(poly []domain.Point) domain.Point {
	lo, hi := poly[0], poly[0]
	var sum domain.Point
	for _, p := range poly {
		lo.X, lo.Y = min(lo.X, p.X), min(lo.Y, p.Y)
		hi.X, hi.Y = max(hi.X, p.X), max(hi.Y, p.Y)
		sum.X += p.X
		sum.Y += p.Y
	}
	for range 32 {
		p := domain.Point{X: lo.X + rand.Float64()*(hi.X-lo.X), Y: lo.Y + rand.Float64()*(hi.Y-lo.Y)}
		if domain.Contains(poly, p) {
			return p
		}
	}
	return domain.Point{X: sum.X / float64(len(poly)), Y: sum.Y / float64(len(poly))}
}

// defaultPlateMask generates plates when a trigger names none (D41).
const defaultPlateMask = "AA999AA"

// plate completes a license plate: one of the trigger's plates, or one its
// masks generate, and a confidence.
func plate(given *engine.Plate, plates, masks []string) *engine.Plate {
	p := engine.Plate{}
	if given != nil {
		p = *given
	}
	if p.Text == "" {
		if len(masks) == 0 && len(plates) == 0 {
			masks = []string{defaultPlateMask}
		}
		if i := rand.IntN(len(plates) + len(masks)); i < len(plates) {
			p.Text = plates[i]
		} else {
			mask := masks[i-len(plates)]
			p.Text, p.Format = domain.ExpandMask(mask, rand.IntN), mask
		}
	}
	if p.Confidence == 0 {
		p.Confidence = round(0.85+rand.Float64()*0.14, 2)
	}
	return &p
}

// speed completes a speed: a value in the trigger's range (20 to 120 km/h
// without one).
func speed(given *engine.Speed, r *domain.SpeedRange) *engine.Speed {
	if given != nil {
		s := *given
		if s.Unit == "" {
			s.Unit = "km/h"
		}
		return &s
	}
	rng := domain.SpeedRange{Min: 20, Max: 120, Unit: "km/h"}
	if r != nil {
		rng = *r
	}
	return &engine.Speed{Value: round(rng.Min+rand.Float64()*(rng.Max-rng.Min), 1), Unit: rng.Unit, Limit: rng.Limit}
}

func round(f float64, digits int) float64 {
	p := math.Pow(10, float64(digits))
	return math.Round(f*p) / p
}
