package app

import (
	"context"
	"crypto/sha256"
	"database/sql"
	"encoding/json"
	"fmt"
	"net/netip"
	"sort"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/oklog/ulid/v2"

	"github.com/corticoide/mockvision/backend/internal/domain"
	"github.com/corticoide/mockvision/backend/internal/ipc"
	"github.com/corticoide/mockvision/backend/internal/netctl"
	"github.com/corticoide/mockvision/backend/internal/profile"
	"github.com/corticoide/mockvision/backend/internal/store"
	"github.com/corticoide/mockvision/backend/internal/store/db"
	"github.com/corticoide/mockvision/sdk/engine"
)

// CreateCameraInput is the request to create a camera.
type CreateCameraInput struct {
	Name           string       `json:"name"`
	ProfileID      string       `json:"profile_id"`
	ProfileVersion string       `json:"profile_version"`
	Network        NetworkInput `json:"network"`
	Users          []UserInput  `json:"users"`
	Stream         StreamInput  `json:"stream"`
	TargetIDs      []string     `json:"target_ids"`
	Autostart      *bool        `json:"autostart"`
	Start          bool         `json:"start"`
	Tags           []string     `json:"tags"`
}

// NetworkInput is the requested network identity; empty fields get the
// node's defaults. When editing a camera an empty MAC keeps the current
// one; DefaultMAC goes back to the one derived from the camera ID.
type NetworkInput struct {
	Parent     string   `json:"parent"`
	MAC        string   `json:"mac"`
	DefaultMAC bool     `json:"default_mac"`
	VendorOUI  bool     `json:"vendor_oui"`
	IP         string   `json:"ip"`
	Netmask    string   `json:"netmask"`
	Gateway    string   `json:"gateway"`
	DNS        []string `json:"dns"`
}

// UserInput is a camera account to create.
type UserInput struct {
	Username string `json:"username"`
	Password string `json:"password"`
	Role     string `json:"role"`
}

// StreamInput selects the image and settings of the main stream.
type StreamInput struct {
	AssetID    string `json:"asset_id"`
	Resolution string `json:"resolution"`
	FPS        int    `json:"fps"`
}

// UpdateCameraInput changes a camera; nil fields stay as they are. The
// network replaces the whole identity and reaches a running camera when it
// restarts (RN-09).
type UpdateCameraInput struct {
	Name      *string       `json:"name"`
	Autostart *bool         `json:"autostart"`
	Tags      *[]string     `json:"tags"`
	TargetIDs *[]string     `json:"target_ids"`
	Network   *NetworkInput `json:"network"`
}

// cameraBundle is everything stored about a camera.
type cameraBundle struct {
	cam     db.Camera
	net     db.CameraNetwork
	prof    db.Profile
	doc     *profile.Document
	model   *profile.Model
	state   []db.CameraState
	protos  []db.CameraProtocol
	users   []db.CameraUser
	streams []db.CameraStream
	targets []db.ListCameraTargetsRow
	status  *db.CameraStatus
}

var profileCache sync.Map // "id@version" -> *profile.Document

func (s *Service) profileDoc(p db.Profile) (*profile.Document, error) {
	key := p.ProfileID + "@" + p.Version
	if d, ok := profileCache.Load(key); ok {
		return d.(*profile.Document), nil
	}
	doc, err := profile.DecodeJSON([]byte(p.ResolvedJson))
	if err != nil {
		return nil, fmt.Errorf("stored profile %s is invalid: %w", key, err)
	}
	profileCache.Store(key, doc)
	return doc, nil
}

func (s *Service) loadBundle(ctx context.Context, id string) (*cameraBundle, error) {
	q := s.store.R()
	cam, err := q.GetCamera(ctx, id)
	if err != nil {
		return nil, store.NotFound(err)
	}
	b := &cameraBundle{cam: cam}
	if b.net, err = q.GetCameraNetwork(ctx, id); err != nil {
		return nil, err
	}
	if b.prof, err = q.GetProfileByRef(ctx, db.GetProfileByRefParams{ProfileID: cam.ProfileID, Version: cam.ProfileVersion}); err != nil {
		return nil, err
	}
	if b.doc, err = s.profileDoc(b.prof); err != nil {
		return nil, err
	}
	b.model = profile.NewModel(b.doc)
	if b.state, err = q.ListCameraState(ctx, id); err != nil {
		return nil, err
	}
	if b.protos, err = q.ListCameraProtocols(ctx, id); err != nil {
		return nil, err
	}
	if b.users, err = q.ListCameraUsers(ctx, id); err != nil {
		return nil, err
	}
	if b.streams, err = q.ListCameraStreams(ctx, id); err != nil {
		return nil, err
	}
	if b.targets, err = q.ListCameraTargets(ctx, id); err != nil {
		return nil, err
	}
	if st, err := q.GetCameraStatus(ctx, id); err == nil {
		b.status = &st
	}
	return b, nil
}

