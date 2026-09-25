// Package netctl gives cameras their place on the network. On Linux, the
// network helper (the only privileged process) creates a network namespace
// per camera with a macvlan interface on the node's physical NIC, probes the
// LAN with ARP before using an address, and starts the camera process inside
// it with its sockets already open. The helper accepts a closed set of
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
	ID      string       `json:"id"`
	Netns   string       `json:"netns"`
	Mode    string       `json:"mode"`
	Parent  string       `json:"parent"`
	MAC     string       `json:"mac"`
	IP      string       `json:"ip"`
	Prefix  int          `json:"prefix"`
	Gateway string       `json:"gateway,omitempty"`
	Sockets []SocketSpec `json:"sockets"`
	// SkipProbe disables the ARP probe; never set by the service, only by
	// tests on isolated links.
	SkipProbe bool `json:"skip_probe,omitempty"`
}

var (
	idPattern     = regexp.MustCompile(`^[0-9A-HJKMNP-TV-Z]{26}$`)
	netnsPattern  = regexp.MustCompile(`^sim-[a-z0-9-]{1,40}$`)
	ifacePattern  = regexp.MustCompile(`^[A-Za-z0-9._@-]{1,15}$`)
	socketPattern = regexp.MustCompile(`^[a-z0-9_-]{1,32}$`)
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
	if s.Mode != string(domain.NetMacvlan) {
		return fmt.Errorf("network mode %q is not supported", s.Mode)
	}
	if !ifacePattern.MatchString(s.Parent) {
		return fmt.Errorf("invalid parent interface %q", s.Parent)
	}
	id := domain.NetIdentity{Mode: domain.NetMacvlan, MAC: s.MAC, IPMode: domain.IPStatic, Prefix: s.Prefix}
	var err error
	if id.IP, err = netip.ParseAddr(s.IP); err != nil {
		return fmt.Errorf("invalid IP %q", s.IP)
	}
	if s.Gateway != "" {
		if id.Gateway, err = netip.ParseAddr(s.Gateway); err != nil {
			return fmt.Errorf("invalid gateway %q", s.Gateway)
		}
	}
	if err := id.Validate(); err != nil {
		return err
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
	// local mode.
	IP string
}

// Exit reports a camera process that ended.
type Exit struct {
	CameraID string `json:"camera_id"`
	PID      int    `json:"pid"`
	Code     int    `json:"code"`
	Signal   string `json:"signal,omitempty"`
}

// Runtime starts and destroys camera processes.
type Runtime interface {
	// Kind is "netns" for the network helper or "local" for development.
	Kind() string
	Launch(ctx context.Context, spec LaunchSpec) (*Launched, error)
	// Destroy stops the process and removes the namespace; it is
	// idempotent.
	Destroy(ctx context.Context, cameraID string) error
	// Live lists the camera IDs whose process or namespace exists.
	Live(ctx context.Context) ([]string, error)
	// Exits reports processes that ended on their own.
	Exits() <-chan Exit
}

// Error codes returned by runtimes.
const (
	CodeIPInUse      = "ip_in_use"
	CodeNoInterface  = "no_interface"
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
