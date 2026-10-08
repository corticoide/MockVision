package app

import (
	"bytes"
	"context"
	"encoding/csv"
	"encoding/json"
	"fmt"
	"log/slog"
	"math"
	"sort"
	"strconv"
	"strings"
	"time"

	"github.com/corticoide/mockvision/backend/internal/domain"
	"github.com/corticoide/mockvision/backend/internal/ipc"
	"github.com/corticoide/mockvision/backend/internal/store"
	"github.com/corticoide/mockvision/backend/internal/store/db"
	"github.com/corticoide/mockvision/backend/internal/telemetry"
)

// Retention of the diagnostics (D92): metrics every 10 s for a day and
// every minute for a week, past the ten minutes the service keeps in
// memory; what cameras served, a minute at a time, for a week.
const (
	statsRetention      = 7 * 24 * time.Hour
	gapsRetention       = 30 * 24 * time.Hour
	logsRetention       = 7 * 24 * time.Hour
	logsPerCamera       = 2000
	metrics10sRetention = 24 * time.Hour
	metrics1mRetention  = 7 * 24 * time.Hour
	// maxStatRows bounds a camera's report.
	maxStatRows = 4096
)

// recordStats stores what a camera served since its last report. What is
// out of bounds is dropped: a camera cannot fill the database (audit B1).
func (s *Service) recordStats(ctx context.Context, id string, st ipc.RequestStats) {
	if n := len(st.Requests) + len(st.Connections) + len(st.Gaps); n > maxStatRows {
		s.log.Warn("dropped a camera's statistics: too many rows", "camera", id, "rows", n)
		return
	}
	now := time.Now()
	minuteOK := func(m int64) bool {
		t := time.UnixMilli(m)
		return m%60000 == 0 && t.After(now.Add(-time.Hour)) && t.Before(now.Add(time.Minute))
	}
	clampMS := func(v float64) float64 {
		if math.IsNaN(v) || v < 0 {
			return 0
		}
		return min(v, 3600e3)
	}
	err := s.store.Tx(ctx, func(q *db.Queries) error {
		for _, r := range st.Requests {
			if !minuteOK(r.Minute) || r.Count <= 0 || r.Count > 1e7 {
				continue
			}
			if err := q.UpsertRequestStat(ctx, db.UpsertRequestStatParams{CameraID: id, Minute: r.Minute, ClientIp: truncate(r.ClientIP, 64),
				Route: truncate(r.Route, 128), Count: int64(r.Count), Errors: int64(min(max(r.Errors, 0), r.Count)),
				AuthFailures: int64(min(max(r.AuthFailures, 0), r.Count)), P50Ms: clampMS(r.P50MS), P95Ms: clampMS(r.P95MS), MaxMs: clampMS(r.MaxMS),
				FirstAt: r.FirstAt, LastAt: max(r.LastAt, r.FirstAt)}); err != nil {
				return err
			}
		}
		for _, c := range st.Connections {
			if !minuteOK(c.Minute) || c.Opened < 0 || c.Closed < 0 || c.Opened+c.Closed > 1e7 {
				continue
			}
			if err := q.UpsertConnectionStat(ctx, db.UpsertConnectionStatParams{CameraID: id, Minute: c.Minute, Protocol: truncate(c.Protocol, 16),
				ClientIp: truncate(c.ClientIP, 64), Opened: int64(c.Opened), Closed: int64(c.Closed), DurationMs: max(c.DurationMS, 0),
				MaxDurationMs: max(c.MaxDurationMS, 0)}); err != nil {
				return err
			}
		}
		for _, g := range st.Gaps {
			if g.Count <= 0 || g.Count > 1e7 || g.Summary == "" {
				continue
			}
			at := g.At
			if t := time.UnixMilli(at); t.After(now.Add(time.Minute)) || t.Before(now.Add(-time.Hour)) {
				at = now.UnixMilli()
			}
			if err := q.UpsertGap(ctx, db.UpsertGapParams{CameraID: id, Protocol: truncate(g.Protocol, 16), Summary: truncate(g.Summary, 512),
				ClientIp: truncate(g.ClientIP, 64), Count: int64(g.Count), At: at}); err != nil {
				return err
			}
		}
		return nil
	})
	if err != nil && ctx.Err() == nil {
		s.log.Warn("cannot store a camera's statistics", "camera", id, "error", err)
	}
}

