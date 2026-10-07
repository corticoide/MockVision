package app

import (
	"context"
	"database/sql"
	"encoding/json"
	"fmt"
	"slices"
	"strings"
	"time"

	"github.com/oklog/ulid/v2"

	"github.com/corticoide/mockvision/backend/internal/domain"
	"github.com/corticoide/mockvision/backend/internal/ipc"
	"github.com/corticoide/mockvision/backend/internal/profile"
	"github.com/corticoide/mockvision/backend/internal/store"
	"github.com/corticoide/mockvision/backend/internal/store/db"
	"github.com/corticoide/mockvision/sdk/engine"
)

// FaultInput injects a fault into a camera (D43).
type FaultInput struct {
	Kind      string `json:"kind"`
	Instance  string `json:"instance,omitempty"`
	Status    int    `json:"status,omitempty"`
	LatencyMS int    `json:"latency_ms,omitempty"`
	SkewS     int64  `json:"skew_s,omitempty"`
	// DurationS is how long the fault lasts; 0 lasts until ended by hand
	// (RN-14).
	DurationS int `json:"duration_s"`
}

// FaultView is an injected fault, on or ended.
type FaultView struct {
	ID         string     `json:"id"`
	CameraID   string     `json:"camera_id"`
	CameraName string     `json:"camera_name,omitempty"`
	Kind       string     `json:"kind"`
	Instance   string     `json:"instance,omitempty"`
	Status     int        `json:"status,omitempty"`
	LatencyMS  int        `json:"latency_ms,omitempty"`
	SkewS      int64      `json:"skew_s,omitempty"`
	StartedAt  time.Time  `json:"started_at"`
	ExpiresAt  *time.Time `json:"expires_at"`
	EndedAt    *time.Time `json:"ended_at"`
	// EndedBy is who ended it: expired, replaced, or a user's name.
	EndedBy   string `json:"ended_by,omitempty"`
	CreatedBy string `json:"created_by"`
	Active    bool   `json:"active"`
}

// Reasons of the faults a camera has.
const (
	ReasonFaults = "faults" // degraded by injected faults
	ReasonReboot = "reboot" // rebooting, simulated
)

// faultHistory is how long ended faults are kept.
const faultHistory = 30 * 24 * time.Hour

func faultOf(r db.Fault) domain.Fault {
	f := domain.Fault{ID: r.ID, CameraID: r.CameraID, Kind: domain.FaultKind(r.Kind), StartedAt: store.Time(r.StartedAt), EndedBy: r.EndedBy, CreatedBy: r.CreatedBy}
	_ = json.Unmarshal([]byte(r.ParamsJson), &f.Params)
	if r.ExpiresAt.Valid {
		t := store.Time(r.ExpiresAt.Int64)
		f.ExpiresAt = &t
	}
	if r.EndedAt.Valid {
		t := store.Time(r.EndedAt.Int64)
		f.EndedAt = &t
	}
	return f
}

func faultView(f domain.Fault, now time.Time) FaultView {
	return FaultView{ID: f.ID, CameraID: f.CameraID, Kind: string(f.Kind), Instance: f.Params.Instance, Status: f.Params.Status,
		LatencyMS: f.Params.LatencyMS, SkewS: f.Params.SkewS, StartedAt: f.StartedAt, ExpiresAt: f.ExpiresAt, EndedAt: f.EndedAt,
		EndedBy: f.EndedBy, CreatedBy: f.CreatedBy, Active: f.Active(now)}
}

func ipcFault(f domain.Fault) ipc.Fault {
	return ipc.Fault{ID: f.ID, Kind: string(f.Kind), FaultParams: f.Params}
}

// openFaults returns the faults on, of one camera or of all when id is "".
func (s *Service) openFaults(ctx context.Context, id string) ([]domain.Fault, error) {
	rows, err := s.store.R().ListOpenFaults(ctx)
	if err != nil {
		return nil, err
	}
	now := time.Now()
	var out []domain.Fault
	for _, r := range rows {
		f := faultOf(r)
		if (id == "" || f.CameraID == id) && f.Active(now) {
			out = append(out, f)
		}
	}
	return out, nil
}

