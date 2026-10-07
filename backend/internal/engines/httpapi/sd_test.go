package httpapi

import (
	"context"
	"encoding/json"
	"io"
	"net"
	"net/http"
	"strings"
	"testing"
	"time"

	"github.com/corticoide/mockvision/sdk/engine"
)

// fakeFiles is a card with two recordings of a motion event.
type fakeFiles struct {
	state string
	files []engine.FileInfo
	data  map[string]string
}

func newFakeFiles() *fakeFiles {
	at := time.Date(2026, 10, 7, 14, 30, 0, 0, time.UTC)
	return &fakeFiles{
		state: engine.StoragePresent,
		files: []engine.FileInfo{
			{Name: "20261007/143000_motion_x7k2pq.jpg", Kind: engine.FileSnapshot, Event: "motion", Stream: "main", Size: 4, Start: at, End: at},
			{Name: "20261007/143000_motion_x7k2pq.ts", Kind: engine.FileClip, Event: "motion", Stream: "main", Size: 6, Start: at, End: at.Add(10 * time.Second)},
		},
		data: map[string]string{"20261007/143000_motion_x7k2pq.jpg": "jpeg", "20261007/143000_motion_x7k2pq.ts": "mpegts"},
	}
}

func (f *fakeFiles) Status() engine.StorageStatus {
	return engine.StorageStatus{Kind: "sd", State: f.state, CapacityBytes: 64 << 20, UsedBytes: 10, Files: len(f.files)}
}

func (f *fakeFiles) Find(_ context.Context, q engine.FileQuery) ([]engine.FileInfo, error) {
	if !f.Status().Usable() {
		return nil, engine.ErrStorageUnavailable
	}
	var out []engine.FileInfo
	for _, fi := range f.files {
		if (!q.To.IsZero() && fi.Start.After(q.To)) || (!q.From.IsZero() && fi.End.Before(q.From)) {
			continue
		}
		if (q.Kind != "" && fi.Kind != q.Kind) || (q.Event != "" && fi.Event != q.Event) {
			continue
		}
		out = append(out, fi)
	}
	if q.Limit > 0 && len(out) > q.Limit {
		out = out[:q.Limit]
	}
	return out, nil
}

func (f *fakeFiles) Open(_ context.Context, name string) (io.ReadCloser, engine.FileInfo, error) {
	if !f.Status().Usable() {
		return nil, engine.FileInfo{}, engine.ErrStorageUnavailable
	}
	for _, fi := range f.files {
		if fi.Name == name {
			return io.NopCloser(strings.NewReader(f.data[name])), fi, nil
		}
	}
	return nil, engine.FileInfo{}, engine.ErrNoFile
}

const sdConfig = `{
  "engine": "http-api@^1",
  "auth": {"scheme": "none"},
  "routes": [
    {"id": "search", "match": {"path": "/cgi-bin/mediaFileFind.cgi"},
     "action": {"handler": "sd.search", "params": {"start": "StartTime", "end": "EndTime", "kind": "Type"},
       "time_format": "2006-01-02 15:04:05", "kinds": {"snapshot": "jpg", "clip": "dav"},
       "then": {"type": "text/plain", "body": "found={{ len .Result }}\n{{ range $i, $f := .Result }}items[{{ $i }}].FilePath={{ $f.Name }}\nitems[{{ $i }}].Type={{ $f.Kind }}\nitems[{{ $i }}].StartTime={{ $f.Start }}\nitems[{{ $i }}].EndTime={{ $f.End }}\n{{ end }}"}}},
    {"id": "plain", "match": {"path": "/search"}, "action": {"handler": "sd.search", "time_format": "unix"}},
    {"id": "json", "match": {"method": "POST", "path": "/search.json"},
     "action": {"handler": "sd.search", "from": "json", "then": {"type": "application/json", "body": "{{ json .Result }}"}}},
    {"id": "download", "match": {"path": "/cgi-bin/RPC_Loadfile/{file}"}, "action": {"handler": "sd.download", "key": "file"}},
    {"id": "get", "match": {"path": "/download.cgi"}, "action": {"handler": "sd.download"}}
  ]
}`

func startSD(t *testing.T, files engine.Files) string {
	t.Helper()
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	e := New()
	h := fakeHost{state: &fakeState{values: map[string]any{}}, files: files}
	if err := e.Start(context.Background(), engine.StartInput{Config: json.RawMessage(sdConfig), Instance: "http",
		Listeners: map[string]net.Listener{"http": ln}, Host: h}); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = e.Stop(context.Background()) })
	return "http://" + ln.Addr().String()
}

func fetch(t *testing.T, method, url, body string) (int, http.Header, string) {
	t.Helper()
	req, _ := http.NewRequest(method, url, strings.NewReader(body))
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	b, _ := io.ReadAll(resp.Body)
	return resp.StatusCode, resp.Header, string(b)
}

