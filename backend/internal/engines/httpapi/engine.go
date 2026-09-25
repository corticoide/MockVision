// Package httpapi implements the http-api engine: a vendor HTTP API whose
// routes, authentication and responses come from the profile. The engine is
// the same for every brand; what changes is the model each profile builds.
package httpapi

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net"
	"net/http"
	"net/url"
	"regexp"
	"sort"
	"strconv"
	"strings"
	"sync/atomic"
	"time"

	"github.com/corticoide/mockvision/sdk/engine"
)

// Name and Version identify the engine in profiles (http-api@^1).
const (
	Name    = "http-api"
	Version = "1.0.0"
)

// MaxBodyBytes bounds request bodies read by the emulated API.
const MaxBodyBytes = 1 << 20

// Engine serves a profile-defined HTTP API for one camera.
type Engine struct {
	in   engine.StartInput
	cfg  atomic.Pointer[compiledConfig]
	srv  *http.Server
	done chan struct{}

	state    atomic.Value // engine.HealthState
	clients  atomic.Int64
	requests atomic.Uint64
	errors   atomic.Uint64
	bytesIn  atomic.Uint64
	bytesOut atomic.Uint64
}

// New returns an engine instance.
func New() engine.Engine {
	e := &Engine{}
	e.state.Store(engine.HealthStopped)
	return e
}

// Describe implements engine.Engine.
func (e *Engine) Describe() engine.Descriptor {
	return engine.Descriptor{
		Name:         Name,
		Version:      Version,
		Contract:     engine.Contract,
		Role:         engine.RoleServer,
		ConfigSchema: json.RawMessage(configSchema),
		Sockets:      []engine.SocketSpec{{Name: "http", Network: "tcp", DefaultPort: 80}},
	}
}

// Validate implements engine.Engine.
func (e *Engine) Validate(config json.RawMessage) []engine.Problem {
	return validate(config)
}

type compiledConfig struct {
	cfg     *Config
	routes  []*compiledRoute
	unknown *compiledAction
	auth    *authenticator
}

type compiledRoute struct {
	route    Route
	segments []string
	query    map[string]matcher
	headers  map[string]matcher
	action   *compiledAction
}

type compiledAction struct {
	a    Action
	body engine.Template
	then *compiledAction
}

type matcher func(values []string) bool

func newMatcher(spec string) matcher {
	switch {
	case spec == "*":
		return func(v []string) bool { return len(v) > 0 }
	case strings.HasPrefix(spec, "~"):
		re := regexp.MustCompile(spec[1:])
		return func(v []string) bool { return len(v) > 0 && re.MatchString(v[0]) }
	default:
		return func(v []string) bool { return len(v) > 0 && v[0] == spec }
	}
}

func (e *Engine) compile(raw json.RawMessage) (*compiledConfig, error) {
	if probs := validate(raw); hasErrors(probs) {
		for _, p := range probs {
			if !p.Warning {
				return nil, fmt.Errorf("%s: %s", p.Path, p.Message)
			}
		}
	}
	c, err := parseConfig(raw)
	if err != nil {
		return nil, err
	}
	cc := &compiledConfig{cfg: c}
	// Accounts are read on every request: an edited password applies at
	// once.
	users := func() []engine.User {
		if e.in.Host == nil {
			return nil
		}
		return e.in.Host.Accounts().List()
	}
	cc.auth = newAuthenticator(c.Auth.Scheme, c.Auth.Realm, users)
	for _, r := range c.Routes {
		cr := &compiledRoute{route: r, segments: splitPath(r.Match.Path), query: map[string]matcher{}, headers: map[string]matcher{}}
		for k, v := range r.Match.Query {
			cr.query[k] = newMatcher(v)
		}
		for k, v := range r.Match.Headers {
			cr.headers[http.CanonicalHeaderKey(k)] = newMatcher(v)
		}
		act, err := e.compileAction(r.ID, r.Action)
		if err != nil {
			return nil, err
		}
		cr.action = act
		cc.routes = append(cc.routes, cr)
	}
	if c.Unknown != nil {
		act, err := e.compileAction("unknown", *c.Unknown)
		if err != nil {
			return nil, err
		}
		cc.unknown = act
	}
	return cc, nil
}