// recordLog keeps a line of a camera's log: one the camera sent, or what
// the service did with it.
func (s *Service) recordLog(ctx context.Context, id, source string, level slog.Level, msg string, attrs map[string]any) {
	raw, _ := json.Marshal(attrs)
	if attrs == nil || len(raw) > 4096 {
		raw = []byte("{}")
	}
	lv := level.String()
	if lv != "DEBUG" && lv != "INFO" && lv != "WARN" && lv != "ERROR" {
		lv = "INFO"
	}
	if err := s.store.W().InsertCameraLog(ctx, db.InsertCameraLogParams{CameraID: id, At: time.Now().UnixMilli(), Level: lv, Source: source,
		Msg: truncate(msg, 1024), AttrsJson: string(raw)}); err != nil && !store.IsForeignKey(err) && ctx.Err() == nil {
		s.log.Warn("cannot store a camera's log", "camera", id, "error", err)
	}
}

// metricsLoop keeps the cameras' metrics past the memory window: every
// 10 s the mean of the samples of each camera, and every minute too
// (D92).
func (s *Service) metricsLoop(ctx context.Context) {
	for {
		now := time.Now()
		next := now.Truncate(10 * time.Second).Add(10*time.Second + 500*time.Millisecond)
		t := time.NewTimer(next.Sub(now))
		select {
		case <-ctx.Done():
			t.Stop()
			return
		case <-t.C:
		}
		s.persistMetrics(ctx, time.Now())
	}
}

// persistMetrics stores the buckets that ended by now.
func (s *Service) persistMetrics(ctx context.Context, now time.Time) {
	end := now.Truncate(10 * time.Second)
	type row struct {
		id  string
		res int64
		m   telemetry.CameraSample
	}
	var rows []row
	for _, id := range s.metrics.IDs() {
		if m, ok := meanSample(s.metrics.History(id, end.Add(-10*time.Second)), end); ok {
			rows = append(rows, row{id, 10, m})
		}
		if end.Equal(end.Truncate(time.Minute)) {
			if m, ok := meanSample(s.metrics.History(id, end.Add(-time.Minute)), end); ok {
				rows = append(rows, row{id, 60, m})
			}
		}
	}
	if len(rows) == 0 {
		return
	}
	err := s.store.Tx(ctx, func(q *db.Queries) error {
		for _, r := range rows {
			err := q.InsertCameraMetric(ctx, db.InsertCameraMetricParams{CameraID: r.id, Resolution: r.res, At: end.UnixMilli(), CpuPercent: r.m.CPUPercent,
				RssBytes: int64(r.m.RSSBytes), Clients: int64(r.m.Clients), BytesIn: int64(r.m.BytesIn), BytesOut: int64(r.m.BytesOut), Requests: int64(r.m.Requests)})
			if err != nil && !store.IsForeignKey(err) {
				return err
			}
		}
		return nil
	})
	if err != nil && ctx.Err() == nil {
		s.log.Warn("cannot store the cameras' metrics", "error", err)
	}
}

// meanSample is the mean of the samples before end: CPU, memory and
// clients averaged, the counters as they ended.
func meanSample(samples []telemetry.CameraSample, end time.Time) (telemetry.CameraSample, bool) {
	var out telemetry.CameraSample
	n := 0
	var rss, clients float64
	for _, x := range samples {
		if x.At.After(end) {
			continue
		}
		n++
		out.CPUPercent += x.CPUPercent
		rss += float64(x.RSSBytes)
		clients += float64(x.Clients)
		out.BytesIn, out.BytesOut, out.Requests = x.BytesIn, x.BytesOut, x.Requests
	}
	if n == 0 {
		return out, false
	}
	out.At = end
	out.CPUPercent /= float64(n)
	out.RSSBytes = uint64(rss / float64(n))
	out.Clients = int(math.Round(clients / float64(n)))
	return out, true
}