// faultTargets are the camera's enabled engine instances, as faults see
// them.
func (s *Service) faultTargets(b *cameraBundle) []domain.FaultTarget {
	enabled := map[string]bool{}
	for _, p := range b.protos {
		enabled[p.EngineKey] = store.Bool(p.Enabled)
	}
	var out []domain.FaultTarget
	for _, inst := range profile.SortedKeys(b.doc.Engines) {
		if on, known := enabled[inst]; known && !on {
			continue
		}
		name, rng, err := profile.EngineName(b.doc.Engines[inst])
		if err != nil {
			continue
		}
		eng, err := s.catalog.Resolve(name, rng)
		if err != nil {
			continue
		}
		d := eng.Describe()
		out = append(out, domain.FaultTarget{Instance: inst, Engine: d.Name, Server: d.Role == engine.RoleServer})
	}
	return out
}

// InjectFault puts a fault on a camera. A fault of the same kind on the
// same protocol replaces the one in place. On a running camera it applies
// at once and the camera turns degraded; a stopped one gets it as it
// starts, while it lasts.
func (s *Service) InjectFault(ctx context.Context, actor Actor, cameraID string, in FaultInput) (*FaultView, error) {
	b, err := s.loadBundle(ctx, cameraID)
	if err != nil {
		return nil, err
	}
	kind := domain.FaultKind(in.Kind)
	params := domain.FaultParams{Instance: strings.TrimSpace(in.Instance), Status: in.Status, LatencyMS: in.LatencyMS, SkewS: in.SkewS}
	duration := time.Duration(in.DurationS) * time.Second
	if err := domain.ValidateFault(kind, params, duration, s.faultTargets(b), b.storageKind()); err != nil {
		return nil, err
	}
	open, err := s.openFaults(ctx, cameraID)
	if err != nil {
		return nil, err
	}
	now := time.Now()
	f := domain.Fault{ID: ulid.Make().String(), CameraID: cameraID, Kind: kind, Params: params, StartedAt: now, CreatedBy: actorName(actor)}
	var replaced []domain.Fault
	for _, o := range open {
		if o.Key() == f.Key() {
			replaced = append(replaced, o)
		}
	}
	if len(open)-len(replaced) >= domain.MaxActiveFaults {
		return nil, domain.Conflict("", "the camera has %d faults on, the most it takes", domain.MaxActiveFaults)
	}
	for _, o := range replaced {
		if err := s.endFault(ctx, o, "replaced"); err != nil {
			return nil, err
		}
	}
	expires := sql.NullInt64{}
	if duration > 0 {
		t := now.Add(duration)
		f.ExpiresAt = &t
		expires = sql.NullInt64{Int64: t.UnixMilli(), Valid: true}
	}
	raw, _ := json.Marshal(params)
	if err := s.store.W().InsertFault(ctx, db.InsertFaultParams{ID: f.ID, CameraID: cameraID, Kind: string(kind), ParamsJson: string(raw),
		StartedAt: now.UnixMilli(), ExpiresAt: expires, CreatedBy: f.CreatedBy}); err != nil {
		return nil, err
	}
	s.audit(ctx, actor, "fault.start", "camera", cameraID, map[string]any{"fault": f.ID, "kind": kind, "params": params, "duration_s": in.DurationS})
	s.applyFault(ctx, f, true)
	v := faultView(f, now)
	return &v, nil
}

// EndFault takes a fault off by hand.
func (s *Service) EndFault(ctx context.Context, actor Actor, cameraID, faultID string) error {
	r, err := s.store.R().GetFault(ctx, faultID)
	if err != nil || r.CameraID != cameraID {
		return domain.ErrNotFound
	}
	f := faultOf(r)
	if !f.Active(time.Now()) {
		return domain.Conflict("", "the fault is over")
	}
	if err := s.endFault(ctx, f, actorName(actor)); err != nil {
		return err
	}
	s.audit(ctx, actor, "fault.end", "camera", cameraID, map[string]any{"fault": f.ID, "kind": f.Kind})
	return nil
}

// endFault stores that a fault ended and takes it off the camera.
func (s *Service) endFault(ctx context.Context, f domain.Fault, by string) error {
	n, err := s.store.W().EndFault(ctx, db.EndFaultParams{ID: f.ID, EndedAt: sql.NullInt64{Int64: time.Now().UnixMilli(), Valid: true}, EndedBy: by})
	if err != nil {
		return err
	}
	if n > 0 {
		s.applyFault(ctx, f, false)
	}
	return nil
}