func hasErrors(probs []engine.Problem) bool {
	for _, p := range probs {
		if !p.Warning {
			return true
		}
	}
	return false
}

func (e *Engine) compileAction(name string, a Action) (*compiledAction, error) {
	ca := &compiledAction{a: a}
	if a.Body != "" {
		t, err := e.in.Host.Templates().Compile(name, a.Body, 0)
		if err != nil {
			return nil, fmt.Errorf("route %s: %w", name, err)
		}
		ca.body = t
	}
	if a.Then != nil {
		then, err := e.compileAction(name, *a.Then)
		if err != nil {
			return nil, err
		}
		ca.then = then
	}
	return ca, nil
}

// Start implements engine.Engine.
func (e *Engine) Start(ctx context.Context, in engine.StartInput) error {
	e.in = in
	ln := in.Listeners["http"]
	if ln == nil {
		return errors.New("http-api: no listener for socket http")
	}
	cc, err := e.compile(in.Config)
	if err != nil {
		return err
	}
	e.cfg.Store(cc)
	e.srv = &http.Server{
		Handler:           e,
		ReadHeaderTimeout: 10 * time.Second,
		ReadTimeout:       30 * time.Second,
		WriteTimeout:      30 * time.Second,
		IdleTimeout:       60 * time.Second,
		MaxHeaderBytes:    16 << 10,
		ErrorLog:          slog.NewLogLogger(slog.Default().Handler(), slog.LevelDebug),
		ConnState:         e.connState,
	}
	e.done = make(chan struct{})
	go func() {
		defer close(e.done)
		if err := e.srv.Serve(ln); err != nil && !errors.Is(err, http.ErrServerClosed) {
			e.state.Store(engine.HealthFailed)
			in.Host.Telemetry().Log(slog.LevelError, "http-api stopped", "error", err)
		}
	}()
	e.state.Store(engine.HealthOK)
	return nil
}

func (e *Engine) connState(c net.Conn, s http.ConnState) {
	ip := hostOnly(c.RemoteAddr().String())
	switch s {
	case http.StateNew:
		e.clients.Add(1)
		e.in.Host.Telemetry().Client("http", ip, true)
	case http.StateClosed, http.StateHijacked:
		e.clients.Add(-1)
		e.in.Host.Telemetry().Client("http", ip, false)
	}
}

// Reload implements engine.Engine; routes and templates change in place.
func (e *Engine) Reload(_ context.Context, config json.RawMessage) error {
	cc, err := e.compile(config)
	if err != nil {
		return err
	}
	e.in.Config = config
	e.cfg.Store(cc)
	return nil
}

// Health implements engine.Engine.
func (e *Engine) Health() engine.Health {
	return engine.Health{
		State:    e.state.Load().(engine.HealthState),
		Clients:  int(e.clients.Load()),
		Requests: e.requests.Load(),
		Errors:   e.errors.Load(),
		BytesIn:  e.bytesIn.Load(),
		BytesOut: e.bytesOut.Load(),
	}
}

// Stop implements engine.Engine.
func (e *Engine) Stop(ctx context.Context) error {
	if e.srv == nil {
		return nil
	}
	err := e.srv.Shutdown(ctx)
	if err != nil {
		_ = e.srv.Close()
	}
	if e.done != nil {
		<-e.done
	}
	e.state.Store(engine.HealthStopped)
	return err
}