// applyDiagnosticsRetention drops what is past its retention.
func (s *Service) applyDiagnosticsRetention(ctx context.Context, now time.Time) {
	w := s.store.W()
	before := now.Add(-statsRetention).UnixMilli()
	_, _ = w.DeleteRequestStatsBefore(ctx, before)
	_, _ = w.DeleteConnectionStatsBefore(ctx, before)
	_, _ = w.DeleteGapsBefore(ctx, now.Add(-gapsRetention).UnixMilli())
	_, _ = w.DeleteCameraLogsBefore(ctx, now.Add(-logsRetention).UnixMilli())
	_, _ = w.DeleteCameraMetricsBefore(ctx, db.DeleteCameraMetricsBeforeParams{Resolution: 10, Before: now.Add(-metrics10sRetention).UnixMilli()})
	_, _ = w.DeleteCameraMetricsBefore(ctx, db.DeleteCameraMetricsBeforeParams{Resolution: 60, Before: now.Add(-metrics1mRetention).UnixMilli()})
	ids, err := s.store.R().ListCameraIDs(ctx)
	if err != nil {
		return
	}
	for _, id := range ids {
		_, _ = w.TrimCameraLogs(ctx, db.TrimCameraLogsParams{CameraID: id, Keep: logsPerCamera})
	}
}

// --- Views ---

// MetricsRanges are the windows CameraMetricsRange answers, and the
// resolution of each.
var MetricsRanges = map[string]struct {
	window     time.Duration
	resolution int64 // 0: the memory, a sample a second
}{
	"10m": {10 * time.Minute, 0},
	"1h":  {time.Hour, 10},
	"24h": {24 * time.Hour, 10},
	"7d":  {7 * 24 * time.Hour, 60},
}

// CameraMetricsRange returns a camera's samples over a window: the last
// ten minutes from memory, longer ones from the database (D92).
func (s *Service) CameraMetricsRange(ctx context.Context, id, rng string) ([]telemetry.CameraSample, error) {
	r, ok := MetricsRanges[rng]
	if !ok {
		return nil, domain.Invalid("range", "must be 10m, 1h, 24h or 7d")
	}
	since := time.Now().Add(-r.window)
	if r.resolution == 0 {
		return s.CameraMetrics(ctx, id, since)
	}
	if _, err := s.store.R().GetCamera(ctx, id); err != nil {
		return nil, store.NotFound(err)
	}
	rows, err := s.store.R().ListCameraMetrics(ctx, db.ListCameraMetricsParams{CameraID: id, Resolution: r.resolution, Since: since.UnixMilli()})
	if err != nil {
		return nil, err
	}
	out := make([]telemetry.CameraSample, 0, len(rows))
	for _, m := range rows {
		out = append(out, telemetry.CameraSample{At: time.UnixMilli(m.At), CPUPercent: m.CpuPercent, RSSBytes: uint64(m.RssBytes),
			Clients: int(m.Clients), BytesIn: uint64(m.BytesIn), BytesOut: uint64(m.BytesOut), Requests: uint64(m.Requests)})
	}
	return out, nil
}

// RouteStat is what a camera served on a route to a client over a window.
type RouteStat struct {
	Route        string  `json:"route"`
	ClientIP     string  `json:"client_ip"`
	Count        int64   `json:"count"`
	Errors       int64   `json:"errors"`
	AuthFailures int64   `json:"auth_failures"`
	P50MS        float64 `json:"p50_ms"`
	P95MS        float64 `json:"p95_ms"`
	MaxMS        float64 `json:"max_ms"`
	// IntervalMS is how often the client asks, on average.
	IntervalMS float64   `json:"interval_ms"`
	FirstAt    time.Time `json:"first_at"`
	LastAt     time.Time `json:"last_at"`
}

// MinuteStat is what a camera served in a minute.
type MinuteStat struct {
	Minute       time.Time `json:"minute"`
	Count        int64     `json:"count"`
	Errors       int64     `json:"errors"`
	AuthFailures int64     `json:"auth_failures"`
}

