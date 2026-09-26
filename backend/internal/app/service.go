// Package app holds MockVision's use cases: it creates and supervises
// cameras, imports profiles, records events and enforces the business rules
// on every change, whether it comes from the panel, the API or a camera's
// emulated API.
package app

import (
	"context"
	"log/slog"
	"os"
	"path/filepath"
	"runtime"
	"sync"
	"time"

	"github.com/corticoide/mockvision/backend/internal/domain"
	"github.com/corticoide/mockvision/backend/internal/engines"
	"github.com/corticoide/mockvision/backend/internal/media"
	"github.com/corticoide/mockvision/backend/internal/netctl"
	"github.com/corticoide/mockvision/backend/internal/secret"
	"github.com/corticoide/mockvision/backend/internal/store"
	"github.com/corticoide/mockvision/backend/internal/telemetry"
	"github.com/corticoide/mockvision/backend/internal/worker"
)

// Options configure the service.
type Options struct {
	DataDir string
	FFmpeg  string
	// Exe is the MockVision binary, used to validate packages in a
	// subprocess; empty validates in-process.
	Exe string
	// ParentInterface overrides the default parent NIC of cameras.
	ParentInterface string
	// Listen is the address of the panel and the API, shown on the
	// dashboard (D63).
	Listen  string
	Runtime netctl.Runtime
	Log     *slog.Logger
}

// Publisher pushes live updates to the panel (WebSocket topics). Forget
// drops what a topic keeps for reconnecting clients, once nothing more will
// be published on it.
type Publisher interface {
	Publish(topic, typ string, data any)
	Forget(topic string)
}

type nopPublisher struct{}

func (nopPublisher) Publish(string, string, any) {}
func (nopPublisher) Forget(string)               {}

// Service is the main service.
type Service struct {
	opts    Options
	log     *slog.Logger
	store   *store.Store
	box     *secret.Box
	lib     *media.Library
	catalog *engines.Catalog
	rt      netctl.Runtime
	pub     Publisher
	node    *telemetry.NodeSampler
	metrics *telemetry.Cameras
	login   *loginGuard
	hashes  *hashLimiter
	jobs    *worker.Runner

	mu       sync.Mutex
	sessions map[string]*session
	retries  map[string]*retryState
	encodes  map[string]*refMutex
	opLocks  map[string]*sync.Mutex
	exits    map[string]netctl.Exit
	regens   map[string]*regenState

	// closing is set, under mu, when shutdown begins: no background work
	// starts after that.
	closing bool

	setupMu   sync.Mutex
	setupCode string
	builtinMu sync.Mutex

	baseCtx context.Context
	cancel  context.CancelFunc
	wg      sync.WaitGroup
}

type retryState struct {
	attempts int
	timer    *time.Timer
}

// New builds the service on an open store.
func New(opts Options, st *store.Store, pub Publisher) (*Service, error) {
	if opts.Log == nil {
		opts.Log = slog.Default()
	}
	if pub == nil {
		pub = nopPublisher{}
	}
	if err := os.MkdirAll(opts.DataDir, 0o755); err != nil {
		return nil, err
	}
	// Cameras read renditions with their own user, so they must be able to
	// traverse the data directory; what it holds stays private.
	if fi, err := os.Stat(opts.DataDir); err == nil && fi.Mode().Perm()&0o011 != 0o011 {
		_ = os.Chmod(opts.DataDir, fi.Mode().Perm()|0o711)
	}
	box, err := secret.LoadOrCreate(filepath.Join(opts.DataDir, "node.key"))
	if err != nil {
		return nil, err
	}
	lib, err := media.NewLibrary(opts.DataDir, opts.FFmpeg)
	if err != nil {
		return nil, err
	}
	if runtime.GOOS == "linux" {
		// FFmpeg decodes uploaded images: it runs confined to the image
		// and the rendition it writes (audit B9).
		lib.Sandbox = opts.Exe
	}
	for _, dir := range []string{"packages", "jobs"} {
		if err := os.MkdirAll(filepath.Join(opts.DataDir, dir), 0o750); err != nil {
			return nil, err
		}
	}
	ctx, cancel := context.WithCancel(context.Background())
	s := &Service{
		opts:     opts,
		log:      opts.Log,
		store:    st,
		box:      box,
		lib:      lib,
		catalog:  engines.Builtin(),
		rt:       opts.Runtime,
		pub:      pub,
		node:     telemetry.NewNodeSampler(),
		metrics:  telemetry.NewCameras(),
		login:    newLoginGuard(),
		hashes:   newHashLimiter(),
		sessions: map[string]*session{},
		retries:  map[string]*retryState{},
		encodes:  map[string]*refMutex{},
		opLocks:  map[string]*sync.Mutex{},
		exits:    map[string]netctl.Exit{},
		regens:   map[string]*regenState{},
		baseCtx:  ctx,
		cancel:   cancel,
	}
	s.jobs = s.newRunner()
	return s, nil
}

