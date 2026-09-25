package secret

import (
	"os"
	"path/filepath"
	"testing"
)

func TestSealOpen(t *testing.T) {
	path := filepath.Join(t.TempDir(), "node.key")
	box, err := LoadOrCreate(path)
	if err != nil {
		t.Fatal(err)
	}
	info, err := os.Stat(path)
	if err != nil || info.Mode().Perm() != 0o600 {
		t.Fatalf("key file must be 0600: %v %v", info.Mode(), err)
	}
	sealed := box.Seal([]byte("ms1234"), "camera_users:c1")
	plain, err := box.Open(sealed, "camera_users:c1")
	if err != nil || string(plain) != "ms1234" {
		t.Fatalf("Open = %q, %v", plain, err)
	}
	if _, err := box.Open(sealed, "camera_users:c2"); err == nil {
		t.Fatal("a ciphertext must not open under another context")
	}

	again, err := LoadOrCreate(path)
	if err != nil {
		t.Fatal(err)
	}
	if plain, err := again.Open(sealed, "camera_users:c1"); err != nil || string(plain) != "ms1234" {
		t.Fatal("reloading the key must open old secrets")
	}
}