// RequestsView is what a camera served over a window.
type RequestsView struct {
	Since   time.Time    `json:"since"`
	Routes  []RouteStat  `json:"routes"`
	Minutes []MinuteStat `json:"minutes"`
}

// StatsWindows are the windows of the request and client views.
var StatsWindows = map[string]time.Duration{"1h": time.Hour, "24h": 24 * time.Hour, "7d": 7 * 24 * time.Hour}

func statsSince(window string) (time.Time, error) {
	d, ok := StatsWindows[window]
	if !ok {
		return time.Time{}, domain.Invalid("window", "must be 1h, 24h or 7d")
	}
	return time.Now().Add(-d).Truncate(time.Minute), nil
}

func (s *Service) requestRows(ctx context.Context, id string, since time.Time) ([]db.RequestStat, error) {
	if _, err := s.store.R().GetCamera(ctx, id); err != nil {
		return nil, store.NotFound(err)
	}
	return s.store.R().ListRequestStats(ctx, db.ListRequestStatsParams{CameraID: id, Since: since.UnixMilli()})
}

// routeStats adds up the minutes of each route and client.
func routeStats(rows []db.RequestStat) []RouteStat {
	type acc struct {
		RouteStat
		p50w float64
	}
	by := map[[2]string]*acc{}
	for _, r := range rows {
		k := [2]string{r.Route, r.ClientIp}
		a := by[k]
		if a == nil {
			a = &acc{RouteStat: RouteStat{Route: r.Route, ClientIP: r.ClientIp, FirstAt: time.UnixMilli(r.FirstAt), LastAt: time.UnixMilli(r.LastAt)}}
			by[k] = a
		}
		a.Count += r.Count
		a.Errors += r.Errors
		a.AuthFailures += r.AuthFailures
		a.p50w += r.P50Ms * float64(r.Count)
		a.P95MS = max(a.P95MS, r.P95Ms)
		a.MaxMS = max(a.MaxMS, r.MaxMs)
		if t := time.UnixMilli(r.FirstAt); t.Before(a.FirstAt) {
			a.FirstAt = t
		}
		if t := time.UnixMilli(r.LastAt); t.After(a.LastAt) {
			a.LastAt = t
		}
	}
	out := make([]RouteStat, 0, len(by))
	for _, a := range by {
		if a.Count > 0 {
			a.P50MS = a.p50w / float64(a.Count)
		}
		if a.Count > 1 {
			a.IntervalMS = float64(a.LastAt.Sub(a.FirstAt).Milliseconds()) / float64(a.Count-1)
		}
		out = append(out, a.RouteStat)
	}
	sort.Slice(out, func(i, j int) bool {
		if out[i].Count != out[j].Count {
			return out[i].Count > out[j].Count
		}
		return out[i].Route+out[i].ClientIP < out[j].Route+out[j].ClientIP
	})
	return out
}

// CameraRequests returns what a camera served over a window, by route and
// client, and a minute at a time.
func (s *Service) CameraRequests(ctx context.Context, id, window string) (*RequestsView, error) {
	since, err := statsSince(window)
	if err != nil {
		return nil, err
	}
	rows, err := s.requestRows(ctx, id, since)
	if err != nil {
		return nil, err
	}
	v := &RequestsView{Since: since, Routes: routeStats(rows), Minutes: []MinuteStat{}}
	byMinute := map[int64]*MinuteStat{}
	for _, r := range rows {
		m := byMinute[r.Minute]
		if m == nil {
			m = &MinuteStat{Minute: time.UnixMilli(r.Minute)}
			byMinute[r.Minute] = m
		}
		m.Count += r.Count
		m.Errors += r.Errors
		m.AuthFailures += r.AuthFailures
	}
	for _, m := range byMinute {
		v.Minutes = append(v.Minutes, *m)
	}
	sort.Slice(v.Minutes, func(i, j int) bool { return v.Minutes[i].Minute.Before(v.Minutes[j].Minute) })
	return v, nil
}