// SetPublisher sets where live updates go.
func (s *Service) SetPublisher(p Publisher) { s.pub = p }

// RuntimeKind says whether cameras run in namespaces or locally.
func (s *Service) RuntimeKind() string { return s.rt.Kind() }

// Run starts the background work: node sampling, the reconciler, runtime
// notices and retention. It blocks until ctx is done, then stops every
// camera.
func (s *Service) Run(ctx context.Context) error {
	s.announceSetup(ctx)
	if err := s.bootCameras(ctx); err != nil {
		return err
	}
	// Jobs cut short by the last stop wait to be resumed (D74); the queue
	// runs before cameras start, since they wait for their renditions.
	if err := s.jobs.Recover(ctx); err != nil {
		return err
	}
	s.goLoop(s.jobs.Run)
	s.measureInterface(ctx)
	s.goLoop(func(ctx context.Context) { s.node.Run(ctx, 2*time.Second) })
	s.goLoop(s.publishNodeMetrics)
	s.goLoop(s.watchExits)
	s.goLoop(s.retentionLoop)
	s.goLoop(func(ctx context.Context) { s.ensureBuiltinAsset(ctx) })
	s.goLoop(s.reconcileLoop)
	<-ctx.Done()
	s.shutdown()
	return nil
}

func (s *Service) goLoop(fn func(context.Context)) {
	s.goBackground(fn)
}

// goBackground runs work that outlives a request, such as re-encoding a
// camera's streams. Shutdown cancels its context and waits for it.
func (s *Service) goBackground(fn func(context.Context)) bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.closing {
		return false
	}
	s.wg.Add(1)
	go func() {
		defer s.wg.Done()
		fn(s.baseCtx)
	}()
	return true
}

// bootCameras applies RN-15: after a restart only cameras with autostart
// want to run, and none is running yet.
func (s *Service) bootCameras(ctx context.Context) error {
	cams, err := s.store.R().ListCameras(ctx)
	if err != nil {
		return err
	}
	now := time.Now()
	for _, c := range cams {
		if b, err := s.loadBundle(ctx, c.ID); err == nil {
			if err := s.completeStreams(ctx, b); err != nil {
				return err
			}
		}
		desired := domain.DesiredStopped
		if store.Bool(c.Autostart) {
			desired = domain.DesiredRunning
		}
		if err := s.setDesired(ctx, c.ID, desired); err != nil {
			return err
		}
		if err := s.saveStatus(ctx, c.ID, domain.StateStopped, "", time.Time{}, now); err != nil {
			return err
		}
	}
	return nil
}

func (s *Service) shutdown() {
	s.mu.Lock()
	s.closing = true
	sessions := make([]*session, 0, len(s.sessions))
	for _, ss := range s.sessions {
		sessions = append(sessions, ss)
	}
	for _, r := range s.retries {
		if r.timer != nil {
			r.timer.Stop()
		}
	}
	s.mu.Unlock()
	var wg sync.WaitGroup
	for _, ss := range sessions {
		wg.Add(1)
		go func(ss *session) {
			defer wg.Done()
			ss.stop("node shutting down")
			<-ss.done
		}(ss)
	}
	wg.Wait()
	s.cancel()
	s.wg.Wait()
}

// opLock serializes lifecycle operations on one camera.
func (s *Service) opLock(id string) *sync.Mutex {
	s.mu.Lock()
	defer s.mu.Unlock()
	m, ok := s.opLocks[id]
	if !ok {
		m = &sync.Mutex{}
		s.opLocks[id] = m
	}
	return m
}

// retentionLoop deletes events, deliveries, sessions, audit entries and
// finished jobs older than their retention (D33, audit 90 days, jobs 30).
func (s *Service) retentionLoop(ctx context.Context) {
	t := time.NewTicker(time.Hour)
	defer t.Stop()
	for {
		s.applyRetention(ctx)
		select {
		case <-ctx.Done():
			return
		case <-t.C:
		}
	}
}

func (s *Service) applyRetention(ctx context.Context) {
	set := s.Settings(ctx)
	now := time.Now()
	eventsBefore := now.AddDate(0, 0, -set.EventsRetentionDays).UnixMilli()
	w := s.store.W()
	if n, err := w.DeleteDeliveriesBefore(ctx, eventsBefore); err == nil && n > 0 {
		s.log.Info("retention: deleted deliveries", "count", n)
	}
	if n, err := w.DeleteEventsBefore(ctx, eventsBefore); err == nil && n > 0 {
		s.log.Info("retention: deleted events", "count", n)
	}
	_, _ = w.DeleteAuditBefore(ctx, now.Add(-AuditRetention).UnixMilli())
	if n, err := s.jobs.Prune(ctx, now.Add(-JobRetention)); err == nil && n > 0 {
		s.log.Info("retention: deleted jobs", "count", n)
	}
	_, _ = w.DeleteExpiredSessions(ctx, now.UnixMilli())
	s.collectRenditions(ctx, now)
}
