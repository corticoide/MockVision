package app

import (
	"context"
	"encoding/json"
	"time"

	"github.com/oklog/ulid/v2"

	"github.com/corticoide/mockvision/backend/internal/domain"
	"github.com/corticoide/mockvision/backend/internal/store"
	"github.com/corticoide/mockvision/backend/internal/store/db"
)

// AuditRetention is how long audit entries are kept.
const AuditRetention = 90 * 24 * time.Hour

// AuditActor is who made a change.
type AuditActor struct {
	Type string `json:"type"`
	ID   string `json:"id"`
	Name string `json:"name"`
}

// AuditEntity is what a change touched.
type AuditEntity struct {
	Type string `json:"type"`
	ID   string `json:"id"`
	Name string `json:"name"`
}

// AuditToken is the API token a change was made with.
type AuditToken struct {
	ID   string `json:"id"`
	Name string `json:"name"`
}

// AuditView is an audit entry: who changed what, when and from where.
type AuditView struct {
	ID       string          `json:"id"`
	At       time.Time       `json:"at"`
	Actor    AuditActor      `json:"actor"`
	Origin   string          `json:"origin"`
	OriginIP string          `json:"origin_ip"`
	Token    *AuditToken     `json:"token"`
	Action   string          `json:"action"`
	Entity   AuditEntity     `json:"entity"`
	Diff     json.RawMessage `json:"diff"`
}

// AuditFilter selects audit entries; empty fields match everything. Action
// matches exactly or as a prefix: "camera" selects camera.start and the rest.
type AuditFilter struct {
	Origin     string
	EntityType string
	EntityID   string
	TokenID    string
	Action     string
	Since      time.Time
	Until      time.Time
	Cursor     string
	Limit      int
}

// ListAudit returns audit entries, newest first.
func (s *Service) ListAudit(ctx context.Context, f AuditFilter) (Page[AuditView], error) {
	switch f.Origin {
	case "", OriginPanel, OriginAPI, OriginCamera, OriginSystem:
	default:
		return Page[AuditView]{}, domain.Invalid("origin", "must be panel, api, camera or system")
	}
	if f.Limit <= 0 || f.Limit > 200 {
		f.Limit = 50
	}
	rows, err := s.store.R().ListAudit(ctx, db.ListAuditParams{
		Cursor: f.Cursor, Origin: f.Origin, EntityType: f.EntityType, EntityID: f.EntityID, TokenID: f.TokenID, Action: f.Action,
		Since: store.Millis(f.Since), Until: store.Millis(f.Until), Limit: int64(f.Limit + 1),
	})
	if err != nil {
		return Page[AuditView]{}, err
	}
	page := Page[AuditView]{Items: make([]AuditView, 0, len(rows))}
	if len(rows) > f.Limit {
		rows = rows[:f.Limit]
		page.NextCursor = rows[len(rows)-1].ID
	}
	for _, r := range rows {
		page.Items = append(page.Items, auditView(db.GetAuditRow(r)))
	}
	return page, nil
}

// audit records who changed what and from where, and shows it live.
func (s *Service) audit(ctx context.Context, a Actor, action, entityType, entityID string, diff any) {
	raw := []byte("{}")
	if diff != nil {
		if b, err := json.Marshal(diff); err == nil {
			raw = b
		}
	}
	row := db.InsertAuditParams{
		ID: ulid.Make().String(), At: time.Now().UnixMilli(), ActorType: a.Type, ActorID: a.ID, ActorName: a.Name,
		Origin: a.origin(), OriginIp: a.IP, Action: action, EntityType: entityType, EntityID: entityID, DiffJson: string(raw),
	}
	if a.Token != nil {
		row.TokenID, row.TokenName = a.Token.ID, a.Token.Name
	}
	if err := s.store.W().InsertAudit(ctx, row); err != nil {
		s.log.Warn("audit write failed", "action", action, "error", err)
		return
	}
	if r, err := s.store.R().GetAudit(ctx, row.ID); err == nil {
		s.pub.Publish("audit", "entry", auditView(r))
	}
}

func auditView(r db.GetAuditRow) AuditView {
	v := AuditView{
		ID: r.ID, At: store.Time(r.At), Actor: AuditActor{Type: r.ActorType, ID: r.ActorID, Name: r.ActorName},
		Origin: r.Origin, OriginIP: r.OriginIp, Action: r.Action,
		Entity: AuditEntity{Type: r.EntityType, ID: r.EntityID, Name: r.EntityName}, Diff: json.RawMessage(r.DiffJson),
	}
	if !json.Valid(v.Diff) {
		v.Diff = json.RawMessage("{}")
	}
	if r.TokenID != "" {
		v.Token = &AuditToken{ID: r.TokenID, Name: r.TokenName}
	}
	// A deleted entity keeps the name its entry recorded.
	if v.Entity.Name == "" {
		var named struct {
			Name string `json:"name"`
		}
		if json.Unmarshal(v.Diff, &named) == nil {
			v.Entity.Name = named.Name
		}
	}
	return v
}
