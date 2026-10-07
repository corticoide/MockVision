// Package netctl gives cameras their place on the network. On Linux, the
// network helper (the only privileged process) creates a network namespace
// per camera with a macvlan or ipvlan interface on the node's physical NIC,
// probes the LAN for the camera's MAC and IP before using them, installs a
// firewall that only lets the camera reach the node's event targets, and
// starts the camera process inside it with its sockets already open. A DHCP
// camera asks for its lease itself, unprivileged; the helper only sets the
// address the service hands it. The helper accepts a closed set of
// validated requests and never runs shell commands.
//
// For development without privileges, LocalRuntime runs cameras as plain
// processes on 127.0.0.1.
package netctl

import (
	"context"
	"fmt"
	"net"
	"net/netip"
	"regexp"

	"github.com/corticoide/mockvision/backend/internal/domain"
)

// SocketSpec is a socket the helper opens inside the camera namespace.
type SocketSpec struct {
	Instance string `json:"instance"`
	Name     string `json:"name"`
	Network  string `json:"network"`
	Port     int    `json:"port"`
}

// CameraSpec is a validated request to create a camera.
type CameraSpec struct {
	ID     string `json:"id"`
	Netns  string `json:"netns"`
	Mode   string `json:"mode"` // macvlan or ipvlan (D27)
	Parent string `json:"parent"`
	// MAC is the camera's own MAC; an ipvlan camera uses the parent's.
	MAC string `json:"mac"`
	// IPMode is static, with IP, Prefix and Gateway, or dhcp: the camera
	// then asks for a lease itself and the service sets the address it
	// gets with SetAddress (D24).
	IPMode  string `json:"ip_mode"`
	IP      string `json:"ip,omitempty"`
	Prefix  int    `json:"prefix,omitempty"`
	Gateway string `json:"gateway,omitempty"`
	// Hostname is what a DHCP camera tells servers about itself.
	Hostname string `json:"hostname,omitempty"`
	// Force starts the camera even if another device answers on its IP
	// or MAC (D14); the conflict is only logged.
	Force bool `json:"force,omitempty"`
	// Firewall limits what the camera may reach (nil: no firewall).
	Firewall *Firewall    `json:"firewall,omitempty"`
	Sockets  []SocketSpec `json:"sockets"`
	// SkipProbe disables the ARP and MAC probes; never set by the service,
	// only by tests on isolated links.
	SkipProbe bool `json:"skip_probe,omitempty"`
}

// DHCP reports whether the camera gets its address from a DHCP server.
func (s *CameraSpec) DHCP() bool { return s.IPMode == string(domain.IPDHCP) }

// Firewall is what a camera may open connections to: the event targets of
// the node and its DNS servers. Everything else it starts is dropped;
// answers to its clients and the traffic of its own ports pass.
type Firewall struct {
	Allow []Destination `json:"allow"`
	DNS   []string      `json:"dns"`
}

// Destination is a host and port a camera may connect to; AnyPort opens
// every port of the host, as FTP's passive data connections need.
type Destination struct {
	IP    string `json:"ip"`
	Port  int    `json:"port"`
	Proto string `json:"proto"` // tcp or udp
}

// AnyPort is the port of a destination open on every port.
const AnyPort = 0

// maxFirewallRules bounds the destinations of a firewall.
const maxFirewallRules = 512

// Validate checks every destination.
func (f *Firewall) Validate() error {
	if len(f.Allow) > maxFirewallRules {
		return fmt.Errorf("too many firewall destinations")
	}
	if len(f.DNS) > domain.MaxDNS {
		return fmt.Errorf("too many DNS servers")
	}
	for _, d := range f.Allow {
		if a, err := netip.ParseAddr(d.IP); err != nil || !a.Is4() {
			return fmt.Errorf("invalid firewall address %q", d.IP)
		}
		if d.Port < AnyPort || d.Port > 65535 {
			return fmt.Errorf("invalid firewall port %d", d.Port)
		}
		if d.Proto != "tcp" && d.Proto != "udp" {
			return fmt.Errorf("invalid firewall protocol %q", d.Proto)
		}
	}
	for _, d := range f.DNS {
		if a, err := netip.ParseAddr(d); err != nil || !a.Is4() {
			return fmt.Errorf("invalid DNS server %q", d)
		}
	}
	return nil
}

