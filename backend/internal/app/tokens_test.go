package app

import (
	"context"
	"errors"
	"io"
	"log/slog"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/corticoide/mockvision/backend/internal/domain"
	"github.com/corticoide/mockvision/backend/internal/netctl"
	"github.com/corticoide/mockvision/backend/internal/store"
	"github.com/corticoide/mockvision/backend/internal/store/db"
)

// recorder keeps what the service publishes.
type recorder struct {
	msgs []published
}

type published struct {
	topic, typ string
	data       any
}

func (r *recorder) Publish(topic, typ string, data any) {
	r.msgs = append(r.msgs, published{topic, typ, data})
}

// newBareService builds a service that is not running: enough for users,
// tokens and the audit log, without FFmpeg or cameras.
func newBareService(t *testing.T) (*Service, *recorder) {
	t.Helper()
	dir := t.TempDir()
	st, err := store.Open(context.Background(), filepath.Join(dir, "db.sqlite"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { st.Close() })
	log := slog.New(slog.NewTextHandler(io.Discard, nil))
	rec := &recorder{}
	svc, err := New(Options{DataDir: dir, Runtime: netctl.NewLocalRuntime("/nonexistent", log), Log: log}, st, rec)
	if err != nil {
		t.Fatal(err)
	}
	return svc, rec
}

// adminActor creates the first administrator and returns it as a panel
// actor.
func adminActor(t *testing.T, svc *Service) Actor {
	t.Helper()
	_, sess, err := svc.Setup(context.Background(), "admin", "correct horse battery", "10.0.0.5", "test")
	if err != nil {
		t.Fatal(err)
	}
	return Actor{Type: "user", ID: sess.User.ID, Name: sess.User.Username, IP: "10.0.0.5"}
}

func TestTokenLifecycle(t *testing.T) {
	svc, _ := newBareService(t)
	ctx := context.Background()
	admin := adminActor(t, svc)

	_, err := svc.CreateToken(ctx, admin, TokenInput{Name: " ", Scopes: []string{"admin"}})
	wantInvalid(t, err, "name")
	wantInvalid(t, err, "scopes")
	days := 0
	_, err = svc.CreateToken(ctx, admin, TokenInput{Name: "ci", ExpiresInDays: &days})
	wantInvalid(t, err, "expires_in_days")

	created, err := svc.CreateToken(ctx, admin, TokenInput{Name: "ci", Scopes: []string{"write"}})
	if err != nil {
		t.Fatal(err)
	}
	if !strings.HasPrefix(created.Secret, "mvt_") || len(created.Secret) != 47 {
		t.Fatalf("secret %q", created.Secret)
	}
	if !strings.HasPrefix(created.Secret, created.Token.Prefix) || len(created.Token.Prefix) != 12 {
		t.Fatalf("prefix %q of %q", created.Token.Prefix, created.Secret)
	}
	if got := strings.Join(created.Token.Scopes, ","); got != "read,write" {
		t.Fatalf("write implies read, got %s", got)
	}
	if created.Token.ExpiresAt != nil {
		t.Fatal("a token without expiry lasts until revoked")
	}
	var conflict *domain.ConflictError
	if _, err := svc.CreateToken(ctx, admin, TokenInput{Name: "ci"}); !errors.As(err, &conflict) {
		t.Fatalf("names are unique per user, got %v", err)
	}

	sess, err := svc.AuthenticateToken(ctx, created.Secret, "10.0.0.9")
	if err != nil {
		t.Fatal(err)
	}
	if sess.User.Username != "admin" || sess.Token == nil || sess.Token.Name != "ci" || !sess.Token.CanWrite() {
		t.Fatalf("session %+v token %+v", sess.User, sess.Token)
	}
	list, err := svc.ListTokens(ctx, admin.ID)
	if err != nil || len(list) != 1 {
		t.Fatalf("list %v, %v", list, err)
	}
	if list[0].LastUsedAt == nil || list[0].LastUsedIP != "10.0.0.9" {
		t.Fatalf("last use not recorded: %+v", list[0])
	}

	for _, bad := range []string{"", "mvt_", "mvt_nope", created.Secret + "x", strings.TrimPrefix(created.Secret, "mvt_")} {
		if _, err := svc.AuthenticateToken(ctx, bad, ""); !errors.Is(err, ErrInvalidToken) {
			t.Fatalf("%q: want ErrInvalidToken, got %v", bad, err)
		}
	}

	if err := svc.RevokeToken(ctx, admin, created.Token.ID); err != nil {
		t.Fatal(err)
	}
	if _, err := svc.AuthenticateToken(ctx, created.Secret, ""); !errors.Is(err, ErrInvalidToken) {
		t.Fatalf("revoked token still works: %v", err)
	}
	if err := svc.RevokeToken(ctx, admin, created.Token.ID); !errors.Is(err, domain.ErrNotFound) {
		t.Fatalf("revoking twice: %v", err)
	}
}

func TestTokenExpiresAndReadScope(t *testing.T) {
	svc, _ := newBareService(t)
	ctx := context.Background()
	admin := adminActor(t, svc)
	days := 30
	created, err := svc.CreateToken(ctx, admin, TokenInput{Name: "monitor", ExpiresInDays: &days})
	if err != nil {
		t.Fatal(err)
	}
	if created.Token.ExpiresAt == nil || time.Until(*created.Token.ExpiresAt) < 29*24*time.Hour {
		t.Fatalf("expiry %v", created.Token.ExpiresAt)
	}
	sess, err := svc.AuthenticateToken(ctx, created.Secret, "")
	if err != nil {
		t.Fatal(err)
	}
	if sess.Token.CanWrite() || strings.Join(sess.Token.Scopes, ",") != "read" {
		t.Fatalf("no scope means read only: %v", sess.Token.Scopes)
	}
	// Move the expiry to the past.
	past := store.NullMillis(time.Now().Add(-time.Minute))
	if _, err := svc.store.W().DeleteAPIToken(ctx, db.DeleteAPITokenParams{ID: created.Token.ID, UserID: admin.ID}); err != nil {
		t.Fatal(err)
	}
	err = svc.store.W().CreateAPIToken(ctx, db.CreateAPITokenParams{
		ID: created.Token.ID, UserID: admin.ID, Name: "monitor", TokenHash: tokenID(created.Secret), Prefix: created.Token.Prefix,
		ScopesJson: `["read"]`, ExpiresAt: past, CreatedAt: time.Now().UnixMilli(),
	})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := svc.AuthenticateToken(ctx, created.Secret, ""); !errors.Is(err, ErrInvalidToken) {
		t.Fatalf("expired token accepted: %v", err)
	}
	list, _ := svc.ListTokens(ctx, admin.ID)
	if len(list) != 1 || !list[0].Expired {
		t.Fatalf("expired token not flagged: %+v", list)
	}
}

func TestAuditRecordsOrigin(t *testing.T) {
	svc, rec := newBareService(t)
	ctx := context.Background()
	admin := adminActor(t, svc)
	created, err := svc.CreateToken(ctx, admin, TokenInput{Name: "ci", Scopes: []string{"read", "write"}})
	if err != nil {
		t.Fatal(err)
	}
	sess, err := svc.AuthenticateToken(ctx, created.Secret, "10.0.0.9")
	if err != nil {
		t.Fatal(err)
	}
	viaAPI := Actor{Type: "user", ID: sess.User.ID, Name: sess.User.Username, IP: "10.0.0.9", Origin: OriginAPI, Token: sess.Token}
	max := 7
	if _, err := svc.UpdateSettings(ctx, viaAPI, SettingsPatch{MaxCameras: &max}); err != nil {
		t.Fatal(err)
	}
	svc.audit(ctx, Actor{Type: "camera", ID: "cam1", IP: "10.0.0.20"}, "camera.config", "camera", "cam1", map[string]any{"x": 1})
	svc.audit(ctx, SystemActor, "camera.start", "camera", "cam1", nil)
	if _, _, err := svc.Login(ctx, "nobody", "wrong password!", "10.0.0.66", ""); !errors.Is(err, ErrUnauthenticated) {
		t.Fatal(err)
	}
	if _, _, err := svc.Login(ctx, "admin", "wrong password!", "10.0.0.66", ""); !errors.Is(err, ErrUnauthenticated) {
		t.Fatal(err)
	}

	page, err := svc.ListAudit(ctx, AuditFilter{})
	if err != nil {
		t.Fatal(err)
	}
	byAction := map[string]AuditView{}
	for _, e := range page.Items {
		if _, ok := byAction[e.Action]; !ok {
			byAction[e.Action] = e
		}
	}
	set := byAction["settings.update"]
	if set.Origin != OriginAPI || set.Token == nil || set.Token.Name != "ci" || set.Actor.Name != "admin" || set.OriginIP != "10.0.0.9" {
		t.Fatalf("settings change via token: %+v", set)
	}
	tok := byAction["token.create"]
	if tok.Origin != OriginPanel || tok.Entity.Type != "token" || tok.Entity.Name != "ci" || tok.Token != nil {
		t.Fatalf("token creation from the panel: %+v", tok)
	}
	if e := byAction["camera.config"]; e.Origin != OriginCamera || e.OriginIP != "10.0.0.20" {
		t.Fatalf("change from the emulated API: %+v", e)
	}
	if e := byAction["camera.start"]; e.Origin != OriginSystem {
		t.Fatalf("reconciler: %+v", e)
	}
	if e := byAction["auth.login_failed"]; e.Actor.Name != "admin" || e.Actor.ID != "" || e.OriginIP != "10.0.0.66" {
		t.Fatalf("failed login: %+v", e)
	}

	only, err := svc.ListAudit(ctx, AuditFilter{Origin: OriginAPI})
	if err != nil || len(only.Items) != 1 || only.Items[0].Action != "settings.update" {
		t.Fatalf("origin filter: %+v, %v", only.Items, err)
	}
	byToken, _ := svc.ListAudit(ctx, AuditFilter{TokenID: created.Token.ID})
	if len(byToken.Items) != 1 {
		t.Fatalf("token filter: %+v", byToken.Items)
	}
	auth, _ := svc.ListAudit(ctx, AuditFilter{Action: "auth"})
	for _, e := range auth.Items {
		if !strings.HasPrefix(e.Action, "auth.") {
			t.Fatalf("action prefix matched %s", e.Action)
		}
	}
	if len(auth.Items) != 2 { // setup and the failed login
		t.Fatalf("auth entries: %+v", auth.Items)
	}
	cam, _ := svc.ListAudit(ctx, AuditFilter{EntityType: "camera", EntityID: "cam1"})
	if len(cam.Items) != 2 {
		t.Fatalf("entity filter: %+v", cam.Items)
	}
	if _, err := svc.ListAudit(ctx, AuditFilter{Origin: "moon"}); err == nil {
		t.Fatal("unknown origin accepted")
	}

	// Pages follow the cursor without gaps or repeats.
	first, _ := svc.ListAudit(ctx, AuditFilter{Limit: 2})
	if len(first.Items) != 2 || first.NextCursor == "" {
		t.Fatalf("first page: %+v", first)
	}
	rest, _ := svc.ListAudit(ctx, AuditFilter{Cursor: first.NextCursor})
	if len(first.Items)+len(rest.Items) != len(page.Items) || rest.Items[0].ID >= first.Items[1].ID {
		t.Fatalf("pagination: %d + %d of %d", len(first.Items), len(rest.Items), len(page.Items))
	}

	live := 0
	for _, m := range rec.msgs {
		if m.topic == "audit" && m.typ == "entry" {
			live++
		}
	}
	if live != len(page.Items) {
		t.Fatalf("published %d audit entries, stored %d", live, len(page.Items))
	}
}
