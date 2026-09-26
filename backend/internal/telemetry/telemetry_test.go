package telemetry

import (
	"bufio"
	"strings"
	"testing"
)

const netDev = `Inter-|   Receive                                                |  Transmit
 face |bytes    packets errs drop fifo frame compressed multicast|bytes    packets errs drop fifo colls carrier compressed
    lo: 1000       10    0    0    0     0          0         0     1000       10    0    0    0     0       0          0
  eth0: 123456789  900    0    0    0     0          0        12  987654321   800    0    0    0     0       0          0
eth0.10: 5 1 0 0 0 0 0 0 7 1 0 0 0 0 0 0
`

func TestParseNetDev(t *testing.T) {
	rx, tx, ok := parseNetDev(bufio.NewScanner(strings.NewReader(netDev)), "eth0")
	if !ok || rx != 123456789 || tx != 987654321 {
		t.Fatalf("eth0: %d %d %v", rx, tx, ok)
	}
	rx, tx, ok = parseNetDev(bufio.NewScanner(strings.NewReader(netDev)), "eth0.10")
	if !ok || rx != 5 || tx != 7 {
		t.Fatalf("eth0.10: %d %d %v", rx, tx, ok)
	}
	if _, _, ok := parseNetDev(bufio.NewScanner(strings.NewReader(netDev)), "wlan0"); ok {
		t.Fatal("missing interface found")
	}
}

func TestSamplerMeasuresLoopback(t *testing.T) {
	n := NewNodeSampler()
	n.SetInterface("lo")
	n.sample()
	n.sample()
	s := n.Latest()
	if s.NetInterface != "lo" || s.NetRxBps < 0 || s.NetTxBps < 0 {
		t.Fatalf("sample %+v", s)
	}
}
