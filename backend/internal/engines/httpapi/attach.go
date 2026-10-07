package httpapi

import (
	"context"
	"encoding/json"
	"fmt"
	"log/slog"
	"net/http"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/corticoide/mockvision/backend/internal/engines/delivery"
	"github.com/corticoide/mockvision/backend/internal/profile"
	"github.com/corticoide/mockvision/sdk/engine"
)

// The events.attach handler: a request the client keeps open, on which the
// camera writes its events as they happen, one part each of a
// multipart/x-mixed-replace answer, as Dahua's
// /cgi-bin/eventManager.cgi?action=attach&codes=[All]&heartbeat=5 does:
//
//	--myboundary
//	Content-Type: text/plain
//	Content-Length: 37
//
//	Code=VideoMotion;action=Start;index=0
//
// Events reach it through the attach transport; the profile renders each
// part, and a second one after a while for events that start and stop.

const (
	defaultBoundary = "myboundary"
	maxHeartbeat    = 300 // seconds
	attachQueue     = 64
	maxAttachBody   = 64 << 10
)

// attachParams are the parameters events.attach reads.
var attachParams = map[string]bool{"codes": true, "heartbeat": true}

// AttachSpec is an event's attach transport in the profile: the part it
// writes, and the one that ends it after a while for an event that lasts.
type AttachSpec struct {
	Body string     `json:"body"`
	Stop *AttachEnd `json:"stop,omitempty"`
}

// AttachEnd ends an event that lasts.
type AttachEnd struct {
	After profile.Duration `json:"after"`
	Body  string           `json:"body"`
}

// attachClient is an open attach request.
type attachClient struct {
	codes map[string]bool // vendor names; nil takes every event
	parts chan string
}

// attachHub holds the open attach requests and fans events out to them.
type attachHub struct {
	mu      sync.Mutex
	clients map[*attachClient]struct{}
	stop    chan struct{}
	timers  map[*time.Timer]struct{}
}

func newAttachHub() *attachHub {
	return &attachHub{clients: map[*attachClient]struct{}{}, stop: make(chan struct{}), timers: map[*time.Timer]struct{}{}}
}

func (h *attachHub) add(c *attachClient) {
	h.mu.Lock()
	h.clients[c] = struct{}{}
	h.mu.Unlock()
}

func (h *attachHub) remove(c *attachClient) {
	h.mu.Lock()
	delete(h.clients, c)
	h.mu.Unlock()
}

// send gives a part to every client that takes the code; a client too slow
// to read loses it.
func (h *attachHub) send(code, part string) {
	h.mu.Lock()
	defer h.mu.Unlock()
	for c := range h.clients {
		if c.codes != nil && !c.codes[code] {
			continue
		}
		select {
		case c.parts <- part:
		default:
		}
	}
}

// later sends a part after a while, unless the engine stops first.
func (h *attachHub) later(d time.Duration, fn func()) {
	h.mu.Lock()
	defer h.mu.Unlock()
	select {
	case <-h.stop:
		return
	default:
	}
	var t *time.Timer
	t = time.AfterFunc(d, func() {
		h.mu.Lock()
		delete(h.timers, t)
		h.mu.Unlock()
		fn()
	})
	h.timers[t] = struct{}{}
}

// close ends every attach request and pending part.
func (h *attachHub) close() {
	h.mu.Lock()
	defer h.mu.Unlock()
	select {
	case <-h.stop:
		return
	default:
	}
	close(h.stop)
	for t := range h.timers {
		t.Stop()
	}
}

// usesAttach reports whether a route serves events.attach.
func usesAttach(c *Config) bool {
	for _, r := range c.Routes {
		if r.Action.Handler == HandlerEventsAttach {
			return true
		}
	}
	return false
}

