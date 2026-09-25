package domain

import (
	"errors"
	"net/netip"
	"strings"
	"testing"
	"time"
)

func TestLifecycleTransitions(t *testing.T) {
	cases := []struct {
		from, to CameraState
		ok       bool
	}{
		{StateStopped, StateProvisioning, true},
		{StateStopped, StateRunning, false},
		{StateProvisioning, StateStarting, true},
		{StateProvisioning, StateError, true},
		{StateStarting, StateRunning, true},
		{StateRunning, StateDegraded, true},
		{StateDegraded, StateRunning, true},
		{StateRunning, StateStopping, true},
		{StateStopping, StateStopped, true},
		{StateError, StateProvisioning, true},
		{StateError, StateStopped, true},
		{StateRunning, StateProvisioning, false},
		{StateStopped, StateStopped, false},
	}
	for _, c := range cases {
		if got := c.from.CanTransition(c.to); got != c.ok {
			t.Errorf("%s -> %s = %v, want %v", c.from, c.to, got, c.ok)
		}
	}
	for s := range transitions {
		for _, next := range transitions[s] {
			if !next.Valid() {
				t.Errorf("transition %s -> %s targets an unknown state", s, next)
			}
		}
	}
}

func TestCameraName(t *testing.T) {
	for _, ok := range []string{"Front door", "Cámara 1", strings.Repeat("a", 64)} {
		if err := ValidateCameraName(ok); err != nil {
			t.Errorf("%q: unexpected error %v", ok, err)
		}
	}
	for _, bad := range []string{"", " lead", "trail ", strings.Repeat("a", 65), "tab\there"} {
		if err := ValidateCameraName(bad); err == nil {
			t.Errorf("%q: expected an error", bad)
		}
	}
}

func TestSlug(t *testing.T) {
	cases := []struct {
		in   string
		max  int
		want string
	}{
		{"Front Door", 40, "front-door"},
		{"Cámara Peaje #2", 40, "camara-peaje-2"},
		{"--x--", 40, "x"},
		{"Ñandú", 40, "nandu"},
		{"a very long camera!", 11, "a-very-long"},
		{"a very long camera!", 7, "a-very"},
	}
	for _, c := range cases {
		if got := Slug(c.in, c.max); got != c.want {
			t.Errorf("Slug(%q, %d) = %q, want %q", c.in, c.max, got, c.want)
		}
	}
}

func TestNetIdentityValidate(t *testing.T) {
	base := NetIdentity{
		Mode:    NetMacvlan,
		MAC:     "02:11:22:33:44:55",
		IPMode:  IPStatic,
		IP:      netip.MustParseAddr("192.168.1.50"),
		Prefix:  24,
		Gateway: netip.MustParseAddr("192.168.1.1"),
	}
	if err := base.Validate(); err != nil {
		t.Fatalf("valid identity rejected: %v", err)
	}
	bad := []func(n *NetIdentity){
		func(n *NetIdentity) { n.Mode = "bridge" },
		func(n *NetIdentity) { n.MAC = "01:00:5e:00:00:01" },
		func(n *NetIdentity) { n.IP = netip.MustParseAddr("192.168.1.0") },
		func(n *NetIdentity) { n.IP = netip.MustParseAddr("192.168.1.255") },
		func(n *NetIdentity) { n.IP = netip.MustParseAddr("127.0.0.1") },
		func(n *NetIdentity) { n.Gateway = netip.MustParseAddr("10.0.0.1") },
		func(n *NetIdentity) { n.Gateway = n.IP },
		func(n *NetIdentity) { n.Prefix = 31 },
		func(n *NetIdentity) { n.IPMode = IPDHCP },
	}
	for i, mutate := range bad {
		n := base
		mutate(&n)
		var verr *ValidationError
		if err := n.Validate(); !errors.As(err, &verr) {
			t.Errorf("case %d: expected a validation error, got %v", i, err)
		}
	}
}

func TestDeriveMAC(t *testing.T) {
	a := DeriveMAC("01J8Z3QK000000000000000000", nil)
	b := DeriveMAC("01J8Z3QK000000000000000000", nil)
	c := DeriveMAC("01J8Z3QK000000000000000001", nil)
	if a.String() != b.String() {
		t.Fatal("MAC must be stable for the same camera ID")
	}
	if a.String() == c.String() {
		t.Fatal("different cameras should get different MACs")
	}
	if a[0]&0x02 == 0 || a[0]&0x01 != 0 {
		t.Fatalf("MAC %s must be locally administered unicast", a)
	}
	oui, err := ParseOUI("1C:C3:16")
	if err != nil {
		t.Fatal(err)
	}
	v := DeriveMAC("x", oui)
	if !strings.HasPrefix(v.String(), "1c:c3:16:") {
		t.Fatalf("OUI not applied: %s", v)
	}
	if _, err := ParseMAC(a.String()); err != nil {
		t.Fatalf("derived MAC does not parse: %v", err)
	}
}

