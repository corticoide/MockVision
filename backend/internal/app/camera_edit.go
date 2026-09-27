package app

import (
	"context"
	"encoding/json"
	"fmt"
	"strconv"
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

// restartKeys summarizes the settings a running camera only picks up when
// it restarts (RN-09): its network identity, and its protocols, whose
// sockets the network helper opens before the camera starts.
func restartKeys(b *cameraBundle) map[string]string {
	protos := make([]string, 0, len(b.protos))
	for _, p := range b.protos {
		protos = append(protos, fmt.Sprintf("%s=%d:%d", p.EngineKey, p.Enabled, p.Port))
	}
	return map[string]string{
		"network": strings.Join([]string{b.net.Mode, b.net.ParentIf, b.net.Mac, b.net.IpMode, b.net.Ip,
			b.net.Netmask, b.net.Gateway, b.net.DnsJson}, "|"),
		"protocols": strings.Join(protos, ","),
	}
}

// pendingRestart lists what changed since the camera was launched.
func pendingRestart(applied, now map[string]string) []string {
	out := []string{}
	if applied == nil {
		return out
	}
	for _, k := range []string{"network", "protocols"} {
		if applied[k] != now[k] {
			out = append(out, k)
		}
	}
	return out
}

// instanceDescriptor describes the engine behind an instance of the
// camera's profile.
func (s *Service) instanceDescriptor(b *cameraBundle, instance string) (engine.Descriptor, error) {
	name, rng, err := engineOf(b, instance)
	if err != nil {
		return engine.Descriptor{}, err
	}
	eng, err := s.catalog.Resolve(name, rng)
	if err != nil {
		return engine.Descriptor{}, err
	}
	return eng.Describe(), nil
}

func (s *Service) protocolViews(b *cameraBundle) []ProtocolView {
	out := make([]ProtocolView, 0, len(b.protos))
	for _, p := range b.protos {
		pv := ProtocolView{Instance: p.EngineKey, Enabled: store.Bool(p.Enabled), Port: int(p.Port)}
		if d, err := s.instanceDescriptor(b, p.EngineKey); err == nil {
			pv.Engine, pv.Role = d.Name, string(d.Role)
		}
		if port, err := s.instancePort(b.doc, p.EngineKey); err == nil {
			pv.DefaultPort = port
		}
		out = append(out, pv)
	}
	return out
}

// publishCamera sends the current view of a camera to the panel.
func (s *Service) publishCamera(ctx context.Context, id string) (*CameraView, error) {
	v, err := s.GetCamera(ctx, id)
	if err == nil {
		s.pub.Publish("cameras", "updated", v)
	}
	return v, err
}

func (s *Service) touchCamera(ctx context.Context, q *db.Queries, id string) error {
	return q.TouchCamera(ctx, db.TouchCameraParams{ID: id, UpdatedAt: time.Now().UnixMilli()})
}

// SetCameraUsers replaces the accounts of a camera (D11, RN-11). A password
// left empty keeps the account's current one. A running camera applies the
// new accounts at once: its engines look them up on every request.
func (s *Service) SetCameraUsers(ctx context.Context, actor Actor, id string, in []UserInput) (*CameraView, error) {
	b, err := s.loadBundle(ctx, id)
	if err != nil {
		return nil, err
	}
	current := map[string]db.CameraUser{}
	for _, u := range b.users {
		current[u.Username] = u
	}
	v := &domain.ValidationError{}
	users := make([]domain.CameraUser, len(in))
	changed := []string{}
	for i, u := range in {
		u.Username = strings.TrimSpace(u.Username)
		if u.Role == "" {
			u.Role = "admin"
		}
		if u.Password == "" {
			old, ok := current[u.Username]
			if !ok {
				v.Add("users["+strconv.Itoa(i)+"].password", "is required for a new account")
				continue
			}
			pw, err := s.box.Open(old.PasswordEnc, "camera_users:"+id+":"+old.Username)
			if err != nil {
				return nil, fmt.Errorf("cannot decrypt the password of %s: %w", old.Username, err)
			}
			u.Password = string(pw)
		} else {
			changed = append(changed, u.Username)
		}
		users[i] = domain.CameraUser{Username: u.Username, Password: u.Password, Role: u.Role}
	}
	if err := v.Err(); err != nil {
		return nil, err
	}
	if err := domain.ValidateCameraUsers(users); err != nil {
		return nil, err
	}
	err = s.store.Tx(ctx, func(q *db.Queries) error {
		if err := q.DeleteCameraUsers(ctx, id); err != nil {
			return err
		}
		for _, u := range users {
			if err := q.InsertCameraUser(ctx, db.InsertCameraUserParams{
				ID: ulid.Make().String(), CameraID: id, Username: u.Username,
				PasswordEnc: s.box.Seal([]byte(u.Password), "camera_users:"+id+":"+u.Username), Role: u.Role,
			}); err != nil {
				return err
			}
		}
		return s.touchCamera(ctx, q, id)
	})
	if err != nil {
		return nil, err
	}
	audit := make([]map[string]string, len(users))
	for i, u := range users {
		audit[i] = map[string]string{"username": u.Username, "role": u.Role}
	}
	s.audit(ctx, actor, "camera.users", "camera", id, map[string]any{"users": audit, "passwords_set": changed})
	if ss := s.session(id); ss != nil && ss.active() {
		accounts := make([]engine.User, len(users))
		for i, u := range users {
			accounts[i] = engine.User{Username: u.Username, Password: u.Password, Role: u.Role}
		}
		if err := ss.conn().Request(ctx, ipc.TypeReload, ipc.Reload{Users: accounts}, nil); err != nil {
			s.log.Warn("the camera did not reload its accounts", "camera", id, "error", err)
		}
	}
	return s.publishCamera(ctx, id)
}

// ProtocolInput enables, disables or moves an engine instance of the
// profile.
type ProtocolInput struct {
	Instance string `json:"instance"`
	Enabled  *bool  `json:"enabled"`
	Port     *int   `json:"port"`
}

// SetCameraProtocols changes which protocols of its profile a camera serves
// and on which ports (RN-04). A running camera applies them when it
// restarts, since the network helper opens its sockets.
func (s *Service) SetCameraProtocols(ctx context.Context, actor Actor, id string, in []ProtocolInput) (*CameraView, error) {
	b, err := s.loadBundle(ctx, id)
	if err != nil {
		return nil, err
	}
	next := map[string]db.CameraProtocol{}
	for _, p := range b.protos {
		next[p.EngineKey] = p
	}
	descs := map[string]engine.Descriptor{}
	for key := range next {
		d, err := s.instanceDescriptor(b, key)
		if err != nil {
			return nil, err
		}
		descs[key] = d
	}
	v := &domain.ValidationError{}
	for i, p := range in {
		field := "protocols[" + strconv.Itoa(i) + "]"
		cur, ok := next[p.Instance]
		if !ok {
			v.Add(field+".instance", "profile %s has no protocol %q", b.prof.ProfileID, p.Instance)
			continue
		}
		if p.Enabled != nil {
			cur.Enabled = store.Int(*p.Enabled)
		}
		if p.Port != nil {
			switch {
			case len(descs[p.Instance].Sockets) == 0:
				if *p.Port != 0 {
					v.Add(field+".port", "%s does not listen on a port", p.Instance)
				}
			case *p.Port < 1 || *p.Port > 65535:
				v.Add(field+".port", "must be between 1 and 65535")
			default:
				cur.Port = int64(*p.Port)
			}
		}
		next[p.Instance] = cur
	}
	// Two enabled protocols cannot listen on the same port.
	used := map[string]string{}
	for _, key := range profile.SortedKeys(next) {
		p := next[key]
		if !store.Bool(p.Enabled) {
			continue
		}
		for j, so := range descs[key].Sockets {
			port := so.DefaultPort
			if j == 0 {
				port = int(p.Port)
			}
			k := so.Network + "/" + strconv.Itoa(port)
			if other, clash := used[k]; clash {
				v.Add("protocols", "%s and %s both use %s port %d", other, key, strings.ToUpper(so.Network), port)
			}
			used[k] = key
		}
	}
	if err := v.Err(); err != nil {
		return nil, err
	}
	err = s.store.Tx(ctx, func(q *db.Queries) error {
		for _, key := range profile.SortedKeys(next) {
			p := next[key]
			if err := q.UpdateCameraProtocol(ctx, db.UpdateCameraProtocolParams{CameraID: id, EngineKey: key, Enabled: p.Enabled, Port: p.Port}); err != nil {
				return err
			}
		}
		return s.touchCamera(ctx, q, id)
	})
	if err != nil {
		return nil, err
	}
	s.audit(ctx, actor, "camera.protocols", "camera", id, in)
	return s.publishCamera(ctx, id)
}

// StreamUpdate changes the picture or the settings of a stream; nil fields
// stay as they are.
type StreamUpdate struct {
	AssetID    *string `json:"asset_id"`
	Codec      *string `json:"codec"`
	Resolution *string `json:"resolution"`
	FPS        *int    `json:"fps"`
	Bitrate    *int    `json:"bitrate"`
	GOP        *int    `json:"gop"`
}

// UpdateCameraStream changes a stream's picture or its encoding: codec,
// resolution, frame rate, bitrate and GOP are the profile parameters bound
// to them, as if set from the panel. The stream is encoded again and a
// running camera switches to it without restarting (RN-09).
func (s *Service) UpdateCameraStream(ctx context.Context, actor Actor, id, name string, in StreamUpdate) (*CameraView, error) {
	b, err := s.loadBundle(ctx, id)
	if err != nil {
		return nil, err
	}
	var cur *db.CameraStream
	for i := range b.streams {
		if b.streams[i].Stream == name {
			cur = &b.streams[i]
		}
	}
	if cur == nil {
		return nil, fmt.Errorf("camera has no stream %q: %w", name, domain.ErrNotFound)
	}
	if in.AssetID == nil && in.Codec == nil && in.Resolution == nil && in.FPS == nil && in.Bitrate == nil && in.GOP == nil {
		return nil, domain.Invalid("", "nothing to change")
	}
	type setting struct {
		field, canon string
		raw          any
	}
	var wanted []setting
	if in.Codec != nil {
		wanted = append(wanted, setting{"codec", "media." + name + ".codec", *in.Codec})
	}
	if in.Resolution != nil {
		wanted = append(wanted, setting{"resolution", "media." + name + ".resolution", *in.Resolution})
	}
	if in.FPS != nil {
		wanted = append(wanted, setting{"fps", "media." + name + ".fps", int64(*in.FPS)})
	}
	if in.Bitrate != nil {
		wanted = append(wanted, setting{"bitrate", "media." + name + ".bitrate", int64(*in.Bitrate)})
	}
	if in.GOP != nil {
		wanted = append(wanted, setting{"gop", "media." + name + ".gop", int64(*in.GOP)})
	}
	values := b.values()
	coerced := map[string]any{}
	for _, w := range wanted {
		key, ok := b.model.NativeFor(w.canon)
		if !ok {
			return nil, domain.Invalid(w.field, "profile %s does not allow changing it", b.prof.ProfileID)
		}
		cv, err := profile.Coerce(b.doc.State[key], w.raw)
		if err != nil {
			return nil, domain.Invalid(w.field, "%v", err)
		}
		coerced[key] = cv
		values[key] = cv
	}
	assetID := cur.AssetID
	if in.AssetID != nil {
		assetID = *in.AssetID
	}
	asset, err := s.store.R().GetAsset(ctx, assetID)
	if err != nil {
		if notFound(err) {
			return nil, domain.Invalid("asset_id", "asset %s does not exist", assetID)
		}
		return nil, err
	}
	settings, err := b.model.StreamFor(name, values)
	if err != nil {
		return nil, domain.Invalid("resolution", "%v", err)
	}
	rend, err := s.ensureRenditionRow(ctx, asset, settings)
	if err != nil {
		return nil, err
	}
	origin := engine.Origin{Kind: engine.OriginPanel, IP: actor.IP}
	changes := make([]engine.Change, 0, len(coerced))
	for _, k := range profile.SortedKeys(coerced) {
		changes = append(changes, engine.Change{Key: k, Value: coerced[k], Bind: b.doc.State[k].Bind, Origin: origin})
	}
	now := time.Now().UnixMilli()
	err = s.store.Tx(ctx, func(q *db.Queries) error {
		for _, c := range changes {
			raw, _ := json.Marshal(c.Value)
			if err := q.UpsertCameraState(ctx, db.UpsertCameraStateParams{CameraID: id, Key: c.Key, ValueJson: string(raw), Origin: "panel", UpdatedAt: now}); err != nil {
				return err
			}
		}
		if err := q.UpsertCameraStream(ctx, db.UpsertCameraStreamParams{CameraID: id, Stream: name, AssetID: asset.ID, RenditionID: store.NullString(rend.ID)}); err != nil {
			return err
		}
		return s.touchCamera(ctx, q, id)
	})
	if err != nil {
		return nil, err
	}
	s.audit(ctx, actor, "camera.stream", "camera", id, map[string]any{"stream": name, "asset_id": asset.ID, "values": coerced})
	if len(coerced) > 0 {
		// The emulated API reports the new values right away.
		if ss := s.session(id); ss != nil && ss.active() {
			if err := ss.conn().Request(ctx, ipc.TypeStateSet, ipc.StateSet{Values: coerced, Origin: origin}, nil); err != nil {
				s.log.Warn("could not apply stream settings to the running camera", "camera", id, "error", err)
			}
		}
		s.pub.Publish("camera:"+id, "config", changes)
	}
	s.regenerateStreamsLater(id)
	return s.publishCamera(ctx, id)
}

// Reset scopes (RN-10).
const (
	// ResetSettings restores everything but the network identity.
	ResetSettings = "settings"
	// ResetFull also takes the profile's factory address.
	ResetFull = "full"
)

// ResetCamera restores a camera to its profile like the reset button of a
// real device (RN-10): parameters, accounts and protocols go back to the
// profile's defaults and, with ResetFull, the network to the factory
// address. The picture is kept. A running camera reboots, as the real one
// does.
func (s *Service) ResetCamera(ctx context.Context, actor Actor, id, scope string) (*CameraView, error) {
	if scope != ResetSettings && scope != ResetFull {
		return nil, domain.Invalid("scope", "must be %s or %s", ResetSettings, ResetFull)
	}
	b, err := s.loadBundle(ctx, id)
	if err != nil {
		return nil, err
	}
	users := make([]domain.CameraUser, 0, len(b.doc.Identity.Factory.Users))
	for _, u := range b.doc.Identity.Factory.Users {
		users = append(users, domain.CameraUser{Username: u.Username, Password: u.Password, Role: u.Role})
	}
	if err := domain.ValidateCameraUsers(users); err != nil {
		return nil, domain.Invalid("scope", "the factory accounts of profile %s are not valid: %v", b.prof.ProfileID, err)
	}
	var netw *resolvedNetwork
	if scope == ResetFull {
		f := b.doc.Identity.Factory.Network
		if s.rt.Kind() == "netns" && f.IP == "" {
			return nil, domain.Invalid("scope", "profile %s has no factory address", b.prof.ProfileID)
		}
		n, err := s.resolveNetwork(ctx, id, b.doc, NetworkInput{Parent: b.net.ParentIf, DefaultMAC: true, IP: f.IP, Netmask: f.Mask, Gateway: f.Gateway})
		if err != nil {
			return nil, err
		}
		if err := s.checkUniqueNetwork(ctx, id, n); err != nil {
			return nil, err
		}
		netw = &n
	}
	ports := map[string]int64{}
	for _, p := range b.protos {
		port, err := s.instancePort(b.doc, p.EngineKey)
		if err != nil {
			return nil, err
		}
		ports[p.EngineKey] = int64(port)
	}
	values := b.model.Defaults()
	now := time.Now().UnixMilli()
	err = s.store.Tx(ctx, func(q *db.Queries) error {
		if err := q.DeleteCameraState(ctx, id); err != nil {
			return err
		}
		for _, key := range profile.SortedKeys(values) {
			raw, _ := json.Marshal(values[key])
			if err := q.UpsertCameraState(ctx, db.UpsertCameraStateParams{CameraID: id, Key: key, ValueJson: string(raw), Origin: "profile", UpdatedAt: now}); err != nil {
				return err
			}
		}
		if err := q.DeleteCameraUsers(ctx, id); err != nil {
			return err
		}
		for _, u := range users {
			if err := q.InsertCameraUser(ctx, db.InsertCameraUserParams{
				ID: ulid.Make().String(), CameraID: id, Username: u.Username,
				PasswordEnc: s.box.Seal([]byte(u.Password), "camera_users:"+id+":"+u.Username), Role: u.Role,
			}); err != nil {
				return err
			}
		}
		for key, port := range ports {
			if err := q.UpdateCameraProtocol(ctx, db.UpdateCameraProtocolParams{CameraID: id, EngineKey: key, Enabled: 1, Port: port}); err != nil {
				return err
			}
		}
		if netw != nil {
			if err := q.UpdateCameraNetwork(ctx, db.UpdateCameraNetworkParams{
				CameraID: id, ParentIf: netw.parent, Mac: netw.mac, Ip: netw.ip, Netmask: netw.netmask, Gateway: netw.gateway, DnsJson: "[]",
			}); err != nil {
				return err
			}
		}
		return s.touchCamera(ctx, q, id)
	})
	if err != nil {
		if store.IsUnique(err) {
			return nil, domain.Conflict("scope", "the factory address is already used by another camera")
		}
		return nil, err
	}
	s.audit(ctx, actor, "camera.reset", "camera", id, map[string]string{"scope": scope})
	if ss := s.session(id); ss != nil {
		return s.RestartCamera(ctx, actor, id)
	}
	s.regenerateStreamsLater(id)
	return s.publishCamera(ctx, id)
}

// CloneCameraInput names the copy and gives it its own address.
type CloneCameraInput struct {
	Name    string       `json:"name"`
	Network NetworkInput `json:"network"`
	Start   bool         `json:"start"`
}

// CloneCamera creates a camera with the same profile, parameters, accounts,
// protocols, picture and targets as another one. It gets its own ID, serial
// and MAC, and the address given.
func (s *Service) CloneCamera(ctx context.Context, actor Actor, srcID string, in CloneCameraInput) (*CameraView, error) {
	b, err := s.loadBundle(ctx, srcID)
	if err != nil {
		return nil, err
	}
	in.Name = strings.TrimSpace(in.Name)
	if err := domain.ValidateCameraName(in.Name); err != nil {
		return nil, err
	}
	if store.Bool(b.prof.Archived) {
		return nil, domain.Invalid("profile_id", "profile %s@%s is archived and no longer offered for new cameras", b.prof.ProfileID, b.prof.Version)
	}
	id := ulid.Make().String()
	in.Network.DefaultMAC = in.Network.MAC == ""
	netw, err := s.resolveNetwork(ctx, id, b.doc, in.Network)
	if err != nil {
		return nil, err
	}
	if err := s.checkUnique(ctx, in.Name, netw); err != nil {
		return nil, err
	}
	if err := s.admitCreate(ctx); err != nil {
		return nil, err
	}
	type account struct{ username, password, role string }
	accounts := make([]account, 0, len(b.users))
	for _, u := range b.users {
		pw, err := s.box.Open(u.PasswordEnc, "camera_users:"+srcID+":"+u.Username)
		if err != nil {
			return nil, fmt.Errorf("cannot decrypt the password of %s: %w", u.Username, err)
		}
		accounts = append(accounts, account{u.Username, string(pw), u.Role})
	}
	now := time.Now()
	err = s.store.Tx(ctx, func(q *db.Queries) error {
		if err := q.InsertCamera(ctx, db.InsertCameraParams{
			ID: id, Name: in.Name, ProfileID: b.prof.ProfileID, ProfileVersion: b.prof.Version, Serial: serialFor(id, b.doc.Identity.Serial),
			DesiredState: string(domain.DesiredStopped), Autostart: b.cam.Autostart, TagsJson: b.cam.TagsJson,
			CreatedAt: now.UnixMilli(), UpdatedAt: now.UnixMilli(),
		}); err != nil {
			return err
		}
		dns, _ := json.Marshal(nonNil(netw.dns))
		if err := q.InsertCameraNetwork(ctx, db.InsertCameraNetworkParams{
			CameraID: id, Mode: b.net.Mode, ParentIf: netw.parent, Mac: netw.mac, IpMode: b.net.IpMode,
			Ip: netw.ip, Netmask: netw.netmask, Gateway: netw.gateway, DnsJson: string(dns),
		}); err != nil {
			return err
		}
		for _, st := range b.state {
			if err := q.UpsertCameraState(ctx, db.UpsertCameraStateParams{CameraID: id, Key: st.Key, ValueJson: st.ValueJson, Origin: st.Origin, UpdatedAt: now.UnixMilli()}); err != nil {
				return err
			}
		}
		for _, p := range b.protos {
			if err := q.InsertCameraProtocol(ctx, db.InsertCameraProtocolParams{CameraID: id, EngineKey: p.EngineKey, Enabled: p.Enabled, Port: p.Port, OptionsJson: p.OptionsJson}); err != nil {
				return err
			}
		}
		for _, a := range accounts {
			if err := q.InsertCameraUser(ctx, db.InsertCameraUserParams{
				ID: ulid.Make().String(), CameraID: id, Username: a.username,
				PasswordEnc: s.box.Seal([]byte(a.password), "camera_users:"+id+":"+a.username), Role: a.role,
			}); err != nil {
				return err
			}
		}
		for _, st := range b.streams {
			if err := q.UpsertCameraStream(ctx, db.UpsertCameraStreamParams{CameraID: id, Stream: st.Stream, AssetID: st.AssetID, RenditionID: st.RenditionID}); err != nil {
				return err
			}
		}
		for _, t := range b.targets {
			if err := q.InsertCameraTarget(ctx, db.InsertCameraTargetParams{CameraID: id, TargetID: t.ID, EventTypesJson: t.EventTypesJson, OverridesJson: t.OverridesJson}); err != nil {
				return err
			}
		}
		return q.UpsertCameraStatus(ctx, db.UpsertCameraStatusParams{CameraID: id, ActualState: string(domain.StateStopped), UpdatedAt: now.UnixMilli()})
	})
	if err != nil {
		if store.IsUnique(err) {
			return nil, domain.Conflict("name", "a camera with this name, IP or MAC already exists")
		}
		return nil, err
	}
	s.audit(ctx, actor, "camera.clone", "camera", id, map[string]string{"from": srcID, "name": in.Name, "ip": netw.ip, "mac": netw.mac})
	view, err := s.GetCamera(ctx, id)
	if err != nil {
		return nil, err
	}
	s.pub.Publish("cameras", "created", view)
	if in.Start {
		if _, err := s.StartCamera(ctx, actor, id); err != nil {
			return view, err
		}
		view, _ = s.GetCamera(ctx, id)
	}
	return view, nil
}
