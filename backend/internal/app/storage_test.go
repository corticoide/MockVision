package app

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"io"
	"net"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/go-git/go-billy/v5/osfs"
	nfs "github.com/willscott/go-nfs"
	nfshelper "github.com/willscott/go-nfs/helpers"

	"github.com/corticoide/mockvision/backend/internal/domain"
	"github.com/corticoide/mockvision/backend/internal/store/db"
	"github.com/corticoide/mockvision/sdk/engine"
)

func TestStorageIsValidated(t *testing.T) {
	svc := newTestService(t)
	ctx := context.Background()
	cam := createCamera(t, svc, "Gate", false)
	v, err := svc.GetStorage(ctx, cam.ID)
	if err != nil || v.Kind != "none" || v.MaxSDMB != 256<<10 || len(v.NASProtocols) != 2 || len(v.Records) != 5 || v.Records[0].ClipS != 10 {
		t.Fatalf("storage %+v %v", v, err)
	}
	for field, in := range map[string]StorageInput{
		"kind":    {Kind: ptr("tape")},
		"size_mb": {Kind: ptr("sd"), SizeMB: ptr(32)},
		"nas_url": {Kind: ptr("nas"), NASURL: ptr("ftp://nas/share")},
	} {
		_, err := svc.UpdateStorage(ctx, testActor, cam.ID, in)
		wantInvalid(t, err, field)
	}
	for _, u := range []string{"nfs://nas", "smb://user:pw@nas/share", "nfs:///export"} {
		_, err := svc.UpdateStorage(ctx, testActor, cam.ID, StorageInput{Kind: ptr("nas"), NASURL: ptr(u)})
		wantInvalid(t, err, "nas_url")
	}
	// A card the node's disk cannot hold, with what other cards promised.
	svc.diskFree = func(string) (uint64, bool) { return 100 << 20, true }
	_, err = svc.UpdateStorage(ctx, testActor, cam.ID, StorageInput{Kind: ptr("sd"), SizeMB: ptr(128)})
	var rej *domain.RejectedError
	if !errors.As(err, &rej) || rej.Code != domain.RejectDisk {
		t.Fatalf("a card larger than the disk: %v", err)
	}
	v, err = svc.UpdateStorage(ctx, testActor, cam.ID, StorageInput{Kind: ptr("sd"), SizeMB: ptr(64)})
	if err != nil || v.Kind != "sd" || v.SizeMB != 64 || !v.Overwrite || v.Status.State != engine.StoragePresent || v.Status.CapacityBytes != 64<<20 {
		t.Fatalf("a 64 MB card: %+v %v", v, err)
	}
	other := createCamera(t, svc, "Door", false)
	if _, err := svc.UpdateStorage(ctx, testActor, other.ID, StorageInput{Kind: ptr("sd"), SizeMB: ptr(64)}); !errors.As(err, &rej) {
		t.Fatalf("the first card's promise is not counted: %v", err)
	}
	// The same card again needs no more disk.
	if _, err := svc.UpdateStorage(ctx, testActor, cam.ID, StorageInput{Overwrite: ptr(false)}); err != nil {
		t.Fatal(err)
	}
	// SD faults need a card.
	if _, err := svc.InjectFault(ctx, testActor, other.ID, FaultInput{Kind: "sd_missing"}); err == nil {
		t.Fatal("an SD fault on a camera without a card")
	}
	// The NAS password is kept, never shown.
	v, err = svc.UpdateStorage(ctx, testActor, other.ID, StorageInput{Kind: ptr("nas"), NASURL: ptr("smb://nas.local/cams"), NASUsername: ptr("cam"), NASPassword: ptr("pw")})
	if err != nil || !v.HasNASPassword || v.NASURL != "smb://nas.local/cams" {
		t.Fatalf("nas %+v %v", v, err)
	}
	raw, _ := json.Marshal(v)
	if strings.Contains(string(raw), `"pw"`) {
		t.Fatal("the password is in the view")
	}
}

// waitRecordings waits for a camera to have n recordings.
func waitRecordings(t *testing.T, svc *Service, id string, n int) []RecordingView {
	t.Helper()
	deadline := time.Now().Add(20 * time.Second)
	for {
		list, err := svc.ListRecordings(context.Background(), id, "")
		if err != nil {
			t.Fatal(err)
		}
		if len(list) >= n {
			return list
		}
		if time.Now().After(deadline) {
			t.Fatalf("%d recordings, want %d", len(list), n)
		}
		time.Sleep(100 * time.Millisecond)
	}
}

