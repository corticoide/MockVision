// Package engine defines the MockVision engine contract, version 1.
//
// An engine implements one protocol role for one camera: serving RTSP,
// answering a vendor HTTP API, pushing events, and so on. Engines built into
// MockVision implement this interface in-process; external plugins implement
// the same contract over gRPC on a Unix socket (see sdk/proto). The contract
// version is an integer; each engine carries its own semver version and
// profiles request engines by range (engine: http-api@^1).
package engine

import (
	"context"
	"encoding/json"
	"net"
)

// Contract is the version of this contract.
const Contract = 1

// Role says whether an engine serves clients or sends data out.
type Role string

const (
	// RoleServer engines listen on the camera's sockets (rtsp, http-api).
	RoleServer Role = "server"
	// RoleClient engines send data from the camera (http-push, mqtt-publish).
	RoleClient Role = "client"
)

// SocketSpec declares a socket an engine needs. Sockets are opened by the
// node before the camera drops its privileges and handed to the engine, so
// engines never need privileges to listen on ports such as 80 or 554.
type SocketSpec struct {
	Name        string `json:"name"`
	Network     string `json:"network"` // tcp or udp
	DefaultPort int    `json:"default_port"`
}

// Descriptor is what Describe returns.
type Descriptor struct {
	Name         string          `json:"name"`
	Version      string          `json:"version"`
	Contract     int             `json:"contract"`
	Role         Role            `json:"role"`
	ConfigSchema json.RawMessage `json:"config_schema,omitempty"`
	Sockets      []SocketSpec    `json:"sockets,omitempty"`
	Emits        []string        `json:"emits,omitempty"`
	Delivers     []string        `json:"delivers,omitempty"`
}

// Problem is a validation finding. Path is a JSON pointer relative to the
// engine's section of the profile. Line, when set, is the line inside a
// multi-line value such as a template.
type Problem struct {
	Path    string `json:"path"`
	Message string `json:"message"`
	Line    int    `json:"line,omitempty"`
	Warning bool   `json:"warning,omitempty"`
}

// Identity is the identity of the camera an engine runs for.
type Identity struct {
	CameraID       string `json:"camera_id"`
	Name           string `json:"name"`
	IP             string `json:"ip"`
	MAC            string `json:"mac"`
	Serial         string `json:"serial"`
	Vendor         string `json:"vendor"`
	Model          string `json:"model"`
	Firmware       string `json:"firmware"`
	ProfileID      string `json:"profile_id"`
	ProfileVersion string `json:"profile_version"`
}

// User is a camera account used to authenticate clients.
type User struct {
	Username string `json:"username"`
	Password string `json:"password"`
	Role     string `json:"role"`
}

// StartInput is everything an engine receives to start.
type StartInput struct {
	Identity Identity
	// Instance is the key of the engine instance in the profile, e.g. "http".
	Instance string
	// Config is the resolved profile section of the instance.
	Config json.RawMessage
	// Port is the port assigned to the instance for this camera.
	Port int
	// Listeners and PacketConns hold the sockets opened for the engine,
	// keyed by SocketSpec.Name.
	Listeners   map[string]net.Listener
	PacketConns map[string]net.PacketConn
	// Host gives access to the camera's services, its accounts among them.
	Host Host
}

// Engine is the contract every engine implements.
type Engine interface {
	// Describe returns the engine's name, version, role and requirements.
	Describe() Descriptor
	// Validate checks the engine's section of a profile at import time.
	Validate(config json.RawMessage) []Problem
	// Start runs the engine with its identity, config and open sockets.
	Start(ctx context.Context, in StartInput) error
	// Reload applies a new config without restarting the camera.
	Reload(ctx context.Context, config json.RawMessage) error
	// Health reports state and metrics.
	Health() Health
	// Stop shuts the engine down before ctx expires.
	Stop(ctx context.Context) error
}

// Factory creates a fresh engine instance.
type Factory func() Engine

// HealthState is the engine state reported in heartbeats.
type HealthState string

const (
	HealthOK       HealthState = "ok"
	HealthDegraded HealthState = "degraded"
	HealthFailed   HealthState = "failed"
	HealthStopped  HealthState = "stopped"
)

// Health is an engine's state and counters.
type Health struct {
	State    HealthState `json:"state"`
	Detail   string      `json:"detail,omitempty"`
	Clients  int         `json:"clients"`
	Requests uint64      `json:"requests"`
	Errors   uint64      `json:"errors"`
	BytesIn  uint64      `json:"bytes_in"`
	BytesOut uint64      `json:"bytes_out"`
}
