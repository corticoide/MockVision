package app

import (
	"context"
	"encoding/json"
	"errors"

	"github.com/oklog/ulid/v2"

	"github.com/corticoide/mockvision/backend/internal/domain"
	"github.com/corticoide/mockvision/backend/internal/ipc"
	"github.com/corticoide/mockvision/backend/internal/store"
	"github.com/corticoide/mockvision/backend/internal/store/db"
	"github.com/corticoide/mockvision/sdk/engine"
)

// TriggerInput is a manual trigger from the panel or the API.
type TriggerInput struct {
	Type      string         `json:"type"`
	Direction string         `json:"direction,omitempty"`
	Object    *engine.Object `json:"object,omitempty"`
	Plate     *engine.Plate  `json:"plate,omitempty"`
	Speed     *engine.Speed  `json:"speed,omitempty"`
	Custom    map[string]any `json:"custom,omitempty"`
}

// TriggerEvent makes a running camera emit an event; the camera serializes
// it with its profile and delivers it to its targets.
func (s *Service) TriggerEvent(ctx context.Context, actor Actor, cameraID string, in TriggerInput) (*EventView, error) {
	b, err := s.loadBundle(ctx, cameraID)
	if err != nil {
		return nil, err
	}
	if !domain.ValidEventType(in.Type) {
		return nil, domain.Invalid("type", "%q is not a canonical event type", in.Type)
	}
	spec, ok := b.doc.Events[in.Type]
	if !ok {
		return nil, domain.Invalid("type", "profile %s does not define %s events", b.prof.ProfileID, in.Type)
	}
	if len(spec.Transports) == 0 {
		return nil, domain.Invalid("type", "%s has no transport in the profile and cannot be enabled", in.Type)
	}
	if in.Direction != "" && !domain.ValidDirection(domain.Direction(in.Direction)) {
		return nil, domain.Invalid("direction", "must be A->B, B->A or none")
	}
	ss := s.session(cameraID)
	if ss == nil || !ss.active() {
		return nil, domain.Conflict("", "camera %s is not running", b.cam.Name)
	}
	var res ipc.TriggerResult
	err = ss.conn().Request(ctx, ipc.TypeTrigger, ipc.Trigger{
		Type: in.Type, Direction: in.Direction, Object: in.Object, Plate: in.Plate, Speed: in.Speed, Custom: in.Custom,
	}, &res)
	if err != nil {
		var ie *ipc.Error
		if errors.As(err, &ie) && ie.Code == "trigger" {
			return nil, domain.Conflict("", "%s", ie.Message)
		}
		return nil, err
	}
	s.audit(ctx, actor, "event.trigger", "camera", cameraID, map[string]any{"type": in.Type, "event_id": res.Event.ID})
	data, _ := json.Marshal(res.Event)
	return &EventView{
		ID: res.Event.ID, CameraID: cameraID, CameraName: b.cam.Name, Type: res.Event.Type, At: res.Event.At,
		Data: data, Deliveries: []DeliveryView{}, DeliveryStatus: deliveryStatus(nil, len(b.targets)),
	}, nil
}

// recordEvent stores an event reported by a camera (RN-13).
func (s *Service) recordEvent(ctx context.Context, cameraID string, e engine.Event) {
	data, _ := json.Marshal(e)
	var ruleID string
	if e.Rule != nil {
		ruleID = e.Rule.ID
	}
	err := s.store.W().InsertEvent(ctx, db.InsertEventParams{
		ID: e.ID, CameraID: cameraID, Type: e.Type, At: e.At.UnixMilli(), DataJson: string(data),
		RuleID: store.NullString(ruleID), TriggerID: store.NullString(e.Trigger),
	})
	if err != nil {
		s.log.Warn("cannot store event", "camera", cameraID, "error", err)
		return
	}
	if v, err := s.GetEvent(ctx, e.ID); err == nil {
		s.pub.Publish("events", "event", v)
	}
}

// recordDelivery stores the outcome of a delivery attempt.
func (s *Service) recordDelivery(ctx context.Context, cameraID string, d engine.DeliveryReport) {
	status := d.Status
	switch status {
	case engine.DeliveryOK, engine.DeliveryRetry, engine.DeliveryFailed:
	default:
		status = engine.DeliveryFailed
	}
	row := db.InsertDeliveryParams{
		ID: ulid.Make().String(), EventID: d.EventID, TargetID: d.TargetID, Attempt: int64(d.Attempt), At: d.At.UnixMilli(),
		Status: status, Error: truncate(d.Error, 500),
	}
	if d.HTTPStatus > 0 {
		row.HttpStatus.Int64, row.HttpStatus.Valid = int64(d.HTTPStatus), true
	}
	row.LatencyMs.Int64, row.LatencyMs.Valid = d.LatencyMS, true
	if err := s.store.W().InsertDelivery(ctx, row); err != nil {
		s.log.Warn("cannot store delivery", "camera", cameraID, "event", d.EventID, "error", err)
		return
	}
	if v, err := s.GetEvent(ctx, d.EventID); err == nil {
		s.pub.Publish("events", "event", v)
	}
}