func readRecording(t *testing.T, svc *Service, id, rec string) []byte {
	t.Helper()
	_, rc, err := svc.OpenRecording(context.Background(), id, rec)
	if err != nil {
		t.Fatal(err)
	}
	defer rc.Close()
	data, err := io.ReadAll(rc)
	if err != nil {
		t.Fatal(err)
	}
	return data
}

func endpointURL(t *testing.T, v *CameraView, instance string) string {
	t.Helper()
	for _, ep := range v.Endpoints {
		if ep.Instance == instance {
			return ep.URL
		}
	}
	t.Fatalf("no %s endpoint in %+v", instance, v.Endpoints)
	return ""
}

func trigger(t *testing.T, svc *Service, id string) *EventView {
	t.Helper()
	ev, err := svc.TriggerEvent(context.Background(), testActor, id, ManualEventInput{Type: "line_crossing"})
	if err != nil {
		t.Fatal(err)
	}
	return ev
}

// An event records its snapshot and clip on the card; the camera's API
// searches and downloads them, until the card goes missing.
func TestSDRecordsEvents(t *testing.T) {
	svc := newTestService(t)
	ctx := context.Background()
	cam := createCamera(t, svc, "Gate", false)
	if _, err := svc.UpdateStorage(ctx, testActor, cam.ID, StorageInput{Kind: ptr("sd"), SizeMB: ptr(64)}); err != nil {
		t.Fatal(err)
	}
	if _, err := svc.StartCamera(ctx, testActor, cam.ID); err != nil {
		t.Fatal(err)
	}
	cam = waitState(t, svc, cam.ID, domain.StateRunning)
	ev := trigger(t, svc, cam.ID)
	list := waitRecordings(t, svc, cam.ID, 2)
	var clip, snap RecordingView
	for _, r := range list {
		if r.Kind == "clip" {
			clip = r
		} else {
			snap = r
		}
	}
	if clip.EventID != ev.ID || snap.EventID != ev.ID || clip.End.Sub(clip.Start) != 10*time.Second || clip.Location != "sd" || clip.Stream != "main" {
		t.Fatalf("recordings %+v", list)
	}
	if data := readRecording(t, svc, cam.ID, clip.ID); len(data) != int(clip.Size) || data[0] != 0x47 {
		t.Fatalf("clip of %d bytes, starts with %x", len(data), data[:1])
	}
	if data := readRecording(t, svc, cam.ID, snap.ID); !bytes.HasPrefix(data, []byte{0xff, 0xd8}) {
		t.Fatal("the snapshot is not a JPEG")
	}
	v, _ := svc.GetStorage(ctx, cam.ID)
	if v.Status.Files != 2 || v.Status.UsedBytes != clip.Size+snap.Size {
		t.Fatalf("status %+v", v.Status)
	}

	// The device's search and download.
	base := endpointURL(t, cam, "http")
	day := clip.Start.UTC()
	q := url.Values{"action": {"get.record.search"}, "starttime": {day.Add(-time.Minute).Format("2006-01-02 15:04:05")},
		"endtime": {day.Add(time.Minute).Format("2006-01-02 15:04:05")}, "type": {"video"}}
	code, body := digestGet(t, base+"/cgi-bin/operator/operator.cgi?"+q.Encode(), "admin", "ms1234")
	var found struct {
		Total   int `json:"total"`
		Records []struct {
			File string `json:"file"`
			Type string `json:"type"`
		} `json:"records"`
	}
	if code != 200 || json.Unmarshal(body, &found) != nil || found.Total != 1 || found.Records[0].File != clip.Name || found.Records[0].Type != "video" {
		t.Fatalf("search %d %s", code, body)
	}
	code, body = digestGet(t, base+"/cgi-bin/operator/download.cgi?file="+url.QueryEscape(clip.Name), "admin", "ms1234")
	if code != 200 || len(body) != int(clip.Size) {
		t.Fatalf("download %d, %d bytes", code, len(body))
	}
	code, body = digestGet(t, base+"/cgi-bin/operator/operator.cgi?action=get.storage.info", "admin", "ms1234")
	if code != 200 || !strings.Contains(string(body), `"status": "present"`) || !strings.Contains(string(body), `"totalMB": 64`) {
		t.Fatalf("storage info %d %s", code, body)
	}

	// The card goes missing: it raises its event, records nothing, and
	// cannot be searched.
	f, err := svc.InjectFault(ctx, testActor, cam.ID, FaultInput{Kind: "sd_missing"})
	if err != nil {
		t.Fatal(err)
	}
	waitStateReason(t, svc, cam.ID, domain.StateDegraded, "SD card missing")
	waitEventType(t, svc, cam.ID, "storage_missing")
	code, _ = digestGet(t, base+"/cgi-bin/operator/operator.cgi?"+q.Encode(), "admin", "ms1234")
	if code != http.StatusServiceUnavailable {
		t.Fatalf("search without a card: %d", code)
	}
	time.Sleep(1100 * time.Millisecond) // line_crossing's minimum interval
	trigger(t, svc, cam.ID)
	time.Sleep(500 * time.Millisecond)
	if list, _ := svc.ListRecordings(ctx, cam.ID, ""); len(list) != 2 {
		t.Fatalf("a missing card recorded: %d", len(list))
	}
	if _, err := svc.FormatStorage(ctx, testActor, cam.ID); err == nil {
		t.Fatal("a missing card was formatted")
	}
	if err := svc.EndFault(ctx, testActor, cam.ID, f.ID); err != nil {
		t.Fatal(err)
	}
	waitState(t, svc, cam.ID, domain.StateRunning)
	if code, _ = digestGet(t, base+"/cgi-bin/operator/operator.cgi?"+q.Encode(), "admin", "ms1234"); code != 200 {
		t.Fatalf("search with the card back: %d", code)
	}

	// Formatting wipes the card, files and all.
	if v, err := svc.FormatStorage(ctx, testActor, cam.ID); err != nil || v.Status.Files != 0 {
		t.Fatalf("format %+v %v", v, err)
	}
	if entries, _ := os.ReadDir(svc.sdDir(cam.ID)); len(entries) != 0 {
		t.Fatalf("%d entries on a formatted card", len(entries))
	}
}