// ServeHTTP authenticates, finds the first matching route and runs it.
func (e *Engine) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	start := time.Now()
	cc := e.cfg.Load()
	e.requests.Add(1)
	clientIP := hostOnly(r.RemoteAddr)
	cw := &countingWriter{ResponseWriter: w, status: http.StatusOK}
	routeID := "unknown"
	defer func() {
		e.bytesOut.Add(uint64(cw.n))
		if cw.status >= 500 {
			e.errors.Add(1)
		}
		e.in.Host.Telemetry().Request(routeID, clientIP, cw.status, time.Since(start))
	}()
	if cc.cfg.Server != "" {
		cw.Header().Set("Server", cc.cfg.Server)
	}

	user, stale := cc.auth.check(r)
	if user == "" {
		routeID = "auth"
		cc.auth.challenge(cw, stale)
		cw.Header().Set("Content-Type", "text/plain; charset=utf-8")
		cw.WriteHeader(http.StatusUnauthorized)
		_, _ = io.WriteString(cw, "401 Unauthorized\n")
		return
	}

	body, err := readBody(r)
	if err != nil {
		http.Error(cw, "413 Request Entity Too Large", http.StatusRequestEntityTooLarge)
		return
	}
	e.bytesIn.Add(uint64(len(body)))

	route, params := cc.match(r)
	data := engine.TemplateData{
		Camera:  e.cameraData(),
		Request: requestData(r, body, clientIP, params),
		Now:     e.now(),
	}
	if route == nil {
		e.in.Host.Telemetry().Gap("http", clientIP, r.Method+" "+summarizeURL(r.URL))
		if cc.unknown != nil {
			e.respond(cw, r, cc.unknown, data)
			return
		}
		http.NotFound(cw, r)
		return
	}
	routeID = route.route.ID
	e.run(cw, r, route, route.action, data)
}

func (e *Engine) cameraData() engine.CameraData {
	id := e.in.Identity
	return engine.CameraData{
		ID: id.CameraID, Name: id.Name, IP: id.IP, MAC: id.MAC, Serial: id.Serial,
		Vendor: id.Vendor, Model: id.Model, Firmware: id.Firmware,
	}
}

// now is the camera clock: node time plus the configured offset.
func (e *Engine) now() time.Time {
	if v, ok := e.in.Host.State().Canon("time.offset"); ok {
		if secs, ok := v.(int64); ok {
			return time.Now().Add(time.Duration(secs) * time.Second)
		}
	}
	return time.Now()
}

func (cc *compiledConfig) match(r *http.Request) (*compiledRoute, map[string]string) {
	segs := splitPath(r.URL.Path)
	query := r.URL.Query()
	for _, cr := range cc.routes {
		if m := cr.route.Match.Method; m != "" && !strings.EqualFold(m, r.Method) {
			continue
		}
		params, ok := matchPath(cr.segments, segs)
		if !ok {
			continue
		}
		matched := true
		for k, m := range cr.query {
			if !m(query[k]) {
				matched = false
				break
			}
		}
		for k, m := range cr.headers {
			if !matched || !m(r.Header.Values(k)) {
				matched = false
				break
			}
		}
		if matched {
			return cr, params
		}
	}
	return nil, nil
}

func splitPath(p string) []string {
	return strings.Split(strings.Trim(p, "/"), "/")
}

func matchPath(pattern, segs []string) (map[string]string, bool) {
	if len(pattern) != len(segs) {
		return nil, false
	}
	var params map[string]string
	for i, p := range pattern {
		if strings.HasPrefix(p, "{") && strings.HasSuffix(p, "}") {
			if segs[i] == "" {
				return nil, false
			}
			if params == nil {
				params = map[string]string{}
			}
			params[p[1:len(p)-1]] = segs[i]
			continue
		}
		if p != segs[i] {
			return nil, false
		}
	}
	return params, true
}