// applyFault takes a fault to a running camera, on or off: the network is
// taken down before the camera hears of it, so the event it raises meets
// the network as it is.
func (s *Service) applyFault(ctx context.Context, f domain.Fault, on bool) {
	defer s.pub.Publish("cameras", "faults", map[string]any{"camera_id": f.CameraID})
	if _, sd := domain.SDFaultStates[f.Kind]; sd {
		// The card's state follows, after the camera raised the event.
		defer s.pushStorage(ctx, f.CameraID)
	}
	ss := s.session(f.CameraID)
	if ss == nil || !ss.active() {
		return
	}
	if f.Kind == domain.FaultNetworkDown {
		s.setOffline(ctx, ss)
	}
	if on {
		s.tellCamera(ctx, f.CameraID, ipc.TypeFaultStart, ipcFault(f))
	} else {
		s.tellCamera(ctx, f.CameraID, ipc.TypeFaultStop, ipc.FaultStop{ID: f.ID})
	}
	ss.settle(ctx)
}

// setOffline takes the camera's network down while a network_down fault
// is on, and back up when none is.
func (s *Service) setOffline(ctx context.Context, ss *session) {
	open, err := s.openFaults(ctx, ss.id)
	if err != nil {
		return
	}
	off := slices.ContainsFunc(open, func(f domain.Fault) bool { return f.Kind == domain.FaultNetworkDown })
	if err := s.rt.SetOffline(ctx, ss.id, off); err != nil {
		s.log.Warn("cannot change the camera's network", "camera", ss.id, "offline", off, "error", err)
	}
}

// settle puts a running camera in degraded while faults are on, naming
// them, and back in running when none is.
func (ss *session) settle(ctx context.Context) {
	ss.mu.Lock()
	st, reason := ss.state, ss.reason
	ss.mu.Unlock()
	if !st.Active() {
		return
	}
	open, err := ss.s.openFaults(ctx, ss.id)
	if err != nil {
		return
	}
	if len(open) == 0 {
		if st == domain.StateDegraded {
			ss.setState(domain.StateRunning, "", "")
		}
		return
	}
	names := make([]string, 0, len(open))
	for _, f := range open {
		names = append(names, faultLabel(f))
	}
	summary := "faults on: " + strings.Join(names, ", ")
	if st != domain.StateDegraded || reason != summary {
		ss.setState(domain.StateDegraded, ReasonFaults, summary)
	}
}

// faultLabel names a fault in a few words, for logs and the API.
func faultLabel(f domain.Fault) string {
	p := f.Params
	switch f.Kind {
	case domain.FaultServiceDown:
		return p.Instance + " down"
	case domain.FaultLatency:
		return fmt.Sprintf("%s +%d ms", p.Instance, p.LatencyMS)
	case domain.FaultErrorStatus:
		return fmt.Sprintf("%s answers %d", p.Instance, p.Status)
	case domain.FaultClockSkew:
		return fmt.Sprintf("clock %+d s", p.SkewS)
	case domain.FaultNetworkDown:
		return "network down"
	case domain.FaultIPConflict:
		return "IP conflict"
	case domain.FaultSDMissing:
		return "SD card missing"
	case domain.FaultSDError:
		return "SD card error"
	case domain.FaultSDReadOnly:
		return "SD card read only"
	case domain.FaultSDFull:
		return "SD card full"
	}
	return string(f.Kind)
}

// ListFaults returns a camera's faults: those on first, then the last
// ended ones.
func (s *Service) ListFaults(ctx context.Context, cameraID string) ([]FaultView, error) {
	if _, err := s.store.R().GetCamera(ctx, cameraID); err != nil {
		return nil, store.NotFound(err)
	}
	rows, err := s.store.R().ListCameraFaults(ctx, db.ListCameraFaultsParams{CameraID: cameraID, Limit: 50})
	if err != nil {
		return nil, err
	}
	now := time.Now()
	out := make([]FaultView, 0, len(rows))
	for _, r := range rows {
		out = append(out, faultView(faultOf(r), now))
	}
	slices.SortStableFunc(out, func(a, b FaultView) int {
		switch {
		case a.Active && !b.Active:
			return -1
		case b.Active && !a.Active:
			return 1
		}
		return 0
	})
	return out, nil
}

// ActiveFaults returns the faults on in every camera, with its name.
func (s *Service) ActiveFaults(ctx context.Context) ([]FaultView, error) {
	open, err := s.openFaults(ctx, "")
	if err != nil {
		return nil, err
	}
	names := map[string]string{}
	now := time.Now()
	out := make([]FaultView, 0, len(open))
	for _, f := range open {
		if _, ok := names[f.CameraID]; !ok {
			if c, err := s.store.R().GetCamera(ctx, f.CameraID); err == nil {
				names[f.CameraID] = c.Name
			}
		}
		v := faultView(f, now)
		v.CameraName = names[f.CameraID]
		out = append(out, v)
	}
	return out, nil
}