// AddressSpec gives a DHCP camera the address it leased, or the profile's
// factory address when no server answered (D24). The address is probed
// first, unless forced.
type AddressSpec struct {
	ID      string `json:"id"`
	IP      string `json:"ip"`
	Prefix  int    `json:"prefix"`
	Gateway string `json:"gateway,omitempty"`
	Force   bool   `json:"force,omitempty"`
}

// Validate checks the address.
func (a *AddressSpec) Validate() error {
	if !idPattern.MatchString(a.ID) {
		return fmt.Errorf("invalid camera id %q", a.ID)
	}
	id := domain.NetIdentity{Mode: domain.NetMacvlan, MAC: "02:00:00:00:00:01", IPMode: domain.IPStatic, Prefix: a.Prefix}
	var err error
	if id.IP, err = netip.ParseAddr(a.IP); err != nil {
		return fmt.Errorf("invalid IP %q", a.IP)
	}
	if a.Gateway != "" {
		if id.Gateway, err = netip.ParseAddr(a.Gateway); err != nil {
			return fmt.Errorf("invalid gateway %q", a.Gateway)
		}
	}
	return id.Validate()
}

// BridgeSpec turns on or off the node's access to its own macvlan cameras
// (D26): a macvlan interface of the node on the parent, with a route to
// each camera. Off by default, since it changes the node's network.
type BridgeSpec struct {
	Enabled bool   `json:"enabled"`
	Parent  string `json:"parent,omitempty"`
}

// BridgeState reports the bridge.
type BridgeState struct {
	Enabled   bool   `json:"enabled"`
	Interface string `json:"interface,omitempty"`
	Parent    string `json:"parent,omitempty"`
	// NodeIPs are the node's addresses on the parent the bridge routes
	// from; the service sets the bridge again when they change.
	NodeIPs []string `json:"node_ips,omitempty"`
	Error   string   `json:"error,omitempty"`
}

var (
	idPattern     = regexp.MustCompile(`^[0-9A-HJKMNP-TV-Z]{26}$`)
	netnsPattern  = regexp.MustCompile(`^sim-[a-z0-9-]{1,40}$`)
	ifacePattern  = regexp.MustCompile(`^[A-Za-z0-9._@-]{1,15}$`)
	socketPattern = regexp.MustCompile(`^[a-z0-9_-]{1,32}$`)
	// hostnamePattern is an RFC 1123 label.
	hostnamePattern = regexp.MustCompile(`^[A-Za-z0-9]([A-Za-z0-9-]{0,61}[A-Za-z0-9])?$`)
)

// Validate checks every field: the helper runs privileged and trusts
// nothing it did not check itself.
func (s *CameraSpec) Validate() error {
	if !idPattern.MatchString(s.ID) {
		return fmt.Errorf("invalid camera id %q", s.ID)
	}
	if !netnsPattern.MatchString(s.Netns) {
		return fmt.Errorf("invalid namespace name %q", s.Netns)
	}
	if s.Mode != string(domain.NetMacvlan) && s.Mode != string(domain.NetIPvlan) {
		return fmt.Errorf("network mode %q is not supported", s.Mode)
	}
	if !ifacePattern.MatchString(s.Parent) {
		return fmt.Errorf("invalid parent interface %q", s.Parent)
	}
	if s.IPMode == "" {
		s.IPMode = string(domain.IPStatic)
	}
	id := domain.NetIdentity{Mode: domain.NetMode(s.Mode), MAC: s.MAC, IPMode: domain.IPMode(s.IPMode), Prefix: s.Prefix}
	var err error
	switch id.IPMode {
	case domain.IPStatic:
		if id.IP, err = netip.ParseAddr(s.IP); err != nil {
			return fmt.Errorf("invalid IP %q", s.IP)
		}
		if s.Gateway != "" {
			if id.Gateway, err = netip.ParseAddr(s.Gateway); err != nil {
				return fmt.Errorf("invalid gateway %q", s.Gateway)
			}
		}
	case domain.IPDHCP:
		if s.IP != "" || s.Gateway != "" {
			return fmt.Errorf("a DHCP camera gets its address from the lease")
		}
		if !hostnamePattern.MatchString(s.Hostname) {
			return fmt.Errorf("invalid DHCP host name %q", s.Hostname)
		}
	default:
		return fmt.Errorf("invalid IP mode %q", s.IPMode)
	}
	if err := id.Validate(); err != nil {
		return err
	}
	if s.Firewall != nil {
		if err := s.Firewall.Validate(); err != nil {
			return err
		}
	}
	if len(s.Sockets) > 16 {
		return fmt.Errorf("too many sockets")
	}
	seen := map[string]bool{}
	for _, so := range s.Sockets {
		if !socketPattern.MatchString(so.Instance) || !socketPattern.MatchString(so.Name) {
			return fmt.Errorf("invalid socket name %q/%q", so.Instance, so.Name)
		}
		if so.Network != "tcp" && so.Network != "udp" {
			return fmt.Errorf("invalid socket network %q", so.Network)
		}
		if so.Port < 1 || so.Port > 65535 {
			return fmt.Errorf("invalid port %d", so.Port)
		}
		key := fmt.Sprintf("%s/%d", so.Network, so.Port)
		if seen[key] {
			return fmt.Errorf("port %s is requested twice", key)
		}
		seen[key] = true
	}
	return nil
}

