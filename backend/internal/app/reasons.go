package app

import (
	"errors"
	"fmt"

	"github.com/corticoide/mockvision/backend/internal/domain"
	"github.com/corticoide/mockvision/backend/internal/netctl"
)

// Reason codes explain a camera's state in a stable word the panel turns
// into the user's language; the reason text stays in English for logs and
// the API. The network helper's codes (netctl.CodeIPInUse and the rest)
// pass through unchanged.
const (
	ReasonDHCPWaiting  = "dhcp_waiting"    // leasing its address
	ReasonNoFactory    = "dhcp_no_factory" // no lease and no factory address
	ReasonFactoryInUse = "dhcp_factory_in_use"
	ReasonStream       = "stream"          // its streams could not be encoded
	ReasonConfig       = "config"          // its stored configuration is incomplete
	ReasonLaunch       = "launch"          // the helper could not start it
	ReasonExited       = "exited"          // the process ended
	ReasonNoHello      = "no_hello"        // the process never answered
	ReasonNotReady     = "not_ready"       // its engines did not start in time
	ReasonRejected     = "config_rejected" // the process refused its configuration
	ReasonCameraFailed = "camera_failed"   // the process reported a failure
	ReasonNoHeartbeat  = "no_heartbeat"    // it stopped answering
	ReasonAdmission    = "admission"       // the node had no room to start it
	ReasonLeaseRestart = "lease_restart"   // restarting on its new lease
	ReasonLeaseLost    = "lease_lost"      // restarting after losing its lease
)

// failure is why a camera stopped working: a reason code and its detail.
type failure struct {
	code string
	text string
}

func (f *failure) Error() string { return f.text }

func failed(code, format string, args ...any) *failure {
	return &failure{code: code, text: fmt.Sprintf(format, args...)}
}

// launchFailure keeps the network helper's code, so the panel can say an
// address or a MAC is in use, or a network card is busy.
func launchFailure(err error) *failure {
	var ne *netctl.Error
	if errors.As(err, &ne) {
		code := ne.Code
		if code == netctl.CodeInternal || code == netctl.CodeAlreadyExist {
			code = ReasonLaunch
		}
		return &failure{code: code, text: ne.Message}
	}
	var f *failure
	if errors.As(err, &f) {
		return f
	}
	var re *domain.RejectedError
	if errors.As(err, &re) {
		return &failure{code: ReasonAdmission, text: err.Error()}
	}
	return &failure{code: ReasonLaunch, text: err.Error()}
}

// retryable reports whether trying again later may start the camera. A
// busy network card, a kernel without ipvlan or a refused request stay so
// until someone changes the camera.
func retryable(code string) bool {
	switch code {
	case netctl.CodeParentBusy, netctl.CodeUnsupported, netctl.CodeInvalid, ReasonConfig:
		return false
	}
	return true
}