// ExportRequests writes a camera's requests over a window, a minute at a
// time, as CSV or JSON.
func (s *Service) ExportRequests(ctx context.Context, id, window, format string) ([]byte, error) {
	since, err := statsSince(window)
	if err != nil {
		return nil, err
	}
	rows, err := s.requestRows(ctx, id, since)
	if err != nil {
		return nil, err
	}
	type item struct {
		Minute       time.Time `json:"minute"`
		ClientIP     string    `json:"client_ip"`
		Route        string    `json:"route"`
		Count        int64     `json:"count"`
		Errors       int64     `json:"errors"`
		AuthFailures int64     `json:"auth_failures"`
		P50MS        float64   `json:"p50_ms"`
		P95MS        float64   `json:"p95_ms"`
		MaxMS        float64   `json:"max_ms"`
	}
	items := make([]item, 0, len(rows))
	for _, r := range rows {
		items = append(items, item{time.UnixMilli(r.Minute).UTC(), r.ClientIp, r.Route, r.Count, r.Errors, r.AuthFailures, r.P50Ms, r.P95Ms, r.MaxMs})
	}
	switch format {
	case "json":
		return json.MarshalIndent(map[string]any{"camera_id": id, "since": since.UTC(), "requests": items}, "", "  ")
	case "csv":
		var buf bytes.Buffer
		w := csv.NewWriter(&buf)
		_ = w.Write([]string{"minute", "client_ip", "route", "count", "errors", "auth_failures", "p50_ms", "p95_ms", "max_ms"})
		f := func(v float64) string { return strconv.FormatFloat(v, 'f', 2, 64) }
		for _, it := range items {
			// A spreadsheet must not take a route for a formula.
			route := it.Route
			if strings.ContainsAny(route[:min(1, len(route))], "=+-@") {
				route = "'" + route
			}
			_ = w.Write([]string{it.Minute.Format(time.RFC3339), it.ClientIP, route, strconv.FormatInt(it.Count, 10), strconv.FormatInt(it.Errors, 10),
				strconv.FormatInt(it.AuthFailures, 10), f(it.P50MS), f(it.P95MS), f(it.MaxMS)})
		}
		w.Flush()
		return buf.Bytes(), w.Error()
	}
	return nil, domain.Invalid("format", "must be csv or json")
}

// ClientView is a client of a camera over a window: what it asks, how
// often, how long it stays and what fails for it.
type ClientView struct {
	IP        string    `json:"ip"`
	Protocols []string  `json:"protocols"`
	Connected bool      `json:"connected"`
	FirstSeen time.Time `json:"first_seen"`
	LastSeen  time.Time `json:"last_seen"`
	// Connections counts the connections it opened; a client that keeps
	// reconnecting opens many.
	Connections   int64       `json:"connections"`
	MeanSessionMS int64       `json:"mean_session_ms"`
	MaxSessionMS  int64       `json:"max_session_ms"`
	Requests      int64       `json:"requests"`
	Errors        int64       `json:"errors"`
	AuthFailures  int64       `json:"auth_failures"`
	Gaps          int64       `json:"gaps"`
	Routes        []RouteStat `json:"routes"`
}