func (e *Engine) run(w *countingWriter, r *http.Request, route *compiledRoute, a *compiledAction, data engine.TemplateData) {
	if a.a.Handler == "" {
		e.respond(w, r, a, data)
		return
	}
	switch a.a.Handler {
	case HandlerSnapshot:
		stream := a.a.Stream
		if stream == "" {
			stream = "main"
		}
		jpeg, err := e.in.Host.Media().Snapshot(stream)
		if err != nil {
			e.fail(w, http.StatusServiceUnavailable, "snapshot unavailable")
			return
		}
		w.Header().Set("Content-Type", "image/jpeg")
		w.Header().Set("Content-Length", strconv.Itoa(len(jpeg)))
		w.Header().Set("Cache-Control", "no-cache")
		_, _ = w.Write(jpeg)
		return
	case HandlerStateGet:
		result, err := e.stateGet(r, a.a)
		if err != nil {
			e.fail(w, http.StatusBadRequest, err.Error())
			return
		}
		data.Result = result
		if a.then == nil {
			w.Header().Set("Content-Type", "text/plain; charset=utf-8")
			for _, kv := range result {
				fmt.Fprintf(w, "%s=%v\n", kv.Key, kv.Value)
			}
			return
		}
	case HandlerStateSet:
		changes, err := e.stateSet(r, route, a.a, data.Request)
		if err != nil {
			e.fail(w, http.StatusBadRequest, err.Error())
			return
		}
		data.Result = changes
		if a.then == nil {
			w.Header().Set("Content-Type", "text/plain; charset=utf-8")
			_, _ = io.WriteString(w, "OK\n")
			return
		}
	}
	e.respond(w, r, a.then, data)
}

// KV is a parameter exposed to templates as .Result of state.get.
type KV struct {
	Key   string
	Value any
}

func (e *Engine) stateGet(r *http.Request, a Action) ([]KV, error) {
	keyParam := a.Key
	if keyParam == "" {
		keyParam = "name"
	}
	var raw string
	switch a.From {
	case "form":
		raw = r.PostFormValue(keyParam)
	default:
		raw = r.URL.Query().Get(keyParam)
	}
	st := e.in.Host.State()
	if strings.TrimSpace(raw) == "" {
		var out []KV
		for _, p := range st.List("") {
			out = append(out, KV{Key: p.Key, Value: p.Value})
		}
		return out, nil
	}
	seen := map[string]bool{}
	var out []KV
	for _, name := range strings.Split(raw, ",") {
		name = strings.TrimSpace(name)
		if name == "" {
			continue
		}
		if v, ok := st.Get(name); ok {
			if !seen[name] {
				out = append(out, KV{Key: name, Value: v})
				seen[name] = true
			}
			continue
		}
		matches := st.List(name + ".")
		if len(matches) == 0 {
			return nil, fmt.Errorf("unknown parameter %s", name)
		}
		for _, p := range matches {
			if !seen[p.Key] {
				out = append(out, KV{Key: p.Key, Value: p.Value})
				seen[p.Key] = true
			}
		}
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Key < out[j].Key })
	return out, nil
}

func (e *Engine) stateSet(r *http.Request, route *compiledRoute, a Action, req *engine.RequestData) ([]engine.Change, error) {
	values := map[string]any{}
	switch a.From {
	case "query":
		for k, v := range r.URL.Query() {
			if _, isMatch := route.route.Match.Query[k]; isMatch || len(v) == 0 {
				continue
			}
			values[k] = v[0]
		}
	case "form":
		form, err := url.ParseQuery(req.Body)
		if err != nil {
			return nil, errors.New("invalid form body")
		}
		for k, v := range form {
			if len(v) > 0 {
				values[k] = v[0]
			}
		}
	case "json":
		if err := json.Unmarshal([]byte(req.Body), &values); err != nil {
			return nil, errors.New("body must be a JSON object")
		}
	}
	if len(values) == 0 {
		return nil, errors.New("no parameters to set")
	}
	origin := engine.Origin{Kind: engine.OriginClient, IP: req.ClientIP, Engine: e.in.Instance}
	changes, err := e.in.Host.State().Set(r.Context(), values, origin)
	if err != nil {
		var se *engine.StateError
		if errors.As(err, &se) {
			keys := make([]string, 0, len(se.Problems))
			for k := range se.Problems {
				keys = append(keys, k)
			}
			sort.Strings(keys)
			parts := make([]string, 0, len(keys))
			for _, k := range keys {
				parts = append(parts, k+": "+se.Problems[k])
			}
			return nil, errors.New(strings.Join(parts, "; "))
		}
		return nil, err
	}
	return changes, nil
}

