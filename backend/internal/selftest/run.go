package selftest

import (
	"bytes"
	"context"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"slices"
	"strings"
	"time"

	"github.com/corticoide/mockvision/backend/internal/digest"
)

// Statuses of a fixture.
const (
	Passed  = "passed"
	Failed  = "failed"
	Skipped = "skipped"
)

// Coverage of what a profile declares: a route or an event with a fixture
// that passes is verified; the rest is declared.
const (
	Verified = "verified"
	Declared = "declared"
)

// Timeouts of the replay.
const (
	requestTimeout = 10 * time.Second
	eventTimeout   = 10 * time.Second
	maxAnswer      = 8 << 20
)

// Env is the camera under test as the replay reaches it.
type Env interface {
	// URL is where an engine instance answers HTTP.
	URL(instance string) (string, bool)
	// DefaultHTTP is the instance a fixture without engine goes to.
	DefaultHTTP() string
	// Routes lists the route ids of an instance.
	Routes(instance string) []string
	// Credentials are the camera's administrator, for its challenges.
	Credentials() (user, password string)
	Serial() string
	// Trigger raises an event, forgetting what was sent before.
	Trigger(ctx context.Context, t Trigger) error
	// Captures says whether the replay sees what a transport sends.
	Captures(transport string) bool
	// Sent waits for what a transport sent since the last trigger.
	Sent(ctx context.Context, transport string) (Answer, error)
}

// Result is the outcome of one fixture.
type Result struct {
	ID   string `json:"id"`
	File string `json:"file,omitempty"`
	// Covers is what the fixture verifies: route:<instance>/<id>,
	// request:<method> <path> or event:<type>.
	Covers string `json:"covers"`
	Status string `json:"status"`
	Detail string `json:"detail,omitempty"`
}

// Report is the outcome of a replay.
type Report struct {
	Results []Result `json:"results"`
	Passed  int      `json:"passed"`
	Failed  int      `json:"failed"`
	Skipped int      `json:"skipped"`
}

// Complete reports whether every fixture passed: the self-test a captured
// profile needs.
func (r Report) Complete() bool {
	return len(r.Results) > 0 && r.Passed == len(r.Results)
}

// Run replays the fixtures in order.
func Run(ctx context.Context, env Env, fixtures []Fixture) Report {
	var rep Report
	client := &http.Client{Timeout: requestTimeout, CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }}
	for _, f := range fixtures {
		var res Result
		if f.IsEvent() {
			res = runEvent(ctx, env, f)
		} else {
			res = runRequests(ctx, env, client, f)
		}
		res.ID, res.File = f.ID, f.File
		switch res.Status {
		case Passed:
			rep.Passed++
		case Failed:
			rep.Failed++
		default:
			rep.Skipped++
		}
		rep.Results = append(rep.Results, res)
	}
	return rep
}

// Coverage turns the results into what each route and event of the
// profile has: verified when a fixture covering it passed, failed when one
// failed, declared otherwise.
func Coverage(results []Result, routes map[string][]string, events []string) map[string]string {
	out := map[string]string{}
	for inst, ids := range routes {
		for _, id := range ids {
			out["route:"+inst+"/"+id] = Declared
		}
	}
	for _, e := range events {
		out["event:"+e] = Declared
	}
	for _, r := range results {
		if _, known := out[r.Covers]; !known {
			continue
		}
		switch {
		case r.Status == Failed:
			out[r.Covers] = Failed
		case r.Status == Passed && out[r.Covers] != Failed:
			out[r.Covers] = Verified
		}
	}
	return out
}

func runRequests(ctx context.Context, env Env, client *http.Client, f Fixture) Result {
	inst := f.Engine
	if inst == "" {
		inst = env.DefaultHTTP()
	}
	res := Result{Covers: covers(env, inst, f)}
	base, ok := env.URL(inst)
	if !ok {
		res.Status, res.Detail = Failed, fmt.Sprintf("the camera has no HTTP engine %q", inst)
		return res
	}
	for i, st := range f.sequence() {
		got, err := send(ctx, client, env, base, st.Request)
		if err == nil {
			err = Compare(st.Response, st.Vary, got, env.Serial())
		}
		if err != nil {
			res.Status, res.Detail = Failed, err.Error()
			if len(f.Steps) > 1 {
				res.Detail = fmt.Sprintf("step %d: %v", i+1, err)
			}
			return res
		}
	}
	res.Status = Passed
	return res
}