// values returns the camera's native parameters.
func (b *cameraBundle) values() map[string]any {
	out := b.model.Defaults()
	for _, st := range b.state {
		var v any
		if json.Unmarshal([]byte(st.ValueJson), &v) != nil {
			continue
		}
		if p, ok := b.doc.State[st.Key]; ok {
			if cv, err := profile.Coerce(p, v); err == nil {
				out[st.Key] = cv
			}
		}
	}
	return out
}

func (b *cameraBundle) ip() string {
	return b.net.Ip
}

// CreateCamera validates and stores a new camera, then starts it if asked.
func (s *Service) CreateCamera(ctx context.Context, actor Actor, in CreateCameraInput) (*CameraView, error) {
	in.Name = strings.TrimSpace(in.Name)
	if err := domain.ValidateCameraName(in.Name); err != nil {
		return nil, err
	}
	prof, err := s.store.R().GetProfileByRef(ctx, db.GetProfileByRefParams{ProfileID: in.ProfileID, Version: in.ProfileVersion})
	if err != nil {
		if notFound(err) {
			return nil, domain.Invalid("profile_id", "profile %s@%s is not installed", in.ProfileID, in.ProfileVersion)
		}
		return nil, err
	}
	if store.Bool(prof.Archived) {
		return nil, domain.Invalid("profile_id", "profile %s@%s is archived and no longer offered for new cameras", in.ProfileID, in.ProfileVersion)
	}
	doc, err := s.profileDoc(prof)
	if err != nil {
		return nil, err
	}
	model := profile.NewModel(doc)
	id := ulid.Make().String()

	netw, err := s.resolveNetwork(ctx, id, doc, in.Network)
	if err != nil {
		return nil, err
	}

	users := in.Users
	if len(users) == 0 {
		for _, u := range doc.Identity.Factory.Users {
			users = append(users, UserInput{Username: u.Username, Password: u.Password, Role: u.Role})
		}
	}
	cu := make([]domain.CameraUser, len(users))
	for i, u := range users {
		if u.Role == "" {
			u.Role = "admin"
			users[i].Role = "admin"
		}
		cu[i] = domain.CameraUser{Username: u.Username, Password: u.Password, Role: u.Role}
	}
	if err := domain.ValidateCameraUsers(cu); err != nil {
		return nil, err
	}

	// State: profile defaults plus the stream settings chosen in the wizard,
	// written through the parameters bound to them.
	values := model.Defaults()
	overrides := map[string]any{}
	if in.Stream.Resolution != "" {
		if err := s.setBound(doc, model, values, overrides, "media.main.resolution", in.Stream.Resolution, "stream.resolution"); err != nil {
			return nil, err
		}
	}
	if in.Stream.FPS != 0 {
		if err := s.setBound(doc, model, values, overrides, "media.main.fps", int64(in.Stream.FPS), "stream.fps"); err != nil {
			return nil, err
		}
	}
	settings, err := model.StreamFor("main", values)
	if err != nil {
		return nil, err
	}
	if settings.Codec != "h264" {
		return nil, domain.Invalid("stream", "codec %s is not supported yet", settings.Codec)
	}

	assetID := in.Stream.AssetID
	if assetID == "" {
		a, err := s.ensureBuiltinAsset(ctx)
		if err != nil {
			return nil, fmt.Errorf("default image unavailable: %w", err)
		}
		assetID = a.ID
	}
	asset, err := s.store.R().GetAsset(ctx, assetID)
	if err != nil {
		if notFound(err) {
			return nil, domain.Invalid("stream.asset_id", "asset %s does not exist", assetID)
		}
		return nil, err
	}
	for _, t := range in.TargetIDs {
		if _, err := s.store.R().GetTarget(ctx, t); err != nil {
			return nil, domain.Invalid("target_ids", "target %s does not exist", t)
		}
	}
	if err := s.checkUnique(ctx, in.Name, netw); err != nil {
		return nil, err
	}
	if err := s.admitCreate(ctx); err != nil {
		return nil, err
	}

	rend, err := s.ensureRenditionRow(ctx, asset, settings)
	if err != nil {
		return nil, err
	}
	autostart := true
	if in.Autostart != nil {
		autostart = *in.Autostart
	}
	tags, _ := json.Marshal(nonNil(in.Tags))
	now := time.Now()
	serial := serialFor(id, doc.Identity.Serial)
	err = s.store.Tx(ctx, func(q *db.Queries) error {
		if err := q.InsertCamera(ctx, db.InsertCameraParams{
			ID: id, Name: in.Name, ProfileID: prof.ProfileID, ProfileVersion: prof.Version, Serial: serial,
			DesiredState: string(domain.DesiredStopped), Autostart: store.Int(autostart), TagsJson: string(tags),
			CreatedAt: now.UnixMilli(), UpdatedAt: now.UnixMilli(),
		}); err != nil {
			return err
		}
		dns, _ := json.Marshal(nonNil(netw.dns))
		if err := q.InsertCameraNetwork(ctx, db.InsertCameraNetworkParams{
			CameraID: id, Mode: string(domain.NetMacvlan), ParentIf: netw.parent, Mac: netw.mac, IpMode: string(domain.IPStatic),
			Ip: netw.ip, Netmask: netw.netmask, Gateway: netw.gateway, DnsJson: string(dns),
		}); err != nil {
			return err
		}
		for _, key := range profile.SortedKeys(values) {
			origin := "profile"
			if _, ok := overrides[key]; ok {
				origin = "panel"
			}
			raw, _ := json.Marshal(values[key])
			if err := q.UpsertCameraState(ctx, db.UpsertCameraStateParams{CameraID: id, Key: key, ValueJson: string(raw), Origin: origin, UpdatedAt: now.UnixMilli()}); err != nil {
				return err
			}
		}
		for _, inst := range profile.SortedKeys(doc.Engines) {
			port, err := s.instancePort(doc, inst)
			if err != nil {
				return err
			}
			if err := q.InsertCameraProtocol(ctx, db.InsertCameraProtocolParams{CameraID: id, EngineKey: inst, Enabled: 1, Port: int64(port), OptionsJson: "{}"}); err != nil {
				return err
			}
		}
		for _, u := range users {
			if err := q.InsertCameraUser(ctx, db.InsertCameraUserParams{
				ID: ulid.Make().String(), CameraID: id, Username: u.Username,
				PasswordEnc: s.box.Seal([]byte(u.Password), "camera_users:"+id+":"+u.Username), Role: u.Role,
			}); err != nil {
				return err
			}
		}
		if err := q.UpsertCameraStream(ctx, db.UpsertCameraStreamParams{CameraID: id, Stream: "main", AssetID: asset.ID, RenditionID: store.NullString(rend.ID)}); err != nil {
			return err
		}
		for _, t := range dedupe(in.TargetIDs) {
			if err := q.InsertCameraTarget(ctx, db.InsertCameraTargetParams{CameraID: id, TargetID: t, EventTypesJson: "[]", OverridesJson: "{}"}); err != nil {
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
	s.encodeAsync(rend.ID)
	s.audit(ctx, actor, "camera.create", "camera", id, map[string]any{"name": in.Name, "profile": prof.ProfileID + "@" + prof.Version, "ip": netw.ip, "mac": netw.mac})
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

func (s *Service) setBound(doc *profile.Document, m *profile.Model, values, overrides map[string]any, canon string, v any, field string) error {
	key, ok := m.NativeFor(canon)
	if !ok {
		return domain.Invalid(field, "this profile does not allow changing %s", canon)
	}
	cv, err := profile.Coerce(doc.State[key], v)
	if err != nil {
		return domain.Invalid(field, "%v", err)
	}
	values[key] = cv
	overrides[key] = cv
	return nil
}

type resolvedNetwork struct {
	parent, mac, ip, netmask, gateway string
	prefix                            int
	dns                               []string
}

// resolveNetwork fills the network identity with the node's defaults and
// validates it (RN-05 uniqueness is checked separately).
func (s *Service) resolveNetwork(ctx context.Context, id string, doc *profile.Document, in NetworkInput) (resolvedNetwork, error) {
	var oui []byte
	if in.VendorOUI && doc.Identity.OUI != "" {
		oui, _ = domain.ParseOUI(doc.Identity.OUI)
	}
	out := resolvedNetwork{mac: domain.DeriveMAC(id, oui).String()}
	dns, err := parseDNS(in.DNS)
	if err != nil {
		return out, err
	}
	out.dns = dns
	if in.MAC != "" && !in.DefaultMAC {
		hw, err := domain.ParseMAC(in.MAC)
		if err != nil {
			return out, domain.Invalid("network.mac", "%v", err)
		}
		out.mac = hw.String()
	}
	if s.rt.Kind() == "local" {
		// Local mode: cameras answer on 127.0.0.1 with ephemeral ports.
		return out, nil
	}
	info, err := nodeInfo()
	if err != nil {
		return out, err
	}
	out.parent = in.Parent
	if out.parent == "" {
		out.parent = s.Settings(ctx).ParentInterface
	}
	if out.parent == "" {
		out.parent = s.opts.ParentInterface
	}
	if out.parent == "" {
		out.parent = info.DefaultInterface
	}
	iface, ok := info.Lookup(out.parent)
	if !ok {
		return out, domain.Invalid("network.parent", "interface %q does not exist on this node", out.parent)
	}
	if strings.TrimSpace(in.IP) == "" {
		return out, domain.Invalid("network.ip", "is required")
	}
	ip, err := netip.ParseAddr(strings.TrimSpace(in.IP))
	if err != nil || !ip.Is4() {
		return out, domain.Invalid("network.ip", "must be an IPv4 address")
	}
	for _, i := range info.Interfaces {
		for _, a := range i.Addrs {
			if p, err := netip.ParsePrefix(a); err == nil && p.Addr() == ip {
				return out, domain.Invalid("network.ip", "%s belongs to the node itself (%s)", ip, i.Name)
			}
		}
	}
	prefix := 24
	switch {
	case in.Netmask != "":
		if prefix, err = domain.MaskToPrefix(in.Netmask); err != nil {
			return out, domain.Invalid("network.netmask", "%v", err)
		}
	default:
		for _, a := range iface.Addrs {
			if p, err := netip.ParsePrefix(a); err == nil && p.Masked().Contains(ip) {
				prefix = p.Bits()
			}
		}
	}
	var gw netip.Addr
	switch {
	case in.Gateway != "":
		if gw, err = netip.ParseAddr(in.Gateway); err != nil {
			return out, domain.Invalid("network.gateway", "must be an IPv4 address")
		}
	case info.DefaultGateway != "":
		if g, err := netip.ParseAddr(info.DefaultGateway); err == nil && netip.PrefixFrom(ip, prefix).Masked().Contains(g) {
			gw = g
		}
	}
	nid := domain.NetIdentity{Mode: domain.NetMacvlan, ParentIf: out.parent, MAC: out.mac, IPMode: domain.IPStatic, IP: ip, Prefix: prefix, Gateway: gw}
	if err := nid.Validate(); err != nil {
		return out, err
	}
	out.ip = ip.String()
	out.prefix = prefix
	out.netmask = domain.PrefixToMask(prefix)
	if gw.IsValid() {
		out.gateway = gw.String()
	}
	return out, nil
}

func (s *Service) checkUnique(ctx context.Context, name string, n resolvedNetwork) error {
	if _, err := s.store.R().CameraIDByName(ctx, name); err == nil {
		return domain.Conflict("name", "a camera named %q already exists", name)
	}
	return s.checkUniqueNetwork(ctx, "", n)
}

// checkUniqueNetwork enforces RN-05 for IP and MAC, ignoring the camera
// being edited.
func (s *Service) checkUniqueNetwork(ctx context.Context, self string, n resolvedNetwork) error {
	q := s.store.R()
	if n.ip != "" {
		if other, err := q.CameraIDByIP(ctx, n.ip); err == nil && other != self {
			name := other
			if c, err := q.GetCamera(ctx, other); err == nil {
				name = c.Name
			}
			return domain.Conflict("network.ip", "IP %s is already used by camera %s", n.ip, name)
		}
	}
	if other, err := q.CameraIDByMAC(ctx, n.mac); err == nil && other != self {
		return domain.Conflict("network.mac", "MAC %s is already used by another camera", n.mac)
	}
	return nil
}

// parseDNS validates the DNS servers of a camera; none means the node's.
func parseDNS(in []string) ([]string, error) {
	out := []string{}
	seen := map[string]bool{}
	for _, raw := range in {
		raw = strings.TrimSpace(raw)
		if raw == "" {
			continue
		}
		addr, err := netip.ParseAddr(raw)
		if err != nil {
			return nil, domain.Invalid("network.dns", "%q is not an IP address", raw)
		}
		if !seen[addr.String()] {
			seen[addr.String()] = true
			out = append(out, addr.String())
		}
	}
	if len(out) > 3 {
		return nil, domain.Invalid("network.dns", "at most 3 servers")
	}
	return out, nil
}

// instancePort is the default port of an engine instance: the port in its
// profile section, else the engine's first socket.
func (s *Service) instancePort(doc *profile.Document, inst string) (int, error) {
	var head struct {
		Engine string `json:"engine"`
		Port   int    `json:"port"`
	}
	if err := json.Unmarshal(doc.Engines[inst], &head); err != nil {
		return 0, err
	}
	if head.Port > 0 {
		return head.Port, nil
	}
	name, rng, err := profile.EngineRef(head.Engine)
	if err != nil {
		return 0, err
	}
	eng, err := s.catalog.Resolve(name, rng)
	if err != nil {
		return 0, err
	}
	if socks := eng.Describe().Sockets; len(socks) > 0 {
		return socks[0].DefaultPort, nil
	}
	return 0, nil
}

// serialFor fills the profile's serial mask deterministically from the
// camera ID, so a camera keeps its serial for life.
func serialFor(id, mask string) string {
	seed := sha256.Sum256([]byte("mockvision-serial:" + id))
	i := 0
	return domain.ExpandMask(mask, func(n int) int {
		b := seed[i%len(seed)]
		i++
		if i%len(seed) == 0 {
			seed = sha256.Sum256(seed[:])
		}
		return int(b) % n
	})
}

func nonNil(s []string) []string {
	if s == nil {
		return []string{}
	}
	return s
}

func dedupe(in []string) []string {
	seen := map[string]bool{}
	var out []string
	for _, s := range in {
		if !seen[s] {
			seen[s] = true
			out = append(out, s)
		}
	}
	return out
}

// ListCameras returns the cameras that pass the filter, by name.
func (s *Service) ListCameras(ctx context.Context, f CameraFilter) ([]CameraView, error) {
	cams, err := s.store.R().ListCameras(ctx)
	if err != nil {
		return nil, err
	}
	out := make([]CameraView, 0, len(cams))
	for _, c := range cams {
		v, err := s.GetCamera(ctx, c.ID)
		if err != nil {
			continue // deleted meanwhile
		}
		if f.Match(v) {
			out = append(out, *v)
		}
	}
	return out, nil
}

// GetCamera returns one camera.
func (s *Service) GetCamera(ctx context.Context, id string) (*CameraView, error) {
	b, err := s.loadBundle(ctx, id)
	if err != nil {
		return nil, err
	}
	return s.cameraView(ctx, b), nil
}

func (s *Service) cameraView(ctx context.Context, b *cameraBundle) *CameraView {
	var tags []string
	_ = json.Unmarshal([]byte(b.cam.TagsJson), &tags)
	var dns []string
	_ = json.Unmarshal([]byte(b.net.DnsJson), &dns)
	prefix, _ := domain.MaskToPrefix(b.net.Netmask)
	v := &CameraView{
		ID:   b.cam.ID,
		Name: b.cam.Name,
		Profile: ProfileRef{ID: b.prof.ProfileID, Version: b.prof.Version, Name: b.prof.Name, Vendor: b.prof.Vendor,
			Model: b.prof.Model, Level: b.prof.Level},
		Serial:       b.cam.Serial,
		DesiredState: b.cam.DesiredState,
		Autostart:    store.Bool(b.cam.Autostart),
		Tags:         nonNil(tags),
		Network: NetworkView{Mode: b.net.Mode, Parent: b.net.ParentIf, MAC: b.net.Mac, IPMode: b.net.IpMode, IP: b.net.Ip,
			Netmask: b.net.Netmask, Prefix: prefix, Gateway: b.net.Gateway, DNS: nonNil(dns)},
		CreatedAt: store.Time(b.cam.CreatedAt),
		UpdatedAt: store.Time(b.cam.UpdatedAt),
		Users:     []UserView{},
		Targets:   []TargetRef{},
		Streams:   []StreamView{},
		Endpoints: []EndpointView{},
	}
	v.Status = StatusView{State: string(domain.StateStopped)}
	if b.status != nil {
		v.Status.State = b.status.ActualState
		v.Status.Reason = b.status.Reason
		if t := store.NullTime(b.status.StartedAt); !t.IsZero() {
			v.Status.StartedAt = &t
		}
		if t := store.NullTime(b.status.LastHeartbeat); !t.IsZero() {
			v.Status.LastHeartbeat = &t
		}
	}
	s.mu.Lock()
	ss := s.sessions[b.cam.ID]
	if r := s.retries[b.cam.ID]; r != nil {
		v.Status.Retries = r.attempts
	}
	s.mu.Unlock()
	var endpoints []EndpointView
	if ss != nil {
		st := ss.snapshot()
		v.Status.State = string(st.state)
		v.Status.Reason = st.reason
		if !st.started.IsZero() {
			t := st.started
			v.Status.StartedAt = &t
		}
		if !st.lastHB.IsZero() {
			t := st.lastHB
			v.Status.LastHeartbeat = &t
		}
		v.Status.Netns = st.netns
		v.Status.PID = st.pid
		if len(st.endpoints) > 0 {
			endpoints = s.endpointViews(b, st.ip, st.endpoints)
		}
	}
	if endpoints == nil {
		endpoints = s.expectedEndpoints(b)
	}
	v.Endpoints = endpoints
	v.Status.PendingRestart = []string{}
	if ss != nil {
		v.Status.PendingRestart = pendingRestart(ss.applied, restartKeys(b))
	}
	v.Protocols = s.protocolViews(b)
	for _, u := range b.users {
		v.Users = append(v.Users, UserView{Username: u.Username, Role: u.Role})
	}
	for _, t := range b.targets {
		var types []string
		_ = json.Unmarshal([]byte(t.EventTypesJson), &types)
		v.Targets = append(v.Targets, TargetRef{ID: t.ID, Name: t.Name, EventTypes: nonNil(types)})
	}
	values := b.values()
	for _, st := range b.streams {
		sv := StreamView{Name: st.Stream, AssetID: st.AssetID, RenditionStatus: "missing"}
		if set, err := b.model.StreamFor(st.Stream, values); err == nil {
			sv.Codec, sv.FPS, sv.GOP, sv.Bitrate = set.Codec, set.FPS, set.GOP, set.Bitrate
			sv.Resolution = fmt.Sprintf("%dx%d", set.Width, set.Height)
		}
		if st.RenditionID.Valid {
			sv.RenditionID = st.RenditionID.String
			if r, err := s.store.R().GetRendition(ctx, st.RenditionID.String); err == nil {
				sv.RenditionStatus = r.Status
				sv.RenditionError = r.Error
			}
		}
		v.Streams = append(v.Streams, sv)
	}
	if m, ok := s.metrics.Latest(b.cam.ID); ok && domain.CameraState(v.Status.State).Active() {
		v.Metrics = metricsView(m)
	}
	return v
}

// expectedEndpoints lists where a stopped camera will answer.
func (s *Service) expectedEndpoints(b *cameraBundle) []EndpointView {
	var eps []ipcEndpoint
	for _, p := range b.protos {
		if !store.Bool(p.Enabled) || p.Port == 0 {
			continue
		}
		eps = append(eps, ipcEndpoint{instance: p.EngineKey, socket: "main", port: int(p.Port)})
	}
	ip := b.ip()
	if ip == "" {
		ip = "127.0.0.1"
	}
	return s.endpointViewsFrom(b, ip, eps)
}

type ipcEndpoint struct {
	instance, socket, network string
	port                      int
}

func (s *Service) endpointViewsFrom(b *cameraBundle, ip string, eps []ipcEndpoint) []EndpointView {
	out := []EndpointView{}
	for _, ep := range eps {
		section, ok := b.doc.Engines[ep.instance]
		if !ok {
			continue
		}
		name, _, err := profile.EngineName(section)
		if err != nil {
			continue
		}
		ev := EndpointView{Instance: ep.instance, Engine: name, Port: ep.port}
		switch name {
		case "rtsp":
			if ep.socket != "main" && ep.socket != "rtsp" {
				continue
			}
			var cfg struct {
				Paths map[string]string `json:"paths"`
			}
			_ = json.Unmarshal(section, &cfg)
			path := cfg.Paths["main"]
			ev.Protocol = "rtsp"
			ev.URL = "rtsp://" + hostPort(ip, ep.port, 554) + path
		case "http-api":
			ev.Protocol = "http"
			ev.URL = "http://" + hostPort(ip, ep.port, 80) + "/"
		default:
			continue
		}
		out = append(out, ev)
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Instance < out[j].Instance })
	return out
}

func hostPort(ip string, port, def int) string {
	if port == def {
		return ip
	}
	return ip + ":" + strconv.Itoa(port)
}

// UpdateCamera changes name, autostart, tags or targets. Targets reach a
// running camera immediately.
func (s *Service) UpdateCamera(ctx context.Context, actor Actor, id string, in UpdateCameraInput) (*CameraView, error) {
	b, err := s.loadBundle(ctx, id)
	if err != nil {
		return nil, err
	}
	name := b.cam.Name
	if in.Name != nil {
		name = strings.TrimSpace(*in.Name)
		if err := domain.ValidateCameraName(name); err != nil {
			return nil, err
		}
		if other, err := s.store.R().CameraIDByName(ctx, name); err == nil && other != id {
			return nil, domain.Conflict("name", "a camera named %q already exists", name)
		}
	}
	autostart := store.Bool(b.cam.Autostart)
	if in.Autostart != nil {
		autostart = *in.Autostart
	}
	tags := b.cam.TagsJson
	if in.Tags != nil {
		raw, _ := json.Marshal(nonNil(*in.Tags))
		tags = string(raw)
	}
	if in.TargetIDs != nil {
		for _, t := range *in.TargetIDs {
			if _, err := s.store.R().GetTarget(ctx, t); err != nil {
				return nil, domain.Invalid("target_ids", "target %s does not exist", t)
			}
		}
	}
	var netw *resolvedNetwork
	if in.Network != nil {
		nin := *in.Network
		if nin.MAC == "" && !nin.DefaultMAC && !nin.VendorOUI {
			nin.MAC = b.net.Mac
		}
		n, err := s.resolveNetwork(ctx, id, b.doc, nin)
		if err != nil {
			return nil, err
		}
		if err := s.checkUniqueNetwork(ctx, id, n); err != nil {
			return nil, err
		}
		netw = &n
	}
	err = s.store.Tx(ctx, func(q *db.Queries) error {
		if err := q.UpdateCameraMeta(ctx, db.UpdateCameraMetaParams{ID: id, Name: name, Autostart: store.Int(autostart), TagsJson: tags, UpdatedAt: time.Now().UnixMilli()}); err != nil {
			return err
		}
		if netw != nil {
			dns, _ := json.Marshal(nonNil(netw.dns))
			if err := q.UpdateCameraNetwork(ctx, db.UpdateCameraNetworkParams{
				CameraID: id, ParentIf: netw.parent, Mac: netw.mac, Ip: netw.ip, Netmask: netw.netmask, Gateway: netw.gateway, DnsJson: string(dns),
			}); err != nil {
				return err
			}
		}
		if in.TargetIDs != nil {
			if err := q.DeleteCameraTargets(ctx, id); err != nil {
				return err
			}
			for _, t := range dedupe(*in.TargetIDs) {
				if err := q.InsertCameraTarget(ctx, db.InsertCameraTargetParams{CameraID: id, TargetID: t, EventTypesJson: "[]", OverridesJson: "{}"}); err != nil {
					return err
				}
			}
		}
		return nil
	})
	if err != nil {
		if store.IsUnique(err) {
			return nil, domain.Conflict("name", "a camera with this name, IP or MAC already exists")
		}
		return nil, err
	}
	s.audit(ctx, actor, "camera.update", "camera", id, in)
	if in.TargetIDs != nil {
		s.reloadTargets(ctx, id)
	}
	view, err := s.GetCamera(ctx, id)
	if err == nil {
		s.pub.Publish("cameras", "updated", view)
	}
	return view, err
}

// DeleteCamera stops a camera and removes it with its events.
func (s *Service) DeleteCamera(ctx context.Context, actor Actor, id string) error {
	cam, err := s.store.R().GetCamera(ctx, id)
	if err != nil {
		return store.NotFound(err)
	}
	lock := s.opLock(id)
	lock.Lock()
	defer lock.Unlock()
	s.cancelRetry(id)
	s.mu.Lock()
	ss := s.sessions[id]
	s.mu.Unlock()
	if ss != nil {
		ss.stop("camera deleted")
		<-ss.done
	}
	_ = s.rt.Destroy(ctx, id)
	if _, err := s.store.W().DeleteCamera(ctx, id); err != nil {
		return err
	}
	s.metrics.Remove(id)
	s.audit(ctx, actor, "camera.delete", "camera", id, map[string]string{"name": cam.Name})
	s.pub.Publish("cameras", "deleted", map[string]string{"id": id})
	return nil
}

// CameraConfig returns the camera's native parameters.
func (s *Service) CameraConfig(ctx context.Context, id string) ([]ParamView, error) {
	b, err := s.loadBundle(ctx, id)
	if err != nil {
		return nil, err
	}
	return paramViews(b), nil
}

func paramViews(b *cameraBundle) []ParamView {
	values := b.values()
	meta := map[string]db.CameraState{}
	for _, st := range b.state {
		meta[st.Key] = st
	}
	out := make([]ParamView, 0, len(b.doc.State))
	for _, key := range profile.SortedKeys(b.doc.State) {
		p := b.doc.State[key]
		pv := ParamView{Key: key, Type: p.Type, Value: values[key], Default: p.Default, Values: p.Values, Min: p.Min, Max: p.Max,
			Bind: p.Bind, Effective: p.Bind != "", Description: p.Description, Origin: "profile"}
		if st, ok := meta[key]; ok {
			pv.Origin = st.Origin
			pv.UpdatedAt = store.Time(st.UpdatedAt)
		}
		out = append(out, pv)
	}
	return out
}

// UpdateCameraConfig changes native parameters from the panel or the API.
// The last change wins, whatever its origin (RN-08).
func (s *Service) UpdateCameraConfig(ctx context.Context, actor Actor, id string, in map[string]any) ([]ParamView, error) {
	b, err := s.loadBundle(ctx, id)
	if err != nil {
		return nil, err
	}
	if len(in) == 0 {
		return nil, domain.Invalid("", "no parameters to change")
	}
	v := &domain.ValidationError{}
	coerced := map[string]any{}
	for k, val := range in {
		p, ok := b.doc.State[k]
		if !ok {
			v.Add(k, "unknown parameter")
			continue
		}
		cv, err := profile.Coerce(p, val)
		if err != nil {
			v.Add(k, "%v", err)
			continue
		}
		coerced[k] = cv
	}
	if err := v.Err(); err != nil {
		return nil, err
	}
	changes := make([]engine.Change, 0, len(coerced))
	origin := engine.Origin{Kind: engine.OriginPanel, IP: actor.IP}
	for _, k := range profile.SortedKeys(coerced) {
		changes = append(changes, engine.Change{Key: k, Value: coerced[k], Bind: b.doc.State[k].Bind, Origin: origin})
	}
	if err := s.persistChanges(ctx, id, changes, "panel"); err != nil {
		return nil, err
	}
	s.audit(ctx, actor, "camera.config", "camera", id, coerced)
	if ss := s.session(id); ss != nil && ss.active() {
		if err := ss.conn().Request(ctx, ipc.TypeStateSet, ipc.StateSet{Values: coerced, Origin: origin}, nil); err != nil {
			s.log.Warn("could not apply config to the running camera", "camera", id, "error", err)
		}
	}
	s.afterChanges(id, changes)
	return s.CameraConfig(ctx, id)
}

// persistChanges stores parameter changes with their origin.
func (s *Service) persistChanges(ctx context.Context, id string, changes []engine.Change, origin string) error {
	now := time.Now().UnixMilli()
	return s.store.Tx(ctx, func(q *db.Queries) error {
		for _, c := range changes {
			raw, _ := json.Marshal(c.Value)
			if err := q.UpsertCameraState(ctx, db.UpsertCameraStateParams{CameraID: id, Key: c.Key, ValueJson: string(raw), Origin: origin, UpdatedAt: now}); err != nil {
				return err
			}
		}
		return nil
	})
}

// afterChanges applies the effects of bound parameters: media.* changes
// regenerate the stream in place (RN-09).
func (s *Service) afterChanges(id string, changes []engine.Change) {
	media := false
	for _, c := range changes {
		if strings.HasPrefix(c.Bind, "media.") {
			media = true
		}
	}
	s.pub.Publish("camera:"+id, "config", changes)
	if media {
		go s.regenerateStreams(id)
	}
}

// reloadTargets sends a running camera its new targets.
func (s *Service) reloadTargets(ctx context.Context, id string) {
	ss := s.session(id)
	if ss == nil || !ss.active() {
		return
	}
	b, err := s.loadBundle(ctx, id)
	if err != nil {
		return
	}
	targets, err := s.ipcTargets(b)
	if err != nil {
		return
	}
	_ = ss.conn().Request(ctx, ipc.TypeReload, ipc.Reload{Targets: targets}, nil)
}

// cameraIDs lists the IDs of every camera.
func (s *Service) cameraIDs(ctx context.Context) ([]db.Camera, error) {
	return s.store.R().ListCameras(ctx)
}

func (s *Service) setDesired(ctx context.Context, id string, d domain.DesiredState) error {
	return s.store.W().SetCameraDesired(ctx, db.SetCameraDesiredParams{ID: id, DesiredState: string(d), UpdatedAt: time.Now().UnixMilli()})
}

func (s *Service) saveStatus(ctx context.Context, id string, st domain.CameraState, reason string, started, now time.Time) error {
	err := s.store.W().UpsertCameraStatus(ctx, db.UpsertCameraStatusParams{
		CameraID: id, ActualState: string(st), Reason: reason, StartedAt: store.NullMillis(started),
		LastHeartbeat: sql.NullInt64{}, UpdatedAt: now.UnixMilli(),
	})
	if store.IsForeignKey(err) {
		return nil // camera deleted meanwhile
	}
	return err
}

func nodeInfo() (netctl.NodeInfo, error) { return netctl.ReadNodeInfo() }