func (e *Engine) respond(w *countingWriter, r *http.Request, a *compiledAction, data engine.TemplateData) {
	var out []byte
	if a.body != nil {
		b, err := a.body.Render(r.Context(), data)
		if err != nil {
			e.in.Host.Telemetry().Log(slog.LevelWarn, "template error", "route", a.a.Template, "error", err)
			e.fail(w, http.StatusInternalServerError, "template error")
			return
		}
		out = b
	}
	for k, v := range a.a.Headers {
		w.Header().Set(k, v)
	}
	ct := a.a.Type
	if ct == "" && len(out) > 0 {
		ct = "text/plain; charset=utf-8"
	}
	if ct != "" {
		w.Header().Set("Content-Type", ct)
	}
	status := a.a.Status
	if status == 0 {
		status = http.StatusOK
	}
	w.Header().Set("Content-Length", strconv.Itoa(len(out)))
	w.WriteHeader(status)
	if r.Method != http.MethodHead {
		_, _ = w.Write(out)
	}
}

func (e *Engine) fail(w *countingWriter, status int, msg string) {
	w.Header().Set("Content-Type", "text/plain; charset=utf-8")
	w.WriteHeader(status)
	_, _ = io.WriteString(w, "Error: "+msg+"\n")
}

func readBody(r *http.Request) (string, error) {
	if r.Body == nil || r.Method == http.MethodGet || r.Method == http.MethodHead {
		return "", nil
	}
	b, err := io.ReadAll(io.LimitReader(r.Body, MaxBodyBytes+1))
	if err != nil {
		return "", err
	}
	if len(b) > MaxBodyBytes {
		return "", errors.New("body too large")
	}
	return string(b), nil
}

func requestData(r *http.Request, body, clientIP string, params map[string]string) *engine.RequestData {
	q := map[string]string{}
	for k, v := range r.URL.Query() {
		if len(v) > 0 {
			q[k] = v[0]
		}
	}
	h := map[string]string{}
	for k, v := range r.Header {
		if len(v) > 0 && k != "Authorization" {
			h[k] = v[0]
		}
	}
	return &engine.RequestData{Method: r.Method, Path: r.URL.Path, Query: q, Headers: h, Body: body, ClientIP: clientIP, Params: params}
}

// summarizeURL keeps the path and the query keys of an unknown request, so
// gaps are recorded without values that could carry credentials.
func summarizeURL(u *url.URL) string {
	keys := make([]string, 0, len(u.Query()))
	for k := range u.Query() {
		keys = append(keys, k)
	}
	if len(keys) == 0 {
		return u.Path
	}
	sort.Strings(keys)
	return u.Path + "?" + strings.Join(keys, "&")
}

func hostOnly(addr string) string {
	host, _, err := net.SplitHostPort(addr)
	if err != nil {
		return addr
	}
	return host
}

type countingWriter struct {
	http.ResponseWriter
	status int
	n      int
	wrote  bool
}

func (c *countingWriter) WriteHeader(status int) {
	if !c.wrote {
		c.status = status
		c.wrote = true
	}
	c.ResponseWriter.WriteHeader(status)
}

func (c *countingWriter) Write(b []byte) (int, error) {
	c.wrote = true
	n, err := c.ResponseWriter.Write(b)
	c.n += n
	return n, err
}

func sortedKeys(m map[string]string) []string {
	keys := make([]string, 0, len(m))
	for k := range m {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	return keys
}
