package app

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"slices"
	"strconv"
	"strings"
	"time"

	"github.com/oklog/ulid/v2"

	"github.com/corticoide/mockvision/backend/internal/domain"
	"github.com/corticoide/mockvision/backend/internal/ipc"
	"github.com/corticoide/mockvision/backend/internal/profile"
	"github.com/corticoide/mockvision/backend/internal/store"
	"github.com/corticoide/mockvision/backend/internal/store/db"
)

// Rules and triggers (D39, D40): the analytics rules drawn on a camera's
// picture say where events happen, and its stored triggers make them happen.
// Both apply at once to a running camera (RN-09).

// ruleParams is what a rule stores besides its points.
type ruleParams struct {
	Direction     string   `json:"direction,omitempty"`
	Events        []string `json:"events,omitempty"`
	ObjectClasses []string `json:"object_classes,omitempty"`
}

// triggerParams is what a trigger stores.
type triggerParams struct {
	EventType  string             `json:"event_type"`
	RuleID     string             `json:"rule_id,omitempty"`
	MinSeconds int                `json:"min_seconds"`
	MaxSeconds int                `json:"max_seconds"`
	Plates     []string           `json:"plates,omitempty"`
	PlateMasks []string           `json:"plate_masks,omitempty"`
	Speed      *domain.SpeedRange `json:"speed,omitempty"`
}

func ruleOf(row db.Rule) domain.Rule {
	r := domain.Rule{ID: row.ID, Name: row.Name, Type: domain.RuleType(row.Type), Enabled: store.Bool(row.Enabled)}
	_ = json.Unmarshal([]byte(row.GeometryJson), &r.Points)
	if r.Points == nil {
		r.Points = []domain.Point{}
	}
	var p ruleParams
	_ = json.Unmarshal([]byte(row.ParamsJson), &p)
	r.Direction, r.Events, r.ObjectClasses = p.Direction, nonNil(p.Events), nonNil(p.ObjectClasses)
	return r
}

func triggerOf(row db.Trigger) domain.Trigger {
	t := domain.Trigger{ID: row.ID, Name: row.Name, Type: domain.TriggerType(row.Type), Enabled: store.Bool(row.Enabled)}
	var p triggerParams
	_ = json.Unmarshal([]byte(row.ParamsJson), &p)
	t.EventType, t.RuleID, t.MinSeconds, t.MaxSeconds = p.EventType, p.RuleID, p.MinSeconds, p.MaxSeconds
	t.Plates, t.PlateMasks, t.Speed = nonNil(p.Plates), nonNil(p.PlateMasks), p.Speed
	return t
}

func rulesOf(rows []db.Rule) []domain.Rule {
	out := make([]domain.Rule, len(rows))
	for i, row := range rows {
		out[i] = ruleOf(row)
	}
	return out
}

func triggersOf(rows []db.Trigger) []domain.Trigger {
	out := make([]domain.Trigger, len(rows))
	for i, row := range rows {
		out[i] = triggerOf(row)
	}
	return out
}

// insertRules stores a camera's rules in order.
func insertRules(ctx context.Context, q *db.Queries, cameraID string, rules []domain.Rule) error {
	for i, r := range rules {
		points, _ := json.Marshal(r.Points)
		params, _ := json.Marshal(ruleParams{Direction: r.Direction, Events: r.Events, ObjectClasses: r.ObjectClasses})
		if err := q.InsertRule(ctx, db.InsertRuleParams{ID: r.ID, CameraID: cameraID, Position: int64(i), Name: r.Name, Type: string(r.Type),
			GeometryJson: string(points), ParamsJson: string(params), Enabled: store.Int(r.Enabled)}); err != nil {
			return err
		}
	}
	return nil
}

// insertTriggers stores a camera's triggers in order.
func insertTriggers(ctx context.Context, q *db.Queries, cameraID string, triggers []domain.Trigger) error {
	for i, t := range triggers {
		params, _ := json.Marshal(triggerParams{EventType: t.EventType, RuleID: t.RuleID, MinSeconds: t.MinSeconds, MaxSeconds: t.MaxSeconds,
			Plates: t.Plates, PlateMasks: t.PlateMasks, Speed: t.Speed})
		if err := q.InsertTrigger(ctx, db.InsertTriggerParams{ID: t.ID, CameraID: cameraID, Position: int64(i), Name: t.Name, Type: string(t.Type),
			ParamsJson: string(params), Enabled: store.Int(t.Enabled)}); err != nil {
			return err
		}
	}
	return nil
}