// CameraClients returns the clients of a camera over a window, the most
// recent first.
func (s *Service) CameraClients(ctx context.Context, id, window string) ([]ClientView, error) {
	since, err := statsSince(window)
	if err != nil {
		return nil, err
	}
	rows, err := s.requestRows(ctx, id, since)
	if err != nil {
		return nil, err
	}
	conns, err := s.store.R().ListConnectionStats(ctx, db.ListConnectionStatsParams{CameraID: id, Since: since.UnixMilli()})
	if err != nil {
		return nil, err
	}
	gaps, err := s.store.R().ListGaps(ctx, id)
	if err != nil {
		return nil, err
	}
	by := map[string]*ClientView{}
	get := func(ip string, at time.Time) *ClientView {
		c := by[ip]
		if c == nil {
			c = &ClientView{IP: ip, Protocols: []string{}, Routes: []RouteStat{}, FirstSeen: at, LastSeen: at}
			by[ip] = c
		}
		if at.Before(c.FirstSeen) {
			c.FirstSeen = at
		}
		if at.After(c.LastSeen) {
			c.LastSeen = at
		}
		return c
	}
	closed := map[string]int64{}
	durations := map[string]int64{}
	for _, cs := range conns {
		c := get(cs.ClientIp, time.UnixMilli(cs.Minute))
		if !hasString(c.Protocols, cs.Protocol) {
			c.Protocols = append(c.Protocols, cs.Protocol)
		}
		c.Connections += cs.Opened
		closed[cs.ClientIp] += cs.Closed
		durations[cs.ClientIp] += cs.DurationMs
		c.MaxSessionMS = max(c.MaxSessionMS, cs.MaxDurationMs)
	}
	for _, r := range routeStats(rows) {
		c := get(r.ClientIP, r.FirstAt)
		get(r.ClientIP, r.LastAt)
		c.Requests += r.Count
		c.Errors += r.Errors
		c.AuthFailures += r.AuthFailures
		c.Routes = append(c.Routes, r)
	}
	for _, g := range gaps {
		if c, ok := by[g.ClientIp]; ok && time.UnixMilli(g.LastAt).After(since) {
			c.Gaps += g.Count
		}
	}
	live := s.liveClients(id)
	out := make([]ClientView, 0, len(by))
	for ip, c := range by {
		if n := closed[ip]; n > 0 {
			c.MeanSessionMS = durations[ip] / n
		}
		c.Connected = live[ip]
		sort.Strings(c.Protocols)
		out = append(out, *c)
	}
	sort.Slice(out, func(i, j int) bool { return out[i].LastSeen.After(out[j].LastSeen) })
	return out, nil
}

// GapView is a request a camera's profile did not know (D79).
type GapView struct {
	Protocol string    `json:"protocol"`
	Summary  string    `json:"summary"`
	ClientIP string    `json:"client_ip"`
	Count    int64     `json:"count"`
	FirstAt  time.Time `json:"first_at"`
	LastAt   time.Time `json:"last_at"`
}

// CameraGaps returns what a camera was asked that its profile does not
// know, the most recent first.
func (s *Service) CameraGaps(ctx context.Context, id string) ([]GapView, error) {
	if _, err := s.store.R().GetCamera(ctx, id); err != nil {
		return nil, store.NotFound(err)
	}
	rows, err := s.store.R().ListGaps(ctx, id)
	if err != nil {
		return nil, err
	}
	out := make([]GapView, 0, len(rows))
	for _, g := range rows {
		out = append(out, GapView{Protocol: g.Protocol, Summary: g.Summary, ClientIP: g.ClientIp, Count: g.Count, FirstAt: time.UnixMilli(g.FirstAt),
			LastAt: time.UnixMilli(g.LastAt)})
	}
	return out, nil
}

// LogView is a line of a camera's log.
type LogView struct {
	ID     int64          `json:"id"`
	At     time.Time      `json:"at"`
	Level  string         `json:"level"`
	Source string         `json:"source"`
	Msg    string         `json:"msg"`
	Attrs  map[string]any `json:"attrs"`
}

// CameraLogs returns a page of a camera's log, the newest first; before
// is the id the previous page ended at.
func (s *Service) CameraLogs(ctx context.Context, id string, before int64, limit int) ([]LogView, error) {
	if _, err := s.store.R().GetCamera(ctx, id); err != nil {
		return nil, store.NotFound(err)
	}
	if before <= 0 {
		before = math.MaxInt64
	}
	if limit <= 0 || limit > 500 {
		limit = 200
	}
	rows, err := s.store.R().ListCameraLogs(ctx, db.ListCameraLogsParams{CameraID: id, Before: before, Lim: int64(limit)})
	if err != nil {
		return nil, err
	}
	out := make([]LogView, 0, len(rows))
	for _, r := range rows {
		v := LogView{ID: r.ID, At: time.UnixMilli(r.At), Level: r.Level, Source: r.Source, Msg: r.Msg, Attrs: map[string]any{}}
		_ = json.Unmarshal([]byte(r.AttrsJson), &v.Attrs)
		out = append(out, v)
	}
	return out, nil
}