// dispatchAttach renders an event's parts and gives them to the clients.
func (e *Engine) dispatchAttach(d engine.Dispatch) {
	var spec AttachSpec
	if err := json.Unmarshal(d.Transport, &spec); err != nil || spec.Body == "" {
		e.in.Host.Telemetry().Log(slog.LevelWarn, "attach: the event has no body", "type", d.Event.Type)
		return
	}
	data := delivery.Data(e.in.Identity, d)
	render := func(name, text string) (string, bool) {
		ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		out, err := e.attachTemplates.Render(ctx, name, text, maxAttachBody, data)
		if err != nil {
			e.in.Host.Telemetry().Log(slog.LevelWarn, "attach: template error", "type", d.Event.Type, "error", err.Error())
			return "", false
		}
		return string(out), true
	}
	if part, ok := render("attach:"+d.Event.Type, spec.Body); ok {
		e.attachHub.send(d.VendorName, part)
	}
	if spec.Stop != nil && spec.Stop.Body != "" {
		e.attachHub.later(spec.Stop.After.D(), func() {
			if part, ok := render("attach-stop:"+d.Event.Type, spec.Stop.Body); ok {
				e.attachHub.send(d.VendorName, part)
			}
		})
	}
}

// parseCodes reads [All] or [VideoMotion,VideoBlind]; nil takes them all.
func parseCodes(raw string) map[string]bool {
	raw = strings.TrimSpace(strings.Trim(strings.TrimSpace(raw), "[]"))
	if raw == "" || strings.EqualFold(raw, "All") {
		return nil
	}
	out := map[string]bool{}
	for _, c := range strings.Split(raw, ",") {
		if c = strings.TrimSpace(c); c != "" {
			if strings.EqualFold(c, "All") {
				return nil
			}
			out[c] = true
		}
	}
	return out
}

// attach keeps the request open and writes the events the client asked
// for, and a heartbeat every few seconds when asked, until the client
// leaves or the engine stops.
func (e *Engine) attach(w *countingWriter, r *http.Request, a Action, req *engine.RequestData) {
	v, err := params(r, a.From, req)
	if err != nil {
		e.fail(w, http.StatusBadRequest, err.Error())
		return
	}
	var heartbeat time.Duration
	if s := v.Get(paramName(a, "heartbeat")); s != "" {
		n, err := strconv.Atoi(s)
		if err != nil || n < 0 || n > maxHeartbeat {
			e.fail(w, http.StatusBadRequest, fmt.Sprintf("heartbeat must be between 0 and %d seconds", maxHeartbeat))
			return
		}
		heartbeat = time.Duration(n) * time.Second
	}
	boundary := a.Boundary
	if boundary == "" {
		boundary = defaultBoundary
	}
	// The answer lasts as long as the client listens.
	rc := http.NewResponseController(w)
	_ = rc.SetReadDeadline(time.Time{})
	_ = rc.SetWriteDeadline(time.Time{})
	c := &attachClient{codes: parseCodes(v.Get(paramName(a, "codes"))), parts: make(chan string, attachQueue)}
	e.attachHub.add(c)
	defer e.attachHub.remove(c)
	w.Header().Set("Content-Type", "multipart/x-mixed-replace; boundary="+boundary)
	w.Header().Set("Cache-Control", "no-cache")
	w.WriteHeader(http.StatusOK)
	if err := rc.Flush(); err != nil {
		return
	}
	write := func(body string) bool {
		_, err := fmt.Fprintf(w, "--%s\r\nContent-Type: text/plain\r\nContent-Length: %d\r\n\r\n%s\r\n\r\n", boundary, len(body), body)
		return err == nil && rc.Flush() == nil
	}
	var tick <-chan time.Time
	if heartbeat > 0 {
		t := time.NewTicker(heartbeat)
		defer t.Stop()
		tick = t.C
	}
	for {
		select {
		case <-r.Context().Done():
			return
		case <-e.attachHub.stop:
			return
		case part := <-c.parts:
			if !write(part) {
				return
			}
		case <-tick:
			if !write("Heartbeat") {
				return
			}
		}
	}
}
