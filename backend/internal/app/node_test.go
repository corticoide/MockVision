package app

import (
	"strings"
	"testing"

	"github.com/corticoide/mockvision/backend/internal/netctl"
)

func TestPanelAccess(t *testing.T) {
	ifaces := []netctl.Interface{
		{Name: "lo", Up: true, Loopback: true, Addrs: []string{"127.0.0.1/8"}},
		{Name: "eth0", Up: true, Addrs: []string{"192.168.1.20/24", "fe80::1/64"}},
		{Name: "eth1", Up: false, Addrs: []string{"10.0.0.2/24"}},
		{Name: "wlan0", Up: true, Addrs: []string{"10.1.0.5/16"}},
	}
	all := panelAccess(":8080", ifaces)
	if !all.AllInterfaces || strings.Join(all.URLs, " ") != "http://192.168.1.20:8080 http://10.1.0.5:8080" {
		t.Fatalf("all interfaces: %+v", all)
	}
	if p := panelAccess("0.0.0.0:80", ifaces); !p.AllInterfaces || len(p.URLs) != 2 {
		t.Fatalf("0.0.0.0: %+v", p)
	}
	mgmt := panelAccess("192.168.1.20:8443", ifaces)
	if mgmt.AllInterfaces || strings.Join(mgmt.URLs, " ") != "http://192.168.1.20:8443" {
		t.Fatalf("management IP: %+v", mgmt)
	}
	if p := panelAccess("", ifaces); len(p.URLs) != 0 || p.URLs == nil {
		t.Fatalf("unknown listen: %+v", p)
	}
}
