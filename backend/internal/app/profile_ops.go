package app

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"slices"
	"time"

	"go.yaml.in/yaml/v3"

	"github.com/corticoide/mockvision/backend/internal/domain"
	"github.com/corticoide/mockvision/backend/internal/pkg"
	"github.com/corticoide/mockvision/backend/internal/profile"
	"github.com/corticoide/mockvision/backend/internal/store"
	"github.com/corticoide/mockvision/backend/internal/store/db"
)

// packageData returns what was imported for a profile version: the
// package as uploaded, or the loose profile.yaml.
func (s *Service) packageData(ctx context.Context, profileID, version string) ([]byte, error) {
	p, err := s.store.R().GetProfileByRef(ctx, db.GetProfileByRefParams{ProfileID: profileID, Version: version})
	if err != nil {
		return nil, store.NotFound(err)
	}
	pk, err := s.store.R().GetPackage(ctx, p.PackageID)
	if err != nil {
		return nil, err
	}
	for _, ext := range []string{".mvpkg", ".yaml"} {
		data, err := os.ReadFile(filepath.Join(s.opts.DataDir, "packages", pk.Sha256+ext))
		if err == nil {
			return data, nil
		}
	}
	return nil, fmt.Errorf("the package of %s@%s is missing from the data directory", profileID, version)
}

// ExportProfile returns a profile version as a .mvpkg (D22): the package
// as it was imported, signature included, or a loose profile wrapped in an
// unsigned one.
func (s *Service) ExportProfile(ctx context.Context, profileID, version string) (string, []byte, error) {
	data, err := s.packageData(ctx, profileID, version)
	if err != nil {
		return "", nil, err
	}
	if !bytes.HasPrefix(data, []byte("PK")) {
		if data, err = pkg.WrapLoose(data, profileID, version); err != nil {
			return "", nil, err
		}
	}
	return pkg.FileName(profileID, version), data, nil
}

// DuplicateInput names the copy of a profile.
type DuplicateInput struct {
	ProfileID string `json:"profile_id"`
	Version   string `json:"version,omitempty"`
	Name      string `json:"name,omitempty"`
}

// DuplicateProfile copies a profile version under another ID: the way to
// change a profile that is read only, such as the official catalog's
// (D19). The copy is a new unsigned package, imported as any other; to
// edit it, export it, change it and import the new version.
func (s *Service) DuplicateProfile(ctx context.Context, actor Actor, profileID, version string, in DuplicateInput) (*ImportResult, error) {
	if !domain.ValidProfileID(in.ProfileID) {
		return nil, domain.Invalid("profile_id", "the copy needs an ID such as acme/my-camera")
	}
	if in.Version == "" {
		in.Version = "0.1.0"
	}
	if _, err := s.store.R().GetProfileByRef(ctx, db.GetProfileByRefParams{ProfileID: in.ProfileID, Version: in.Version}); err == nil {
		return nil, domain.Conflict("profile_id", "%s@%s is installed already", in.ProfileID, in.Version)
	}
	data, err := s.packageData(ctx, profileID, version)
	if err != nil {
		return nil, err
	}
	files := map[string][]byte{}
	if bytes.HasPrefix(data, []byte("PK")) {
		if files, err = pkg.Files(data); err != nil {
			return nil, err
		}
		delete(files, "manifest.yaml")
		delete(files, "manifest.sig")
	} else {
		files["profile.yaml"] = data
	}
	fields := map[string]string{"id": in.ProfileID, "version": in.Version}
	if in.Name != "" {
		fields["name"] = in.Name
	}
	if files["profile.yaml"], err = setProfileFields(files["profile.yaml"], fields); err != nil {
		return nil, domain.Invalid("profile_id", "cannot copy the profile: %v", err)
	}
	copyData, err := pkg.Pack(in.ProfileID, in.Version, pkg.Provenance{Source: "draft"}, files)
	if err != nil {
		return nil, err
	}
	res, err := s.importAs(ctx, actor, pkg.FileName(in.ProfileID, in.Version), copyData, sourceDuplicate)
	if err != nil {
		return nil, err
	}
	s.audit(ctx, actor, "profile.duplicate", "profile", res.Profile.ID, map[string]any{"from": profileID + "@" + version, "to": in.ProfileID + "@" + in.Version})
	return res, nil
}