func waitEventType(t *testing.T, svc *Service, id, typ string) {
	t.Helper()
	deadline := time.Now().Add(10 * time.Second)
	for {
		page, err := svc.ListEvents(context.Background(), EventFilter{CameraID: id, Type: typ, Limit: 1})
		if err == nil && len(page.Items) > 0 {
			return
		}
		if time.Now().After(deadline) {
			t.Fatalf("no %s event", typ)
		}
		time.Sleep(100 * time.Millisecond)
	}
}

// fillCard stores a recording of size bytes that takes the card's space.
func fillCard(t *testing.T, svc *Service, id string, size int64) {
	t.Helper()
	at := time.Now().Add(-time.Hour)
	name := domain.RecordingName("motion", "01OLD000000000000000000000", at, "ts")
	p, _ := svc.sdPath(id, name)
	if err := writeFileAtomic(p, []byte("old")); err != nil {
		t.Fatal(err)
	}
	if err := svc.store.W().InsertRecording(context.Background(), db.InsertRecordingParams{ID: "01OLD000000000000000000000", CameraID: id, EventType: "motion",
		Kind: "clip", Name: name, Size: size, StartAt: at.UnixMilli(), EndAt: at.Add(time.Second).UnixMilli(), Location: "sd", CreatedAt: at.UnixMilli()}); err != nil {
		t.Fatal(err)
	}
}