func TestMasks(t *testing.T) {
	for in, want := range map[string]int{"255.255.255.0": 24, "24": 24, "/16": 16, "255.255.254.0": 23} {
		got, err := MaskToPrefix(in)
		if err != nil || got != want {
			t.Errorf("MaskToPrefix(%q) = %d, %v; want %d", in, got, err, want)
		}
	}
	for _, bad := range []string{"", "255.0.255.0", "33", "abc"} {
		if _, err := MaskToPrefix(bad); err == nil {
			t.Errorf("MaskToPrefix(%q) should fail", bad)
		}
	}
	if PrefixToMask(20) != "255.255.240.0" {
		t.Errorf("PrefixToMask(20) = %s", PrefixToMask(20))
	}
	if got := LastAddr(netip.MustParsePrefix("10.1.2.0/23")); got.String() != "10.1.3.255" {
		t.Errorf("LastAddr = %s", got)
	}
}

func TestEventTypes(t *testing.T) {
	for _, ok := range []string{"line_crossing", "lpr", "custom:door_open"} {
		if !ValidEventType(ok) {
			t.Errorf("%q should be valid", ok)
		}
	}
	for _, bad := range []string{"", "fire", "custom:", "custom:Bad Name"} {
		if ValidEventType(bad) {
			t.Errorf("%q should be invalid", bad)
		}
	}
}

func TestValidateTarget(t *testing.T) {
	ok := Target{Name: "Backend", Type: TargetHTTP, URL: "http://10.0.0.5:9000/events", Method: "POST"}
	if err := ValidateTarget(ok); err != nil {
		t.Fatal(err)
	}
	for i, bad := range []Target{
		{Name: "", Type: TargetHTTP, URL: "http://x/", Method: "POST"},
		{Name: "a", Type: "mqtt", URL: "http://x/", Method: "POST"},
		{Name: "a", Type: TargetHTTP, URL: "ftp://x/", Method: "POST"},
		{Name: "a", Type: TargetHTTP, URL: "http://u:p@x/", Method: "POST"},
		{Name: "a", Type: TargetHTTP, URL: "http://x/", Method: "DELETE"},
	} {
		if err := ValidateTarget(bad); err == nil {
			t.Errorf("case %d should fail", i)
		}
	}
}

func TestParseResolution(t *testing.T) {
	r, err := ParseResolution("1920x1080")
	if err != nil || r.Width != 1920 || r.Height != 1080 || r.String() != "1920x1080" {
		t.Fatalf("got %+v, %v", r, err)
	}
	for _, bad := range []string{"1920*1080", "0x0", "1921x1080", "99999x2"} {
		if _, err := ParseResolution(bad); err == nil {
			t.Errorf("%q should fail", bad)
		}
	}
}

func TestCameraUsers(t *testing.T) {
	ok := []CameraUser{{Username: "admin", Password: "ms1234", Role: "admin"}, {Username: "viewer", Password: "x", Role: "viewer"}}
	if err := ValidateCameraUsers(ok); err != nil {
		t.Fatal(err)
	}
	noAdmin := []CameraUser{{Username: "viewer", Password: "x", Role: "viewer"}}
	if err := ValidateCameraUsers(noAdmin); err == nil {
		t.Fatal("RN-11: a camera needs an admin")
	}
	dup := []CameraUser{{Username: "admin", Password: "a", Role: "admin"}, {Username: "ADMIN", Password: "b", Role: "admin"}}
	if err := ValidateCameraUsers(dup); err == nil {
		t.Fatal("duplicated usernames must fail")
	}
}

func TestAdmission(t *testing.T) {
	limits := DefaultAdmissionLimits()
	usage := NodeUsage{Cameras: 3, MemTotal: 8 << 30, MemUsed: 2 << 30, CPUPercent: 10, CPUCount: 4}
	cost := CameraCost{RAM: 25 << 20, CPUPercent: 1}
	if err := AdmitCreate(limits, usage, cost); err != nil {
		t.Fatalf("should admit: %v", err)
	}

	limits.MaxCameras = 3
	var rej *RejectedError
	if err := AdmitCreate(limits, usage, cost); !errors.As(err, &rej) || rej.Code != RejectMaxCameras {
		t.Fatalf("expected max_cameras rejection, got %v", err)
	}
	if err := AdmitStart(limits, usage, cost); err != nil {
		t.Fatalf("starting an existing camera ignores the count: %v", err)
	}

	limits = DefaultAdmissionLimits()
	usage.MemUsed = 7 << 30 // 87.5%
	if err := AdmitCreate(limits, usage, cost); !errors.As(err, &rej) || rej.Code != RejectRAM {
		t.Fatalf("expected RAM rejection, got %v", err)
	}
	usage.MemUsed = 1 << 30
	usage.CPUPercent = 80
	if err := AdmitStart(limits, usage, cost); !errors.As(err, &rej) || rej.Code != RejectCPU {
		t.Fatalf("expected CPU rejection, got %v", err)
	}
}

func TestRetryBackoff(t *testing.T) {
	if RetryBackoff(1) != 2*time.Second || RetryBackoff(2) != 4*time.Second {
		t.Fatal("unexpected backoff")
	}
	if RetryBackoff(20) != time.Minute {
		t.Fatal("backoff must be capped")
	}
	if HumanBytes(25<<20) != "25.0 MiB" {
		t.Fatalf("HumanBytes = %s", HumanBytes(25<<20))
	}
}