// setProfileFields sets keys of a profile.yaml's profile section, keeping
// its comments and layout.
func setProfileFields(data []byte, fields map[string]string) ([]byte, error) {
	var root yaml.Node
	if err := yaml.Unmarshal(data, &root); err != nil {
		return nil, err
	}
	if len(root.Content) == 0 || root.Content[0].Kind != yaml.MappingNode {
		return nil, errors.New("the profile is not a mapping")
	}
	doc := root.Content[0]
	var meta *yaml.Node
	for i := 0; i+1 < len(doc.Content); i += 2 {
		if doc.Content[i].Value == "profile" {
			meta = doc.Content[i+1]
		}
	}
	if meta == nil || meta.Kind != yaml.MappingNode {
		return nil, errors.New("the profile has no profile section")
	}
	for _, key := range []string{"id", "version", "name"} {
		val, ok := fields[key]
		if !ok {
			continue
		}
		set := false
		for i := 0; i+1 < len(meta.Content); i += 2 {
			if meta.Content[i].Value == key {
				meta.Content[i+1].SetString(val)
				set = true
			}
		}
		if !set {
			k, v := &yaml.Node{}, &yaml.Node{}
			k.SetString(key)
			v.SetString(val)
			meta.Content = append([]*yaml.Node{k, v}, meta.Content...)
		}
	}
	var buf bytes.Buffer
	enc := yaml.NewEncoder(&buf)
	enc.SetIndent(2)
	if err := enc.Encode(&root); err != nil {
		return nil, err
	}
	return buf.Bytes(), enc.Close()
}

// DiffProfiles compares two versions of a profile (D05).
func (s *Service) DiffProfiles(ctx context.Context, profileID, from, to string) ([]profile.Change, error) {
	a, err := s.store.R().GetProfileByRef(ctx, db.GetProfileByRefParams{ProfileID: profileID, Version: from})
	if err != nil {
		return nil, store.NotFound(err)
	}
	b, err := s.store.R().GetProfileByRef(ctx, db.GetProfileByRefParams{ProfileID: profileID, Version: to})
	if err != nil {
		return nil, store.NotFound(err)
	}
	changes, err := profile.Diff([]byte(a.ResolvedJson), []byte(b.ResolvedJson))
	if changes == nil {
		changes = []profile.Change{}
	}
	return changes, err
}

// Actions of an upgrade on each part of the camera.
const (
	UpgradeKept    = "kept"
	UpgradeDefault = "default"
	UpgradeReset   = "reset"
	UpgradeAdded   = "added"
	UpgradeDropped = "dropped"
)

// UpgradeItem is what an upgrade does to a parameter, protocol, stream,
// rule or trigger of the camera.
type UpgradeItem struct {
	Key    string `json:"key"`
	Action string `json:"action"`
	Value  any    `json:"value,omitempty"`
	Reason string `json:"reason,omitempty"`
}

// UpgradePlan is what moving a camera to another version of its profile
// does, before it is done (D05).
type UpgradePlan struct {
	ProfileID string           `json:"profile_id"`
	From      string           `json:"from"`
	To        string           `json:"to"`
	Changes   []profile.Change `json:"changes"`
	Params    []UpgradeItem    `json:"params"`
	Protocols []UpgradeItem    `json:"protocols"`
	Streams   []UpgradeItem    `json:"streams"`
	Rules     []UpgradeItem    `json:"rules"`
	Triggers  []UpgradeItem    `json:"triggers"`
	// Restart says whether the camera restarts to apply it.
	Restart bool `json:"restart"`
}

