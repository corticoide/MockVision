package store

import (
	"context"
	"database/sql"
	"io/fs"
	"path/filepath"
	"testing"

	"github.com/pressly/goose/v3"

	"github.com/corticoide/mockvision/database"
)

// Audit entries written before API tokens existed get an origin: the
// panel for users, the camera or the node for the rest.
func TestMigrationGivesOldAuditEntriesAnOrigin(t *testing.T) {
	ctx := context.Background()
	path := filepath.Join(t.TempDir(), "db.sqlite")
	w, _ := dsn(path)
	conn, err := sql.Open("sqlite", w)
	if err != nil {
		t.Fatal(err)
	}
	defer conn.Close()
	migrations, _ := fs.Sub(database.Migrations, "migrations")
	provider, err := goose.NewProvider(goose.DialectSQLite3, conn, migrations)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := provider.UpTo(ctx, 1); err != nil {
		t.Fatal(err)
	}
	for _, q := range []string{
		`INSERT INTO users (id, username, password_hash, role, created_at) VALUES ('u1', 'admin', 'x', 'admin', 1)`,
		`INSERT INTO audit_log (id, at, actor_type, actor_id, action, entity_type) VALUES ('a1', 1, 'user', 'u1', 'camera.start', 'camera')`,
		`INSERT INTO audit_log (id, at, actor_type, actor_id, action, entity_type) VALUES ('a2', 2, 'user', 'mallory', 'auth.login_failed', 'user')`,
		`INSERT INTO audit_log (id, at, actor_type, actor_id, action, entity_type) VALUES ('a3', 3, 'camera', 'c1', 'camera.config', 'camera')`,
		`INSERT INTO audit_log (id, at, actor_type, actor_id, action, entity_type) VALUES ('a4', 4, 'system', 'reconciler', 'camera.start', 'camera')`,
	} {
		if _, err := conn.ExecContext(ctx, q); err != nil {
			t.Fatal(err)
		}
	}
	if _, err := provider.Up(ctx); err != nil {
		t.Fatal(err)
	}
	want := map[string][3]string{ // origin, actor_id, actor_name
		"a1": {"panel", "u1", "admin"},
		"a2": {"panel", "", "mallory"},
		"a3": {"camera", "c1", ""},
		"a4": {"system", "reconciler", ""},
	}
	rows, err := conn.QueryContext(ctx, `SELECT id, origin, actor_id, actor_name FROM audit_log`)
	if err != nil {
		t.Fatal(err)
	}
	defer rows.Close()
	n := 0
	for rows.Next() {
		var id, origin, actorID, actorName string
		if err := rows.Scan(&id, &origin, &actorID, &actorName); err != nil {
			t.Fatal(err)
		}
		if got := [3]string{origin, actorID, actorName}; got != want[id] {
			t.Errorf("%s: got %v, want %v", id, got, want[id])
		}
		n++
	}
	if n != len(want) {
		t.Fatalf("%d rows", n)
	}
	// The migration can be rolled back.
	if _, err := provider.Down(ctx); err != nil {
		t.Fatal(err)
	}
}
