package store

import (
	"context"
	"errors"
	"path/filepath"
	"testing"

	"github.com/corticoide/mockvision/backend/internal/domain"
	"github.com/corticoide/mockvision/backend/internal/store/db"
)

func openTest(t *testing.T) *Store {
	t.Helper()
	s, err := Open(context.Background(), filepath.Join(t.TempDir(), "db.sqlite"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { s.Close() })
	return s
}

func TestOpenMigratesAndCounts(t *testing.T) {
	ctx := context.Background()
	s := openTest(t)

	n, err := s.R().CountUsers(ctx)
	if err != nil || n != 0 {
		t.Fatalf("CountUsers = %d, %v", n, err)
	}
	err = s.W().CreateUser(ctx, db.CreateUserParams{ID: "u1", Username: "admin", PasswordHash: "x", Role: "admin", CreatedAt: 1})
	if err != nil {
		t.Fatal(err)
	}
	err = s.W().CreateUser(ctx, db.CreateUserParams{ID: "u2", Username: "ADMIN", PasswordHash: "x", Role: "admin", CreatedAt: 1})
	if !IsUnique(err) {
		t.Fatalf("usernames are unique case-insensitively, got %v", err)
	}
	if _, err := s.R().GetUserByID(ctx, "missing"); !errors.Is(NotFound(err), domain.ErrNotFound) {
		t.Fatalf("expected not found, got %v", err)
	}
	if st := s.Stats(); st.WriteStatements < 2 {
		t.Fatalf("writes not counted: %+v", st)
	}
}

func TestForeignKeysAreEnforced(t *testing.T) {
	ctx := context.Background()
	s := openTest(t)
	err := s.W().InsertCamera(ctx, db.InsertCameraParams{
		ID: "c1", Name: "cam", ProfileID: "acme/x", ProfileVersion: "1.0.0", Serial: "S",
		DesiredState: "stopped", Autostart: 1, TagsJson: "[]", CreatedAt: 1, UpdatedAt: 1,
	})
	if !IsForeignKey(err) {
		t.Fatalf("camera without profile must violate the foreign key, got %v", err)
	}
}

func TestTxRollback(t *testing.T) {
	ctx := context.Background()
	s := openTest(t)
	boom := errors.New("boom")
	err := s.Tx(ctx, func(q *db.Queries) error {
		if err := q.UpsertSetting(ctx, db.UpsertSettingParams{Key: "k", ValueJson: "1"}); err != nil {
			return err
		}
		return boom
	})
	if !errors.Is(err, boom) {
		t.Fatalf("got %v", err)
	}
	if _, err := s.R().GetSetting(ctx, "k"); !errors.Is(NotFound(err), domain.ErrNotFound) {
		t.Fatalf("setting should have been rolled back, got %v", err)
	}
}