// UpgradeResult is an applied upgrade.
type UpgradeResult struct {
	Plan   UpgradePlan `json:"plan"`
	Camera *CameraView `json:"camera"`
}

// upgrade is a plan and what it writes.
type upgrade struct {
	plan     UpgradePlan
	target   db.Profile
	values   map[string]any
	origins  map[string]string
	protos   []db.CameraProtocol
	streams  []string
	rules    []domain.Rule
	triggers []domain.Trigger
}

// PlanProfileUpgrade says what moving a camera to another version of its
// profile would do: parameters keep the values someone set when the new
// version accepts them; those nobody set follow its defaults.
func (s *Service) PlanProfileUpgrade(ctx context.Context, cameraID, version string) (*UpgradePlan, error) {
	u, err := s.planUpgrade(ctx, cameraID, version)
	if err != nil {
		return nil, err
	}
	return &u.plan, nil
}

func (s *Service) planUpgrade(ctx context.Context, cameraID, version string) (*upgrade, error) {
	b, err := s.loadBundle(ctx, cameraID)
	if err != nil {
		return nil, err
	}
	if version == "" || version == b.prof.Version {
		return nil, domain.Invalid("version", "the camera runs %s@%s already", b.prof.ProfileID, b.prof.Version)
	}
	target, err := s.store.R().GetProfileByRef(ctx, db.GetProfileByRefParams{ProfileID: b.prof.ProfileID, Version: version})
	if err != nil {
		if notFound(err) {
			return nil, domain.Invalid("version", "%s@%s is not installed", b.prof.ProfileID, version)
		}
		return nil, err
	}
	if store.Bool(target.Archived) {
		return nil, domain.Invalid("version", "%s@%s is archived", b.prof.ProfileID, version)
	}
	doc, err := s.profileDoc(target)
	if err != nil {
		return nil, err
	}
	model := profile.NewModel(doc)
	changes, err := profile.Diff([]byte(b.prof.ResolvedJson), []byte(target.ResolvedJson))
	if err != nil {
		return nil, err
	}
	u := &upgrade{target: target, values: map[string]any{}, origins: map[string]string{},
		plan: UpgradePlan{ProfileID: b.prof.ProfileID, From: b.prof.Version, To: version, Changes: nonNilChanges(changes),
			Params: []UpgradeItem{}, Protocols: []UpgradeItem{}, Streams: []UpgradeItem{}, Rules: []UpgradeItem{}, Triggers: []UpgradeItem{}}}

	// Parameters.
	current := b.values()
	set := map[string]db.CameraState{}
	for _, st := range b.state {
		set[st.Key] = st
	}
	defaults := model.DefaultsFor(b.identity())
	var kept []string
	for _, key := range profile.SortedKeys(doc.State) {
		p := doc.State[key]
		u.values[key], u.origins[key] = defaults[key], "profile"
		st, had := set[key]
		switch {
		case !had:
			u.plan.Params = append(u.plan.Params, UpgradeItem{Key: key, Action: UpgradeAdded, Value: defaults[key]})
		case st.Origin == "profile":
			if fmt.Sprint(current[key]) != fmt.Sprint(defaults[key]) {
				u.plan.Params = append(u.plan.Params, UpgradeItem{Key: key, Action: UpgradeDefault, Value: defaults[key]})
			}
		default:
			cv, err := profile.Coerce(p, current[key])
			if err == nil {
				err = profile.CheckValue(p, cv)
			}
			if err != nil {
				u.plan.Params = append(u.plan.Params, UpgradeItem{Key: key, Action: UpgradeReset, Value: defaults[key], Reason: err.Error()})
				continue
			}
			u.values[key], u.origins[key] = cv, st.Origin
			kept = append(kept, key)
			u.plan.Params = append(u.plan.Params, UpgradeItem{Key: key, Action: UpgradeKept, Value: cv})
		}
	}
	// Kept values must still make streams the new version has.
	for key, msg := range model.CheckStreams(u.values, kept) {
		u.values[key], u.origins[key] = defaults[key], "profile"
		for i := range u.plan.Params {
			if u.plan.Params[i].Key == key {
				u.plan.Params[i] = UpgradeItem{Key: key, Action: UpgradeReset, Value: defaults[key], Reason: msg}
			}
		}
	}
	for _, key := range profile.SortedKeys(set) {
		if _, still := doc.State[key]; !still {
			u.plan.Params = append(u.plan.Params, UpgradeItem{Key: key, Action: UpgradeDropped})
		}
	}

	// Protocols: the instances of the new version, with the port and switch
	// they had when their engine did not change.
	had := map[string]db.CameraProtocol{}
	for _, p := range b.protos {
		had[p.EngineKey] = p
	}
	for _, inst := range profile.SortedKeys(doc.Engines) {
		port, err := s.instancePort(doc, inst)
		if err != nil {
			return nil, err
		}
		newName, _, _ := profile.EngineName(doc.Engines[inst])
		old, ok := had[inst]
		oldName, _, _ := profile.EngineName(b.doc.Engines[inst])
		switch {
		case !ok:
			u.protos = append(u.protos, db.CameraProtocol{EngineKey: inst, Enabled: 1, Port: int64(port), OptionsJson: "{}"})
			u.plan.Protocols = append(u.plan.Protocols, UpgradeItem{Key: inst, Action: UpgradeAdded, Value: port})
		case oldName != newName:
			u.protos = append(u.protos, db.CameraProtocol{EngineKey: inst, Enabled: 1, Port: int64(port), OptionsJson: "{}"})
			u.plan.Protocols = append(u.plan.Protocols, UpgradeItem{Key: inst, Action: UpgradeReset, Value: port, Reason: "its engine is " + newName + " now"})
		default:
			u.protos = append(u.protos, old)
			u.plan.Protocols = append(u.plan.Protocols, UpgradeItem{Key: inst, Action: UpgradeKept, Value: old.Port})
		}
	}
	for _, p := range b.protos {
		if _, still := doc.Engines[p.EngineKey]; !still {
			u.plan.Protocols = append(u.plan.Protocols, UpgradeItem{Key: p.EngineKey, Action: UpgradeDropped})
		}
	}

	// Streams.
	var oldStreams []string
	for _, st := range b.streams {
		oldStreams = append(oldStreams, st.Stream)
	}
	u.streams = streamNames(doc)
	for _, name := range u.streams {
		action := UpgradeKept
		if !slices.Contains(oldStreams, name) {
			action = UpgradeAdded
		}
		if _, err := model.StreamFor(name, u.values); err != nil {
			return nil, domain.Invalid("version", "stream %s of %s@%s: %v", name, b.prof.ProfileID, version, err)
		}
		u.plan.Streams = append(u.plan.Streams, UpgradeItem{Key: name, Action: action})
	}
	for _, name := range oldStreams {
		if !slices.Contains(u.streams, name) {
			u.plan.Streams = append(u.plan.Streams, UpgradeItem{Key: name, Action: UpgradeDropped})
		}
	}

	// Analytics: rules of a kind the new version has, triggers of events it
	// sends.
	caps := doc.VCACaps()
	droppedRules := map[string]bool{}
	for _, r := range b.rules {
		if slices.Contains(caps.RuleTypes, r.Type) {
			u.rules = append(u.rules, r)
			u.plan.Rules = append(u.plan.Rules, UpgradeItem{Key: r.Name, Action: UpgradeKept})
			continue
		}
		droppedRules[r.ID] = true
		u.plan.Rules = append(u.plan.Rules, UpgradeItem{Key: r.Name, Action: UpgradeDropped, Reason: "the new version has no " + string(r.Type) + " rules"})
	}
	for _, t := range b.triggers {
		if !caps.Delivers(t.EventType) {
			u.plan.Triggers = append(u.plan.Triggers, UpgradeItem{Key: t.Name, Action: UpgradeDropped, Reason: "the new version sends no " + t.EventType + " events"})
			continue
		}
		if droppedRules[t.RuleID] {
			t.RuleID = ""
		}
		u.triggers = append(u.triggers, t)
		u.plan.Triggers = append(u.plan.Triggers, UpgradeItem{Key: t.Name, Action: UpgradeKept})
	}
	u.plan.Restart = s.session(cameraID) != nil
	return u, nil
}

