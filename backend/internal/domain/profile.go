package domain

import (
	"regexp"
	"strconv"
	"time"
)

// VerificationLevel is the evidence backing a profile (D76, D88). It is
// computed by the importer, never declared.
type VerificationLevel string

const (
	LevelDraft      VerificationLevel = "draft"
	LevelDocumented VerificationLevel = "documented"
	LevelCaptured   VerificationLevel = "captured"
	LevelVerified   VerificationLevel = "verified"
)

// Profile is an immutable, installed profile version (RN-02).
type Profile struct {
	ID        string // row id
	PackageID string
	ProfileID string // vendor/model identifier, e.g. milesight/traffic-x
	Version   string
	Name      string
	Vendor    string
	Model     string
	Firmware  []string
	Level     VerificationLevel
	Archived  bool
	CreatedAt time.Time
}

var profileIDPattern = regexp.MustCompile(`^[a-z0-9][a-z0-9._-]{0,62}/[a-z0-9][a-z0-9._-]{0,62}$`)

// ValidProfileID reports whether id has the vendor/model form.
func ValidProfileID(id string) bool {
	return profileIDPattern.MatchString(id)
}

// Asset is a reusable media file (an image in v1).
type Asset struct {
	ID        string
	SHA256    string
	Kind      string
	MIME      string
	Width     int
	Height    int
	Size      int64
	Filename  string
	Builtin   bool
	CreatedAt time.Time
}

// RenditionStatus tracks the precoding of a rendition.
type RenditionStatus string

const (
	RenditionPending RenditionStatus = "pending"
	RenditionReady   RenditionStatus = "ready"
	RenditionFailed  RenditionStatus = "failed"
)

// Rendition is a stream precoded from an asset for one combination of codec,
// resolution, fps, GOP and bitrate. It is encoded once and cached.
type Rendition struct {
	ID      string
	AssetID string
	Codec   string
	Width   int
	Height  int
	FPS     int
	GOP     int
	Bitrate int // kbit/s
	SHA256  string
	Status  RenditionStatus
	Error   string
}

// Resolution is a frame size.
type Resolution struct {
	Width  int
	Height int
}

var resolutionPattern = regexp.MustCompile(`^([1-9][0-9]{1,4})x([1-9][0-9]{1,4})$`)

// ParseResolution parses "1920x1080".
func ParseResolution(s string) (Resolution, error) {
	m := resolutionPattern.FindStringSubmatch(s)
	if m == nil {
		return Resolution{}, Invalid("resolution", "must look like 1920x1080")
	}
	w, _ := strconv.Atoi(m[1])
	h, _ := strconv.Atoi(m[2])
	r := Resolution{Width: w, Height: h}
	if r.Width%2 != 0 || r.Height%2 != 0 {
		return Resolution{}, Invalid("resolution", "width and height must be even")
	}
	if r.Width > 7680 || r.Height > 4320 {
		return Resolution{}, Invalid("resolution", "must be at most 7680x4320")
	}
	return r, nil
}

func (r Resolution) String() string {
	return strconv.Itoa(r.Width) + "x" + strconv.Itoa(r.Height)
}
