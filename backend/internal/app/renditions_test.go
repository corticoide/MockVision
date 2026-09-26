package app

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/oklog/ulid/v2"

	"github.com/corticoide/mockvision/backend/internal/store/db"
)

// Renditions no camera uses are removed with their files once their grace
// period is over; stray directories go too, anything else stays.
func TestCollectRenditions(t *testing.T) {
	svc, _ := newBareService(t)
	ctx := context.Background()
	w := svc.store.W()
	sha := strings.Repeat("ab", 32)
	asset := ulid.Make().String()
	if err := w.InsertAsset(ctx, db.InsertAssetParams{ID: asset, Sha256: sha, Kind: "image", Mime: "image/jpeg", Width: 64, Height: 64, Size: 1, Filename: "a.jpg", CreatedAt: 1}); err != nil {
		t.Fatal(err)
	}
	now := time.Now()
	add := func(bitrate int64, created time.Time) (string, string) {
		id := ulid.Make().String()
		if err := w.InsertRendition(ctx, db.InsertRenditionParams{ID: id, AssetID: asset, Codec: "h264", Width: 64, Height: 64, Fps: 5, Gop: 5,
			Bitrate: bitrate, CreatedAt: created.UnixMilli()}); err != nil {
			t.Fatal(err)
		}
		key := renditionParams(db.Rendition{Codec: "h264", Width: 64, Height: 64, Fps: 5, Gop: 5, Bitrate: bitrate}).Key(sha)
		dir := svc.lib.RenditionFiles(key, "h264").Dir
		if err := os.MkdirAll(dir, 0o755); err != nil {
			t.Fatal(err)
		}
		return id, dir
	}
	oldID, oldDir := add(100, now.Add(-2*renditionGrace))
	newID, newDir := add(200, now.Add(-time.Minute))

	stray := filepath.Join(svc.lib.RenditionsDir, strings.Repeat("cd", 32))
	other := filepath.Join(svc.lib.RenditionsDir, "keep-me")
	for _, d := range []string{stray, other} {
		if err := os.MkdirAll(d, 0o755); err != nil {
			t.Fatal(err)
		}
		old := now.Add(-2 * time.Hour)
		_ = os.Chtimes(d, old, old)
	}

	svc.collectRenditions(ctx, now)

	if _, err := svc.store.R().GetRendition(ctx, oldID); err == nil {
		t.Fatal("an unused rendition past its grace period must be deleted")
	}
	if _, err := os.Stat(oldDir); !os.IsNotExist(err) {
		t.Fatalf("its files must go too: %v", err)
	}
	if _, err := svc.store.R().GetRendition(ctx, newID); err != nil {
		t.Fatal("a recent rendition must be kept")
	}
	if _, err := os.Stat(newDir); err != nil {
		t.Fatal("the files of a recent rendition must be kept")
	}
	if _, err := os.Stat(stray); !os.IsNotExist(err) {
		t.Fatal("a stray rendition directory must be removed")
	}
	if _, err := os.Stat(other); err != nil {
		t.Fatal("a directory that is not a rendition key must be left alone")
	}
	if len(svc.encodes) != 0 {
		t.Fatalf("encode locks leaked: %d", len(svc.encodes))
	}
}

// A burst of encoder changes leads to one background run per camera, not
// one per change.
func TestRegenerationIsCoalesced(t *testing.T) {
	svc, _ := newBareService(t)
	for range 100 {
		svc.regenerateStreamsLater("01J00000000000000000000000")
	}
	svc.mu.Lock()
	pending := len(svc.regens)
	svc.mu.Unlock()
	if pending != 1 {
		t.Fatalf("%d regenerations pending, want 1", pending)
	}
	deadline := time.Now().Add(5 * time.Second)
	for {
		svc.mu.Lock()
		pending = len(svc.regens)
		svc.mu.Unlock()
		if pending == 0 {
			break
		}
		if time.Now().After(deadline) {
			t.Fatal("the regeneration never finished")
		}
		time.Sleep(20 * time.Millisecond)
	}
}

// Cameras whose names give the same slug get distinct namespaces, even
// when they start at once (audit B12).
func TestNamespaceNamesAreReserved(t *testing.T) {
	svc, _ := newBareService(t)
	bundle := func(id, name string) *cameraBundle {
		return &cameraBundle{cam: db.Camera{ID: id, Name: name}}
	}
	a := bundle("01J8Z3QK00000000000000AAAA", "Cam 1")
	b := bundle("01J8Z3QK00000000000000BBBB", "cam-1")
	na, nb := svc.netnsName(a), svc.netnsName(b)
	if na == nb {
		t.Fatalf("both cameras got %s", na)
	}
	if na != "sim-cam-1" || nb != "sim-cam-1-bbbb" {
		t.Fatalf("names %s and %s", na, nb)
	}
	if again := svc.netnsName(a); again != na {
		t.Fatalf("a camera keeps its name: %s, was %s", again, na)
	}
	svc.releaseNetns(a.cam.ID)
	if got := svc.netnsName(bundle("01J8Z3QK00000000000000CCCC", "CAM 1")); got != "sim-cam-1" {
		t.Fatalf("a released name is reused: %s", got)
	}
}