func nonNilChanges(c []profile.Change) []profile.Change {
	if c == nil {
		return []profile.Change{}
	}
	return c
}

// UpgradeCameraProfile moves a camera to another version of its profile,
// as the plan says, and restarts it if it runs. Cameras stay on their
// version until someone does this (D05).
func (s *Service) UpgradeCameraProfile(ctx context.Context, actor Actor, cameraID, version string) (*UpgradeResult, error) {
	u, err := s.planUpgrade(ctx, cameraID, version)
	if err != nil {
		return nil, err
	}
	b, err := s.loadBundle(ctx, cameraID)
	if err != nil {
		return nil, err
	}
	asset := ""
	for _, st := range b.streams {
		if asset == "" || st.Stream == "main" {
			asset = st.AssetID
		}
	}
	keep := map[string]string{}
	for _, st := range b.streams {
		keep[st.Stream] = st.AssetID
	}
	now := time.Now().UnixMilli()
	err = s.store.Tx(ctx, func(q *db.Queries) error {
		if err := q.SetCameraProfileVersion(ctx, db.SetCameraProfileVersionParams{ID: cameraID, ProfileVersion: version, UpdatedAt: now}); err != nil {
			return err
		}
		if err := q.DeleteCameraState(ctx, cameraID); err != nil {
			return err
		}
		for _, key := range profile.SortedKeys(u.values) {
			raw, _ := json.Marshal(u.values[key])
			if err := q.UpsertCameraState(ctx, db.UpsertCameraStateParams{CameraID: cameraID, Key: key, ValueJson: string(raw), Origin: u.origins[key], UpdatedAt: now}); err != nil {
				return err
			}
		}
		if err := q.DeleteCameraProtocols(ctx, cameraID); err != nil {
			return err
		}
		for _, p := range u.protos {
			if err := q.InsertCameraProtocol(ctx, db.InsertCameraProtocolParams{CameraID: cameraID, EngineKey: p.EngineKey, Enabled: p.Enabled, Port: p.Port, OptionsJson: p.OptionsJson}); err != nil {
				return err
			}
		}
		if err := q.DeleteCameraStreams(ctx, cameraID); err != nil {
			return err
		}
		for _, name := range u.streams {
			a := keep[name]
			if a == "" {
				a = asset
			}
			if err := q.UpsertCameraStream(ctx, db.UpsertCameraStreamParams{CameraID: cameraID, Stream: name, AssetID: a}); err != nil {
				return err
			}
		}
		if err := q.DeleteCameraRules(ctx, cameraID); err != nil {
			return err
		}
		if err := insertRules(ctx, q, cameraID, u.rules); err != nil {
			return err
		}
		if err := q.DeleteCameraTriggers(ctx, cameraID); err != nil {
			return err
		}
		if err := insertTriggers(ctx, q, cameraID, u.triggers); err != nil {
			return err
		}
		return s.touchCamera(ctx, q, cameraID)
	})
	if err != nil {
		return nil, err
	}
	s.audit(ctx, actor, "camera.upgrade_profile", "camera", cameraID, map[string]any{"profile": u.plan.ProfileID, "from": u.plan.From, "to": u.plan.To})
	var view *CameraView
	if s.session(cameraID) != nil {
		view, err = s.RestartCamera(ctx, actor, cameraID)
	} else {
		s.regenerateStreamsLater(cameraID)
		view, err = s.publishCamera(ctx, cameraID)
	}
	if err != nil {
		return nil, err
	}
	return &UpgradeResult{Plan: u.plan, Camera: view}, nil
}