// faultLoop ends faults as they expire, every second, and forgets old
// ended ones every hour.
func (s *Service) faultLoop(ctx context.Context) {
	tick := time.NewTicker(time.Second)
	defer tick.Stop()
	last := time.Time{}
	for {
		select {
		case <-ctx.Done():
			return
		case now := <-tick.C:
			s.expireFaults(ctx, now)
			if now.Sub(last) > time.Hour {
				last = now
				if _, err := s.store.W().DeleteFaultsBefore(ctx, sql.NullInt64{Int64: now.Add(-faultHistory).UnixMilli(), Valid: true}); err != nil {
					s.log.Warn("cannot forget old faults", "error", err)
				}
			}
		}
	}
}

func (s *Service) expireFaults(ctx context.Context, now time.Time) {
	rows, err := s.store.R().ListExpiredFaults(ctx, sql.NullInt64{Int64: now.UnixMilli(), Valid: true})
	if err != nil {
		return
	}
	for _, r := range rows {
		f := faultOf(r)
		if err := s.endFault(ctx, f, "expired"); err != nil {
			s.log.Warn("cannot end an expired fault", "fault", f.ID, "error", err)
			continue
		}
		s.audit(ctx, Actor{Type: "system", Name: "faults"}, "fault.end", "camera", f.CameraID, map[string]any{"fault": f.ID, "kind": f.Kind, "expired": true})
	}
}

// startFaults are the faults a camera starts with, as its configuration
// carries them.
func (s *Service) startFaults(ctx context.Context, id string) []ipc.Fault {
	open, err := s.openFaults(ctx, id)
	if err != nil {
		return nil
	}
	out := make([]ipc.Fault, 0, len(open))
	for _, f := range open {
		out = append(out, ipcFault(f))
	}
	return out
}

// DefaultBootTime is how long a camera takes to come back from a reboot
// when its profile does not say.
const DefaultBootTime = 30 * time.Second

// MaxBootTime bounds a simulated reboot.
const MaxBootTime = 10 * time.Minute

// RebootCamera reboots a running camera as the real one does (D43): it
// leaves the network, and comes back after its boot time, the profile's
// unless seconds is given.
func (s *Service) RebootCamera(ctx context.Context, actor Actor, id string, seconds *int) (*CameraView, error) {
	lock := s.opLock(id)
	lock.Lock()
	defer lock.Unlock()
	b, err := s.loadBundle(ctx, id)
	if err != nil {
		return nil, err
	}
	boot := DefaultBootTime
	if d := b.doc.Identity.BootTime.D(); d > 0 {
		boot = d
	}
	if seconds != nil {
		if *seconds < 0 || time.Duration(*seconds)*time.Second > MaxBootTime {
			return nil, domain.Invalid("seconds", "must be between 0 and %d", int(MaxBootTime/time.Second))
		}
		boot = time.Duration(*seconds) * time.Second
	}
	ss := s.session(id)
	if ss == nil || !ss.active() {
		return nil, domain.Conflict("", "the camera is not running")
	}
	reason := fmt.Sprintf("rebooting; back in %d s", int(boot/time.Second))
	ss.setState(domain.StateRestarting, ReasonReboot, reason)
	ss.stop("rebooted by " + actorName(actor))
	select {
	case <-ss.done:
	case <-ctx.Done():
		return nil, ctx.Err()
	}
	_ = s.saveStatus(ctx, id, domain.StateRestarting, ReasonReboot, reason, time.Time{}, time.Now())
	s.publishStatus(id, domain.StateRestarting, ReasonReboot, reason)
	s.holdStart(id, boot)
	s.audit(ctx, actor, "camera.reboot", "camera", id, map[string]any{"boot_s": int(boot / time.Second)})
	return s.GetCamera(ctx, id)
}

// holdStart starts a camera after a pause, as its boot: until then the
// reconciler leaves it alone. Starting or stopping it by hand ends the
// pause.
func (s *Service) holdStart(id string, d time.Duration) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if r := s.retries[id]; r != nil && r.timer != nil {
		r.timer.Stop()
	}
	s.retries[id] = &retryState{timer: time.AfterFunc(d, func() {
		s.resetRetries(id)
		s.retryStart(id)
	})}
}