// vcaCaps is what a profile allows a camera's analytics: its kinds of
// rule, its object classes, and the events it can deliver.
func vcaCaps(doc *profile.Document) domain.VCACaps {
	caps := domain.VCACaps{Events: map[string]time.Duration{}}
	if doc.VCA != nil {
		for _, r := range doc.VCA.Rules {
			caps.RuleTypes = append(caps.RuleTypes, domain.RuleType(r))
		}
		caps.ObjectClasses = doc.VCA.ObjectClasses
	}
	for typ, spec := range doc.Events {
		if len(spec.Transports) > 0 {
			caps.Events[typ] = spec.MinInterval.D()
		}
	}
	return caps
}

// vcaConfig is what a camera runs its analytics with.
func vcaConfig(b *cameraBundle) ipc.VCA {
	return ipc.VCA{Rules: b.rules, Triggers: b.triggers}
}

// CameraRules returns the analytics rules of a camera, in order.
func (s *Service) CameraRules(ctx context.Context, id string) ([]domain.Rule, error) {
	b, err := s.loadBundle(ctx, id)
	if err != nil {
		return nil, err
	}
	return append([]domain.Rule{}, b.rules...), nil
}

// CameraTriggers returns the stored triggers of a camera, in order.
func (s *Service) CameraTriggers(ctx context.Context, id string) ([]domain.Trigger, error) {
	b, err := s.loadBundle(ctx, id)
	if err != nil {
		return nil, err
	}
	return append([]domain.Trigger{}, b.triggers...), nil
}

// RuleInput is a rule to save; without an ID it is a new one.
type RuleInput struct {
	ID     string         `json:"id"`
	Name   string         `json:"name"`
	Type   string         `json:"type"`
	Points []domain.Point `json:"points"`
	// Direction of a line: A->B, B->A or both (the default).
	Direction string `json:"direction"`
	// Events of a region: region_entrance, region_exit, loitering and
	// intrusion.
	Events        []string `json:"events"`
	ObjectClasses []string `json:"object_classes"`
	// Enabled defaults to true.
	Enabled *bool `json:"enabled"`
}

// SetCameraRules replaces the analytics rules of a camera (D39). A running
// camera applies them at once (RN-09). A trigger that fires on a rule keeps
// it: that rule cannot go away or stop reporting the trigger's events.
func (s *Service) SetCameraRules(ctx context.Context, actor Actor, id string, in []RuleInput) (*CameraView, error) {
	b, err := s.loadBundle(ctx, id)
	if err != nil {
		return nil, err
	}
	rules := make([]domain.Rule, len(in))
	v := &domain.ValidationError{}
	for i, r := range in {
		if r.ID == "" {
			r.ID = ulid.Make().String()
		} else if !slices.ContainsFunc(b.rules, func(x domain.Rule) bool { return x.ID == r.ID }) {
			v.Add("rules["+strconv.Itoa(i)+"].id", "is not a rule of the camera")
		}
		if r.Type == string(domain.RuleLine) && r.Direction == "" {
			r.Direction = domain.CrossBoth
		}
		rules[i] = domain.Rule{ID: r.ID, Name: strings.TrimSpace(r.Name), Type: domain.RuleType(r.Type), Points: r.Points, Direction: r.Direction,
			Events: nonNil(r.Events), ObjectClasses: nonNil(r.ObjectClasses), Enabled: r.Enabled == nil || *r.Enabled}
		if rules[i].Points == nil {
			rules[i].Points = []domain.Point{}
		}
	}
	if err := v.Err(); err != nil {
		return nil, err
	}
	if err := domain.ValidateRules(rules, vcaCaps(b.doc)); err != nil {
		return nil, err
	}
	var triggers []domain.Trigger
	err = s.store.Tx(ctx, func(q *db.Queries) error {
		// Read in the transaction: a trigger saved meanwhile is checked too.
		rows, err := q.ListCameraTriggers(ctx, id)
		if err != nil {
			return err
		}
		triggers = triggersOf(rows)
		if err := triggersKeepRules(triggers, b.rules, rules); err != nil {
			return err
		}
		if err := q.DeleteCameraRules(ctx, id); err != nil {
			return err
		}
		if err := insertRules(ctx, q, id, rules); err != nil {
			return err
		}
		return s.touchCamera(ctx, q, id)
	})
	if err != nil {
		return nil, err
	}
	summary := make([]map[string]any, len(rules))
	for i, r := range rules {
		summary[i] = map[string]any{"id": r.ID, "name": r.Name, "type": r.Type, "enabled": r.Enabled}
	}
	s.audit(ctx, actor, "camera.rules", "camera", id, map[string]any{"rules": summary})
	s.tellCamera(ctx, id, ipc.TypeReload, ipc.Reload{VCA: &ipc.VCA{Rules: rules, Triggers: triggers}})
	return s.publishCamera(ctx, id)
}

