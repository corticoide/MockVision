package app

import (
	"context"
	"encoding/json"
	"net/netip"
	"strings"
	"time"

	"github.com/oklog/ulid/v2"

	"github.com/corticoide/mockvision/backend/internal/domain"
	"github.com/corticoide/mockvision/backend/internal/scraper"
	"github.com/corticoide/mockvision/backend/internal/store"
	"github.com/corticoide/mockvision/backend/internal/store/db"
)

// probeTimeout bounds a read-only look at a device.
const probeTimeout = 30 * time.Second

// DeviceView is a device registered for capture. The password never comes
// back; only whether one is set (RN-18).
type DeviceView struct {
	ID          string            `json:"id"`
	Name        string            `json:"name"`
	Host        string            `json:"host"`
	Ports       []int             `json:"ports"`
	Username    string            `json:"username,omitempty"`
	HasPassword bool              `json:"has_password"`
	Kind        string            `json:"kind"`
	Authorized  bool              `json:"authorized"`
	Detected    *scraper.Detected `json:"detected,omitempty"`
	CreatedAt   time.Time         `json:"created_at"`
	UpdatedAt   time.Time         `json:"updated_at"`
}

// DeviceInput registers or edits a device. Authorized must be set before
// any probe runs: it is the user confirming they own the device or may
// capture it (RN-17).
type DeviceInput struct {
	Name       string  `json:"name"`
	Host       string  `json:"host"`
	Ports      []int   `json:"ports"`
	Username   string  `json:"username"`
	Password   *string `json:"password"`
	Authorized bool    `json:"authorized"`
}

func deviceView(d db.Device) DeviceView {
	var ports []int
	_ = json.Unmarshal([]byte(d.PortsJson), &ports)
	if ports == nil {
		ports = []int{}
	}
	v := DeviceView{ID: d.ID, Name: d.Name, Host: d.Host, Ports: ports, Username: d.Username, HasPassword: len(d.SecretEnc) > 0,
		Kind: d.Kind, Authorized: store.Bool(d.Authorized), CreatedAt: store.Time(d.CreatedAt), UpdatedAt: store.Time(d.UpdatedAt)}
	if d.DetectedJson != "" && d.DetectedJson != "{}" {
		var det scraper.Detected
		if json.Unmarshal([]byte(d.DetectedJson), &det) == nil && det.Reachable {
			v.Detected = &det
		}
	}
	return v
}

// validateDevice checks a device's address and ports.
func (s *Service) validateDevice(in DeviceInput, v *domain.ValidationError) (string, []int) {
	host := strings.TrimSpace(in.Host)
	if host == "" {
		v.Add("host", "is required")
	} else if _, err := netip.ParseAddr(host); err != nil {
		// A name is allowed; a probe resolves it once and pins the address.
		if strings.ContainsAny(host, " /\\") || len(host) > 255 {
			v.Add("host", "must be an IP address or a host name")
		}
	}
	for _, p := range in.Ports {
		if p < 1 || p > 65535 {
			v.Add("ports", "must be between 1 and 65535")
			break
		}
	}
	if strings.TrimSpace(in.Name) == "" {
		v.Add("name", "is required")
	}
	return host, in.Ports
}

// deviceKind marks a device simulated when its host is one of this node's
// cameras (D75); otherwise it is a real device.
func (s *Service) deviceKind(ctx context.Context, host string) string {
	if _, err := s.store.R().CameraIDByIP(ctx, host); err == nil {
		return "simulated"
	}
	return "real"
}

// CreateDevice registers a device to capture.
func (s *Service) CreateDevice(ctx context.Context, actor Actor, in DeviceInput) (*DeviceView, error) {
	v := &domain.ValidationError{}
	host, ports := s.validateDevice(in, v)
	if err := v.Err(); err != nil {
		return nil, err
	}
	id := ulid.Make().String()
	var secret []byte
	if in.Password != nil && *in.Password != "" {
		secret = s.box.Seal([]byte(*in.Password), "devices:"+id)
	}
	portsJSON, _ := json.Marshal(intsOrEmpty(ports))
	now := time.Now().UnixMilli()
	err := s.store.W().InsertDevice(ctx, db.InsertDeviceParams{
		ID: id, Name: strings.TrimSpace(in.Name), Host: host, PortsJson: string(portsJSON), Username: strings.TrimSpace(in.Username),
		SecretEnc: secret, Kind: s.deviceKind(ctx, host), Authorized: store.Int(in.Authorized), DetectedJson: "{}", CreatedAt: now, UpdatedAt: now,
	})
	if err != nil {
		return nil, err
	}
	s.audit(ctx, actor, "device.register", "device", id, map[string]any{"host": host, "authorized": in.Authorized})
	return s.GetDevice(ctx, id)
}