// covers names what a request fixture verifies.
func covers(env Env, inst string, f Fixture) string {
	route := f.Route
	if route == "" && slices.Contains(env.Routes(inst), f.ID) {
		route = f.ID
	}
	if route != "" {
		return "route:" + inst + "/" + route
	}
	if seq := f.sequence(); len(seq) > 0 {
		return "request:" + seq[0].Request.Method + " " + seq[0].Request.Path
	}
	return "request:" + f.ID
}

// send makes a request, answering the camera's challenge with its
// administrator unless the fixture says not to.
func send(ctx context.Context, client *http.Client, env Env, base string, r *Request) (Answer, error) {
	target := base + r.Path
	if len(r.Query) > 0 {
		q := url.Values{}
		for k, v := range r.Query {
			q.Set(k, v)
		}
		sep := "?"
		if strings.Contains(r.Path, "?") {
			sep = "&"
		}
		target += sep + q.Encode()
	}
	do := func(auth string) (Answer, error) {
		rctx, cancel := context.WithTimeout(ctx, requestTimeout)
		defer cancel()
		req, err := http.NewRequestWithContext(rctx, r.Method, target, strings.NewReader(r.Body))
		if err != nil {
			return Answer{}, err
		}
		for k, v := range r.Headers {
			req.Header.Set(k, v)
		}
		if auth != "" {
			req.Header.Set("Authorization", auth)
		}
		resp, err := client.Do(req)
		if err != nil {
			return Answer{}, fmt.Errorf("the camera did not answer: %v", err)
		}
		defer resp.Body.Close()
		body, err := io.ReadAll(io.LimitReader(resp.Body, maxAnswer))
		if err != nil {
			return Answer{}, err
		}
		return Answer{Status: resp.StatusCode, Headers: resp.Header, Body: body}, nil
	}
	got, err := do("")
	if err != nil || got.Status != http.StatusUnauthorized || (r.Auth != nil && !*r.Auth) {
		return got, err
	}
	user, password := env.Credentials()
	u, _ := url.Parse(target)
	if ch, ok := digest.ParseChallenge(got.Headers.Values("WWW-Authenticate")); ok {
		auth, err := ch.Authorize(r.Method, u.RequestURI(), user, password, []byte(r.Body))
		if err != nil {
			return got, err
		}
		return do(auth)
	}
	for _, h := range got.Headers.Values("WWW-Authenticate") {
		if strings.HasPrefix(strings.ToLower(strings.TrimSpace(h)), "basic") {
			req, _ := http.NewRequest(r.Method, target, bytes.NewReader(nil))
			req.SetBasicAuth(user, password)
			return do(req.Header.Get("Authorization"))
		}
	}
	return got, nil
}

func runEvent(ctx context.Context, env Env, f Fixture) Result {
	res := Result{Covers: "event:" + f.Trigger.Type}
	var unchecked []string
	for _, t := range sortedKeys(f.Expect) {
		if !env.Captures(t) {
			unchecked = append(unchecked, t)
		}
	}
	if len(unchecked) == len(f.Expect) {
		res.Status, res.Detail = Skipped, "the self-test does not see what "+strings.Join(unchecked, " and ")+" sends yet"
		return res
	}
	if err := env.Trigger(ctx, *f.Trigger); err != nil {
		res.Status, res.Detail = Failed, "the event could not be raised: "+err.Error()
		return res
	}
	for _, t := range sortedKeys(f.Expect) {
		if !env.Captures(t) {
			continue
		}
		wctx, cancel := context.WithTimeout(ctx, eventTimeout)
		got, err := env.Sent(wctx, t)
		cancel()
		if err == nil {
			err = CompareSent(f.Expect[t], got, env.Serial())
		}
		if err != nil {
			res.Status, res.Detail = Failed, t+": "+err.Error()
			return res
		}
	}
	if len(unchecked) > 0 {
		res.Status, res.Detail = Skipped, "matched; the self-test does not see what "+strings.Join(unchecked, " and ")+" sends yet"
		return res
	}
	res.Status = Passed
	return res
}
