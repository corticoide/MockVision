package camera

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"net"
	"net/http"
	"strconv"
	"time"

	"github.com/corticoide/mockvision/backend/internal/ipc"
	"github.com/corticoide/mockvision/backend/internal/profile"
	"github.com/corticoide/mockvision/backend/internal/selftest"
	"github.com/corticoide/mockvision/sdk/engine"
)

// selfTestTarget is the target an ephemeral camera's events go to: its own
// sink, where the replay reads what http-push sent.
const selfTestTarget = "selftest"

// selfTestSink receives what the camera's events push over HTTP during a
// self-test. It listens on 127.0.0.1 of the camera's own namespace, which
// reaches nothing else.
type selfTestSink struct {
	srv *http.Server
	url string
	got chan selftest.Answer
}

func newSelfTestSink() (*selfTestSink, error) {
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		return nil, err
	}
	s := &selfTestSink{url: "http://127.0.0.1:" + strconv.Itoa(ln.Addr().(*net.TCPAddr).Port) + "/selftest", got: make(chan selftest.Answer, 16)}
	s.srv = &http.Server{ReadHeaderTimeout: 5 * time.Second, Handler: http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		body, _ := io.ReadAll(io.LimitReader(r.Body, 8<<20))
		select {
		case s.got <- selftest.Answer{Headers: r.Header.Clone(), Body: body}:
		default:
		}
		w.WriteHeader(http.StatusOK)
	})}
	go func() { _ = s.srv.Serve(ln) }()
	return s, nil
}

func (s *selfTestSink) target() ipc.Target {
	return ipc.Target{Target: engine.Target{ID: selfTestTarget, Name: "Self-test", Type: "http", URL: s.url, Method: http.MethodPost}}
}

// drain forgets what was sent before.
func (s *selfTestSink) drain() {
	for {
		select {
		case <-s.got:
		default:
			return
		}
	}
}

func (s *selfTestSink) close() {
	if s != nil {
		_ = s.srv.Close()
	}
}

// runSelfTest replays fixtures against the camera's own engines.
func (r *Runtime) runSelfTest(ctx context.Context, st ipc.SelfTest) (selftest.Report, error) {
	if r.sink == nil || r.model == nil {
		return selftest.Report{}, errors.New("not a self-test camera")
	}
	var fixtures []selftest.Fixture
	if err := json.Unmarshal(st.Fixtures, &fixtures); err != nil {
		return selftest.Report{}, err
	}
	return selftest.Run(ctx, &selfTestEnv{r: r}, fixtures), nil
}

// selfTestEnv is the camera as the replay reaches it.
type selfTestEnv struct{ r *Runtime }

func (e *selfTestEnv) URL(instance string) (string, bool) {
	e.r.mu.Lock()
	defer e.r.mu.Unlock()
	for _, re := range e.r.running {
		if re.instance != instance {
			continue
		}
		for _, ep := range re.endpoints {
			if ep.Network == "tcp" {
				return "http://127.0.0.1:" + strconv.Itoa(ep.Port), true
			}
		}
	}
	return "", false
}

func (e *selfTestEnv) DefaultHTTP() string {
	for _, inst := range profile.SortedKeys(e.r.model.Doc.Engines) {
		if name, _, _ := profile.EngineName(e.r.model.Doc.Engines[inst]); name == "http-api" {
			return inst
		}
	}
	return ""
}

func (e *selfTestEnv) Routes(instance string) []string {
	return profile.RouteIDs(e.r.model.Doc.Engines[instance])
}

func (e *selfTestEnv) Credentials() (string, string) {
	users := e.r.accounts.List()
	for _, u := range users {
		if u.Role == "admin" {
			return u.Username, u.Password
		}
	}
	if len(users) > 0 {
		return users[0].Username, users[0].Password
	}
	return "", ""
}

func (e *selfTestEnv) Serial() string { return e.r.identity.Serial }

func (e *selfTestEnv) Trigger(ctx context.Context, t selftest.Trigger) error {
	e.r.sink.drain()
	tr := ipc.Trigger{Type: t.Type, Direction: t.Direction, Custom: t.Custom}
	v := e.r.vca
	v.mu.Lock()
	if v.caps.RuleTypeFor(t.Type) != "" {
		for _, r := range v.rules {
			if r.Enabled && r.Reports(t.Type) && (t.Rule == "" || r.Name == t.Rule) {
				tr.Rule = &r
				break
			}
		}
	}
	v.mu.Unlock()
	if t.Rule != "" && tr.Rule == nil {
		return errors.New("the camera has no enabled rule " + t.Rule + " that reports " + t.Type + " events")
	}
	if t.Object != nil {
		tr.Object = &engine.Object{}
		if err := remarshal(t.Object, tr.Object); err != nil {
			return err
		}
	}
	if t.Plate != nil {
		tr.Plate = &engine.Plate{}
		if err := remarshal(t.Plate, tr.Plate); err != nil {
			return err
		}
	}
	_, err := e.r.trigger(ctx, &tr)
	return err
}

func remarshal(from, to any) error {
	b, err := json.Marshal(from)
	if err != nil {
		return err
	}
	return json.Unmarshal(b, to)
}

func (e *selfTestEnv) Captures(transport string) bool { return transport == engine.TransportHTTPPush }

func (e *selfTestEnv) Sent(ctx context.Context, _ string) (selftest.Answer, error) {
	select {
	case a := <-e.r.sink.got:
		return a, nil
	case <-ctx.Done():
		return selftest.Answer{}, errors.New("the camera sent nothing")
	}
}