func TestSDSearchAndDownload(t *testing.T) {
	files := newFakeFiles()
	base := startSD(t, files)

	// The device's words and times, both ways.
	code, _, body := fetch(t, "GET", base+"/cgi-bin/mediaFileFind.cgi?StartTime=2026-10-07%2014:00:00&EndTime=2026-10-07%2015:00:00&Type=dav", "")
	want := "found=1\nitems[0].FilePath=20261007/143000_motion_x7k2pq.ts\nitems[0].Type=dav\nitems[0].StartTime=2026-10-07 14:30:00\nitems[0].EndTime=2026-10-07 14:30:10\n"
	if code != http.StatusOK || body != want {
		t.Fatalf("search: %d %q", code, body)
	}
	if code, _, body := fetch(t, "GET", base+"/cgi-bin/mediaFileFind.cgi?StartTime=2026-10-07%2015:00:00&EndTime=2026-10-07%2016:00:00", ""); code != 200 || body != "found=0\n" {
		t.Fatalf("an empty range: %d %q", code, body)
	}
	for _, q := range []string{"StartTime=yesterday", "Type=mp4", "StartTime=2026-10-07%2015:00:00&EndTime=2026-10-07%2014:00:00"} {
		if code, _, _ := fetch(t, "GET", base+"/cgi-bin/mediaFileFind.cgi?"+q, ""); code != http.StatusBadRequest {
			t.Fatalf("%s: %d", q, code)
		}
	}
	// Without a template, one line each; parameters from a JSON body.
	if code, _, body := fetch(t, "GET", base+"/search?kind=snapshot", ""); code != 200 || body != "20261007/143000_motion_x7k2pq.jpg snapshot 1791383400 1791383400 4\n" {
		t.Fatalf("plain: %d %q", code, body)
	}
	code, _, body = fetch(t, "POST", base+"/search.json", `{"event": "motion", "limit": 1}`)
	var list []map[string]any
	if code != 200 || json.Unmarshal([]byte(body), &list) != nil || len(list) != 1 || list[0]["kind"] != "snapshot" || list[0]["duration"] != 0.0 {
		t.Fatalf("json: %d %s", code, body)
	}

	// The file, by a path segment or a query parameter.
	code, h, body := fetch(t, "GET", base+"/cgi-bin/RPC_Loadfile/x?file=20261007/143000_motion_x7k2pq.ts", "")
	if code != 200 || body != "mpegts" || h.Get("Content-Type") != "video/mp2t" || !strings.Contains(h.Get("Content-Disposition"), "143000_motion_x7k2pq.ts") {
		t.Fatalf("download: %d %q %v", code, body, h)
	}
	if code, h, body := fetch(t, "GET", base+"/download.cgi?name=20261007/143000_motion_x7k2pq.jpg", ""); code != 200 || body != "jpeg" || h.Get("Content-Type") != "image/jpeg" {
		t.Fatalf("download jpg: %d %q", code, body)
	}
	if code, _, _ := fetch(t, "GET", base+"/download.cgi?name=20261007/nope.jpg", ""); code != http.StatusNotFound {
		t.Fatalf("a missing file: %d", code)
	}
	if code, _, _ := fetch(t, "GET", base+"/download.cgi", ""); code != http.StatusBadRequest {
		t.Fatalf("no name: %d", code)
	}

	// A card out of its slot answers neither.
	files.state = engine.StorageAbsent
	if code, _, _ := fetch(t, "GET", base+"/search", ""); code != http.StatusServiceUnavailable {
		t.Fatalf("search without a card: %d", code)
	}
	if code, _, _ := fetch(t, "GET", base+"/download.cgi?name=20261007/143000_motion_x7k2pq.jpg", ""); code != http.StatusServiceUnavailable {
		t.Fatalf("download without a card: %d", code)
	}
}

func TestSDWithoutStorage(t *testing.T) {
	base := startSD(t, nil)
	if code, _, body := fetch(t, "GET", base+"/search", ""); code != http.StatusServiceUnavailable || !strings.Contains(body, "no storage") {
		t.Fatalf("%d %q", code, body)
	}
}

func TestSDActionsAreValidated(t *testing.T) {
	e := New()
	for _, bad := range []string{
		`{"handler": "sd.search", "params": {"from": "x"}}`,
		`{"handler": "sd.search", "time_format": "dd/mm"}`,
		`{"handler": "sd.search", "kinds": {"video": "dav"}}`,
		`{"handler": "sd.search", "kinds": {"snapshot": "x", "clip": "x"}}`,
		`{"handler": "snapshot", "params": {"start": "s"}}`,
		`{"handler": "sd.download", "time_format": "unix"}`,
	} {
		cfg := `{"engine": "http-api@^1", "routes": [{"id": "r", "match": {"path": "/x"}, "action": ` + bad + `}]}`
		if probs := e.Validate(json.RawMessage(cfg)); !hasErrors(probs) {
			t.Fatalf("%s was accepted", bad)
		}
	}
	if probs := e.Validate(json.RawMessage(sdConfig)); hasErrors(probs) {
		t.Fatalf("the SD routes: %+v", probs)
	}
}
