package camera

import (
	"math/rand/v2"
	"slices"
	"strings"
	"time"

	"github.com/corticoide/mockvision/backend/internal/ipc"
)

// What a camera aggregates between two reports (D92): past these, the
// requests of more clients and routes count under "other", and the
// latencies of a route are a sample of them.
const (
	statsMaxKeys    = 512
	statsMaxSamples = 256
	statsMaxGaps    = 256
	// StatsInterval is how often the camera reports what it served.
	StatsInterval = 10 * time.Second
	// statsMaxOpen bounds the open connections of a client it times.
	statsMaxOpen = 1024
)

type reqKey struct{ client, route string }

type reqAgg struct {
	count, errors, authFailures int
	durs                        []float64 // ms, a sample of them
	seen                        int
	max                         float64
	first, last                 time.Time
}

type connKey struct{ protocol, client string }

type connAgg struct {
	opened, closed int
	durMS, maxMS   int64
}

type gapKey struct{ protocol, summary, client string }

type gapAgg struct {
	count int
	at    time.Time
}

// minuteStats is what the camera served in a minute since its last report.
type minuteStats struct {
	reqs  map[reqKey]*reqAgg
	conns map[connKey]*connAgg
}

// stats aggregates the requests, connections and gaps of the camera, a
// minute at a time, until it reports them. Callers hold telemetry.mu.
type stats struct {
	minutes map[int64]*minuteStats
	gaps    map[gapKey]*gapAgg
	open    map[connKey][]time.Time
}

func newStats() *stats {
	return &stats{minutes: map[int64]*minuteStats{}, gaps: map[gapKey]*gapAgg{}, open: map[connKey][]time.Time{}}
}

func (s *stats) minute(now time.Time) *minuteStats {
	m := now.Truncate(time.Minute).UnixMilli()
	ms := s.minutes[m]
	if ms == nil {
		ms = &minuteStats{reqs: map[reqKey]*reqAgg{}, conns: map[connKey]*connAgg{}}
		s.minutes[m] = ms
	}
	return ms
}

// request counts a served request. A route that ends in auth-failed is a
// client whose credentials were refused; an answer of 400 or more is an
// error, but for the 401 of a challenge.
func (s *stats) request(now time.Time, route, client string, status int, dur time.Duration) {
	m := s.minute(now)
	k := reqKey{client: clip(client, 64), route: clip(route, 128)}
	a := m.reqs[k]
	if a == nil {
		if len(m.reqs) >= statsMaxKeys {
			k = reqKey{client: "other", route: "other"}
			a = m.reqs[k]
		}
		if a == nil {
			a = &reqAgg{first: now}
			m.reqs[k] = a
		}
	}
	a.count++
	a.last = now
	switch {
	case strings.HasSuffix(route, "auth-failed"):
		a.authFailures++
	case status >= 400 && status != 401:
		a.errors++
	}
	ms := float64(dur.Microseconds()) / 1000
	a.max = max(a.max, ms)
	a.seen++
	if len(a.durs) < statsMaxSamples {
		a.durs = append(a.durs, ms)
	} else if j := rand.IntN(a.seen); j < statsMaxSamples {
		a.durs[j] = ms
	}
}

// connection counts a connection opening or closing, and times it.
func (s *stats) connection(now time.Time, protocol, client string, connected bool) {
	m := s.minute(now)
	k := connKey{protocol: clip(protocol, 16), client: clip(client, 64)}
	a := m.conns[k]
	if a == nil {
		if len(m.conns) >= statsMaxKeys {
			return
		}
		a = &connAgg{}
		m.conns[k] = a
	}
	if connected {
		a.opened++
		if len(s.open[k]) < statsMaxOpen {
			s.open[k] = append(s.open[k], now)
		}
		return
	}
	a.closed++
	if opens := s.open[k]; len(opens) > 0 {
		d := now.Sub(opens[0]).Milliseconds()
		a.durMS += d
		a.maxMS = max(a.maxMS, d)
		if len(opens) == 1 {
			delete(s.open, k)
		} else {
			s.open[k] = opens[1:]
		}
	}
}

// gap counts a request the profile does not know.
func (s *stats) gap(now time.Time, protocol, client, summary string) {
	k := gapKey{protocol: clip(protocol, 16), summary: clip(summary, 512), client: clip(client, 64)}
	g := s.gaps[k]
	if g == nil {
		if len(s.gaps) >= statsMaxGaps {
			return
		}
		g = &gapAgg{}
		s.gaps[k] = g
	}
	g.count++
	g.at = now
}

// take returns what was aggregated since the last report and starts again;
// nil when there is nothing.
func (s *stats) take() *ipc.RequestStats {
	if len(s.minutes) == 0 && len(s.gaps) == 0 {
		return nil
	}
	out := &ipc.RequestStats{}
	for minute, m := range s.minutes {
		for k, a := range m.reqs {
			slices.Sort(a.durs)
			out.Requests = append(out.Requests, ipc.RequestStat{Minute: minute, ClientIP: k.client, Route: k.route, Count: a.count,
				Errors: a.errors, AuthFailures: a.authFailures, P50MS: percentile(a.durs, 0.5), P95MS: percentile(a.durs, 0.95), MaxMS: a.max,
				FirstAt: a.first.UnixMilli(), LastAt: a.last.UnixMilli()})
		}
		for k, a := range m.conns {
			out.Connections = append(out.Connections, ipc.ConnStat{Minute: minute, Protocol: k.protocol, ClientIP: k.client, Opened: a.opened,
				Closed: a.closed, DurationMS: a.durMS, MaxDurationMS: a.maxMS})
		}
	}
	for k, g := range s.gaps {
		out.Gaps = append(out.Gaps, ipc.GapStat{Protocol: k.protocol, ClientIP: k.client, Summary: k.summary, Count: g.count, At: g.at.UnixMilli()})
	}
	s.minutes = map[int64]*minuteStats{}
	s.gaps = map[gapKey]*gapAgg{}
	return out
}

// percentile of sorted values, by nearest rank.
func percentile(sorted []float64, p float64) float64 {
	if len(sorted) == 0 {
		return 0
	}
	i := int(p*float64(len(sorted))+0.5) - 1
	return sorted[min(max(i, 0), len(sorted)-1)]
}

// clip cuts a string to n bytes.
func clip(s string, n int) string {
	if len(s) > n {
		return s[:n]
	}
	return s
}