func truncate(s string, n int) string {
	if len(s) <= n {
		return s
	}
	return s[:n]
}

// EventFilter selects events.
type EventFilter struct {
	CameraID string
	Type     string
	Cursor   string
	Limit    int
}

// ListEvents returns events, newest first, with their deliveries.
func (s *Service) ListEvents(ctx context.Context, f EventFilter) (Page[EventView], error) {
	if f.Limit <= 0 || f.Limit > 200 {
		f.Limit = 50
	}
	rows, err := s.store.R().ListEvents(ctx, db.ListEventsParams{Cursor: f.Cursor, CameraID: f.CameraID, Type: f.Type, Limit: int64(f.Limit + 1)})
	if err != nil {
		return Page[EventView]{}, err
	}
	page := Page[EventView]{Items: []EventView{}}
	if len(rows) > f.Limit {
		rows = rows[:f.Limit]
		page.NextCursor = rows[len(rows)-1].ID
	}
	ids := make([]string, len(rows))
	for i, r := range rows {
		ids[i] = r.ID
	}
	byEvent, err := s.deliveries(ctx, ids)
	if err != nil {
		return page, err
	}
	for _, r := range rows {
		page.Items = append(page.Items, eventView(r.ID, r.CameraID, r.CameraName, r.Type, r.At, r.DataJson, byEvent[r.ID]))
	}
	return page, nil
}

// GetEvent returns an event with its deliveries.
func (s *Service) GetEvent(ctx context.Context, id string) (*EventView, error) {
	r, err := s.store.R().GetEvent(ctx, id)
	if err != nil {
		return nil, store.NotFound(err)
	}
	byEvent, err := s.deliveries(ctx, []string{id})
	if err != nil {
		return nil, err
	}
	v := eventView(r.ID, r.CameraID, r.CameraName, r.Type, r.At, r.DataJson, byEvent[id])
	return &v, nil
}

func (s *Service) deliveries(ctx context.Context, ids []string) (map[string][]DeliveryView, error) {
	out := map[string][]DeliveryView{}
	if len(ids) == 0 {
		return out, nil
	}
	rows, err := s.store.R().ListDeliveriesForEvents(ctx, ids)
	if err != nil {
		return nil, err
	}
	for _, d := range rows {
		out[d.EventID] = append(out[d.EventID], DeliveryView{
			ID: d.ID, EventID: d.EventID, TargetID: d.TargetID, TargetName: d.TargetName, Attempt: int(d.Attempt),
			At: store.Time(d.At), Status: d.Status, HTTPStatus: int(d.HttpStatus.Int64), LatencyMS: d.LatencyMs.Int64, Error: d.Error,
		})
	}
	return out, nil
}

func eventView(id, cameraID, cameraName, typ string, at int64, data string, dels []DeliveryView) EventView {
	if dels == nil {
		dels = []DeliveryView{}
	}
	v := EventView{ID: id, CameraID: cameraID, CameraName: cameraName, Type: typ, At: store.Time(at), Data: json.RawMessage(data), Deliveries: dels}
	v.DeliveryStatus = deliveryStatus(dels, -1)
	// Latency of the last successful attempt, or of the last attempt.
	for i := len(dels) - 1; i >= 0; i-- {
		if dels[i].Status == engine.DeliveryOK {
			l := dels[i].LatencyMS
			v.LatencyMS = &l
			break
		}
	}
	if v.LatencyMS == nil && len(dels) > 0 {
		l := dels[len(dels)-1].LatencyMS
		v.LatencyMS = &l
	}
	return v
}

// deliveryStatus summarizes the latest attempt of every target: ok, failed,
// pending (retrying or not reported yet) or none (no targets).
func deliveryStatus(dels []DeliveryView, targets int) string {
	if len(dels) == 0 {
		if targets == 0 {
			return "none"
		}
		return "pending"
	}
	latest := map[string]DeliveryView{}
	for _, d := range dels {
		if cur, ok := latest[d.TargetID]; !ok || d.Attempt >= cur.Attempt {
			latest[d.TargetID] = d
		}
	}
	status := "ok"
	for _, d := range latest {
		switch d.Status {
		case engine.DeliveryFailed:
			return "failed"
		case engine.DeliveryRetry:
			status = "pending"
		}
	}
	return status
}
