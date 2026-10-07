package ftpupload

import (
	"crypto/ed25519"
	"crypto/rand"
	"fmt"
	"net"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/pkg/sftp"
	"golang.org/x/crypto/ssh"

	"github.com/corticoide/mockvision/backend/internal/engines/enginetest"
	"github.com/corticoide/mockvision/sdk/engine"
)

const section = `{"path": "{{ .Camera.Serial }}/{{ fmtTime .Event.At \"2006-01-02\" }}", "file": "{{ fmtTime .Event.At \"150405\" }}_{{ .EventName }}.jpg"}`

func TestUploadsTheSnapshotToFTP(t *testing.T) {
	for _, epsv := range []bool{true, false} {
		t.Run(fmt.Sprintf("epsv=%v", epsv), func(t *testing.T) {
			srv := enginetest.NewFTPServer(t)
			srv.NoEPSV = !epsv
			h := enginetest.NewHost(t)
			h.SetTargets(engine.Target{ID: "T1", Name: "NAS", Type: engine.TargetFTP, URL: "ftp://" + srv.Addr() + "/cams", Username: "cam", Password: "secret"})
			h.Start(New(), `{"engine": "ftp-upload@^1"}`)
			h.Dispatch(engine.TransportFTP, section, engine.Event{Type: "line_crossing"}, engine.DeliveryPolicy{Timeout: 3 * time.Second})
			reps := h.WaitReports(1, 5*time.Second)
			if reps[0].Status != engine.DeliveryOK {
				t.Fatalf("report %+v; server log %q", reps[0], srv.Log())
			}
			got, ok := srv.File("/home/cams/SN123/2026-10-07/143005_LineCrossing.jpg")
			if !ok || string(got) != string(enginetest.Snapshot) {
				t.Fatalf("files %v", srv.Files())
			}
		})
	}
}

func TestFTPDocumentAndRefusedLogin(t *testing.T) {
	srv := enginetest.NewFTPServer(t)
	h := enginetest.NewHost(t)
	h.SetTargets(
		engine.Target{ID: "OK", Type: engine.TargetFTP, URL: "ftp://" + srv.Addr() + "/%2Fexports", Username: "cam", Password: "secret"},
		engine.Target{ID: "BAD", Type: engine.TargetFTP, URL: "ftp://" + srv.Addr(), Username: "cam", Password: "nope"},
	)
	h.Start(New(), `{"engine": "ftp-upload@^1"}`)
	h.Dispatch(engine.TransportFTP, `{"file": "{{ .Event.ID }}.json", "content": "body", "body": "{\"event\": {{ json .EventName }}}"}`,
		engine.Event{ID: "E9", Type: "motion"}, engine.DeliveryPolicy{Timeout: 3 * time.Second})
	reps := h.WaitReports(2, 5*time.Second)
	by := map[string]engine.DeliveryReport{}
	for _, r := range reps {
		by[r.TargetID] = r
	}
	if by["OK"].Status != engine.DeliveryOK {
		t.Fatalf("report %+v", by["OK"])
	}
	if got, _ := srv.File("/exports/E9.json"); string(got) != `{"event": "LineCrossing"}` {
		t.Fatalf("files %v", srv.Files())
	}
	if b := by["BAD"]; b.Status != engine.DeliveryFailed || !strings.Contains(b.Error, "530") {
		t.Fatalf("refused login: %+v", b)
	}
}

func TestRenderedPathsStayInTheTargetsDirectory(t *testing.T) {
	for _, bad := range []string{"../etc", "a/../../b", "x\ny"} {
		if _, err := cleanDir(bad); err == nil {
			t.Errorf("%q accepted", bad)
		}
	}
	if _, err := cleanName("a/b.jpg"); err == nil {
		t.Error("a file name with a slash was accepted")
	}
	if d, err := cleanDir("/SN1//2026-10-07/"); err != nil || d != "SN1/2026-10-07" {
		t.Errorf("cleanDir = %q, %v", d, err)
	}
	for url, want := range map[string]string{
		"ftp://h/cams":        "cams",
		"ftp://h/%2Fcams":     "/cams",
		"ftp://h":             "",
		"sftp://h/srv/cams":   "/srv/cams",
		"sftp://h/~/cams":     "cams",
		"sftp://h:2222/~":     "",
		"ftp://h:2121/a/b/c/": "a/b/c",
	} {
		typ := strings.SplitN(url, ":", 2)[0]
		loc, err := Locate(engine.Target{Type: typ, URL: url})
		if err != nil || loc.Dir != want {
			t.Errorf("%s: dir %q (%v), want %q", url, loc.Dir, err, want)
		}
	}
}

