package domain

import "fmt"

// AdmissionLimits are the node policies checked before creating or starting
// a camera (RN-16, D04, D91).
type AdmissionLimits struct {
	MaxCameras    int     // configurable, 100 by default
	MaxRAMPercent float64 // projected RAM ceiling, 85 by default
	MaxCPUPercent float64 // sustained CPU ceiling, 80 by default
}

// DefaultAdmissionLimits returns the documented defaults.
func DefaultAdmissionLimits() AdmissionLimits {
	return AdmissionLimits{MaxCameras: 100, MaxRAMPercent: 85, MaxCPUPercent: 80}
}

// NodeUsage is the measured state of the node.
type NodeUsage struct {
	Cameras    int     // cameras defined on the node
	MemTotal   uint64  // bytes
	MemUsed    uint64  // bytes, excluding reclaimable cache
	CPUPercent float64 // sustained usage of the whole node, 0-100
	CPUCount   int
}

// CameraCost is the estimated cost of one running camera. It starts from a
// static estimate and is corrected with what running cameras really use.
type CameraCost struct {
	RAM        uint64  // bytes
	CPUPercent float64 // percent of one core
}

// Admission codes returned in RejectedError.Code.
const (
	RejectMaxCameras = "max_cameras"
	RejectRAM        = "ram"
	RejectCPU        = "cpu"
)

// AdmitCreate decides whether a new camera may be created.
func AdmitCreate(l AdmissionLimits, u NodeUsage, c CameraCost) error {
	if l.MaxCameras > 0 && u.Cameras >= l.MaxCameras {
		return &RejectedError{
			Code:   RejectMaxCameras,
			Reason: fmt.Sprintf("the node already has %d cameras, the configured maximum is %d", u.Cameras, l.MaxCameras),
		}
	}
	return AdmitStart(l, u, c)
}

// AdmitStart decides whether one more camera may start running.
func AdmitStart(l AdmissionLimits, u NodeUsage, c CameraCost) error {
	if u.MemTotal > 0 && l.MaxRAMPercent > 0 {
		projected := float64(u.MemUsed+c.RAM) / float64(u.MemTotal) * 100
		if projected > l.MaxRAMPercent {
			return &RejectedError{
				Code: RejectRAM,
				Reason: fmt.Sprintf("projected RAM usage would be %.1f%% (%s of %s), above the %.0f%% limit",
					projected, HumanBytes(u.MemUsed+c.RAM), HumanBytes(u.MemTotal), l.MaxRAMPercent),
			}
		}
	}
	if l.MaxCPUPercent > 0 {
		cpus := u.CPUCount
		if cpus < 1 {
			cpus = 1
		}
		projected := u.CPUPercent + c.CPUPercent/float64(cpus)
		if projected > l.MaxCPUPercent {
			return &RejectedError{
				Code:   RejectCPU,
				Reason: fmt.Sprintf("sustained CPU usage would be %.1f%%, above the %.0f%% limit", projected, l.MaxCPUPercent),
			}
		}
	}
	return nil
}

// HumanBytes formats a byte count with binary units.
func HumanBytes(n uint64) string {
	const unit = 1024
	if n < unit {
		return fmt.Sprintf("%d B", n)
	}
	div, exp := uint64(unit), 0
	for m := n / unit; m >= unit; m /= unit {
		div *= unit
		exp++
	}
	return fmt.Sprintf("%.1f %ciB", float64(n)/float64(div), "KMGTPE"[exp])
}
