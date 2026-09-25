package camera

import (
	"context"
	"log/slog"
	"os"
	"runtime"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"syscall"
	"time"

	"github.com/corticoide/mockvision/backend/internal/ipc"
)

// telemetry implements engine.Telemetry: logs, request counters, gaps and
// clients of the camera, forwarded to the service.
type telemetry struct {
	rt       *Runtime
	requests atomic.Uint64

	mu      sync.Mutex
	gaps    map[string]*gapEntry
	clients map[string]int
}

type gapEntry struct {
	count    int
	lastSent time.Time
}

func newTelemetry(rt *Runtime) *telemetry {
	return &telemetry{rt: rt, gaps: map[string]*gapEntry{}, clients: map[string]int{}}
}

func (t *telemetry) Log(level slog.Level, msg string, attrs ...any) {
	t.rt.log.Log(context.Background(), level, msg, attrs...)
	if level < slog.LevelWarn || t.rt.conn == nil {
		return
	}
	m := map[string]any{}
	for i := 0; i+1 < len(attrs); i += 2 {
		k, _ := attrs[i].(string)
		v := attrs[i+1]
		if err, ok := v.(error); ok {
			v = err.Error()
		}
		m[k] = v
	}
	_ = t.rt.conn.Notify(ipc.TypeLog, ipc.Log{Level: level.String(), Msg: msg, Attrs: m})
}

func (t *telemetry) Request(string, string, int, time.Duration) {
	t.requests.Add(1)
}

// Gap reports an unknown request at most once a minute per request shape.
func (t *telemetry) Gap(protocol, clientIP, summary string) {
	key := protocol + " " + summary
	t.mu.Lock()
	g := t.gaps[key]
	if g == nil {
		if len(t.gaps) > 1000 {
			t.gaps = map[string]*gapEntry{}
		}
		g = &gapEntry{}
		t.gaps[key] = g
	}
	g.count++
	send := time.Since(g.lastSent) > time.Minute
	count := g.count
	if send {
		g.lastSent = time.Now()
	}
	t.mu.Unlock()
	if send && t.rt.conn != nil {
		_ = t.rt.conn.Notify(ipc.TypeGap, ipc.Gap{Protocol: protocol, ClientIP: clientIP, Summary: summary, Count: count})
	}
}

func (t *telemetry) Client(protocol, clientIP string, connected bool) {
	key := protocol + "|" + clientIP
	t.mu.Lock()
	before := t.clients[key]
	if connected {
		t.clients[key]++
	} else if before > 0 {
		t.clients[key]--
		if t.clients[key] == 0 {
			delete(t.clients, key)
		}
	}
	after := t.clients[key]
	t.mu.Unlock()
	// Report a client when it first appears and when its last connection
	// closes, not for every keep-alive connection.
	if (before == 0) != (after == 0) && t.rt.conn != nil {
		_ = t.rt.conn.Notify(ipc.TypeClient, ipc.ClientMsg{Protocol: protocol, IP: clientIP, Connected: connected})
	}
}

// cpuSampler measures the CPU used by the process between samples, as a
// percentage of one core.
type cpuSampler struct {
	lastCPU  time.Duration
	lastWall time.Time
}

func (c *cpuSampler) sample() float64 {
	var ru syscall.Rusage
	if err := syscall.Getrusage(syscall.RUSAGE_SELF, &ru); err != nil {
		return 0
	}
	cpu := time.Duration(ru.Utime.Nano() + ru.Stime.Nano())
	now := time.Now()
	defer func() { c.lastCPU, c.lastWall = cpu, now }()
	if c.lastWall.IsZero() {
		return 0
	}
	wall := now.Sub(c.lastWall)
	if wall <= 0 {
		return 0
	}
	return float64(cpu-c.lastCPU) / float64(wall) * 100
}

// rssBytes returns the resident memory of the process.
func rssBytes() uint64 {
	if b, err := os.ReadFile("/proc/self/statm"); err == nil {
		fields := strings.Fields(string(b))
		if len(fields) > 1 {
			if pages, err := strconv.ParseUint(fields[1], 10, 64); err == nil {
				return pages * uint64(os.Getpagesize())
			}
		}
	}
	var ms runtime.MemStats
	runtime.ReadMemStats(&ms)
	return ms.Sys
}