// A full card overwrites its oldest recordings; one that does not
// overwrite stops and says it is full.
func TestSDFullCard(t *testing.T) {
	svc := newTestService(t)
	ctx := context.Background()
	cam := createCamera(t, svc, "Gate", false)
	if _, err := svc.UpdateStorage(ctx, testActor, cam.ID, StorageInput{Kind: ptr("sd"), SizeMB: ptr(64)}); err != nil {
		t.Fatal(err)
	}
	fillCard(t, svc, cam.ID, 64<<20-200<<10)
	if _, err := svc.StartCamera(ctx, testActor, cam.ID); err != nil {
		t.Fatal(err)
	}
	waitState(t, svc, cam.ID, domain.StateRunning)
	trigger(t, svc, cam.ID)
	// The snapshot fits in the last 200 KiB; the clip takes the oldest
	// recording's place.
	deadline := time.Now().Add(20 * time.Second)
	for {
		list, _ := svc.ListRecordings(ctx, cam.ID, "")
		if len(list) == 2 && list[0].ID != "01OLD000000000000000000000" && list[1].ID != "01OLD000000000000000000000" {
			break
		}
		if time.Now().After(deadline) {
			t.Fatalf("the oldest recording was not overwritten: %+v", list)
		}
		time.Sleep(100 * time.Millisecond)
	}

	// Without overwriting, the card fills up and raises storage_full.
	if _, err := svc.UpdateStorage(ctx, testActor, cam.ID, StorageInput{Overwrite: ptr(false)}); err != nil {
		t.Fatal(err)
	}
	fillCard(t, svc, cam.ID, 62<<20)
	svc.pushStorage(ctx, cam.ID)
	v, _ := svc.GetStorage(ctx, cam.ID)
	if v.Status.State != engine.StorageFull {
		t.Fatalf("a card with %d of %d bytes used: %s", v.Status.UsedBytes, v.Status.CapacityBytes, v.Status.State)
	}
	waitEventType(t, svc, cam.ID, "storage_full")
	time.Sleep(1100 * time.Millisecond)
	trigger(t, svc, cam.ID)
	time.Sleep(500 * time.Millisecond)
	if after, _ := svc.ListRecordings(ctx, cam.ID, ""); len(after) != 3 {
		t.Fatalf("a full card recorded: %d recordings", len(after))
	}
	// Formatting empties it.
	if v, err := svc.FormatStorage(ctx, testActor, cam.ID); err != nil || v.Status.State != engine.StoragePresent {
		t.Fatalf("format %+v %v", v, err)
	}
}

// A camera records to an NFS share itself; the panel reads the recordings
// through it.
func TestNASRecordsEvents(t *testing.T) {
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer ln.Close()
	share := t.TempDir()
	go func() {
		_ = nfs.Serve(ln, nfshelper.NewCachingHandler(nfshelper.NewNullAuthHandler(osfs.New(share)), 1024))
	}()
	svc := newTestService(t)
	ctx := context.Background()
	cam := createCamera(t, svc, "Gate", true)
	if _, err := svc.UpdateStorage(ctx, testActor, cam.ID, StorageInput{Kind: ptr("nas"), NASURL: ptr("nfs://" + ln.Addr().String() + "/export")}); err != nil {
		t.Fatal(err)
	}
	trigger(t, svc, cam.ID)
	list := waitRecordings(t, svc, cam.ID, 2)
	cam, _ = svc.GetCamera(ctx, cam.ID)
	for _, r := range list {
		if r.Location != "nas" {
			t.Fatalf("recording %+v", r)
		}
		on, err := os.ReadFile(filepath.Join(share, cam.Serial, filepath.FromSlash(r.Name)))
		if err != nil || int64(len(on)) != r.Size {
			t.Fatalf("on the share: %d bytes, %v", len(on), err)
		}
		if data := readRecording(t, svc, cam.ID, r.ID); !bytes.Equal(data, on) {
			t.Fatalf("read through the camera: %d bytes, the share has %d", len(data), len(on))
		}
	}
	v, _ := svc.GetStorage(ctx, cam.ID)
	if v.Status.State != engine.StoragePresent || v.NASError != "" || v.Status.Files != 2 {
		t.Fatalf("status %+v %q", v.Status, v.NASError)
	}

	// A share nobody serves: the camera says why.
	ln.Close()
	dead, _ := net.Listen("tcp", "127.0.0.1:0")
	addr := dead.Addr().String()
	dead.Close()
	if _, err := svc.UpdateStorage(ctx, testActor, cam.ID, StorageInput{NASURL: ptr("nfs://" + addr + "/export")}); err != nil {
		t.Fatal(err)
	}
	deadline := time.Now().Add(10 * time.Second)
	for {
		v, _ := svc.GetStorage(ctx, cam.ID)
		if v.Status.State == engine.StorageError && strings.Contains(v.NASError, "connection refused") {
			break
		}
		if time.Now().After(deadline) {
			t.Fatalf("status %+v %q", v.Status, v.NASError)
		}
		time.Sleep(100 * time.Millisecond)
	}
	// A new share: the old one's recordings are no longer the camera's.
	if list, _ := svc.ListRecordings(ctx, cam.ID, ""); len(list) != 0 {
		t.Fatalf("%d recordings of the old share", len(list))
	}
}
