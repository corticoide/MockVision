package netctl

import "testing"

func TestHexIP(t *testing.T) {
	if got := hexIP("010200C0"); got != "192.0.2.1" {
		t.Fatalf("hexIP = %q", got)
	}
	if got := hexIP("00000000"); got != "" {
		t.Fatalf("zero gateway must be empty, got %q", got)
	}
}

func TestReadNodeInfo(t *testing.T) {
	info, err := ReadNodeInfo()
	if err != nil {
		t.Fatal(err)
	}
	t.Logf("default %s via %s", info.DefaultInterface, info.DefaultGateway)
	for _, i := range info.Interfaces {
		t.Logf("%+v", i)
	}
}