// liveClients are the clients connected to a running camera now, as its
// notices said.
func (s *Service) liveClients(id string) map[string]bool {
	s.mu.Lock()
	ss := s.sessions[id]
	s.mu.Unlock()
	out := map[string]bool{}
	if ss == nil {
		return out
	}
	ss.mu.Lock()
	defer ss.mu.Unlock()
	for k := range ss.clients {
		_, ip, _ := strings.Cut(k, "|")
		out[ip] = true
	}
	return out
}

// PrometheusMetrics writes the node's and the cameras' latest metrics in
// Prometheus' text format.
func (s *Service) PrometheusMetrics(ctx context.Context) ([]byte, error) {
	cams, err := s.store.R().ListCameraStates(ctx)
	if err != nil {
		return nil, err
	}
	var b bytes.Buffer
	n := s.node.Latest()
	metric := func(name, help, typ string) { fmt.Fprintf(&b, "# HELP %s %s\n# TYPE %s %s\n", name, help, name, typ) }
	metric("mockvision_node_cpu_percent", "CPU use of the node.", "gauge")
	fmt.Fprintf(&b, "mockvision_node_cpu_percent %g\n", n.CPUPercent)
	metric("mockvision_node_memory_used_bytes", "Memory used on the node.", "gauge")
	fmt.Fprintf(&b, "mockvision_node_memory_used_bytes %d\n", n.MemUsed)
	type cam struct {
		id, name, state string
		sample          telemetry.CameraSample
		ok              bool
	}
	var list []cam
	for _, r := range cams {
		c := cam{id: r.ID, name: r.Name, state: r.State}
		s.mu.Lock()
		ss := s.sessions[r.ID]
		s.mu.Unlock()
		if ss != nil {
			c.state = string(ss.snapshot().state)
		}
		c.sample, c.ok = s.metrics.Latest(r.ID)
		list = append(list, c)
	}
	label := func(c cam) string {
		return fmt.Sprintf(`camera_id=%q,name=%q`, c.id, c.name)
	}
	metric("mockvision_camera_up", "1 when the camera runs.", "gauge")
	for _, c := range list {
		up := 0
		if c.state == string(domain.StateRunning) || c.state == string(domain.StateDegraded) {
			up = 1
		}
		fmt.Fprintf(&b, "mockvision_camera_up{%s,state=%q} %d\n", label(c), c.state, up)
	}
	gauges := []struct {
		name, help, typ string
		v               func(telemetry.CameraSample) float64
	}{
		{"mockvision_camera_cpu_percent", "CPU use of the camera's process.", "gauge", func(x telemetry.CameraSample) float64 { return x.CPUPercent }},
		{"mockvision_camera_memory_bytes", "Resident memory of the camera's process.", "gauge", func(x telemetry.CameraSample) float64 { return float64(x.RSSBytes) }},
		{"mockvision_camera_clients", "Clients connected to the camera.", "gauge", func(x telemetry.CameraSample) float64 { return float64(x.Clients) }},
		{"mockvision_camera_requests_total", "Requests the camera served since it started.", "counter", func(x telemetry.CameraSample) float64 { return float64(x.Requests) }},
		{"mockvision_camera_received_bytes_total", "Bytes the camera received since it started.", "counter", func(x telemetry.CameraSample) float64 { return float64(x.BytesIn) }},
		{"mockvision_camera_sent_bytes_total", "Bytes the camera sent since it started.", "counter", func(x telemetry.CameraSample) float64 { return float64(x.BytesOut) }},
	}
	for _, g := range gauges {
		metric(g.name, g.help, g.typ)
		for _, c := range list {
			if c.ok {
				fmt.Fprintf(&b, "%s{%s} %g\n", g.name, label(c), g.v(c.sample))
			}
		}
	}
	return b.Bytes(), nil
}
