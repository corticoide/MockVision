package domain

import (
	"strings"
	"time"
	"unicode"
	"unicode/utf8"
)

// CameraState is the actual lifecycle state of a camera, as observed from
// the kernel and the camera process.
type CameraState string

const (
	StateStopped      CameraState = "stopped"
	StateProvisioning CameraState = "provisioning"
	StateStarting     CameraState = "starting"
	StateRunning      CameraState = "running"
	StateDegraded     CameraState = "degraded"
	StateRestarting   CameraState = "restarting"
	StateStopping     CameraState = "stopping"
	StateError        CameraState = "error"
)

// transitions follows the camera lifecycle diagram. Running and Starting can
// also fall into Error when the process dies or stops answering heartbeats.
var transitions = map[CameraState][]CameraState{
	StateStopped:      {StateProvisioning},
	StateProvisioning: {StateStarting, StateError, StateStopping},
	StateStarting:     {StateRunning, StateError, StateStopping},
	StateRunning:      {StateDegraded, StateRestarting, StateStopping, StateError},
	StateDegraded:     {StateRunning, StateStopping, StateError},
	StateRestarting:   {StateStarting, StateStopping, StateError},
	StateStopping:     {StateStopped, StateError},
	StateError:        {StateProvisioning, StateStopping, StateStopped},
}

// Valid reports whether s is a known state.
func (s CameraState) Valid() bool {
	_, ok := transitions[s]
	return ok
}

// CanTransition reports whether the lifecycle allows going from s to next.
func (s CameraState) CanTransition(next CameraState) bool {
	for _, allowed := range transitions[s] {
		if allowed == next {
			return true
		}
	}
	return false
}

// Active reports whether the camera answers on the network.
func (s CameraState) Active() bool {
	return s == StateRunning || s == StateDegraded
}

// Settled reports whether the state is not a transient one.
func (s CameraState) Settled() bool {
	switch s {
	case StateStopped, StateRunning, StateDegraded, StateError:
		return true
	}
	return false
}

// DesiredState is what the user asked for; the reconciler closes the gap
// between it and the actual state.
type DesiredState string

const (
	DesiredRunning DesiredState = "running"
	DesiredStopped DesiredState = "stopped"
)

// Camera is a live instance of a profile version (RN-01: pinned version).
type Camera struct {
	ID             string
	Name           string
	ProfileID      string
	ProfileVersion string
	Serial         string
	Desired        DesiredState
	Autostart      bool
	Tags           []string
	CreatedAt      time.Time
	UpdatedAt      time.Time
}

// CameraStatus is the runtime view of a camera.
type CameraStatus struct {
	State         CameraState
	Reason        string
	StartedAt     time.Time
	LastHeartbeat time.Time
	UpdatedAt     time.Time
}

// MaxCameraNameLength bounds camera names.
const MaxCameraNameLength = 64

// ValidateCameraName checks a camera name. Names are unique per node (RN-05);
// uniqueness is enforced by the store.
func ValidateCameraName(name string) error {
	trimmed := strings.TrimSpace(name)
	switch {
	case trimmed == "":
		return Invalid("name", "is required")
	case trimmed != name:
		return Invalid("name", "must not start or end with spaces")
	case utf8.RuneCountInString(name) > MaxCameraNameLength:
		return Invalid("name", "must be at most %d characters", MaxCameraNameLength)
	}
	for _, r := range name {
		if unicode.IsControl(r) {
			return Invalid("name", "must not contain control characters")
		}
	}
	return nil
}

// Limits of camera tags.
const (
	MaxCameraTags = 20
	MaxTagLength  = 32
)

// NormalizeTags trims tags, drops empty ones and repeats (ignoring case,
// keeping the first spelling) and checks their limits.
func NormalizeTags(tags []string) ([]string, error) {
	out := make([]string, 0, len(tags))
	seen := map[string]bool{}
	for _, t := range tags {
		t = strings.TrimSpace(t)
		if t == "" || seen[strings.ToLower(t)] {
			continue
		}
		if utf8.RuneCountInString(t) > MaxTagLength {
			return nil, Invalid("tags", "%q is longer than %d characters", t, MaxTagLength)
		}
		if strings.IndexFunc(t, unicode.IsControl) >= 0 || strings.Contains(t, ",") {
			return nil, Invalid("tags", "%q must not contain commas or control characters", t)
		}
		seen[strings.ToLower(t)] = true
		out = append(out, t)
	}
	if len(out) > MaxCameraTags {
		return nil, Invalid("tags", "a camera can have at most %d tags", MaxCameraTags)
	}
	return out, nil
}

// Slug turns a name into a lowercase ASCII identifier made of letters,
// digits and dashes, at most max characters long. It is used for network
// namespace names such as sim-front-door.
func Slug(name string, max int) string {
	var b strings.Builder
	dash := false
	for _, r := range strings.ToLower(name) {
		switch {
		case r >= 'a' && r <= 'z', r >= '0' && r <= '9':
			b.WriteRune(r)
			dash = false
		case r == 'á' || r == 'à' || r == 'ä' || r == 'â':
			b.WriteByte('a')
			dash = false
		case r == 'é' || r == 'è' || r == 'ë' || r == 'ê':
			b.WriteByte('e')
			dash = false
		case r == 'í' || r == 'ì' || r == 'ï' || r == 'î':
			b.WriteByte('i')
			dash = false
		case r == 'ó' || r == 'ò' || r == 'ö' || r == 'ô':
			b.WriteByte('o')
			dash = false
		case r == 'ú' || r == 'ù' || r == 'ü' || r == 'û':
			b.WriteByte('u')
			dash = false
		case r == 'ñ':
			b.WriteByte('n')
			dash = false
		default:
			if !dash && b.Len() > 0 {
				b.WriteByte('-')
				dash = true
			}
		}
		if b.Len() >= max {
			break
		}
	}
	s := b.String()
	if len(s) > max {
		s = s[:max]
	}
	return strings.Trim(s, "-")
}

// RetryBackoff returns the wait before the given automatic restart attempt
// of a camera in Error (D13: growing wait, up to MaxAutoRetries attempts).
func RetryBackoff(attempt int) time.Duration {
	if attempt < 1 {
		attempt = 1
	}
	d := 2 * time.Second
	for i := 1; i < attempt; i++ {
		d *= 2
	}
	if d > time.Minute {
		d = time.Minute
	}
	return d
}

// MaxAutoRetries is the number of automatic restarts from Error (D13).
const MaxAutoRetries = 5
