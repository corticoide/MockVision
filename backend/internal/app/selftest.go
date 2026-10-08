package app

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/oklog/ulid/v2"

	"github.com/corticoide/mockvision/backend/internal/domain"
	"github.com/corticoide/mockvision/backend/internal/ipc"
	"github.com/corticoide/mockvision/backend/internal/netctl"
	"github.com/corticoide/mockvision/backend/internal/pkg"
	"github.com/corticoide/mockvision/backend/internal/profile"
	"github.com/corticoide/mockvision/backend/internal/selftest"
	"github.com/corticoide/mockvision/sdk/engine"
)

// Time limits of a self-test camera.
const (
	selfTestStart   = 60 * time.Second
	selfTestFixture = 60 * time.Second
	// maxFixtureMessage keeps a fixture under the IPC message limit.
	maxFixtureMessage = 900 << 10
)

// runSelfTest replays a package's fixtures against an ephemeral camera of
// its profile, with its factory values, in a namespace that has only its
// loopback: it reaches nothing and nothing reaches it (D88). In local mode
// it is a plain process on 127.0.0.1, as every camera there.
func (s *Service) runSelfTest(ctx context.Context, res *pkg.Result, step func(string)) (selftest.Report, error) {
	doc, err := profile.DecodeJSON(res.Resolved)
	if err != nil {
		return selftest.Report{}, err
	}
	model := profile.NewModel(doc)
	id := ulid.Make().String()
	identity := engine.Identity{CameraID: id, Name: "Self-test", IP: "127.0.0.1", MAC: "02:00:00:00:00:01", Serial: serialFor(id, doc.Identity.Serial),
		Vendor: doc.Profile.Vendor, Model: doc.Profile.Model, ProfileID: res.Report.ID, ProfileVersion: res.Report.Version}
	if m := doc.Identity.Device["model"]; m != "" {
		identity.Model = m
	}
	identity.Firmware = doc.Identity.Device["firmware"]
	if identity.Firmware == "" && len(doc.Profile.Firmware) > 0 {
		identity.Firmware = doc.Profile.Firmware[0]
	}
	step("Encoding the self-test camera's streams")
	streams, err := s.selfTestStreams(ctx, model)
	if err != nil {
		return selftest.Report{}, fmt.Errorf("streams: %w", err)
	}
	cfg := ipc.Configure{Identity: identity, Profile: res.Resolved, State: model.DefaultsFor(profile.CameraIdentity{Serial: identity.Serial,
		Name: identity.Name, Model: identity.Model, MAC: identity.MAC, IP: identity.IP, Firmware: identity.Firmware}),
		Streams: streams, Storage: ipc.Storage{Kind: string(domain.StorageNone)}, SelfTest: true, VCA: ipc.VCA{Rules: factoryRules(doc), Triggers: []domain.Trigger{}},
		Targets: []ipc.Target{}}
	for _, u := range doc.Identity.Factory.Users {
		cfg.Users = append(cfg.Users, engine.User{Username: u.Username, Password: u.Password, Role: u.Role})
	}
	spec := netctl.CameraSpec{ID: id, Mode: netctl.ModeIsolated, Netns: "sim-selftest-" + strings.ToLower(id[len(id)-8:])}
	for _, inst := range profile.SortedKeys(doc.Engines) {
		port, err := s.instancePort(doc, inst)
		if err != nil {
			return selftest.Report{}, err
		}
		cfg.Engines = append(cfg.Engines, ipc.EngineConfig{Instance: inst, Enabled: true, Port: port})
		name, rng, err := profile.EngineName(doc.Engines[inst])
		if err != nil {
			return selftest.Report{}, err
		}
		eng, err := s.catalog.Resolve(name, rng)
		if err != nil {
			return selftest.Report{}, err
		}
		for i, so := range eng.Describe().Sockets {
			p := so.DefaultPort
			if i == 0 && port > 0 {
				p = port
			}
			spec.Sockets = append(spec.Sockets, netctl.SocketSpec{Instance: inst, Name: so.Name, Network: so.Network, Port: p})
		}
	}

	step("Starting the self-test camera")
	s.mu.Lock()
	s.selfTests[id] = true
	s.mu.Unlock()
	defer func() {
		_ = s.rt.Destroy(context.Background(), id)
		s.mu.Lock()
		delete(s.selfTests, id)
		s.mu.Unlock()
	}()
	launched, err := s.rt.Launch(ctx, netctl.LaunchSpec{Camera: spec})
	if err != nil {
		return selftest.Report{}, fmt.Errorf("cannot start the self-test camera: %w", err)
	}
	hello := make(chan struct{}, 1)
	ready := make(chan string, 1)
	conn := ipc.NewConn(launched.Conn, func(_ context.Context, msg *ipc.Envelope) (any, error) {
		switch msg.Type {
		case ipc.TypeHello:
			select {
			case hello <- struct{}{}:
			default:
			}
		case ipc.TypeReady:
			select {
			case ready <- "":
			default:
			}
		case ipc.TypeFailed:
			var f ipc.Failed
			_ = msg.Decode(&f)
			select {
			case ready <- truncate(f.Reason, 512):
			default:
			}
		}
		return nil, nil
	}, s.log.With("selftest", id))
	cctx, cancel := context.WithCancel(ctx)
	defer cancel()
	go func() { _ = conn.Run(cctx) }()
	defer conn.Close()
	wait := func(ch <-chan struct{}) error {
		select {
		case <-ch:
			return nil
		case <-conn.Done():
			return errors.New("the self-test camera exited")
		case <-time.After(selfTestStart):
			return errors.New("the self-test camera did not start in time")
		case <-ctx.Done():
			return ctx.Err()
		}
	}
	if err := wait(hello); err != nil {
		return selftest.Report{}, err
	}
	rctx, rcancel := context.WithTimeout(ctx, selfTestStart)
	err = conn.Request(rctx, ipc.TypeConfigure, cfg, nil)
	rcancel()
	if err != nil {
		return selftest.Report{}, fmt.Errorf("the self-test camera refused its configuration: %w", err)
	}
	select {
	case reason := <-ready:
		if reason != "" {
			return selftest.Report{}, fmt.Errorf("the self-test camera failed: %s", reason)
		}
	case <-conn.Done():
		return selftest.Report{}, errors.New("the self-test camera exited")
	case <-time.After(selfTestStart):
		return selftest.Report{}, errors.New("the self-test camera did not become ready")
	}

	var rep selftest.Report
	for i, f := range res.Fixtures {
		step(fmt.Sprintf("Replaying fixture %d of %d", i+1, len(res.Fixtures)))
		one, err := s.replay(ctx, conn, f)
		if err != nil {
			one = selftest.Report{Results: []selftest.Result{{ID: f.ID, File: f.File, Status: selftest.Failed, Detail: err.Error()}}, Failed: 1}
		}
		rep.Results = append(rep.Results, one.Results...)
		rep.Passed += one.Passed
		rep.Failed += one.Failed
		rep.Skipped += one.Skipped
	}
	sctx, scancel := context.WithTimeout(context.Background(), 5*time.Second)
	_ = conn.Request(sctx, ipc.TypeStop, ipc.Stop{DeadlineMS: 2000}, nil)
	scancel()
	return rep, nil
}

