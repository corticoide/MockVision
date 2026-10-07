// Package delivery runs the attempts of event deliveries for the engines
// that send events out (http-push, mqtt-publish, ftp-upload, smtp-mail):
// a bounded number in flight, each attempt within the device's timeout, a
// pause between attempts, and every attempt reported (RN-13). The policy
// is the profile's, with the target's override (D42).
package delivery

import (
	"context"
	"crypto/tls"
	"errors"
	"fmt"
	"net"
	"net/url"
	"sync"
	"sync/atomic"
	"syscall"
	"time"

	"github.com/corticoide/mockvision/sdk/engine"
)

// DefaultTimeout bounds an attempt when the policy sets none.
const DefaultTimeout = 5 * time.Second

// Attempt is the outcome of one try: Err nil means the target took the
// event. Status is the HTTP status, when the protocol has one.
type Attempt struct {
	Status int
	Bytes  int
	Err    error
}

// Send makes one attempt within ctx, which carries the attempt's timeout.
type Send func(ctx context.Context, attempt int) Attempt

// Runner delivers the events of one engine.
type Runner struct {
	events engine.Events
	sem    chan struct{}
	ctx    context.Context
	cancel context.CancelFunc
	wg     sync.WaitGroup

	delivered atomic.Uint64
	failures  atomic.Uint64
	bytesOut  atomic.Uint64
	inflight  atomic.Int64
}

// NewRunner returns a runner reporting to events with at most parallel
// attempts in flight.
func NewRunner(events engine.Events, parallel int) *Runner {
	if parallel <= 0 {
		parallel = 8
	}
	ctx, cancel := context.WithCancel(context.Background())
	return &Runner{events: events, sem: make(chan struct{}, parallel), ctx: ctx, cancel: cancel}
}

// Context ends when the runner stops.
func (r *Runner) Context() context.Context { return r.ctx }

// Go delivers an event to a target in the background, with the dispatch's
// policy toward that target.
func (r *Runner) Go(d engine.Dispatch, t engine.Target, send Send) {
	r.wg.Add(1)
	go func() {
		defer r.wg.Done()
		r.run(d.Event.ID, t.ID, d.PolicyFor(t), send)
	}()
}

// Fail reports an event that cannot even be tried, for example because its
// template does not render.
func (r *Runner) Fail(d engine.Dispatch, t engine.Target, err error) {
	r.failures.Add(1)
	r.events.Report(engine.DeliveryReport{EventID: d.Event.ID, TargetID: t.ID, Attempt: 1, At: time.Now(), Status: engine.DeliveryFailed, Error: err.Error()})
}

func (r *Runner) run(eventID, targetID string, policy engine.DeliveryPolicy, send Send) {
	if policy.Timeout <= 0 {
		policy.Timeout = DefaultTimeout
	}
	attempts := policy.Retries + 1
	for attempt := 1; attempt <= attempts; attempt++ {
		select {
		case r.sem <- struct{}{}:
		case <-r.ctx.Done():
			return
		}
		r.inflight.Add(1)
		start := time.Now()
		ctx, cancel := context.WithTimeout(r.ctx, policy.Timeout)
		res := send(ctx, attempt)
		timedOut := errors.Is(ctx.Err(), context.DeadlineExceeded)
		cancel()
		latency := time.Since(start)
		r.inflight.Add(-1)
		<-r.sem
		if r.ctx.Err() != nil {
			return // the camera stops: abandoned, not failed
		}

		rep := engine.DeliveryReport{EventID: eventID, TargetID: targetID, Attempt: attempt, At: start, HTTPStatus: res.Status, LatencyMS: latency.Milliseconds()}
		if res.Err == nil {
			rep.Status = engine.DeliveryOK
			r.delivered.Add(1)
			r.bytesOut.Add(uint64(res.Bytes))
			r.events.Report(rep)
			return
		}
		r.failures.Add(1)
		rep.Error = res.Err.Error()
		if timedOut {
			rep.Error = fmt.Sprintf("timeout after %s", policy.Timeout)
		}
		rep.Status = engine.DeliveryFailed
		if attempt < attempts {
			rep.Status = engine.DeliveryRetry
		}
		r.events.Report(rep)
		if attempt < attempts {
			select {
			case <-time.After(policy.Backoff):
			case <-r.ctx.Done():
				return
			}
		}
	}
}

