// Package scraper observes a camera the user owns or is authorized to
// capture and compiles what it sees into a draft profile. It never changes
// the device: every probe is a read-only request from a closed whitelist
// (RN-17), paced by a per-device rate limit (D72), and the credentials,
// addresses and serial numbers it sees are removed before anything is
// compiled into a profile (RN-18).
package scraper

import (
	"strings"
	"time"
)

// DefaultPorts are the ports a camera usually answers, probed when a
// device lists none.
var DefaultPorts = []int{80, 443, 554, 8000, 8080, 8554, 2020, 37777, 161}

// Target is a device to probe: where it is and the credentials the user
// gave for its API. Nothing here is written anywhere a profile can reach.
type Target struct {
	Host     string
	Ports    []int
	Username string
	Password string
}

func (t Target) ports() []int {
	if len(t.Ports) > 0 {
		return t.Ports
	}
	return DefaultPorts
}

// Detected is what a read-only look at a device found: which ports answer
// and what the services say they are. It guides which programs apply; it
// is not a profile.
type Detected struct {
	At         time.Time  `json:"at"`
	OpenPorts  []int      `json:"open_ports"`
	Services   []Service  `json:"services"`
	Vendor     string     `json:"vendor,omitempty"`
	Model      string     `json:"model,omitempty"`
	Firmware   string     `json:"firmware,omitempty"`
	Reachable  bool       `json:"reachable"`
	Discovered []Announce `json:"discovered,omitempty"`
}

// Service is a protocol a port answers, and what it said about itself.
type Service struct {
	Port   int    `json:"port"`
	Proto  string `json:"proto"` // http, https, rtsp, onvif, snmp, unknown
	Server string `json:"server,omitempty"`
	Auth   string `json:"auth,omitempty"` // basic, digest, none
	Note   string `json:"note,omitempty"`
}

// Announce is a device that answered a passive discovery query on the LAN.
type Announce struct {
	From   string `json:"from"`
	Proto  string `json:"proto"` // ssdp, mdns, ws-discovery
	Server string `json:"server,omitempty"`
	URL    string `json:"url,omitempty"`
}

// guessVendor reads a vendor from a server banner. It only labels the
// detection; the profile's vendor comes from the capture, sanitized.
func guessVendor(banners ...string) string {
	all := strings.ToLower(strings.Join(banners, " "))
	for _, v := range []struct{ needle, name string }{
		{"milesight", "Milesight"}, {"dahua", "Dahua"}, {"hikvision", "Hikvision"},
		{"axis", "Axis"}, {"bosch", "Bosch"}, {"vivotek", "Vivotek"}, {"reolink", "Reolink"},
	} {
		if strings.Contains(all, v.needle) {
			return v.name
		}
	}
	return ""
}
