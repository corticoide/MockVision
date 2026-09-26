//go:build linux && integration

package netctl

import (
	"encoding/json"
	"fmt"
	"io"
	"log/slog"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"golang.org/x/sys/unix"

	"github.com/corticoide/mockvision/backend/internal/ipc"
)

// A delete that arrives while the camera is being created leaves nothing
// behind: no namespace, no process, no entry (audit M6).
func TestDeleteDuringCreate(t *testing.T) {
	requireRoot(t)
	testParent(t, "mvtest1")

	// The "camera" only sleeps; it runs as nobody, so the directory must
	// be readable by everyone.
	dir := t.TempDir()
	if err := os.Chmod(dir, 0o755); err != nil {
		t.Fatal(err)
	}
	exe := filepath.Join(dir, "cam.sh")
	if err := os.WriteFile(exe, []byte("#!/bin/sh\nexec sleep 3017\n"), 0o755); err != nil {
		t.Fatal(err)
	}
	helperEnd, serviceEnd, err := seqPair()
	if err != nil {
		t.Fatal(err)
	}
	defer serviceEnd.Close()
	go func() { _, _ = io.Copy(io.Discard, serviceEnd) }()
	h, err := NewHelper(helperEnd, HelperOptions{Exe: exe, CameraUID: 65534, CameraGID: 65534, Log: slog.New(slog.NewTextHandler(io.Discard, nil))})
	helperEnd.Close()
	if err != nil {
		t.Fatal(err)
	}
	defer h.Shutdown()

	// Every other round, creation pauses right after the camera is
	// registered, so the delete lands while nothing is created yet.
	var pause bool
	testHookCreating = func() {
		if pause {
			time.Sleep(30 * time.Millisecond)
		}
	}
	defer func() { testHookCreating = nil }()
	for i := range 12 {
		pause = i%2 == 0
		id := fmt.Sprintf("01J8Z3QK00000000000000%04d", i)
		spec := testSpec(id, fmt.Sprintf("10.98.0.%d", 10+i))
		spec.Parent = "mvtest1"
		spec.SkipProbe = true
		raw, _ := json.Marshal(spec)
		fds, err := unix.Socketpair(unix.AF_UNIX, unix.SOCK_STREAM|unix.SOCK_CLOEXEC, 0)
		if err != nil {
			t.Fatal(err)
		}
		var wg sync.WaitGroup
		wg.Add(2)
		go func() {
			defer wg.Done()
			_, _ = h.create(&ipc.Envelope{V: ipc.Version, Data: raw}, []int{fds[1]})
		}()
		go func() {
			defer wg.Done()
			// Vary the moment the delete lands: before, during and after.
			time.Sleep(time.Duration(i%4) * 5 * time.Millisecond)
			if err := h.delete(id); err != nil {
				t.Errorf("delete %s: %v", id, err)
			}
		}()
		wg.Wait()
		unix.Close(fds[0])
		// Whatever order they ran in, one more delete must leave nothing.
		if err := h.delete(id); err != nil {
			t.Fatal(err)
		}
		if _, err := os.Stat(filepath.Join(netnsDir, spec.Netns)); !os.IsNotExist(err) {
			t.Fatalf("round %d: namespace %s left behind (%v)", i, spec.Netns, err)
		}
		if live := h.list(); len(live) != 0 {
			t.Fatalf("round %d: cameras left: %+v", i, live)
		}
		if pids := cameraProcesses(t); len(pids) != 0 {
			t.Fatalf("round %d: camera processes left: %v", i, pids)
		}
	}
}

// cameraProcesses lists the live camera processes: the script becomes a
// sleep with a duration no other process uses.
func cameraProcesses(t *testing.T) []string {
	t.Helper()
	// Give a killed process a moment to be reaped.
	deadline := time.Now().Add(time.Second)
	for {
		var pids []string
		entries, _ := os.ReadDir("/proc")
		for _, e := range entries {
			status, err := os.ReadFile(filepath.Join("/proc", e.Name(), "status"))
			if err != nil || strings.Contains(string(status), "State:\tZ") {
				continue
			}
			if cmd, _ := os.ReadFile(filepath.Join("/proc", e.Name(), "cmdline")); string(cmd) == "sleep\x003017\x00" {
				pids = append(pids, e.Name())
			}
		}
		if len(pids) == 0 || time.Now().After(deadline) {
			return pids
		}
		time.Sleep(50 * time.Millisecond)
	}
}