// replay sends one fixture, so no message passes the IPC size limit.
func (s *Service) replay(ctx context.Context, conn *ipc.Conn, f selftest.Fixture) (selftest.Report, error) {
	raw, err := json.Marshal([]selftest.Fixture{f})
	if err != nil {
		return selftest.Report{}, err
	}
	if len(raw) > maxFixtureMessage {
		return selftest.Report{}, errors.New("the fixture is too large to replay")
	}
	rctx, cancel := context.WithTimeout(ctx, selfTestFixture)
	defer cancel()
	var rep selftest.Report
	if err := conn.Request(rctx, ipc.TypeSelfTest, ipc.SelfTest{Fixtures: raw}, &rep); err != nil {
		return selftest.Report{}, err
	}
	if len(rep.Results) != 1 {
		return selftest.Report{}, errors.New("the self-test camera did not report the fixture")
	}
	return rep, nil
}

// selfTestStreams encodes the built-in test pattern as the profile's
// streams are by default.
func (s *Service) selfTestStreams(ctx context.Context, model *profile.Model) ([]ipc.Stream, error) {
	view, err := s.ensureBuiltinAsset(ctx)
	if err != nil {
		return nil, err
	}
	asset, err := s.store.R().GetAsset(ctx, view.ID)
	if err != nil {
		return nil, err
	}
	values := model.Defaults()
	var out []ipc.Stream
	for _, name := range streamNames(model.Doc) {
		set, err := model.StreamFor(name, values)
		if err != nil {
			return nil, err
		}
		rend, err := s.ensureRenditionRow(ctx, asset, set)
		if err != nil {
			return nil, err
		}
		files, err := s.encodeRendition(ctx, rend.ID)
		if err != nil {
			return nil, err
		}
		out = append(out, ipc.Stream{Name: name, Codec: set.Codec, Width: set.Width, Height: set.Height, FPS: set.FPS, GOP: set.GOP,
			Bitrate: set.Bitrate, StreamPath: files.Stream, SnapshotPath: files.Snapshot})
	}
	return out, nil
}

// applySelfTest records the replay in the report: the self-test step, the
// coverage of every route and event, and the level the profile earns. A
// profile is captured when every fixture passes, verified when the
// official catalog also signed it (D76, D88).
func applySelfTest(res *pkg.Result, rep selftest.Report, err error) {
	r := &res.Report
	for i := range r.Steps {
		if r.Steps[i].Name != profile.StepSelfTest {
			continue
		}
		switch {
		case err != nil:
			r.Steps[i].Status, r.Steps[i].Note = "failed", err.Error()
		case rep.Complete():
			r.Steps[i].Status, r.Steps[i].Note = "passed", fmt.Sprintf("%d of %d fixtures match the device", rep.Passed, len(rep.Results))
		default:
			r.Steps[i].Status = "failed"
			r.Steps[i].Note = fmt.Sprintf("%d of %d fixtures match the device; %d differ, %d were not checked", rep.Passed, len(rep.Results), rep.Failed, rep.Skipped)
		}
	}
	r.SelfTest = &rep
	if doc, derr := profile.DecodeJSON(res.Resolved); derr == nil {
		routes := map[string][]string{}
		for _, inst := range profile.SortedKeys(doc.Engines) {
			if ids := profile.RouteIDs(doc.Engines[inst]); len(ids) > 0 {
				routes[inst] = ids
			}
		}
		r.Verified = selftest.Coverage(rep.Results, routes, profile.SortedKeys(doc.Events))
	}
	if err == nil && rep.Complete() {
		r.Level = string(domain.LevelCaptured)
		if r.Signature == pkg.SignatureOfficial {
			r.Level = string(domain.LevelVerified)
		}
	}
}