// LaunchSpec is what the service asks a runtime to start.
type LaunchSpec struct {
	Camera CameraSpec
}

// Launched is a started camera process.
type Launched struct {
	// Conn is the service end of the camera's private IPC socket.
	Conn  net.Conn
	PID   int
	Netns string
	// IP is where the camera answers: its own address, or 127.0.0.1 in
	// local mode. A DHCP camera has none until SetAddress.
	IP string
	// MAC is the MAC the camera uses on the LAN: its own with macvlan, the
	// parent's with ipvlan.
	MAC string
	// Firewall reports whether the camera's firewall is in place.
	Firewall bool
}

// Exit reports a camera process that ended.
type Exit struct {
	CameraID string `json:"camera_id"`
	PID      int    `json:"pid"`
	Code     int    `json:"code"`
	Signal   string `json:"signal,omitempty"`
}

// Kinds of runtime.
const (
	// KindNetns runs cameras in network namespaces through the helper.
	KindNetns = "netns"
	// KindLocal runs cameras as plain processes on 127.0.0.1, for
	// development without privileges.
	KindLocal = "local"
)

// Runtime starts and destroys camera processes.
type Runtime interface {
	// Kind is KindNetns or KindLocal.
	Kind() string
	Launch(ctx context.Context, spec LaunchSpec) (*Launched, error)
	// Destroy stops the process and removes the namespace; it is
	// idempotent.
	Destroy(ctx context.Context, cameraID string) error
	// Live lists the camera IDs whose process or namespace exists.
	Live(ctx context.Context) ([]string, error)
	// Exits reports processes that ended on their own.
	Exits() <-chan Exit
	// SetAddress gives a running DHCP camera its address.
	SetAddress(ctx context.Context, a AddressSpec) error
	// SetFirewall replaces the firewall of a running camera.
	SetFirewall(ctx context.Context, cameraID string, f Firewall) error
	// SetBridge turns the node's access to its cameras on or off (D26).
	SetBridge(ctx context.Context, b BridgeSpec) (BridgeState, error)
}

// Error codes returned by runtimes.
const (
	CodeIPInUse     = "ip_in_use"
	CodeMACInUse    = "mac_in_use"
	CodeNoInterface = "no_interface"
	// CodeParentBusy: the parent already carries interfaces of the other
	// kind. The kernel gives a NIC macvlan or ipvlan children, not both,
	// and the node bridge is a macvlan.
	CodeParentBusy   = "parent_busy"
	CodeUnsupported  = "unsupported"
	CodeInvalid      = "invalid"
	CodeInternal     = "internal"
	CodeAlreadyExist = "exists"
)

// Error is a runtime failure with a stable code.
type Error struct {
	Code    string `json:"code"`
	Message string `json:"message"`
}

func (e *Error) Error() string { return e.Message }

func errorf(code, format string, args ...any) *Error {
	return &Error{Code: code, Message: fmt.Sprintf(format, args...)}
}