// sshServer serves SFTP over SSH with a password, on the real filesystem.
func sshServer(t *testing.T) (addr string, fingerprint string) {
	_, key, err := ed25519.GenerateKey(rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	signer, err := ssh.NewSignerFromKey(key)
	if err != nil {
		t.Fatal(err)
	}
	cfg := &ssh.ServerConfig{PasswordCallback: func(c ssh.ConnMetadata, pw []byte) (*ssh.Permissions, error) {
		if c.User() == "cam" && string(pw) == "secret" {
			return nil, nil
		}
		return nil, fmt.Errorf("denied")
	}}
	cfg.AddHostKey(signer)
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { ln.Close() })
	go func() {
		for {
			c, err := ln.Accept()
			if err != nil {
				return
			}
			go func() {
				_, chans, reqs, err := ssh.NewServerConn(c, cfg)
				if err != nil {
					return
				}
				go ssh.DiscardRequests(reqs)
				for nc := range chans {
					ch, in, err := nc.Accept()
					if err != nil {
						continue
					}
					go func() {
						for req := range in {
							ok := req.Type == "subsystem" && string(req.Payload[4:]) == "sftp"
							_ = req.Reply(ok, nil)
							if ok {
								srv, err := sftp.NewServer(ch)
								if err == nil {
									_ = srv.Serve()
								}
								ch.Close()
							}
						}
					}()
				}
			}()
		}
	}()
	return ln.Addr().String(), ssh.FingerprintSHA256(signer.PublicKey())
}

func TestUploadsToSFTPWithAPinnedKey(t *testing.T) {
	addr, fp := sshServer(t)
	dir := t.TempDir()
	h := enginetest.NewHost(t)
	h.SetTargets(
		engine.Target{ID: "OK", Type: engine.TargetSFTP, URL: "sftp://" + addr + dir + "/up", Username: "cam", Password: "secret", HostKey: fp},
		engine.Target{ID: "PIN", Type: engine.TargetSFTP, URL: "sftp://" + addr + dir + "/up", Username: "cam", Password: "secret", HostKey: "SHA256:" + strings.Repeat("A", 43)},
	)
	h.Start(New(), `{"engine": "ftp-upload@^1"}`)
	h.Dispatch(engine.TransportFTP, section, engine.Event{Type: "line_crossing"}, engine.DeliveryPolicy{Timeout: 5 * time.Second})
	reps := h.WaitReports(2, 10*time.Second)
	by := map[string]engine.DeliveryReport{}
	for _, r := range reps {
		by[r.TargetID] = r
	}
	if by["OK"].Status != engine.DeliveryOK {
		t.Fatalf("report %+v", by["OK"])
	}
	got, err := os.ReadFile(filepath.Join(dir, "up", "SN123", "2026-10-07", "143005_LineCrossing.jpg"))
	if err != nil || string(got) != string(enginetest.Snapshot) {
		t.Fatalf("uploaded %q, %v", got, err)
	}
	if p := by["PIN"]; p.Status != engine.DeliveryFailed || !strings.Contains(p.Error, "not the pinned") {
		t.Fatalf("pinned key: %+v", p)
	}
	if err := Probe(t.Context(), engine.Target{Type: engine.TargetSFTP, URL: "sftp://" + addr + dir + "/probe", Username: "cam", Password: "nope"}); err == nil {
		t.Fatal("a refused password passed the test")
	}
	if err := Probe(t.Context(), engine.Target{Type: engine.TargetSFTP, URL: "sftp://" + addr + dir + "/probe", Username: "cam", Password: "secret"}); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(filepath.Join(dir, "probe")); err != nil {
		t.Fatal("the test did not create the target's directory")
	}
}
