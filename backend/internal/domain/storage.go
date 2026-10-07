package domain

import (
	"fmt"
	"net/url"
	"path"
	"slices"
	"strconv"
	"strings"
	"time"
)

// StorageKind is where a camera keeps its recordings.
type StorageKind string

const (
	// StorageNone keeps nothing.
	StorageNone StorageKind = "none"
	// StorageSD is the camera's simulated SD card (D68, D69): a directory
	// of the node with a quota.
	StorageSD StorageKind = "sd"
	// StorageNAS is a share the camera records to, NFS or SMB, for models
	// without an SD card.
	StorageNAS StorageKind = "nas"
)

// SDState is the state of a camera's SD card, as the device reports it.
type SDState string

const (
	SDPresent  SDState = "present"
	SDFull     SDState = "full"
	SDAbsent   SDState = "absent"
	SDError    SDState = "error"
	SDReadOnly SDState = "read_only"
)

// Storage faults force the SD card's states (D43, feature 10).
const (
	FaultSDMissing  FaultKind = "sd_missing"
	FaultSDError    FaultKind = "sd_error"
	FaultSDReadOnly FaultKind = "sd_read_only"
	FaultSDFull     FaultKind = "sd_full"
)

// Storage events: the device reports its card's state.
const (
	EventStorageMissing EventType = "storage_missing"
	EventStorageFailure EventType = "storage_failure"
	EventStorageFull    EventType = "storage_full"
)

// SDFaultStates are the states each storage fault forces.
var SDFaultStates = map[FaultKind]SDState{
	FaultSDMissing:  SDAbsent,
	FaultSDError:    SDError,
	FaultSDReadOnly: SDReadOnly,
	FaultSDFull:     SDFull,
}

// Limits of the simulated SD card, in MiB.
const (
	MinSDSize     = 64
	DefaultSDSize = 1024
	MaxSDSize     = 1 << 20 // 1 TiB
)

// Storage is where a camera keeps its recordings.
type Storage struct {
	Kind StorageKind
	// SizeMB is the SD card's capacity, its quota on the node.
	SizeMB int
	// Overwrite replaces the oldest recordings when the card is full, as
	// devices do by default (D69); off, recording stops.
	Overwrite bool
	// NASURL is nfs://host[:port]/export[/dir] or smb://host[:port]/share[/dir].
	NASURL      string
	NASUsername string
}

// ValidateStorage checks a camera's storage against what its profile
// offers: an SD card up to maxSDMB, or NAS protocols.
func ValidateStorage(s Storage, maxSDMB int, nas []string) error {
	v := &ValidationError{}
	switch s.Kind {
	case StorageNone:
	case StorageSD:
		switch {
		case maxSDMB <= 0:
			v.Add("kind", "the profile's model has no SD card")
		case s.SizeMB < MinSDSize || s.SizeMB > maxSDMB:
			v.Add("size_mb", "must be between %d and %d", MinSDSize, maxSDMB)
		}
	case StorageNAS:
		if len(nas) == 0 {
			v.Add("kind", "the profile's model does not record to a NAS")
			break
		}
		u, err := url.Parse(s.NASURL)
		switch {
		case err != nil || u.Hostname() == "":
			v.Add("nas_url", "must look like nfs://host/export or smb://host/share")
		case !slices.Contains(nas, u.Scheme):
			v.Add("nas_url", "the model records over %s", strings.Join(nas, " or "))
		case u.User != nil:
			v.Add("nas_url", "must not embed credentials; use username and password")
		case strings.Trim(u.Path, "/") == "":
			v.Add("nas_url", "names no export or share")
		default:
			if p := u.Port(); p != "" {
				if n, err := strconv.Atoi(p); err != nil || n < 1 || n > 65535 {
					v.Add("nas_url", "has an invalid port")
				}
			}
		}
	default:
		v.Add("kind", "must be none, sd or nas")
	}
	return v.Err()
}

// RecordingKind is what a recording holds.
type RecordingKind string

const (
	RecordingSnapshot RecordingKind = "snapshot"
	RecordingClip     RecordingKind = "clip"
)

// Recording is a file a camera recorded for an event.
type Recording struct {
	ID        string
	CameraID  string
	EventID   string
	EventType string
	Kind      RecordingKind
	// Name is its path inside the card or the share.
	Name     string
	Size     int64
	StartAt  time.Time
	EndAt    time.Time
	Location StorageKind
}

// RejectDisk is the admission code of a camera whose SD card does not fit
// on the node's disk (D91).
const RejectDisk = "disk"

// AdmitDisk decides whether an SD card of sizeMB fits: the free space of
// the node's disk must hold what the cards already promised and have not
// used yet, plus the new one.
func AdmitDisk(free, promised uint64, sizeMB int) error {
	need := uint64(sizeMB) << 20
	if free < promised+need {
		avail := uint64(0)
		if free > promised {
			avail = free - promised
		}
		return &RejectedError{
			Code:   RejectDisk,
			Reason: fmt.Sprintf("an SD card of %s does not fit: the disk has %s free for new cards", HumanBytes(need), HumanBytes(avail)),
		}
	}
	return nil
}

// MaxRecordingName bounds a recording's name.
const MaxRecordingName = 200

// RecordingName names a recording as cameras do: by day, then time and
// event, as 20261007/143000_motion_x7k2pq.jpg, the suffix taken from the
// event's ID.
func RecordingName(eventType, eventID string, at time.Time, ext string) string {
	at = at.UTC()
	suffix := strings.ToLower(eventID)
	if len(suffix) > 6 {
		suffix = suffix[len(suffix)-6:]
	}
	typ := strings.Map(func(r rune) rune {
		switch {
		case r >= 'a' && r <= 'z', r >= 'A' && r <= 'Z', r >= '0' && r <= '9', r == '_', r == '-':
			return r
		}
		return '-'
	}, eventType)
	if len(typ) > 48 {
		typ = typ[:48]
	}
	return fmt.Sprintf("%s/%s_%s_%s.%s", at.Format("20060102"), at.Format("150405"), typ, suffix, ext)
}

// ValidRecordingName accepts a relative slash path without empty or dot
// segments: a name that stays inside the card or the share.
func ValidRecordingName(name string) bool {
	if name == "" || len(name) > MaxRecordingName || strings.HasPrefix(name, "/") || strings.ContainsAny(name, "\\\x00") {
		return false
	}
	if path.Clean(name) != name {
		return false
	}
	for _, seg := range strings.Split(name, "/") {
		if seg == "" || seg == "." || seg == ".." {
			return false
		}
	}
	return true
}