// GetDevice returns a registered device.
func (s *Service) GetDevice(ctx context.Context, id string) (*DeviceView, error) {
	d, err := s.store.R().GetDevice(ctx, id)
	if err != nil {
		return nil, store.NotFound(err)
	}
	v := deviceView(d)
	return &v, nil
}

// ListDevices returns the registered devices, newest first.
func (s *Service) ListDevices(ctx context.Context) ([]DeviceView, error) {
	rows, err := s.store.R().ListDevices(ctx)
	if err != nil {
		return nil, err
	}
	out := make([]DeviceView, 0, len(rows))
	for _, d := range rows {
		out = append(out, deviceView(d))
	}
	return out, nil
}

// UpdateDevice changes a device; a nil password keeps the stored one and
// an empty string clears it.
func (s *Service) UpdateDevice(ctx context.Context, actor Actor, id string, in DeviceInput) (*DeviceView, error) {
	d, err := s.store.R().GetDevice(ctx, id)
	if err != nil {
		return nil, store.NotFound(err)
	}
	v := &domain.ValidationError{}
	host, ports := s.validateDevice(in, v)
	if err := v.Err(); err != nil {
		return nil, err
	}
	secret := d.SecretEnc
	if in.Password != nil {
		if *in.Password == "" {
			secret = nil
		} else {
			secret = s.box.Seal([]byte(*in.Password), "devices:"+id)
		}
	}
	portsJSON, _ := json.Marshal(intsOrEmpty(ports))
	err = s.store.W().UpdateDevice(ctx, db.UpdateDeviceParams{
		ID: id, Name: strings.TrimSpace(in.Name), Host: host, PortsJson: string(portsJSON), Username: strings.TrimSpace(in.Username),
		SecretEnc: secret, Kind: s.deviceKind(ctx, host), Authorized: store.Int(in.Authorized), UpdatedAt: time.Now().UnixMilli(),
	})
	if err != nil {
		return nil, err
	}
	s.audit(ctx, actor, "device.update", "device", id, map[string]any{"host": host, "authorized": in.Authorized})
	return s.GetDevice(ctx, id)
}

// DeleteDevice removes a device and its captures.
func (s *Service) DeleteDevice(ctx context.Context, actor Actor, id string) error {
	n, err := s.store.W().DeleteDevice(ctx, id)
	if err != nil {
		return err
	}
	if n == 0 {
		return domain.ErrNotFound
	}
	s.audit(ctx, actor, "device.delete", "device", id, nil)
	return nil
}

// target builds the probe target of a device, with its credentials.
func (s *Service) target(d db.Device) (scraper.Target, error) {
	var ports []int
	_ = json.Unmarshal([]byte(d.PortsJson), &ports)
	t := scraper.Target{Host: d.Host, Ports: ports, Username: d.Username}
	if len(d.SecretEnc) > 0 {
		pw, err := s.box.Open(d.SecretEnc, "devices:"+d.ID)
		if err != nil {
			return t, err
		}
		t.Password = string(pw)
	}
	return t, nil
}

// ProbeDevice looks at a device read-only and stores what it found: open
// ports and what each service says it is. It refuses until the device is
// marked authorized (RN-17).
func (s *Service) ProbeDevice(ctx context.Context, actor Actor, id string) (*DeviceView, error) {
	d, err := s.store.R().GetDevice(ctx, id)
	if err != nil {
		return nil, store.NotFound(err)
	}
	if !store.Bool(d.Authorized) {
		return nil, domain.Conflict("authorized", "confirm you own this device or are allowed to capture it before probing it")
	}
	t, err := s.target(d)
	if err != nil {
		return nil, err
	}
	pctx, cancel := context.WithTimeout(ctx, probeTimeout)
	defer cancel()
	det := scraper.NewProber(t, float64(s.scrapeRate())).Detect(pctx)
	raw, _ := json.Marshal(det)
	kind := s.deviceKind(ctx, d.Host)
	if err := s.store.W().SetDeviceDetected(ctx, db.SetDeviceDetectedParams{ID: id, DetectedJson: string(raw), Kind: kind, UpdatedAt: time.Now().UnixMilli()}); err != nil {
		return nil, err
	}
	s.audit(ctx, actor, "device.probe", "device", id, map[string]any{"open_ports": det.OpenPorts, "vendor": det.Vendor})
	return s.GetDevice(ctx, id)
}

// scrapeRate is how many requests a second a device gets (D72).
func (s *Service) scrapeRate() int {
	r := s.Settings(s.baseCtx).ScrapeRatePerSecond
	if r <= 0 {
		return scraper.DefaultRate
	}
	return r
}

// intsOrEmpty returns a non-nil slice so it serializes as [] not null.
func intsOrEmpty(v []int) []int {
	if v == nil {
		return []int{}
	}
	return v
}