// triggersKeepRules checks that every trigger bound to a rule still finds
// it among the new rules, reporting the trigger's events.
func triggersKeepRules(triggers []domain.Trigger, old, rules []domain.Rule) error {
	for _, t := range triggers {
		if t.RuleID == "" {
			continue
		}
		i := slices.IndexFunc(rules, func(r domain.Rule) bool { return r.ID == t.RuleID })
		if i < 0 {
			name := t.RuleID
			if j := slices.IndexFunc(old, func(r domain.Rule) bool { return r.ID == t.RuleID }); j >= 0 {
				name = old[j].Name
			}
			return domain.Conflict("rules", "trigger %s fires on rule %s; change the trigger first", t.Name, name)
		}
		if !rules[i].Reports(t.EventType) {
			return domain.Conflict("rules", "trigger %s fires %s events on rule %s; change the trigger first", t.Name, t.EventType, rules[i].Name)
		}
	}
	return nil
}

// TriggerInput is a stored trigger to save; without an ID it is a new one.
type TriggerInput struct {
	ID   string `json:"id"`
	Name string `json:"name"`
	// Type is random, the only kind stored in v1 (the default).
	Type      string `json:"type"`
	EventType string `json:"event_type"`
	// RuleID is the rule the events happen on; empty picks any enabled
	// rule that reports the type.
	RuleID     string             `json:"rule_id"`
	MinSeconds int                `json:"min_seconds"`
	MaxSeconds int                `json:"max_seconds"`
	Plates     []string           `json:"plates"`
	PlateMasks []string           `json:"plate_masks"`
	Speed      *domain.SpeedRange `json:"speed"`
	// Enabled defaults to true.
	Enabled *bool `json:"enabled"`
}

// SetCameraTriggers replaces the stored triggers of a camera (D40). A
// running camera applies them at once (RN-09): enabled random triggers
// start raising events, disabled ones stop.
func (s *Service) SetCameraTriggers(ctx context.Context, actor Actor, id string, in []TriggerInput) (*CameraView, error) {
	b, err := s.loadBundle(ctx, id)
	if err != nil {
		return nil, err
	}
	triggers := make([]domain.Trigger, len(in))
	v := &domain.ValidationError{}
	for i, t := range in {
		if t.ID == "" {
			t.ID = ulid.Make().String()
		} else if !slices.ContainsFunc(b.triggers, func(x domain.Trigger) bool { return x.ID == t.ID }) {
			v.Add("triggers["+strconv.Itoa(i)+"].id", "is not a trigger of the camera")
		}
		if t.Type == "" {
			t.Type = string(domain.TriggerRandom)
		}
		triggers[i] = domain.Trigger{ID: t.ID, Name: strings.TrimSpace(t.Name), Type: domain.TriggerType(t.Type), EventType: t.EventType,
			RuleID: t.RuleID, MinSeconds: t.MinSeconds, MaxSeconds: t.MaxSeconds, Plates: nonNil(t.Plates), PlateMasks: nonNil(t.PlateMasks),
			Speed: t.Speed, Enabled: t.Enabled == nil || *t.Enabled}
	}
	if err := v.Err(); err != nil {
		return nil, err
	}
	var rules []domain.Rule
	err = s.store.Tx(ctx, func(q *db.Queries) error {
		// Read in the transaction: a rule changed meanwhile is checked too.
		rows, err := q.ListCameraRules(ctx, id)
		if err != nil {
			return err
		}
		rules = rulesOf(rows)
		if err := domain.ValidateTriggers(triggers, rules, vcaCaps(b.doc)); err != nil {
			return err
		}
		if err := q.DeleteCameraTriggers(ctx, id); err != nil {
			return err
		}
		if err := insertTriggers(ctx, q, id, triggers); err != nil {
			return err
		}
		return s.touchCamera(ctx, q, id)
	})
	if err != nil {
		return nil, err
	}
	summary := make([]map[string]any, len(triggers))
	for i, t := range triggers {
		summary[i] = map[string]any{"id": t.ID, "name": t.Name, "type": t.Type, "event_type": t.EventType, "enabled": t.Enabled}
	}
	s.audit(ctx, actor, "camera.triggers", "camera", id, map[string]any{"triggers": summary})
	s.tellCamera(ctx, id, ipc.TypeReload, ipc.Reload{VCA: &ipc.VCA{Rules: rules, Triggers: triggers}})
	return s.publishCamera(ctx, id)
}

