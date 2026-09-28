package app

import (
	"cmp"
	"context"
	"crypto/sha256"
	"database/sql"
	"encoding/json"
	"fmt"
	"net/netip"
	"sort"
	"strconv"
	"strings"
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
	// Mode is macvlan (the default: own MAC) or ipvlan (the node's MAC,
	// for Wi-Fi and switches that limit MACs) (D27).
	Mode string `json:"mode"`
	// IPMode is static (the default) or dhcp: the camera leases its
	// address, and takes the profile's factory one if no server answers
	// (D24).
	IPMode string `json:"ip_mode"`
	// Force starts the camera even if its IP or MAC answers on the LAN
	// (D14); nil keeps the current choice.
	Force      *bool    `json:"force"`
	Parent     string   `json:"parent"`
	MAC        string   `json:"mac"`
	DefaultMAC bool     `json:"default_mac"`
	VendorOUI  bool     `json:"vendor_oui"`
	IP         string   `json:"ip"`
	Netmask    string   `json:"netmask"`
	Gateway    string   `json:"gateway"`
	DNS        []string `json:"dns"`
}

// withDefaults keeps the current mode, addressing and force where an edit
// or a copy leaves them out.
func (in NetworkInput) withDefaults(cur db.CameraNetwork) NetworkInput {
	in.Mode = cmp.Or(in.Mode, cur.Mode)
	in.IPMode = cmp.Or(in.IPMode, cur.IpMode)
	if in.Force == nil {
		f := store.Bool(cur.Force)
		in.Force = &f
	}
	return in
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
	Codec      string `json:"codec"`
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

func (s *Service) profileDoc(p db.Profile) (*profile.Document, error) {
	key := p.ProfileID + "@" + p.Version
	if d, ok := s.profiles.Load(key); ok {
		return d.(*profile.Document), nil
	}
	doc, err := profile.DecodeJSON([]byte(p.ResolvedJson))
	if err != nil {
		return nil, fmt.Errorf("stored profile %s is invalid: %w", key, err)
	}
	s.profiles.Store(key, doc)
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

// loadBundles loads every camera for the list, with one query per table
// instead of one per camera, and each profile once.
func (s *Service) loadBundles(ctx context.Context) ([]*cameraBundle, error) {
	q := s.store.R()
	cams, err := q.ListCameras(ctx)
	if err != nil {
		return nil, err
	}
	nets, err := q.ListCameraNetworks(ctx)
	if err != nil {
		return nil, err
	}
	states, err := q.ListAllCameraState(ctx)
	if err != nil {
		return nil, err
	}
	protos, err := q.ListAllCameraProtocols(ctx)
	if err != nil {
		return nil, err
	}
	users, err := q.ListAllCameraUsers(ctx)
	if err != nil {
		return nil, err
	}
	streams, err := q.ListAllCameraStreams(ctx)
	if err != nil {
		return nil, err
	}
	targets, err := q.ListAllCameraTargets(ctx)
	if err != nil {
		return nil, err
	}
	statuses, err := q.ListCameraStatuses(ctx)
	if err != nil {
		return nil, err
	}
	byID := make(map[string]*cameraBundle, len(cams))
	profiles := map[string]*cameraBundle{} // a bundle of each profile, to share it
	out := make([]*cameraBundle, 0, len(cams))
	for _, c := range cams {
		b := &cameraBundle{cam: c}
		key := c.ProfileID + "@" + c.ProfileVersion
		if p := profiles[key]; p != nil {
			b.prof, b.doc, b.model = p.prof, p.doc, p.model
		} else {
			if b.prof, err = q.GetProfileByRef(ctx, db.GetProfileByRefParams{ProfileID: c.ProfileID, Version: c.ProfileVersion}); err != nil {
				return nil, err
			}
			if b.doc, err = s.profileDoc(b.prof); err != nil {
				return nil, err
			}
			b.model = profile.NewModel(b.doc)
			profiles[key] = b
		}
		byID[c.ID] = b
		out = append(out, b)
	}
	for _, n := range nets {
		if b := byID[n.CameraID]; b != nil {
			b.net = n
		}
	}
	for _, st := range states {
		if b := byID[st.CameraID]; b != nil {
			b.state = append(b.state, st)
		}
	}
	for _, p := range protos {
		if b := byID[p.CameraID]; b != nil {
			b.protos = append(b.protos, p)
		}
	}
	for _, u := range users {
		if b := byID[u.CameraID]; b != nil {
			b.users = append(b.users, u)
		}
	}
	for _, st := range streams {
		if b := byID[st.CameraID]; b != nil {
			b.streams = append(b.streams, st)
		}
	}
	for _, t := range targets {
		if b := byID[t.CameraID]; b != nil {
			b.targets = append(b.targets, db.ListCameraTargetsRow(t))
		}
	}
	for _, st := range statuses {
		if b := byID[st.CameraID]; b != nil {
			b.status = &st
		}
	}
	return out, nil
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

// dns returns the camera's own DNS servers; none means the lease's or the
// node's.
func (b *cameraBundle) dns() []string {
	var dns []string
	_ = json.Unmarshal([]byte(b.net.DnsJson), &dns)
	return dns
}

// ip is where the camera answers: its static address, or the last one a
// DHCP camera held.
func (b *cameraBundle) ip() string {
	if b.net.IpMode == string(domain.IPDHCP) && b.status != nil {
		return b.status.Ip
	}
	return b.net.Ip
}

// CreateCamera validates and stores a new camera, then starts it if asked.
func (s *Service) CreateCamera(ctx context.Context, actor Actor, in CreateCameraInput) (*CameraView, error) {
	in.Name = strings.TrimSpace(in.Name)
	if err := domain.ValidateCameraName(in.Name); err != nil {
		return nil, err
	}
	cleanTags, err := domain.NormalizeTags(in.Tags)
	if err != nil {
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
		cu[i] = domain.CameraUser{Username: u.Username, Password: u.Password, Role: cmp.Or(u.Role, "admin")}
	}
	if err := domain.ValidateCameraUsers(cu); err != nil {
		return nil, err
	}

	// State: profile defaults plus the stream settings chosen in the wizard,
	// written through the parameters bound to them.
	values := model.Defaults()
	overrides := map[string]any{}
	if in.Stream.Codec != "" {
		if err := s.setBound(doc, model, values, overrides, "media.main.codec", in.Stream.Codec, "stream.codec"); err != nil {
			return nil, err
		}
	}
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
	// Every stream of the profile: the wizard sets the main one, the sub
	// and third streams start from their defaults, all with one picture.
	names := streamNames(doc)
	settings := make([]profile.StreamSettings, len(names))
	for i, name := range names {
		if settings[i], err = model.StreamFor(name, values); err != nil {
			return nil, domain.Invalid("stream", "%v", err)
		}
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

	rends := make([]db.Rendition, len(names))
	for i, name := range names {
		if rends[i], err = s.ensureRenditionRow(ctx, asset, settings[i]); err != nil {
			return nil, streamError(name, err)
		}
	}
	autostart := true
	if in.Autostart != nil {
		autostart = *in.Autostart
	}
	tags, _ := json.Marshal(cleanTags)
	now := time.Now().UnixMilli()
	c := newCamera{
		row: db.InsertCameraParams{
			ID: id, Name: in.Name, ProfileID: prof.ProfileID, ProfileVersion: prof.Version, Serial: serialFor(id, doc.Identity.Serial),
			DesiredState: string(domain.DesiredStopped), Autostart: store.Int(autostart), TagsJson: string(tags), CreatedAt: now, UpdatedAt: now,
		},
		netw: netw, users: cu,
	}
	for _, key := range profile.SortedKeys(values) {
		origin := "profile"
		if _, ok := overrides[key]; ok {
			origin = "panel"
		}
		raw, _ := json.Marshal(values[key])
		c.state = append(c.state, db.CameraState{Key: key, ValueJson: string(raw), Origin: origin})
	}
	for _, inst := range profile.SortedKeys(doc.Engines) {
		port, err := s.instancePort(doc, inst)
		if err != nil {
			return nil, err
		}
		c.protos = append(c.protos, db.CameraProtocol{EngineKey: inst, Enabled: 1, Port: int64(port), OptionsJson: "{}"})
	}
	for i, name := range names {
		c.streams = append(c.streams, db.CameraStream{Stream: name, AssetID: asset.ID, RenditionID: store.NullString(rends[i].ID)})
	}
	for _, t := range dedupe(in.TargetIDs) {
		c.targets = append(c.targets, db.ListCameraTargetsRow{ID: t, EventTypesJson: "[]", OverridesJson: "{}"})
	}
	err = s.insertCamera(ctx, c)
	if err != nil {
		if store.IsUnique(err) {
			return nil, domain.Conflict("name", "a camera with this name, IP or MAC already exists")
		}
		return nil, err
	}
	for _, r := range rends {
		s.encodeAsync(r.ID)
	}
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

// newCamera is what is stored for a camera being created or copied.
type newCamera struct {
	row     db.InsertCameraParams
	netw    resolvedNetwork
	state   []db.CameraState
	protos  []db.CameraProtocol
	users   []domain.CameraUser
	streams []db.CameraStream
	targets []db.ListCameraTargetsRow
}

// insertCamera stores a new camera and all it has in one transaction; the
// camera IDs of the parts come from its row.
func (s *Service) insertCamera(ctx context.Context, c newCamera) error {
	id, now := c.row.ID, c.row.CreatedAt
	return s.store.Tx(ctx, func(q *db.Queries) error {
		if err := q.InsertCamera(ctx, c.row); err != nil {
			return err
		}
		if err := q.InsertCameraNetwork(ctx, networkRow(id, c.netw)); err != nil {
			return err
		}
		for _, st := range c.state {
			if err := q.UpsertCameraState(ctx, db.UpsertCameraStateParams{CameraID: id, Key: st.Key, ValueJson: st.ValueJson, Origin: st.Origin, UpdatedAt: now}); err != nil {
				return err
			}
		}
		for _, p := range c.protos {
			if err := q.InsertCameraProtocol(ctx, db.InsertCameraProtocolParams{CameraID: id, EngineKey: p.EngineKey, Enabled: p.Enabled, Port: p.Port, OptionsJson: p.OptionsJson}); err != nil {
				return err
			}
		}
		if err := s.insertUsers(ctx, q, id, c.users); err != nil {
			return err
		}
		for _, st := range c.streams {
			if err := q.UpsertCameraStream(ctx, db.UpsertCameraStreamParams{CameraID: id, Stream: st.Stream, AssetID: st.AssetID, RenditionID: st.RenditionID}); err != nil {
				return err
			}
		}
		for _, t := range c.targets {
			if err := q.InsertCameraTarget(ctx, db.InsertCameraTargetParams{CameraID: id, TargetID: t.ID, EventTypesJson: t.EventTypesJson, OverridesJson: t.OverridesJson}); err != nil {
				return err
			}
		}
		return q.UpsertCameraStatus(ctx, db.UpsertCameraStatusParams{CameraID: id, ActualState: string(domain.StateStopped), UpdatedAt: now})
	})
}

// insertUsers stores a camera's accounts, their passwords sealed to it.
func (s *Service) insertUsers(ctx context.Context, q *db.Queries, id string, users []domain.CameraUser) error {
	for _, u := range users {
		if err := q.InsertCameraUser(ctx, db.InsertCameraUserParams{
			ID: ulid.Make().String(), CameraID: id, Username: u.Username,
			PasswordEnc: s.box.Seal([]byte(u.Password), "camera_users:"+id+":"+u.Username), Role: u.Role,
		}); err != nil {
			return err
		}
	}
	return nil
}

// networkRow is how a camera's network is stored.
func networkRow(id string, n resolvedNetwork) db.InsertCameraNetworkParams {
	dns, _ := json.Marshal(nonNil(n.dns))
	return db.InsertCameraNetworkParams{
		CameraID: id, Mode: n.mode, ParentIf: n.parent, Mac: n.mac, IpMode: n.ipMode, Ip: n.ip,
		Netmask: n.netmask, Gateway: n.gateway, DnsJson: string(dns), Force: store.Int(n.force),
	}
}

// networkUpdate is networkRow for an existing camera.
func networkUpdate(id string, n resolvedNetwork) db.UpdateCameraNetworkParams {
	r := networkRow(id, n)
	return db.UpdateCameraNetworkParams{
		CameraID: r.CameraID, Mode: r.Mode, ParentIf: r.ParentIf, Mac: r.Mac, IpMode: r.IpMode, Ip: r.Ip,
		Netmask: r.Netmask, Gateway: r.Gateway, DnsJson: r.DnsJson, Force: r.Force,
	}
}

func (s *Service) setBound(doc *profile.Document, m *profile.Model, values, overrides map[string]any, canon string, v any, field string) error {
	key, ok := m.NativeFor(canon)
	if !ok {
		// Asking for what the profile fixes anyway is not a change.
		if cur, ok := m.Canon(canon, values); ok && fmt.Sprint(cur) == fmt.Sprint(v) {
			return nil
		}
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
	mode, ipMode                      string
	force                             bool
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
	out := resolvedNetwork{mac: domain.DeriveMAC(id, oui).String(), mode: in.Mode, ipMode: in.IPMode, force: in.Force != nil && *in.Force}
	if out.mode == "" {
		out.mode = string(domain.NetMacvlan)
	}
	if out.ipMode == "" {
		out.ipMode = string(domain.IPStatic)
	}
	switch {
	case out.mode != string(domain.NetMacvlan) && out.mode != string(domain.NetIPvlan):
		return out, domain.Invalid("network.mode", "must be macvlan or ipvlan")
	case out.ipMode != string(domain.IPStatic) && out.ipMode != string(domain.IPDHCP):
		return out, domain.Invalid("network.ip_mode", "must be static or dhcp")
	case out.mode == string(domain.NetIPvlan) && out.ipMode == string(domain.IPDHCP):
		return out, domain.Invalid("network.ip_mode", "ipvlan cameras share the node's MAC and cannot use DHCP; give them a static IP")
	}
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
	if s.rt.Kind() == netctl.KindLocal {
		// Local mode: cameras answer on 127.0.0.1 with ephemeral ports.
		return out, nil
	}
	info, err := nodeInfo()
	if err != nil {
		return out, err
	}
	set := s.Settings(ctx)
	out.parent = cmp.Or(in.Parent, s.defaultParentFor(set))
	iface, ok := info.Lookup(out.parent)
	if !ok {
		return out, domain.Invalid("network.parent", "interface %q does not exist on this node", out.parent)
	}
	if iface.Wireless && out.mode == string(domain.NetMacvlan) {
		return out, domain.Invalid("network.mode", "%s is a Wi-Fi interface: access points refuse the extra MACs of macvlan cameras; use ipvlan", out.parent)
	}
	use, err := s.parentsInUse(ctx, set, id)
	if err != nil {
		return out, err
	}
	if msg := use[out.parent].modeConflict(out.parent, out.mode); msg != "" {
		return out, domain.Invalid("network.mode", "%s", msg)
	}
	if out.ipMode == string(domain.IPDHCP) {
		return out, nil
	}
	if strings.TrimSpace(in.IP) == "" {
		return out, domain.Invalid("network.ip", "is required")
	}
	ip, err := netip.ParseAddr(strings.TrimSpace(in.IP))
	if err != nil || !ip.Is4() {
		return out, domain.Invalid("network.ip", "must be an IPv4 address")
	}
	if name, ok := nodeInterfaceWith(info, ip); ok {
		return out, domain.Invalid("network.ip", "%s belongs to the node itself (%s)", ip, name)
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
		gw, _ = domain.GatewayIn(ip, prefix, info.DefaultGateway)
	}
	nid := domain.NetIdentity{Mode: domain.NetMode(out.mode), ParentIf: out.parent, MAC: out.mac, IPMode: domain.IPStatic, IP: ip, Prefix: prefix, Gateway: gw}
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
	bundles, err := s.loadBundles(ctx)
	if err != nil {
		return nil, err
	}
	rows, err := s.store.R().ListRenditionStates(ctx)
	if err != nil {
		return nil, err
	}
	rends := make(map[string]db.ListRenditionStatesRow, len(rows))
	for _, r := range rows {
		rends[r.ID] = r
	}
	lookup := func(id string) (db.ListRenditionStatesRow, bool) {
		r, ok := rends[id]
		return r, ok
	}
	out := make([]CameraView, 0, len(bundles))
	for _, b := range bundles {
		if v := s.cameraView(b, lookup); f.Match(v) {
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
	return s.cameraView(b, func(id string) (db.ListRenditionStatesRow, bool) {
		r, err := s.store.R().GetRendition(ctx, id)
		return db.ListRenditionStatesRow{ID: r.ID, Status: r.Status, Error: r.Error}, err == nil
	}), nil
}

// cameraView builds a camera's view; rendition tells the state of the
// renditions its streams use.
func (s *Service) cameraView(b *cameraBundle, rendition func(id string) (db.ListRenditionStatesRow, bool)) *CameraView {
	var tags []string
	_ = json.Unmarshal([]byte(b.cam.TagsJson), &tags)
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
			Netmask: b.net.Netmask, Prefix: prefix, Gateway: b.net.Gateway, DNS: nonNil(b.dns()), Force: store.Bool(b.net.Force)},
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
		v.Status.ReasonCode, v.Status.Reason = b.status.ReasonCode, b.status.Reason
		v.Status.IP, v.Status.IPSource = b.status.Ip, b.status.IpSource
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
		v.Status.ReasonCode, v.Status.Reason = st.reasonCode, st.reason
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
		v.Status.IP, v.Status.IPSource, v.Status.MAC, v.Status.Firewall = st.ip, string(st.ipSource), st.mac, st.firewall
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
	urls := map[string]string{}
	for _, e := range endpoints {
		for stream, url := range e.streams {
			if urls[stream] == "" {
				urls[stream] = url
			}
		}
	}
	values := b.values()
	for _, st := range b.streams {
		sv := StreamView{Name: st.Stream, URL: urls[st.Stream], AssetID: st.AssetID, RenditionStatus: "missing"}
		if set, err := b.model.StreamFor(st.Stream, values); err == nil {
			sv.Codec, sv.FPS, sv.GOP, sv.Bitrate = set.Codec, set.FPS, set.GOP, set.Bitrate
			sv.Resolution = fmt.Sprintf("%dx%d", set.Width, set.Height)
		}
		if st.RenditionID.Valid {
			sv.RenditionID = st.RenditionID.String
			if r, ok := rendition(st.RenditionID.String); ok {
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
			base := "rtsp://" + hostPort(ip, ep.port, 554)
			ev.Protocol = "rtsp"
			ev.streams = map[string]string{}
			for _, stream := range profile.SortedKeys(cfg.Paths) {
				ev.streams[stream] = base + cfg.Paths[stream]
			}
			ev.URL = ev.streams["main"]
			if ev.URL == "" && len(cfg.Paths) > 0 {
				ev.URL = ev.streams[profile.SortedKeys(cfg.Paths)[0]]
			}
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
		clean, err := domain.NormalizeTags(*in.Tags)
		if err != nil {
			return nil, err
		}
		raw, _ := json.Marshal(clean)
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
		n, err := s.resolveNetwork(ctx, id, b.doc, nin.withDefaults(b.net))
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
			if err := q.UpdateCameraNetwork(ctx, networkUpdate(id, *netw)); err != nil {
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
	// Runs after the unlock below: nothing about the camera stays in
	// memory once it is gone (audit B14).
	defer s.forgetCamera(id)
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
	s.pub.Forget("camera:" + id)
	return nil
}

// forgetCamera drops what the service keeps in memory about a camera.
func (s *Service) forgetCamera(id string) {
	if _, err := s.store.R().GetCamera(context.Background(), id); err == nil {
		return // the delete failed: the camera is still there
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	delete(s.opLocks, id)
	delete(s.exits, id)
	delete(s.retries, id)
	for name, owner := range s.netnsNames {
		if owner == id {
			delete(s.netnsNames, name)
		}
	}
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
	s.tellCamera(ctx, id, ipc.TypeStateSet, ipc.StateSet{Values: coerced, Origin: origin})
	s.afterChanges(id, changes)
	return s.CameraConfig(ctx, id)
}

// persistChanges stores parameter changes with their origin.
func (s *Service) persistChanges(ctx context.Context, id string, changes []engine.Change, origin string) error {
	return s.store.Tx(ctx, func(q *db.Queries) error { return upsertChanges(ctx, q, id, changes, origin) })
}

// upsertChanges writes parameter changes within a transaction.
func upsertChanges(ctx context.Context, q *db.Queries, id string, changes []engine.Change, origin string) error {
	now := time.Now().UnixMilli()
	for _, c := range changes {
		raw, _ := json.Marshal(c.Value)
		if err := q.UpsertCameraState(ctx, db.UpsertCameraStateParams{CameraID: id, Key: c.Key, ValueJson: string(raw), Origin: origin, UpdatedAt: now}); err != nil {
			return err
		}
	}
	return nil
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
		s.regenerateStreamsLater(id)
	}
}

// reloadTargets sends a running camera its targets, none included.
func (s *Service) reloadTargets(ctx context.Context, id string) {
	b, err := s.loadBundle(ctx, id)
	if err != nil {
		return
	}
	targets, err := s.ipcTargets(b)
	if err != nil {
		return
	}
	s.tellCamera(ctx, id, ipc.TypeReload, ipc.Reload{Targets: &targets})
}

// cameraIDs lists the IDs of every camera.
func (s *Service) cameraIDs(ctx context.Context) ([]db.Camera, error) {
	return s.store.R().ListCameras(ctx)
}

func (s *Service) setDesired(ctx context.Context, id string, d domain.DesiredState) error {
	return s.store.W().SetCameraDesired(ctx, db.SetCameraDesiredParams{ID: id, DesiredState: string(d), UpdatedAt: time.Now().UnixMilli()})
}

func (s *Service) saveStatus(ctx context.Context, id string, st domain.CameraState, code, reason string, started, now time.Time) error {
	err := s.store.W().UpsertCameraStatus(ctx, db.UpsertCameraStatusParams{
		CameraID: id, ActualState: string(st), ReasonCode: code, Reason: reason, StartedAt: store.NullMillis(started),
		LastHeartbeat: sql.NullInt64{}, UpdatedAt: now.UnixMilli(),
	})
	if store.IsForeignKey(err) {
		return nil // camera deleted meanwhile
	}
	return err
}

func nodeInfo() (netctl.NodeInfo, error) { return netctl.ReadNodeInfo() }
