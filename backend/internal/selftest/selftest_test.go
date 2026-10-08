package selftest

import (
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

func TestReadFixtures(t *testing.T) {
	files := map[string][]byte{
		"fixtures/info.yaml": []byte(`id: device-info
request: { method: GET, path: /cgi-bin/magicBox.cgi, query: { action: getSystemInfo } }
response:
  status: 200
  body_file: info.body
vary:
  - { in: body, regex: "serialNumber=(.*)", as: serial }
`),
		"fixtures/info.body": []byte("serialNumber=ABC\r\n"),
		"fixtures/more.yaml": []byte(`fixtures:
  - id: set-then-get
    steps:
      - request: { method: GET, path: "/set?x=1" }
        response: { status: 200, body: OK }
      - request: { method: GET, path: /get }
        response: { status: 200, body: "x=1" }
  - id: motion
    trigger: { type: motion }
    expect:
      http_push: { body: "{}" }
`),
		"profile.yaml": []byte("not a fixture"),
	}
	got, probs := Read(files)
	if len(probs) != 0 {
		t.Fatalf("problems: %+v", probs)
	}
	if len(got) != 3 || got[0].ID != "device-info" || got[0].Response.Body != "serialNumber=ABC\r\n" || got[0].File != "fixtures/info.yaml" {
		t.Fatalf("fixtures %+v", got)
	}
	if len(got[1].sequence()) != 2 || !got[2].IsEvent() {
		t.Fatalf("steps and events: %+v", got[1:])
	}

	for name, doc := range map[string]string{
		"no id":           "request: { method: GET, path: / }\nresponse: { status: 200 }\n",
		"bad method":      "id: x\nrequest: { method: FETCH, path: / }\nresponse: { status: 200 }\n",
		"no response":     "id: x\nrequest: { method: GET, path: / }\n",
		"bad vary type":   "id: x\nrequest: { method: GET, path: / }\nresponse: { status: 200 }\nvary: [{ in: body, regex: 'a(.)', as: color }]\n",
		"two groups":      "id: x\nrequest: { method: GET, path: / }\nresponse: { status: 200 }\nvary: [{ in: body, regex: '(a)(b)', as: any }]\n",
		"missing body":    "id: x\nrequest: { method: GET, path: / }\nresponse: { status: 200, body_file: nope.txt }\n",
		"both kinds":      "id: x\nrequest: { method: GET, path: / }\nresponse: { status: 200 }\ntrigger: { type: motion }\nexpect: { http_push: {} }\n",
		"unknown field":   "id: x\nrequest: { method: GET, path: / }\nresponse: { status: 200 }\nextra: 1\n",
		"event no expect": "id: x\ntrigger: { type: motion }\n",
	} {
		if _, probs := Read(map[string][]byte{"fixtures/x.yaml": []byte(doc)}); len(probs) == 0 {
			t.Errorf("%s: accepted", name)
		}
	}
	dup := map[string][]byte{
		"fixtures/a.yaml": []byte("id: same\nrequest: { method: GET, path: / }\nresponse: { status: 200 }\n"),
		"fixtures/b.yaml": []byte("id: same\nrequest: { method: GET, path: / }\nresponse: { status: 200 }\n"),
	}
	if _, probs := Read(dup); len(probs) != 1 || !strings.Contains(probs[0].Message, "also in fixtures/a.yaml") {
		t.Fatalf("duplicate ids: %+v", probs)
	}
}

func answer(status int, body string, headers ...string) Answer {
	h := http.Header{}
	for i := 0; i+1 < len(headers); i += 2 {
		h.Add(headers[i], headers[i+1])
	}
	return Answer{Status: status, Headers: h, Body: []byte(body)}
}

func TestCompare(t *testing.T) {
	want := &Response{Status: 200, Headers: map[string]string{"Content-Type": "text/plain", "Date": "Mon, 01 Jan 2024 00:00:00 GMT"},
		Body: "deviceType=IPC\nserialNumber=REAL123\n"}
	vary := []Vary{{In: "body", Regex: "serialNumber=(.*)", As: TypeSerial}, {In: "header", Name: "date", As: TypeHTTPDate}}
	ok := answer(200, "deviceType=IPC\r\nserialNumber=SIM999\r\n", "Content-Type", "text/plain", "Date", "Tue, 08 Oct 2026 10:00:00 GMT")
	if err := Compare(want, vary, ok, "SIM999"); err != nil {
		t.Fatal(err)
	}
	for name, c := range map[string]struct {
		got  Answer
		want string
	}{
		"status":       {answer(404, ""), "status 404"},
		"header gone":  {answer(200, ok.Body2(), "Date", "Tue, 08 Oct 2026 10:00:00 GMT"), "Content-Type is missing"},
		"header value": {answer(200, ok.Body2(), "Content-Type", "text/html", "Date", "Tue, 08 Oct 2026 10:00:00 GMT"), `"text/html"`},
		"bad date":     {answer(200, ok.Body2(), "Content-Type", "text/plain", "Date", "yesterday"), "is not a http-date"},
		"other serial": {answer(200, "deviceType=IPC\nserialNumber=OTHER\n", "Content-Type", "text/plain", "Date", "Tue, 08 Oct 2026 10:00:00 GMT"), "not the camera's serial"},
		"other line":   {answer(200, "deviceType=NVR\nserialNumber=SIM999\n", "Content-Type", "text/plain", "Date", "Tue, 08 Oct 2026 10:00:00 GMT"), `line 1 is "deviceType=NVR"`},
	} {
		if err := Compare(want, vary, c.got, "SIM999"); err == nil || !strings.Contains(err.Error(), c.want) {
			t.Errorf("%s: %v, want %q", name, err, c.want)
		}
	}

	// JSON compares as JSON, with paths.
	jw := &Response{Status: 200, Body: `{"id": "6b1f4c2e-0000-4000-8000-000000000000", "at": 1700000000, "count": 2}`}
	jv := []Vary{{In: "body", Path: "$.id", As: TypeUUID}, {In: "body", Path: "$.at", As: TypeTimestamp}}
	if err := Compare(jw, jv, answer(200, `{"count":2,"at":1791400000,"id":"0d9a2b7e-1111-4222-8333-444444444444"}`), ""); err != nil {
		t.Fatal(err)
	}
	if err := Compare(jw, jv, answer(200, `{"count":3,"at":1791400000,"id":"0d9a2b7e-1111-4222-8333-444444444444"}`), ""); err == nil || !strings.Contains(err.Error(), "$.count is 3") {
		t.Fatalf("json difference: %v", err)
	}
	if err := Compare(jw, jv, answer(200, `{"count":2,"at":1791400000,"id":"nope"}`), ""); err == nil || !strings.Contains(err.Error(), "$.id") {
		t.Fatalf("json uuid: %v", err)
	}
	// A body that is an image.
	img := &Response{Status: 200}
	if err := Compare(img, []Vary{{In: "body", As: TypeImage}}, answer(200, "\xff\xd8\xff\xe0rest"), ""); err != nil {
		t.Fatal(err)
	}
	if err := Compare(img, []Vary{{In: "body", As: TypeImage}}, answer(200, "text"), ""); err == nil {
		t.Fatal("text passed as an image")
	}
}

// Body2 is the body as a string, for the table above.
func (a Answer) Body2() string { return string(a.Body) }

type fakeEnv struct {
	url    string
	sent   chan Answer
	routes []string
}

func (e *fakeEnv) URL(inst string) (string, bool) { return e.url, inst == "http" }
func (e *fakeEnv) DefaultHTTP() string            { return "http" }
func (e *fakeEnv) Routes(string) []string         { return e.routes }
func (e *fakeEnv) Credentials() (string, string)  { return "admin", "secret" }
func (e *fakeEnv) Serial() string                 { return "SIM999" }
func (e *fakeEnv) Captures(t string) bool         { return t == "http_push" }
func (e *fakeEnv) Trigger(context.Context, Trigger) error {
	e.sent <- answer(0, `{"type":"motion","serial":"SIM999"}`, "Content-Type", "application/json")
	return nil
}
func (e *fakeEnv) Sent(ctx context.Context, _ string) (Answer, error) {
	select {
	case a := <-e.sent:
		return a, nil
	case <-ctx.Done():
		return Answer{}, errors.New("nothing was sent")
	}
}

func TestRun(t *testing.T) {
	var value string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if u, p, ok := r.BasicAuth(); !ok || u != "admin" || p != "secret" {
			w.Header().Set("WWW-Authenticate", `Basic realm="cam"`)
			w.WriteHeader(http.StatusUnauthorized)
			return
		}
		switch r.URL.Path {
		case "/set":
			value = r.URL.Query().Get("x")
			_, _ = w.Write([]byte("OK"))
		case "/get":
			_, _ = w.Write([]byte("x=" + value))
		case "/info":
			_, _ = w.Write([]byte("serialNumber=SIM999"))
		}
	}))
	defer srv.Close()
	no := false
	fixtures := []Fixture{
		{ID: "info", Request: &Request{Method: "GET", Path: "/info"}, Response: &Response{Status: 200, Body: "serialNumber=REAL"},
			Vary: []Vary{{In: "body", Regex: "serialNumber=(.*)", As: TypeSerial}}},
		{ID: "challenge", Request: &Request{Method: "GET", Path: "/info", Auth: &no}, Response: &Response{Status: 401, Headers: map[string]string{"WWW-Authenticate": `Basic realm="cam"`}}},
		{ID: "set-get", Steps: []Step{
			{Request: &Request{Method: "GET", Path: "/set", Query: map[string]string{"x": "7"}}, Response: &Response{Status: 200, Body: "OK"}},
			{Request: &Request{Method: "GET", Path: "/get"}, Response: &Response{Status: 200, Body: "x=8"}},
		}},
		{ID: "motion", Trigger: &Trigger{Type: "motion"}, Expect: map[string]Expect{"http_push": {Body: `{"serial":"X","type":"motion"}`,
			Vary: []Vary{{In: "body", Path: "$.serial", As: TypeSerial}}}}},
		{ID: "lpr", Trigger: &Trigger{Type: "lpr"}, Expect: map[string]Expect{"mqtt": {Body: "{}"}}},
	}
	env := &fakeEnv{url: srv.URL, sent: make(chan Answer, 1), routes: []string{"info"}}
	rep := Run(context.Background(), env, fixtures)
	status := map[string]string{}
	for _, r := range rep.Results {
		status[r.ID] = r.Status
	}
	if status["info"] != Passed || status["challenge"] != Passed || status["set-get"] != Failed || status["motion"] != Passed || status["lpr"] != Skipped {
		t.Fatalf("results %+v", rep.Results)
	}
	if !strings.Contains(rep.Results[2].Detail, "step 2") || rep.Complete() {
		t.Fatalf("report %+v", rep)
	}
	cov := Coverage(rep.Results, map[string][]string{"http": {"info", "other"}}, []string{"motion", "lpr"})
	if cov["route:http/info"] != Verified || cov["route:http/other"] != Declared || cov["event:motion"] != Verified || cov["event:lpr"] != Declared {
		t.Fatalf("coverage %v", cov)
	}
}