// FireTrigger emits one event of a stored trigger now, enabled or not: a
// way to try it before letting it run.
func (s *Service) FireTrigger(ctx context.Context, actor Actor, cameraID, triggerID string) (*EventView, error) {
	b, err := s.loadBundle(ctx, cameraID)
	if err != nil {
		return nil, err
	}
	i := slices.IndexFunc(b.triggers, func(t domain.Trigger) bool { return t.ID == triggerID })
	if i < 0 {
		return nil, fmt.Errorf("camera has no trigger %q: %w", triggerID, domain.ErrNotFound)
	}
	t := b.triggers[i]
	res, err := s.askCamera(ctx, b, ipc.Trigger{TriggerID: t.ID})
	if err != nil {
		return nil, err
	}
	s.audit(ctx, actor, "trigger.fire", "camera", cameraID, map[string]any{"trigger_id": t.ID, "trigger": t.Name, "event_id": res.Event.ID})
	return s.firedView(b, res, t.ID), nil
}

// askCamera has a running camera emit an event.
func (s *Service) askCamera(ctx context.Context, b *cameraBundle, tr ipc.Trigger) (ipc.TriggerResult, error) {
	var res ipc.TriggerResult
	ss := s.session(b.cam.ID)
	if ss == nil || !ss.active() {
		return res, domain.Conflict("", "camera %s is not running", b.cam.Name)
	}
	if err := ss.conn().Request(ctx, ipc.TypeTrigger, tr, &res); err != nil {
		var ie *ipc.Error
		if errors.As(err, &ie) && ie.Code == "trigger" {
			return res, domain.Conflict("", "%s", ie.Message)
		}
		return res, err
	}
	return res, nil
}

// firedView is the event a camera just emitted, as the API returns it: its
// deliveries are still to come.
func (s *Service) firedView(b *cameraBundle, res ipc.TriggerResult, triggerID string) *EventView {
	data, _ := json.Marshal(res.Event)
	v := &EventView{
		ID: res.Event.ID, CameraID: b.cam.ID, CameraName: b.cam.Name, Type: res.Event.Type, At: res.Event.At,
		Data: data, TriggerID: triggerID, Deliveries: []DeliveryView{}, DeliveryStatus: deliveryStatus(nil, expectedDeliveries(b, res.Event.Type)),
	}
	if r := res.Event.Rule; r != nil && b.hasRule(r.ID) {
		v.RuleID = r.ID
	}
	return v
}

// ruleForEvent picks the rule of a manual event: the one asked for, which
// must be enabled and report the type, or else the first enabled rule
// that does. Without one, the camera stands in a default line or region.
func ruleForEvent(b *cameraBundle, typ, ruleID string) (*domain.Rule, error) {
	if ruleID == "" {
		for _, r := range b.rules {
			if r.Enabled && r.Reports(typ) {
				return &r, nil
			}
		}
		return nil, nil
	}
	if domain.RuleTypeFor(typ) == "" {
		return nil, domain.Invalid("rule_id", "%s events do not come from a rule", typ)
	}
	i := slices.IndexFunc(b.rules, func(r domain.Rule) bool { return r.ID == ruleID })
	if i < 0 {
		return nil, domain.Invalid("rule_id", "is not a rule of the camera")
	}
	r := b.rules[i]
	if !r.Reports(typ) {
		return nil, domain.Invalid("rule_id", "rule %s does not report %s events", r.Name, typ)
	}
	if !r.Enabled {
		return nil, domain.Conflict("rule_id", "rule %s is disabled", r.Name)
	}
	return &r, nil
}

// hasRule and hasTrigger tell whether an ID a camera reports is one of
// its own: the camera serves the LAN and is not trusted (audit B1).
func (b *cameraBundle) hasRule(id string) bool {
	return id != "" && slices.ContainsFunc(b.rules, func(r domain.Rule) bool { return r.ID == id })
}

func (b *cameraBundle) hasTrigger(id string) bool {
	return id != "" && slices.ContainsFunc(b.triggers, func(t domain.Trigger) bool { return t.ID == id })
}

// copyVCA gives a camera's rules and triggers new IDs, for a clone: its
// triggers keep firing on the copies of their rules.
func copyVCA(rules []domain.Rule, triggers []domain.Trigger) ([]domain.Rule, []domain.Trigger) {
	ids := map[string]string{}
	outRules := make([]domain.Rule, len(rules))
	for i, r := range rules {
		ids[r.ID] = ulid.Make().String()
		r.ID = ids[r.ID]
		outRules[i] = r
	}
	outTriggers := make([]domain.Trigger, len(triggers))
	for i, t := range triggers {
		t.ID = ulid.Make().String()
		t.RuleID = ids[t.RuleID]
		outTriggers[i] = t
	}
	return outRules, outTriggers
}