// Stop abandons pending deliveries and waits for those in flight until ctx
// expires.
func (r *Runner) Stop(ctx context.Context) {
	r.cancel()
	done := make(chan struct{})
	go func() { r.wg.Wait(); close(done) }()
	select {
	case <-done:
	case <-ctx.Done():
	}
}

// Health reports the runner's counters with an engine state.
func (r *Runner) Health(state engine.HealthState) engine.Health {
	return engine.Health{
		State:    state,
		Clients:  int(r.inflight.Load()),
		Requests: r.delivered.Load(),
		Errors:   r.failures.Load(),
		BytesOut: r.bytesOut.Load(),
	}
}

// Dialer is how the camera opens connections to its targets: directly,
// never through a proxy of the node.
var Dialer = &net.Dialer{Timeout: 5 * time.Second, KeepAlive: 30 * time.Second}

type dialerKey struct{}

// WithDialer makes the connections opened under ctx use d, as the node's
// connection tests do to refuse the node's own addresses.
func WithDialer(ctx context.Context, d *net.Dialer) context.Context {
	return context.WithValue(ctx, dialerKey{}, d)
}

// offline is set while the camera is off the network: it reaches nobody.
var offline atomic.Bool

// SetOffline takes the camera's outgoing connections off the network, or
// back, as a network_down fault does.
func SetOffline(v bool) { offline.Store(v) }

// Dial opens a connection to a target, with the dialer of ctx if it has one.
func Dial(ctx context.Context, network, address string) (net.Conn, error) {
	if offline.Load() {
		return nil, fmt.Errorf("dial %s %s: %w", network, address, syscall.ENETUNREACH)
	}
	d, ok := ctx.Value(dialerKey{}).(*net.Dialer)
	if !ok {
		d = Dialer
	}
	return d.DialContext(ctx, network, address)
}

// TLSConfig is the client TLS configuration toward a target's host.
// insecure accepts any certificate, as a test server's self-signed one.
func TLSConfig(host string, insecure bool) *tls.Config {
	return &tls.Config{ServerName: host, InsecureSkipVerify: insecure, MinVersion: tls.VersionTLS12}
}

// Unwrap drops the URL that net/http repeats in its errors.
func Unwrap(err error) error {
	var ue *url.Error
	if errors.As(err, &ue) && ue.Err != nil {
		return ue.Err
	}
	return err
}

// Templates compiles the templates a camera's engine renders, once each.
type Templates struct {
	compiler engine.Templates
	mu       sync.Mutex
	compiled map[templateKey]engine.Template
}

type templateKey struct {
	text string
	max  int
}

// NewTemplates returns a cache over the camera's template compiler.
func NewTemplates(c engine.Templates) *Templates {
	return &Templates{compiler: c, compiled: map[templateKey]engine.Template{}}
}

// maxCached bounds the cache: profiles have a few templates per event,
// targets one or two each.
const maxCached = 256

// Render renders text, compiling it on first use. maxBytes bounds the
// output; zero is the compiler's default of 1 MB.
func (c *Templates) Render(ctx context.Context, name, text string, maxBytes int, data engine.TemplateData) ([]byte, error) {
	key := templateKey{text, maxBytes}
	c.mu.Lock()
	t, ok := c.compiled[key]
	c.mu.Unlock()
	if !ok {
		var err error
		if t, err = c.compiler.Compile(name, text, maxBytes); err != nil {
			return nil, err
		}
		c.mu.Lock()
		if len(c.compiled) >= maxCached {
			clear(c.compiled)
		}
		c.compiled[key] = t
		c.mu.Unlock()
	}
	return t.Render(ctx, data)
}

// Data is what an event's templates see: the camera, the event and its
// vendor name.
func Data(id engine.Identity, d engine.Dispatch) engine.TemplateData {
	ev := d.Event
	return engine.TemplateData{
		Camera: engine.CameraData{
			ID: id.CameraID, Name: id.Name, IP: id.IP, MAC: id.MAC,
			Serial: id.Serial, Vendor: id.Vendor, Model: id.Model, Firmware: id.Firmware,
		},
		Event:     &ev,
		EventName: d.VendorName,
		Now:       ev.At,
	}
}
